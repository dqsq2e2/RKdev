package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"maps"
	"net/http"
	"os"
	"regexp"
	"slices"
	"strings"
)

var (
	errEnvironmentCRC = errors.New("U-Boot 环境 CRC 无效；可在确认后手动恢复默认值")
	envName           = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.#-]{0,127}$`)
)

type environment struct {
	values map[string]string
	serial byte
	bank   int
}

func decodeEnvironment(raw []byte, redundant bool) (environment, error) {
	var e environment
	start := 4
	if redundant {
		start++
	}
	if len(raw) < start+2 || crc32.ChecksumIEEE(raw[start:]) != binary.LittleEndian.Uint32(raw) {
		return e, errEnvironmentCRC
	}
	if redundant {
		e.serial = raw[4]
	}
	text, _, ok := bytes.Cut(raw[start:], []byte{0, 0})
	if !ok {
		return e, fmt.Errorf("U-Boot 环境没有结束符")
	}
	e.values = make(map[string]string)
	for _, row := range bytes.Split(text, []byte{0}) {
		key, value, ok := strings.Cut(string(row), "=")
		if !ok || key == "" {
			return e, fmt.Errorf("U-Boot 环境包含无效字段")
		}
		if _, exists := e.values[key]; exists {
			return e, fmt.Errorf("U-Boot 环境包含重复字段")
		}
		e.values[key] = value
	}
	return e, nil
}

func encodeEnvironment(values map[string]string, size int, redundant bool, serial byte) ([]byte, error) {
	start := 4
	if redundant {
		start++
	}
	var b bytes.Buffer
	for _, key := range slices.Sorted(maps.Keys(values)) {
		if key == "" || strings.ContainsAny(key, "=\x00") || strings.ContainsRune(values[key], 0) {
			return nil, fmt.Errorf("无效的环境变量 %q", key)
		}
		b.WriteString(key + "=" + values[key] + "\x00")
	}
	if size < start+b.Len()+1 {
		return nil, fmt.Errorf("U-Boot 环境空间不足")
	}
	raw := make([]byte, size)
	copy(raw[start:], b.Bytes())
	if redundant {
		raw[4] = serial
	}
	binary.LittleEndian.PutUint32(raw, crc32.ChecksumIEEE(raw[start:]))
	return raw, nil
}

// Match U-Boot env_check_redund, including the 255 -> 0 rollover.
func newestEnvironment(a, b environment) environment {
	if (a.serial == 255 && b.serial == 0) ||
		(!(b.serial == 255 && a.serial == 0) && b.serial > a.serial) {
		return b
	}
	return a
}

func (l runtimeLayout) readEnvironment(f io.ReaderAt) (environment, error) {
	var active environment
	redundant := len(l.Environment.Offsets) == 2
	for i, off := range l.Environment.Offsets {
		raw := make([]byte, l.Environment.Size)
		if _, err := f.ReadAt(raw, off); err != nil {
			// A failed device read must never be treated as a blank environment.
			return environment{}, err
		}
		e, err := decodeEnvironment(raw, redundant)
		if errors.Is(err, errEnvironmentCRC) {
			continue
		}
		if err != nil {
			return environment{}, err
		}
		e.bank = i
		if active.values == nil {
			active = e
		} else {
			active = newestEnvironment(active, e)
		}
	}
	if active.values == nil {
		return active, errEnvironmentCRC
	}
	if active.values["rkdev_layout"] != l.LayoutID {
		return environment{}, fmt.Errorf("当前 U-Boot 环境不属于此布局")
	}
	return active, nil
}

func (l runtimeLayout) loadEnvironment() (environment, error) {
	if err := l.checkMedium(false); err != nil {
		return environment{}, err
	}
	f, err := os.Open(l.Device)
	if err != nil {
		return environment{}, err
	}
	defer f.Close()
	return l.readEnvironment(f)
}

