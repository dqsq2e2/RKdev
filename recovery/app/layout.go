package main

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

type region struct {
	Offset int64 `json:"offset"`
	Length int64 `json:"length"`
}

type runtimeLayout struct {
	Schema     int      `json:"schema"`
	BoardID    string   `json:"board_id"`
	Revision   string   `json:"revision"`
	Name       string   `json:"name"`
	SoC        string   `json:"soc"`
	Compatible string   `json:"compatible"`
	Profile    string   `json:"profile"`
	LayoutID   string   `json:"layout_id"`
	Device     string   `json:"device"`
	Capacity   int64    `json:"capacity"`
	EraseSize  int64    `json:"erase_size,omitempty"`
	MTDName    string   `json:"mtd_name,omitempty"`
	BootOrder  []string `json:"boot_order"`
	Targets    []string `json:"system_targets"`
	ReleaseURL string   `json:"release_url"`
	Loader     struct {
		region
		Format string `json:"format"`
	} `json:"loader"`
	Environment struct {
		Offsets []int64 `json:"offsets"`
		Size    int64   `json:"size"`
	} `json:"environment"`
	Recovery struct {
		region
		Label       string `json:"label"`
		Partition   int    `json:"partition,omitempty"`
		LoadAddress int64  `json:"load_address,omitempty"`
	} `json:"recovery"`
	System struct {
		Offset int64  `json:"offset"`
		Label  string `json:"label"`
	} `json:"system,omitempty"`
}

var (
	currentLayout runtimeLayout
	identityError = errors.New("未加载 RKdev 板包布局")
	emmcNode      = regexp.MustCompile(`^/dev/mmcblk[0-9]+$`)
	mtdBlockNode  = regexp.MustCompile(`^/dev/mtdblock[0-9]+$`)
	layoutIDRE    = regexp.MustCompile(`^[a-f0-9]{64}$`)
	labelRE       = regexp.MustCompile(`^[A-Za-z0-9_-]{1,16}$`)
)

func decodeLayout(src io.Reader) (runtimeLayout, error) {
	var l runtimeLayout
	decoder := json.NewDecoder(src)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&l); err != nil {
		return l, err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return l, fmt.Errorf("布局 JSON 后有额外数据")
	}
	return l, l.validate()
}

func (l runtimeLayout) validate() error {
	if l.Schema != 1 || l.BoardID == "" || l.Revision == "" || l.Compatible == "" || l.SoC != "rk3588" ||
		!layoutIDRE.MatchString(l.LayoutID) || l.Capacity < 4<<20 || l.Capacity > 1<<40 ||
		!labelRE.MatchString(l.Recovery.Label) || len(l.Environment.Offsets) < 1 || len(l.Environment.Offsets) > 2 ||
		l.Environment.Size < 512 || l.Environment.Size > 1<<20 || l.Environment.Size%512 != 0 {
		return fmt.Errorf("RKdev 布局参数无效")
	}
	align := int64(512)
	regions := []region{l.Loader.region, l.Recovery.region}
	switch l.Profile {
	case "emmc":
		if !emmcNode.MatchString(l.Device) || l.Loader.Format != "rockchip-sd" || l.Recovery.Partition != 1 ||
			l.System.Offset < 512 || !labelRE.MatchString(l.System.Label) || l.EraseSize != 0 {
			return fmt.Errorf("eMMC 布局无效")
		}
		regions = append(regions, region{0, 34 * 512}, region{l.System.Offset, l.Capacity - l.System.Offset})
	case "spi":
		if !mtdBlockNode.MatchString(l.Device) || l.Loader.Format != "rockchip-spi" || l.Recovery.Partition != 0 ||
			l.EraseSize < 512 || l.EraseSize > 1<<20 || l.EraseSize&(l.EraseSize-1) != 0 || l.MTDName == "" ||
			l.System.Offset != 0 || len(l.Environment.Offsets) != 1 {
			return fmt.Errorf("SPI 布局无效")
		}
		align = l.EraseSize
	default:
		return fmt.Errorf("未知 RKdev 模式 %q", l.Profile)
	}
	for i, off := range l.Environment.Offsets {
		if i > 0 && off <= l.Environment.Offsets[i-1] {
			return fmt.Errorf("环境区顺序无效")
		}
		regions = append(regions, region{off, ((l.Environment.Size + align - 1) / align) * align})
	}
	for i, a := range regions {
		if a.Offset < 0 || a.Length <= 0 || a.Offset > l.Capacity || a.Length > l.Capacity-a.Offset ||
			a.Offset%align != 0 || a.Length%align != 0 {
			return fmt.Errorf("布局范围越界或未对齐")
		}
		for _, b := range regions[:i] {
			if a.Offset < b.Offset+b.Length && b.Offset < a.Offset+a.Length {
				return fmt.Errorf("布局范围重叠")
			}
		}
	}
	return l.validateOrder(l.BootOrder)
}

