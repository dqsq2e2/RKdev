package main

import (
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestMediaPhysicalOwnership(t *testing.T) {
	root := t.TempDir()
	class := filepath.Join(root, "class")
	if err := os.Mkdir(class, 0700); err != nil {
		t.Fatal(err)
	}
	makeNode := func(name, rel, dev, seq string) mediaIdentity {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(p, 0700); err != nil {
			t.Fatal(err)
		}
		for k, v := range map[string]string{"dev": dev, "diskseq": seq} {
			if v != "" {
				if err := os.WriteFile(filepath.Join(p, k), []byte(v), 0600); err != nil {
					t.Fatal(err)
				}
			}
		}
		if seq == "" {
			if err := os.WriteFile(filepath.Join(p, "partition"), []byte("1"), 0600); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.Symlink(p, filepath.Join(class, name)); err != nil {
			t.Fatal(err)
		}
		id, err := identifyMedia(class, name)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	e := makeNode("mmcblk0", "mmc/mmcblk0", "179:0", "7")
	e1 := makeNode("mmcblk0p1", "mmc/mmcblk0/mmcblk0p1", "179:1", "")
	e2 := makeNode("mmcblk0p2", "mmc/mmcblk0/mmcblk0p2", "179:2", "")
	ad := filepath.Join(root, "usb", "2-1")
	if err := os.MkdirAll(ad, 0700); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"idVendor", "idProduct"} {
		if err := os.WriteFile(filepath.Join(ad, k), []byte("1234"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	usb1 := makeNode("sda", "usb/2-1/one/block/sda", "8:0", "8")
	usb2 := makeNode("sdb", "usb/2-1/two/block/sdb", "8:16", "9")
	nvme1 := makeNode("nvme0n1", "nvme/nvme0/nvme0n1", "259:0", "10")
	nvme2 := makeNode("nvme0n2", "nvme/nvme0/nvme0n2", "259:1", "11")
	for _, pair := range [][2]mediaIdentity{{e1, e}, {e1, e2}, {usb1, usb2}, {nvme1, nvme2}} {
		if mediaTargetAllowed(pair[0], pair[1]) == nil {
			t.Fatal("same physical medium allowed", pair)
		}
	}
	if err := mediaTargetAllowed(usb1, e); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(usb1.Node, "diskseq"), []byte("99"), 0600); err != nil {
		t.Fatal(err)
	}
	newID, err := identifyMedia(class, "sda")
	if err != nil || newID.token() == usb1.token() {
		t.Fatal("reused block name kept old identity", err)
	}
}

func TestMediaStreamsWithoutStaging(t *testing.T) {
	sample := bytes.Repeat([]byte{0x31, 0x77, 0xb2, 0x15}, 250*1024)
	for _, ext := range []string{".img", ".img.gz", ".zip"} {
		t.Run(ext, func(t *testing.T) {
			var source bytes.Buffer
			switch ext {
			case ".img":
				source.Write(sample)
			case ".img.gz":
				z := gzip.NewWriter(&source)
				if _, err := z.Write(sample); err != nil {
					t.Fatal(err)
				}
				if err := z.Close(); err != nil {
					t.Fatal(err)
				}
			case ".zip":
				z := zip.NewWriter(&source)
				r, err := z.Create("folder/firmware.img")
				if err != nil {
					t.Fatal(err)
				}
				if _, err := r.Write(sample); err != nil {
					t.Fatal(err)
				}
				if err := z.Close(); err != nil {
					t.Fatal(err)
				}
			}
			p := filepath.Join(t.TempDir(), "target")
			f, err := os.OpenFile(p, os.O_RDWR|os.O_CREATE, 0600)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			t.Setenv("TMPDIR", filepath.Join(t.TempDir(), "unused"))
			in, size, err := openMediaImage("source"+ext, int64(source.Len()), bytes.NewReader(source.Bytes()))
			if err != nil {
				t.Fatal(err)
			}
			defer in.Close()
			// Exercise the exact buffered, hashed writer used by both frontends.
			s, err := beginStreamImage(&streamTarget{path: p, file: f, limit: 2 << 20}, sample[:streamProbeBytes], size)
			if err != nil {
				t.Fatal(err)
			}
			if err := s.copyFrom(in); err != nil {
				t.Fatal(err)
			}
			if err := s.verify(context.Background()); err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(p)
			if err != nil || !bytes.Equal(got, sample) {
				t.Fatal("target differs from source", err)
			}
			if _, err := os.Stat(os.Getenv("TMPDIR")); !os.IsNotExist(err) {
				t.Fatal("temporary staging was created")
			}
		})
	}
}

func TestMediaRejectsTruncatedCompressedInput(t *testing.T) {
	var buf bytes.Buffer
	z := gzip.NewWriter(&buf)
	z.Write(bytes.Repeat([]byte{0x33}, 512*1024))
	z.Close()
	in, size, err := openMediaImage("system.img.gz", int64(buf.Len()-5), bytes.NewReader(buf.Bytes()[:buf.Len()-5]))
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	f, err := os.Create(filepath.Join(t.TempDir(), "target"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	s, err := beginStreamImage(&streamTarget{path: f.Name(), file: f, limit: 2 << 20}, []byte{}, size)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.copyFrom(in); err == nil {
		t.Fatal("truncated compressed stream accepted")
	}
}

func TestMediaPinnedGPT(t *testing.T) {
	if _, err := exec.LookPath("sgdisk"); err != nil {
		t.Skip("sgdisk fixture unavailable")
	}
	p := filepath.Join(t.TempDir(), "target.img")
	const base = 16 << 20
	const expanded = 32 << 20
	sparseDisk(t, p, base, "-o", "-n", "1:2048:4095", "-c", "1:Recovery", "-n", "2:8192:10000", "-c", "2:旧分区")
	old, err := readGPT(p)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(p, os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := f.Truncate(expanded); err != nil {
		t.Fatal(err)
	}
	const sectors = expanded / 512
	sentinel := []byte("board recovery bytes")
	const offset = 2048 * 512
	if _, err := f.WriteAt(sentinel, offset); err != nil {
		t.Fatal(err)
	}
	abort := fmt.Errorf("identity changed")
	if err := rewritePinnedGPT(f, sectors, nil, false, func() error { return abort }); err != abort {
		t.Fatalf("identity guard ignored: %v", err)
	}
	if err := rewritePinnedGPT(f, sectors, nil, false, nil); err != nil {
		t.Fatal(err)
	}
	mut := old[1]
	mut.Start, mut.End = 10001, 12000
	mut.Name = "New system"
	parts := []gptPartition{old[0], mut}
	if err := rewritePinnedGPT(f, sectors, parts, true, nil); err != nil {
		t.Fatal(err)
	}
	got, err := readGPTReader(f)
	if err != nil || !sameGPT(got, parts) {
		t.Fatalf("wrong table: %+v: %v", got, err)
	}
	after := make([]byte, len(sentinel))
	if _, err := f.ReadAt(after, offset); err != nil || !bytes.Equal(after, sentinel) {
		t.Fatal("recovery overwritten", err)
	}
	if out, err := exec.Command("sgdisk", "-v", p).CombinedOutput(); err != nil || !bytes.Contains(out, []byte("No problems found")) {
		t.Fatalf("updated GPT failed independent check: %v %s", err, out)
	}
}

func TestMediaRecoveryExtentLoop(t *testing.T) {
	fixture := os.Getenv("RKDEV_MEDIA_TEST_IMAGE")
	if fixture == "" {
		t.Skip("requires a recovery fixture and privileged loop devices")
	}
	meta, err := os.Open(fixture[:len(fixture)-len(".img")] + ".json")
	if err != nil {
		t.Fatal(err)
	}
	defer meta.Close()
	var m releaseManifest
	if err := json.NewDecoder(meta).Decode(&m); err != nil {
		t.Fatal(err)
	}
	image, err := os.Open(fixture)
	if err != nil {
		t.Fatal(err)
	}
	defer image.Close()
	t.Setenv("TMPDIR", filepath.Join(t.TempDir(), "unused"))
	if err := checkMediaRecoveryPayload(context.Background(), image, m.Layout); err != nil {
		t.Fatal(err)
	}
	m.Layout.Recovery.Offset += 512
	if err := checkMediaRecoveryPayload(context.Background(), image, m.Layout); err == nil {
		t.Fatal("invalid embedded filesystem accepted")
	}
}
