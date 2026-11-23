package main

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"slices"
	"strings"
	"syscall"
	"unicode/utf16"
)

const pinnedEntryCount = 128

func writeGPTGuid(dst []byte, guid string) error {
	buf, err := hex.DecodeString(strings.ReplaceAll(guid, "-", ""))
	if err != nil || len(buf) != 16 {
		return fmt.Errorf("GPT 分区 UUID 无效")
	}
	slices.Reverse(buf[:4])
	slices.Reverse(buf[4:6])
	slices.Reverse(buf[6:8])
	copy(dst, buf)
	return nil
}

// Keep the physical device pinned after writing the image. Partition tools
// that canonicalize /proc/.../fd/N may reopen a replacement device by name.
func rewritePinnedGPT(f interface {
	io.ReaderAt
	io.WriterAt
}, sectors uint64, parts []gptPartition, project bool, guard func() error) error {
	existing, err := readGPTReader(f)
	if err != nil {
		return err
	}
	var mbr, primary [512]byte
	if _, err := f.ReadAt(mbr[:], 0); err != nil {
		return err
	}
	if _, err := f.ReadAt(primary[:], 512); err != nil {
		return err
	}
	if sectors < 68 || mbr[510] != 0x55 || mbr[511] != 0xaa {
		return fmt.Errorf("GPT 保护 MBR 无效")
	}
	u64, u32 := binary.LittleEndian.Uint64, binary.LittleEndian.Uint32
	set64, set32 := binary.LittleEndian.PutUint64, binary.LittleEndian.PutUint32
	last := sectors - 1
	nextLastUsable := last - gptBackupSectors
	count := int(u32(primary[80:]))
	entries := make([]byte, pinnedEntryCount*128)
	if project {
		count = pinnedEntryCount
		used := map[int]bool{}
		for _, p := range parts {
			if p.Number < 1 || p.Number > count || used[p.Number] {
				return fmt.Errorf("GPT 分区编号无效")
			}
			used[p.Number] = true
			entry := entries[(p.Number-1)*128 : p.Number*128]
			if err := writeGPTGuid(entry[:16], p.Type); err != nil {
				return err
			}
			if err := writeGPTGuid(entry[16:32], p.UUID); err != nil {
				return err
			}
			set64(entry[32:], p.Start)
			set64(entry[40:], p.End)
			set64(entry[48:], p.Attributes)
			name := utf16.Encode([]rune(p.Name))
			if len(name) > (128-56)/2 {
				return fmt.Errorf("GPT 分区标签过长")
			}
			for i, r := range name {
				binary.LittleEndian.PutUint16(entry[56+2*i:], r)
			}
		}
	} else {
		parts = existing
		if _, err := f.ReadAt(entries[:count*128], 2*512); err != nil {
			return err
		}
	}
	for _, p := range parts {
		if p.Start < u64(primary[40:]) || p.End < p.Start || p.End > nextLastUsable {
			return fmt.Errorf("调整 GPT 后分区超出位置范围")
		}
	}
	protect := -1
	for i := range 4 {
		off := 446 + i*16
		if mbr[off+4] == 0xee && u32(mbr[off+8:]) == 1 {
			protect = off
			break
		}
	}
	if protect < 0 {
		return fmt.Errorf("缺少 GPT 保护分区")
	}
	set32(mbr[protect+12:], uint32(min(last, uint64(0xffffffff))))
	set64(primary[24:], 1)
	set64(primary[32:], last)
	set64(primary[48:], nextLastUsable)
	set32(primary[80:], uint32(count))
	set32(primary[88:], crc32.ChecksumIEEE(entries[:count*128]))
	backup := primary
	set64(backup[24:], last)
	set64(backup[32:], 1)
	backupEntries := last - 32
	set64(backup[72:], backupEntries)
	for _, header := range [][]byte{primary[:], backup[:]} {
		clear(header[16:20])
		set32(header[16:], crc32.ChecksumIEEE(header[:u32(header[12:])]))
	}
	for _, extent := range []struct {
		lba  uint64
		data []byte
	}{
		{backupEntries, entries}, {last, backup[:]}, {2, entries}, {1, primary[:]}, {0, mbr[:]},
	} {
		if guard != nil {
			if err := guard(); err != nil {
				return err
			}
		}
		if _, err := f.WriteAt(extent.data, int64(extent.lba*512)); err != nil {
			return err
		}
	}
	if err := checkGPTCopies(f, sectors); err != nil {
		return err
	}
	got, err := readGPTReader(f)
	if err != nil || !sameGPT(parts, got) {
		return fmt.Errorf("GPT 分区表读回不一致")
	}
	return nil
}

func rereadPinnedGPT(f *os.File) error {
	if err := f.Sync(); err != nil {
		return err
	}
	_, _, err := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), 0x125f, 0) // BLKRRPART
	if err != 0 {
		return fmt.Errorf("数据已写入，内核未能重新读取分区表: %w", err)
	}
	return nil
}