func (l runtimeLayout) validateOrder(order []string) error {
	if len(order) == 0 || len(order) > 5 {
		return fmt.Errorf("请选择至少一个可用的引导设备")
	}
	for i, target := range order {
		if !slices.Contains([]string{"emmc", "sd", "usb", "nvme", "sata"}, target) ||
			!slices.Contains(l.BootOrder, target) || slices.Contains(order[:i], target) {
			return fmt.Errorf("不支持或重复的引导设备 %q", target)
		}
	}
	return nil
}

func (l runtimeLayout) checkDT(root string) error {
	for key, value := range map[string]string{
		"board-id": l.BoardID, "revision": l.Revision, "profile": l.Profile,
		"layout-id": l.LayoutID, "schema": strconv.Itoa(l.Schema),
	} {
		actual, err := os.ReadFile(filepath.Join(root, "chosen", "rkdev,"+key))
		if err != nil || string(actual) != value+"\x00" {
			return fmt.Errorf("启动设备树的 %s 与板包不匹配", key)
		}
	}
	actual, err := os.ReadFile(filepath.Join(root, "compatible"))
	if err != nil || !slices.Contains(strings.Split(string(actual), "\x00"), l.Compatible) {
		return fmt.Errorf("启动设备树不匹配板包 compatible")
	}
	return nil
}

func loadRuntime() (runtimeLayout, error) {
	f, err := os.Open("/etc/rkdev-layout.json")
	if err != nil {
		return runtimeLayout{}, err
	}
	defer f.Close()
	l, err := decodeLayout(io.LimitReader(f, 1<<16))
	if err == nil {
		err = l.checkDT("/sys/firmware/devicetree/base")
	}
	return l, err
}

func sysValue(path string) string {
	raw, _ := os.ReadFile(path)
	return strings.TrimSpace(string(raw))
}

func sysInt(path string) int64 {
	n, _ := strconv.ParseInt(sysValue(path), 10, 64)
	return n
}

func (l runtimeLayout) checkRecoveryFS(src io.ReaderAt) error {
	var super [120]byte
	if _, err := src.ReadAt(super[:], l.Recovery.Offset+1024); err != nil {
		return err
	}
	uuid, err := hex.DecodeString(l.LayoutID)
	if err != nil || len(uuid) != 32 || binary.LittleEndian.Uint16(super[56:58]) != 0xef53 ||
		!bytes.Equal(super[104:120], uuid[:16]) {
		return fmt.Errorf("介质上的恢复文件系统与板包布局不匹配")
	}
	var label [16]byte
	if _, err = src.ReadAt(label[:], l.Recovery.Offset+1024+120); err != nil ||
		string(bytes.TrimRight(label[:], "\x00")) != l.Recovery.Label {
		return fmt.Errorf("介质上的恢复文件系统标签不匹配")
	}
	return nil
}

