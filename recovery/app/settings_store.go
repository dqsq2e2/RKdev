package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
)

const settingsSlotSize = 16 << 10
const settingsMagic = "RkCfgV1\x00"
const settingsHeaderSize = 52

type recoverySettings struct {
	Network  *networkConfig `json:"network,omitempty"`
	SSH      *sshConfig     `json:"ssh,omitempty"`
	Firewall *firewallSave  `json:"firewall,omitempty"`
}

type recoverySettingsStore struct {
	layout runtimeLayout
	offset int64
}

func newRecoverySettingsStore(l runtimeLayout, offset int64) (*recoverySettingsStore, error) {
	if l.Profile != "emmc" || offset%4096 != 0 ||
		offset < l.Loader.Offset+l.Loader.Length || offset > l.Recovery.Offset-2*settingsSlotSize {
		return nil, fmt.Errorf("配置区必须位于 eMMC 引导与恢复之间的保留区域")
	}
	for _, off := range l.Environment.Offsets {
		if off < offset+2*settingsSlotSize && offset < off+l.Environment.Size {
			return nil, fmt.Errorf("配置区与 U-Boot 环境重叠")
		}
	}
	store := &recoverySettingsStore{l, offset}
	if _, err := store.read(); err != nil {
		return nil, err
	}
	return store, nil
}

type settingsSlot struct {
	Gen   uint64
	Value recoverySettings
}

func settingsDigest(layout string, header []byte, payload []byte) []byte {
	h := sha256.New()
	h.Write([]byte(layout))
	h.Write(header)
	h.Write(payload)
	return h.Sum(nil)
}

func decodeSettingsSlot(raw []byte, layout string) (settingsSlot, error) {
	var slot settingsSlot
	if !bytes.HasPrefix(raw, []byte(settingsMagic)) {
		if bytes.Count(raw, []byte{0}) == len(raw) || bytes.Count(raw, []byte{0xff}) == len(raw) {
			return slot, nil
		}
		return slot, fmt.Errorf("配置区出现未知数据，禁止覆盖")
	}
	length := int(binary.LittleEndian.Uint32(raw[16:20]))
	gen := binary.LittleEndian.Uint64(raw[8:16])
	if length <= 0 || length > len(raw)-settingsHeaderSize || gen == 0 {
		return slot, nil // interrupted record; the other slot remains usable
	}
	data := raw[settingsHeaderSize : settingsHeaderSize+length]
	if !bytes.Equal(raw[20:52], settingsDigest(layout, raw[:20], data)) {
		return slot, nil
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(&slot.Value); err != nil {
		return settingsSlot{}, fmt.Errorf("不支持的配置区记录")
	}
	slot.Gen = gen
	return slot, nil
}

func encodeSettingsSlot(s settingsSlot, layout string) ([]byte, error) {
	payload, err := json.Marshal(s.Value)
	if err != nil {
		return nil, err
	}
	if len(payload) > settingsSlotSize-settingsHeaderSize || s.Gen == 0 {
		return nil, fmt.Errorf("恢复配置超出保存区容量")
	}
	raw := make([]byte, settingsSlotSize)
	copy(raw, settingsMagic)
	binary.LittleEndian.PutUint64(raw[8:16], s.Gen)
	binary.LittleEndian.PutUint32(raw[16:20], uint32(len(payload)))
	copy(raw[20:52], settingsDigest(layout, raw[:20], payload))
	copy(raw[settingsHeaderSize:], payload)
	return raw, nil
}

func (s *recoverySettingsStore) load(f io.ReaderAt) (settingsSlot, int, error) {
	var latest settingsSlot
	active := -1
	for i := 0; i < 2; i++ {
		buf := make([]byte, settingsSlotSize)
		if _, err := f.ReadAt(buf, s.offset+int64(i)*settingsSlotSize); err != nil {
			return latest, active, err
		}
		slot, err := decodeSettingsSlot(buf, s.layout.LayoutID)
		if err != nil {
			return latest, active, err
		}
		if slot.Gen > latest.Gen {
			latest, active = slot, i
		}
	}
	return latest, active, nil
}

func (s *recoverySettingsStore) read() (recoverySettings, error) {
	if err := s.layout.checkMedium(false); err != nil {
		return recoverySettings{}, err
	}
	f, err := os.Open(s.layout.Device)
	if err != nil {
		return recoverySettings{}, err
	}
	defer f.Close()
	slot, _, err := s.load(f)
	return slot.Value, err
}

func (s *recoverySettingsStore) save(edit func(*recoverySettings)) error {
	if !releaseWriteMu.TryLock() {
		return fmt.Errorf("已有刷写、备份或升级任务")
	}
	defer releaseWriteMu.Unlock()
	l := s.layout
	if err := l.checkMedium(true); err != nil {
		return err
	}
	f, err := os.OpenFile(l.Device, os.O_RDWR|os.O_SYNC|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	stat, err := f.Stat()
	if err != nil || checkBlockDeviceIdentity(stat, "/sys/class/block/"+filepath.Base(l.Device)) != nil {
		return fmt.Errorf("配置区设备身份不匹配")
	}
	active, slot, err := s.load(f)
	if err != nil {
		return err
	}
	edit(&active.Value)
	active.Gen++
	raw, err := encodeSettingsSlot(active, l.LayoutID)
	if err != nil {
		return err
	}
	off := s.offset + int64((slot+1)%2)*settingsSlotSize
	if _, err := f.WriteAt(raw, off); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	readback := make([]byte, len(raw))
	if _, err := f.ReadAt(readback, off); err != nil || !bytes.Equal(raw, readback) {
		return fmt.Errorf("配置区写入后读回不一致")
	}
	return nil
}
