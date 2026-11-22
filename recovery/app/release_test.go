package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestReleaseProfileAndPayloadValidation(t *testing.T) {
	l := runtimeLayout{
		Schema: 1, BoardID: "fixture", Revision: "r1", Name: "Fixture", SoC: "rk3588",
		Compatible: "example,fixture", Profile: "spi", Device: "/dev/mtdblock9",
		Capacity: 5 << 20, EraseSize: 32768, MTDName: "fixture",
		LayoutID: strings.Repeat("12", 32), BootOrder: []string{"usb"},
	}
	l.Loader.region, l.Loader.Format = region{32768, (1 << 19) - 32768}, "rockchip-spi"
	l.Environment.Offsets, l.Environment.Size = []int64{1 << 19}, 8192
	l.Recovery.region, l.Recovery.Label, l.Recovery.LoadAddress = region{1 << 20, 4 << 20}, "FIXTURE", 1<<30
	if err := l.validate(); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	content := filepath.Join(dir, "contents")
	if err := os.MkdirAll(filepath.Join(content, "extlinux"), 0700); err != nil {
		t.Fatal(err)
	}
	write := func(name string, data []byte) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(content, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	layoutJSON, err := json.Marshal(l)
	if err != nil {
		t.Fatal(err)
	}
	write("layout.json", layoutJSON)
	for _, file := range []string{"rkdev.dtb", "Image.uimg", "initramfs.cpio.gz", "extlinux/extlinux.conf"} {
		write(file, []byte("fixture\n"))
	}
	recovery := filepath.Join(dir, "recovery.ext4")
	if out, err := exec.Command("mkfs.ext4", "-q", "-F", "-L", l.Recovery.Label, "-U",
		"12121212-1212-1212-1212-121212121212", "-d", content, recovery, "4096").CombinedOutput(); err != nil {
		t.Fatalf("mkfs.ext4: %v: %s", err, out)
	}
	fs, err := os.ReadFile(recovery)
	if err != nil {
		t.Fatal(err)
	}
	img := make([]byte, l.Capacity)
	copy(img[l.Loader.Offset:], "RKNS")
	copy(img[l.Recovery.Offset:], fs)
	env, err := encodeEnvironment(map[string]string{"rkdev_layout": l.LayoutID, "bootcmd": "rkdevboot"},
		int(l.Environment.Size), false, 0)
	if err != nil {
		t.Fatal(err)
	}
	copy(img[l.Environment.Offsets[0]:], env)
	sum := func(area region) string {
		t.Helper()
		got, err := rangeSHA(bytes.NewReader(img), area)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	manifestJSON, err := json.Marshal(map[string]any{
		"schema": 1, "kind": "recovery-image", "layout": l,
		"image": map[string]any{"name": "fixture.img", "bytes": l.Capacity, "sha256": sum(region{0, l.Capacity})},
		"extents": []map[string]any{
			{"kind": "loader", "offset": l.Loader.Offset, "length": 4, "sha256": sum(region{l.Loader.Offset, 4})},
			{"kind": "environment", "offset": l.Environment.Offsets[0], "length": l.Environment.Size,
				"sha256": sum(region{l.Environment.Offsets[0], l.Environment.Size})},
			{"kind": "recovery", "offset": l.Recovery.Offset, "length": l.Recovery.Length, "sha256": sum(l.Recovery.region)},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, other := range []runtimeLayout{
		func() runtimeLayout { other := l; other.BoardID = "another-board"; return other }(),
		func() runtimeLayout { other := l; other.Profile = "emmc"; return other }(),
	} {
		if _, err := readReleaseManifest(bytes.NewReader(manifestJSON), other); err == nil {
			t.Fatal("release manifest accepted for a different identity")
		}
	}
	m, err := readReleaseManifest(bytes.NewReader(manifestJSON), l)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateReleaseImage(bytes.NewReader(img), int64(len(img)), l, m); err != nil {
		t.Fatal(err)
	}
	img[l.Recovery.Offset+1024+104] ^= 1 // corrupt recovery layout UUID
	if err := validateReleaseImage(bytes.NewReader(img), int64(len(img)), l, m); err == nil {
		t.Fatal("whole-image damage accepted")
	}
	m.Image.SHA256 = sum(region{0, l.Capacity})
	if err := validateReleaseImage(bytes.NewReader(img), int64(len(img)), l, m); err == nil {
		t.Fatal("bad extent accepted with a refreshed whole-image hash")
	}
	m.Extents[2].SHA256 = sum(l.Recovery.region)
	if err := validateReleaseImage(bytes.NewReader(img), int64(len(img)), l, m); err == nil {
		t.Fatal("wrong recovery UUID accepted with refreshed image and extent hashes")
	}
}

func TestUpgradeWritesExcludeEnvironmentAndOtherReservedBytes(t *testing.T) {
	l := testEmmcLayout()
	p := filepath.Join(t.TempDir(), "disk.img")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := f.Truncate(l.Capacity); err != nil {
		t.Fatal(err)
	}
	before, err := l.preservedReleaseHash(f)
	if err != nil {
		t.Fatal(err)
	}
	for _, area := range l.upgradeRegions() {
		if _, err := f.WriteAt([]byte{7}, area.Offset+area.Length-1); err != nil {
			t.Fatal(err)
		}
	}
	if after, err := l.preservedReleaseHash(f); err != nil || before != after {
		t.Fatal("upgrade overlaps reserved bytes or failed to hash them:", err)
	}
	for _, off := range append([]int64{0}, l.Environment.Offsets...) {
		if _, err := f.WriteAt([]byte{1}, off); err != nil {
			t.Fatal(err)
		}
		if after, err := l.preservedReleaseHash(f); err != nil || before == after {
			t.Fatalf("change at reserved offset %#x went undetected: %v", off, err)
		}
		if _, err := f.WriteAt([]byte{0}, off); err != nil {
			t.Fatal(err)
		}
	}
}