// A freshly written image can have its backup GPT at the image boundary.
// Accept that boundary as long as both copies agree and fit the physical disk.
func checkGPTCopies(f io.ReaderAt, sectors uint64) error {
	p, b := make([]byte, 512), make([]byte, 512)
	if _, err := f.ReadAt(p, 512); err != nil {
		return err
	}
	if _, err := readGPTReader(f); err != nil {
		return err
	}
	u64 := binary.LittleEndian.Uint64
	last := u64(p[32:])
	if u64(p[24:]) != 1 || last >= sectors || last < 67 || u64(p[40:]) < 34 || u64(p[48:]) >= last-32 {
		return fmt.Errorf("GPT 容量或保留区无效")
	}
	if _, err := f.ReadAt(b, int64(last*512)); err != nil {
		return err
	}
	size := binary.LittleEndian.Uint32(b[12:])
	backupEntries := u64(b[72:])
	if string(b[:8]) != "EFI PART" || size < 92 || size > 512 || u64(b[24:]) != last || u64(b[32:]) != 1 ||
		backupEntries != last-32 || !bytes.Equal(b[40:72], p[40:72]) || !bytes.Equal(b[80:92], p[80:92]) {
		return fmt.Errorf("GPT 主副本不一致")
	}
	check := binary.LittleEndian.Uint32(b[16:])
	clear(b[16:20])
	if crc32.ChecksumIEEE(b[:size]) != check {
		return fmt.Errorf("备份 GPT 表头 CRC 无效")
	}
	entries := make([]byte, int(binary.LittleEndian.Uint32(p[80:]))*128)
	if _, err := f.ReadAt(entries, int64(backupEntries*512)); err != nil || crc32.ChecksumIEEE(entries) != binary.LittleEndian.Uint32(p[88:]) {
		return fmt.Errorf("备份 GPT 分区表 CRC 无效")
	}
	return nil
}

func (l runtimeLayout) checkMedium(idle bool) error {
	if identityError != nil {
		return identityError
	}
	info, err := os.Lstat(l.Device)
	if err != nil || info.Mode()&os.ModeDevice == 0 || info.Mode()&os.ModeCharDevice != 0 {
		return fmt.Errorf("找不到布局指定的块设备 %s", l.Device)
	}
	name := filepath.Base(l.Device)
	block := "/sys/class/block/" + name
	if err := checkBlockDeviceIdentity(info, block); err != nil {
		return err
	}
	sectors, err := blockDeviceSectors(l.Device)
	if err != nil || sectors < uint64(l.Capacity/512) || sectors > 1<<40/512 {
		return fmt.Errorf("介质容量与板包不匹配")
	}
	if idle {
		if err := checkBlockIdle(l.Device); err != nil {
			return err
		}
	}
	f, err := os.Open(l.Device)
	if err != nil {
		return err
	}
	defer f.Close()
	if l.Profile == "spi" {
		mtd := "/sys/class/mtd/mtd" + strings.TrimPrefix(name, "mtdblock")
		erase := sysInt(mtd + "/erasesize")
		if sysValue(mtd+"/type") != "nor" || sysValue(mtd+"/name") != l.MTDName ||
			sectors != uint64(l.Capacity/512) || sysInt(mtd+"/size") != l.Capacity ||
			erase < 512 || l.EraseSize%erase != 0 || sysValue(mtd+"/numeraseregions") != "0" {
			return fmt.Errorf("SPI NOR 名称、容量或擦除几何不匹配")
		}
	} else {
		if sysValue(block+"/device/type") != "MMC" || sysInt(block+"/queue/logical_block_size") != 512 {
			return fmt.Errorf("设备不是预期的 512 字节扇区 eMMC")
		}
		if err := checkGPTCopies(f, sectors); err != nil {
			return err
		}
		parts, err := readGPTReader(f)
		if err != nil {
			return err
		}
		if _, err := l.recoveryPartition(parts); err != nil {
			return err
		}
		if sysInt(block+"/"+name+"p1/start") != l.Recovery.Offset/512 ||
			sysInt(block+"/"+name+"p1/size") != l.Recovery.Length/512 {
			return fmt.Errorf("内核分区表与板包不匹配")
		}
	}
	return l.checkRecoveryFS(f)
}

func (l runtimeLayout) startSector() uint64 { return uint64(l.System.Offset / 512) }
