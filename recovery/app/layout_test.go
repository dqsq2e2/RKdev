package main

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const systemStartSector = uint64(524288)

func testEmmcLayout() runtimeLayout {
	l := runtimeLayout{Schema: 1, BoardID: "fixture", Revision: "r1", SoC: "rk3588",
		Compatible: "example,fixture", Profile: "emmc", Device: "/dev/mmcblk4", Capacity: 512 << 20,
		LayoutID: strings.Repeat("11", 32), BootOrder: []string{"emmc", "usb"}}
	l.Loader.region, l.Loader.Format = region{32768, (16 << 20) - 32768}, "rockchip-sd"
	l.Environment.Offsets, l.Environment.Size = []int64{16 << 20, (16 << 20) + 131072}, 131072
	l.Recovery.region, l.Recovery.Label, l.Recovery.Partition = region{32 << 20, 234863616}, "TEST_RECOVERY", 1
	l.System.Offset, l.System.Label = int64(systemStartSector*512), "TEST_SYSTEM"
	return l
}

func TestEnvironmentAccessRejectsFactoryAndOverlappingGPT(t *testing.T) {
	l := testEmmcLayout()
	if err := l.validate(); err != nil {
		t.Fatal(err)
	}
	for _, parts := range [][]gptPartition{
		{{Number: 1, Start: 32768, End: 163839, Name: "boot"}},
		{{Number: 1, Start: 65536, End: 524253, Name: "ANOTHER_RECOVERY"}},
		{{Number: 1, Start: 65536, End: 524253, Name: l.Recovery.Label},
			{Number: 2, Start: 32768, End: 33023, Name: "unsafe"}},
		nil,
	} {
		if _, err := l.recoveryPartition(parts); err == nil {
			t.Fatalf("incorrect GPT accepted: %+v", parts)
		}
	}
	if _, err := l.recoveryPartition([]gptPartition{
		{Number: 1, Start: 65536, End: 524253, Name: l.Recovery.Label},
		{Number: 2, Start: 589824, End: 900000, Name: "system-boot"},
	}); err != nil {
		t.Fatal(err)
	}
}

func TestLayoutChecksRunningDeviceTreeAndOnDiskIdentity(t *testing.T) {
	l := testEmmcLayout()
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "chosen"), 0700); err != nil {
		t.Fatal(err)
	}
	write := func(key, value string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, key), []byte(value+"\x00"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	for key, value := range map[string]string{"board-id": l.BoardID, "revision": l.Revision, "profile": l.Profile,
		"layout-id": l.LayoutID, "schema": strconv.Itoa(l.Schema)} {
		write("chosen/rkdev,"+key, value)
	}
	write("compatible", l.Compatible+"\x00rockchip,rk3588")
	if err := l.checkDT(root); err != nil {
		t.Fatal(err)
	}
	write("chosen/rkdev,profile", "spi")
	if err := l.checkDT(root); err == nil {
		t.Fatal("wrong runtime profile accepted")
	}
	l.Recovery.Offset = 4096
	image := make([]byte, 8192)
	super := image[l.Recovery.Offset+1024:]
	binary.LittleEndian.PutUint16(super[56:], 0xef53)
	copy(super[120:], l.Recovery.Label)
	if err := l.checkRecoveryFS(bytes.NewReader(image)); err == nil {
		t.Fatal("same ext4 label accepted with the wrong layout UUID")
	}
	id, _ := hex.DecodeString(l.LayoutID[:32])
	copy(super[104:], id)
	if err := l.checkRecoveryFS(bytes.NewReader(image)); err != nil {
		t.Fatal(err)
	}
}

func TestEnvironmentCRCAndPendingBankFallback(t *testing.T) {
	l := testEmmcLayout()
	l.Environment.Offsets, l.Environment.Size = []int64{0, 512}, 512
	image := make([]byte, 1024)
	add := func(bank int, serial byte, pending bool) {
		t.Helper()
		values := map[string]string{"rkdev_layout": l.LayoutID, "bootcmd": "rkdevboot", "ethaddr": "12:34:56:78:90:ab"}
		if pending {
			values["rkdev_installing"] = "1"
		}
		blob, err := encodeEnvironment(values, 512, true, serial)
		if err != nil {
			t.Fatal(err)
		}
		copy(image[bank*512:], blob)
	}
	add(0, 255, true)
	add(1, 0, true)
	if e, err := l.readEnvironment(bytes.NewReader(image)); err != nil || e.bank != 1 || e.values["rkdev_installing"] != "1" {
		t.Fatalf("wrapped counter selected the wrong bank: %v %v", e, err)
	}
	image[512] ^= 1
	if e, err := l.readEnvironment(bytes.NewReader(image)); err != nil || e.bank != 0 || e.values["rkdev_installing"] != "1" {
		t.Fatalf("fallback would permit boot after a torn write: %v %v", e, err)
	}
	image[0] ^= 1
	if _, err := l.readEnvironment(bytes.NewReader(image)); !errors.Is(err, errEnvironmentCRC) {
		t.Fatalf("invalid CRC accepted: %v", err)
	}
}

func TestBackupGPTProtection(t *testing.T) {
	p := filepath.Join(t.TempDir(), "disk.img")
	sparseDisk(t, p, 8<<20, "-o", "-n", "1:2048:4095")
	f, err := os.OpenFile(p, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := checkGPTCopies(f, (16<<20)/512); err != nil {
		t.Fatal("initial image on a larger disk should be accepted:", err)
	}
	if _, err := f.WriteAt([]byte{0}, (8<<20)-512); err != nil {
		t.Fatal(err)
	}
	if err := checkGPTCopies(f, (16<<20)/512); err == nil {
		t.Fatal("damaged backup GPT accepted")
	}
}

func TestProtectedTargetAndEnvironment(t *testing.T) {
	for _, target := range []string{"/dev/mmcblk0p1", "/dev/mmcblk0boot0", "/dev/mtdblock0", "/dev/sda1"} {
		if validateFlashTarget(target) == nil {
			t.Fatalf("normal flash accepted %q", target)
		}
	}
	for _, name := range []string{"bootcmd", "-s", "rkdev_layout", "rkdev_installing", "preboot", "kernel_addr_r"} {
		w := httptest.NewRecorder()
		handleUbootSetenv(w, httptest.NewRequest(http.MethodPost, "/api/uboot_setenv",
			strings.NewReader(`{"name":"`+name+`","value":"0"}`)))
		if w.Code != http.StatusBadRequest {
			t.Fatalf("protected name %q was not rejected: %d", name, w.Code)
		}
	}
}
