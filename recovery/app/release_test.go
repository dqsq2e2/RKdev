package main

import (
	"archive/zip"
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
	const version, base = "11.26-r7", "fixture-r1-spi-11.26-r7"
	manifestJSON, err := json.Marshal(map[string]any{
		"schema": 1, "kind": "recovery-image", "layout": l,
		"image":   map[string]any{"name": base + ".img", "bytes": l.Capacity, "sha256": sum(region{0, l.Capacity})},
		"sources": map[string]string{"version": version},
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
	makeZIP := func(imageName string) *bytes.Reader {
		t.Helper()
		var packed bytes.Buffer
		zw := zip.NewWriter(&packed)
		for name, data := range map[string][]byte{imageName: img, base + ".json": manifestJSON} {
			w, err := zw.Create(name)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := w.Write(data); err != nil {
				t.Fatal(err)
			}
		}
		if err := zw.Close(); err != nil {
			t.Fatal(err)
		}
		return bytes.NewReader(packed.Bytes())
	}
	packed := makeZIP(base + ".img")
	var unpacked bytes.Buffer
	fromZIP, n, err := unpackFirmwareZIP(packed, packed.Size(), &unpacked, l, version)
	if err != nil || !bytes.Equal(unpacked.Bytes(), img) || fromZIP.Image != m.Image {
		t.Fatalf("unpack firmware: size=%d manifest=%+v error=%v", n, fromZIP.Image, err)
	}
	wrong := makeZIP("another-board.img")
	if _, _, err := unpackFirmwareZIP(wrong, wrong.Size(), &unpacked, l, version); err == nil {
		t.Fatal("archive contained an unrelated image")
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

func TestSelectGitHubRelease(t *testing.T) {
	l := runtimeLayout{BoardID: "fixture", Revision: "v1", Profile: "spi", Capacity: 32 << 20}
	digest := strings.Repeat("ab", 32)
	release := func(board, profile, day, version string) githubFirmwareRelease {
		return githubFirmwareRelease{
			Tag: board + "-" + profile + "-" + day + "-v" + version,
			Assets: []githubReleaseAsset{{Name: board + "-v1-" + profile + "-" + version + ".zip", Size: 20 << 20,
				State: "uploaded", Digest: "sha256:" + digest}},
		}
	}
	draft := release(l.BoardID, l.Profile, "2026", "11.26-r15")
	draft.Draft = true
	noHash := release(l.BoardID, l.Profile, "2026", "11.26-r14")
	noHash.Assets[0].Digest = ""
	releases := []githubFirmwareRelease{
		draft, noHash, release(l.BoardID, "emmc", "2026", "11.26-r13"),
		release("other", l.Profile, "2026", "11.26-r12"), release(l.BoardID, l.Profile, "2025", "12.31-r20"),
		release(l.BoardID, l.Profile, "2026", "11.26-r9"), release(l.BoardID, l.Profile, "2026", "11.26-r10"),
	}
	got, err := selectGitHubRelease(releases, "fixture/boards", l)
	if err != nil || got.Version != "11.26-r10" || got.Image.SHA256 != digest ||
		got.download != "https://github.com/fixture/boards/releases/download/fixture-spi-2026-v11.26-r10/fixture-v1-spi-11.26-r10.zip" {
		t.Fatalf("selected wrong release: %+v, %v", got, err)
	}
	if _, err := selectGitHubRelease(releases[:4], "fixture/boards", l); err == nil {
		t.Fatal("selected a mismatched, draft or unverified release")
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