func (l runtimeLayout) defaultEnvironment() (map[string]string, error) {
	data, err := os.ReadFile("/etc/rkdev-default-env")
	if err != nil {
		return nil, err
	}
	values := make(map[string]string)
	for _, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if !ok || key == "" {
			return nil, fmt.Errorf("内置 U-Boot 环境格式无效")
		}
		values[key] = value
	}
	if values["rkdev_layout"] != l.LayoutID || values["bootcmd"] != "rkdevboot" ||
		!slices.Equal(strings.Fields(values["rkdev_targets"]), l.BootOrder) {
		return nil, fmt.Errorf("内置 U-Boot 环境标识不匹配")
	}
	return values, nil
}

func (l runtimeLayout) envConfig() string {
	var b strings.Builder
	for _, off := range l.Environment.Offsets {
		fmt.Fprintf(&b, "%s 0x%x 0x%x", l.Device, off, l.Environment.Size)
		if l.Profile == "spi" {
			fmt.Fprintf(&b, " 0x%x", l.EraseSize)
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// The caller holds releaseWriteMu. Switch to the inactive bank for eMMC.
// Before an in-place installation, mark both banks so fallback cannot boot it.
func (l runtimeLayout) changeEnvironment(both, recoverCRC bool, change func(map[string]string) (map[string]string, error)) error {
	if err := l.checkMedium(true); err != nil {
		return err
	}
	f, err := os.OpenFile(l.Device, os.O_RDWR|os.O_SYNC, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	active, err := l.readEnvironment(f)
	if recoverCRC && errors.Is(err, errEnvironmentCRC) {
		active = environment{values: map[string]string{}, bank: -1}
		if l.Profile == "emmc" {
			active.values["rkdev_installing"] = "1"
		}
		err = nil
	}
	if err != nil {
		return err
	}
	values, err := change(active.values)
	if err != nil {
		return err
	}
	if values["rkdev_layout"] != l.LayoutID || values["bootcmd"] != "rkdevboot" {
		return fmt.Errorf("禁止修改引导入口或布局 ID")
	}
	count := len(l.Environment.Offsets)
	for i := 0; i < count; i++ {
		active.bank, active.serial = (active.bank+1)%count, active.serial+1
		raw, err := encodeEnvironment(values, int(l.Environment.Size), count > 1, active.serial)
		if err != nil {
			return err
		}
		offset := l.Environment.Offsets[active.bank]
		if n, err := f.WriteAt(raw, offset); err != nil || n != len(raw) {
			return fmt.Errorf("环境变量写入失败: %v", err)
		}
		if err := f.Sync(); err != nil {
			return err
		}
		got := make([]byte, len(raw))
		if _, err := f.ReadAt(got, offset); err != nil || !bytes.Equal(got, raw) {
			return fmt.Errorf("环境变量写入后读回不一致")
		}
		if !both {
			break
		}
	}
	got, err := l.readEnvironment(f)
	if err != nil || !maps.Equal(got.values, values) {
		return fmt.Errorf("U-Boot 不会选用刚写入的环境变量")
	}
	return nil
}

func (l runtimeLayout) installationFlag(pending bool) error {
	return l.changeEnvironment(pending, false, func(values map[string]string) (map[string]string, error) {
		if pending {
			values["rkdev_installing"] = "1"
		} else {
			delete(values, "rkdev_installing")
		}
		return values, nil
	})
}

func (l runtimeLayout) protectedSystemHash() ([32]byte, error) {
	var checksum [32]byte
	f, err := os.Open(l.Device)
	if err != nil {
		return checksum, err
	}
	defer f.Close()
	start := int64(gptReservedSectors * 512)
	h := sha256.New()
	_, err = io.CopyN(h, io.NewSectionReader(f, start, l.System.Offset-start), l.System.Offset-start)
	copy(checksum[:], h.Sum(nil))
	return checksum, err
}

func (l runtimeLayout) beginSystemInstall() (func() error, error) {
	if err := l.installationFlag(true); err != nil {
		return nil, err
	}
	before, err := l.protectedSystemHash()
	if err != nil {
		return nil, err
	}
	return func() error {
		if after, err := l.protectedSystemHash(); err != nil || after != before {
			return fmt.Errorf("刷写后 eMMC 保留区发生变化；继续保留未完成标记")
		}
		return l.installationFlag(false)
	}, nil
}

type UbootVar struct {
	Name      string `json:"name"`
	Value     string `json:"value"`
	Protected bool   `json:"protected"`
}

func protectedEnv(name string) bool {
	return strings.HasPrefix(name, "rkdev_") || slices.Contains([]string{
		"bootcmd", "altbootcmd", "boot_targets", "preboot", "bootdelay", "bootlimit", "uenvcmd", ".flags", ".callbacks",
		"fdtfile", "scriptaddr", "kernel_addr_r", "fdt_addr_r", "ramdisk_addr_r", "fdtoverlay_addr_r",
		"kernel_comp_addr_r", "kernel_comp_size", "loadaddr", "initrd_high", "fdt_high",
	}, name)
}

func handleBootOrder(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		if !releaseWriteMu.TryLock() {
			jsonErr(w, http.StatusConflict, "已有写入任务")
			return
		}
		defer releaseWriteMu.Unlock()
		e, err := currentLayout.loadEnvironment()
		if err != nil {
			jsonErr(w, http.StatusConflict, err.Error())
			return
		}
		vars := make([]UbootVar, 0, len(e.values))
		for _, key := range slices.Sorted(maps.Keys(e.values)) {
			vars = append(vars, UbootVar{key, e.values[key], protectedEnv(key)})
		}
		order := strings.Fields(e.values["rkdev_targets"])
		if currentLayout.validateOrder(order) != nil {
			order = currentLayout.BootOrder
		}
		jsonOK(w, map[string]any{"vars": vars, "order": order, "available": currentLayout.BootOrder,
			"count": len(vars), "installing": e.values["rkdev_installing"] != ""})
	case http.MethodPost:
		var req struct {
			Order []string `json:"order"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
			jsonErr(w, http.StatusBadRequest, "启动顺序请求无效")
			return
		}
		if err := currentLayout.validateOrder(req.Order); err != nil {
			jsonErr(w, http.StatusBadRequest, err.Error())
			return
		}
		writeEnvResponse(w, false, func(values map[string]string) (map[string]string, error) {
			values["rkdev_targets"] = strings.Join(req.Order, " ")
			return values, nil
		})
	default:
		jsonErr(w, http.StatusMethodNotAllowed, "仅支持 GET/POST")
	}
}

func writeEnvResponse(w http.ResponseWriter, recoverCRC bool, change func(map[string]string) (map[string]string, error)) {
	if !releaseWriteMu.TryLock() {
		jsonErr(w, http.StatusConflict, "已有写入任务")
		return
	}
	defer releaseWriteMu.Unlock()
	if err := currentLayout.changeEnvironment(recoverCRC, recoverCRC, change); err != nil {
		jsonErr(w, http.StatusConflict, err.Error())
		return
	}
	jsonOK(w, map[string]string{"message": "U-Boot 环境变量已保存，重启后生效"})
}

func handleUbootSetenv(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonErr(w, http.StatusMethodNotAllowed, "仅支持 POST")
		return
	}
	var req struct{ Name, Value string }
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16384)).Decode(&req); err != nil ||
		!envName.MatchString(req.Name) || protectedEnv(req.Name) || strings.ContainsRune(req.Value, 0) {
		jsonErr(w, http.StatusBadRequest, "环境变量无效或受保护")
		return
	}
	writeEnvResponse(w, false, func(values map[string]string) (map[string]string, error) {
		values[req.Name] = req.Value
		return values, nil
	})
}

func handleUbootReset(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonErr(w, http.StatusMethodNotAllowed, "仅支持 POST")
		return
	}
	writeEnvResponse(w, true, func(values map[string]string) (map[string]string, error) {
		next, err := currentLayout.defaultEnvironment()
		if err == nil && values["rkdev_installing"] != "" {
			next["rkdev_installing"] = values["rkdev_installing"]
		}
		return next, err
	})
}
