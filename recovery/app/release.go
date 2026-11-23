package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"regexp"
	"slices"
	"sync"
	"time"
)

var releaseWriteMu sync.Mutex

func validateSystemImage(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	head := make([]byte, 65536+8)
	n, _ := io.ReadFull(f, head)
	if n >= 520 && string(head[512:520]) == "EFI PART" {
		return fmt.Errorf("不能把整盘 GPT 镜像直接写入单个分区，请从固件刷写页面写入系统空间")
	}
	if n >= 1082 && head[1080] == 0x53 && head[1081] == 0xef {
		return nil
	}
	if n == len(head) && string(head[65536:65544]) == "_BHRfS_M" {
		return nil
	}
	return fmt.Errorf("需要包含启动配置的 ext4 或 Btrfs 系统分区镜像")
}

type releaseManifest struct {
	Schema int           `json:"schema"`
	Kind   string        `json:"kind"`
	Layout runtimeLayout `json:"layout"`
	Image  struct {
		Name   string `json:"name"`
		Bytes  int64  `json:"bytes"`
		SHA256 string `json:"sha256"`
	} `json:"image"`
	Extents []struct {
		region
		Kind   string `json:"kind"`
		SHA256 string `json:"sha256"`
	} `json:"extents"`
	Sources struct {
		Version string `json:"version"`
	} `json:"sources"`
}

var releaseFileRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,135}\.img$`)

func sameLayout(a, b runtimeLayout) bool {
	return a.Schema == b.Schema && a.BoardID == b.BoardID && a.Revision == b.Revision && a.SoC == b.SoC &&
		a.Compatible == b.Compatible && a.Profile == b.Profile && a.LayoutID == b.LayoutID &&
		a.Device == b.Device && a.Capacity == b.Capacity && a.EraseSize == b.EraseSize && a.MTDName == b.MTDName &&
		a.Loader == b.Loader && a.Recovery == b.Recovery && a.System == b.System &&
		a.Environment.Size == b.Environment.Size && slices.Equal(a.Environment.Offsets, b.Environment.Offsets)
}

func readReleaseManifest(reader io.Reader, l runtimeLayout) (releaseManifest, error) {
	var m releaseManifest
	d := json.NewDecoder(io.LimitReader(reader, 65537))
	if err := d.Decode(&m); err != nil {
		return m, fmt.Errorf("无法读取升级清单: %w", err)
	}
	if err := d.Decode(new(any)); err != io.EOF || m.Schema != 1 || m.Kind != "recovery-image" ||
		!sameLayout(l, m.Layout) || m.Image.Bytes != l.Capacity || !releaseFileRE.MatchString(m.Image.Name) ||
		!layoutIDRE.MatchString(m.Image.SHA256) {
		return m, fmt.Errorf("升级包板型、版本、模式、布局或签发格式不匹配")
	}
	expected := []region{l.Loader.region}
	kinds := []string{"loader"}
	for _, off := range l.Environment.Offsets {
		expected, kinds = append(expected, region{off, l.Environment.Size}), append(kinds, "environment")
	}
	expected, kinds = append(expected, l.Recovery.region), append(kinds, "recovery")
	if len(expected) != len(m.Extents) {
		return m, fmt.Errorf("升级清单缺少镜像区段")
	}
	for i, e := range m.Extents {
		if e.Kind != kinds[i] || e.Offset != expected[i].Offset || !layoutIDRE.MatchString(e.SHA256) ||
			e.Length <= 0 || (i == 0 && e.Length > expected[i].Length) || (i != 0 && e.Length != expected[i].Length) {
			return m, fmt.Errorf("升级清单的 %s 区段无效", kinds[i])
		}
	}
	return m, nil
}

func rangeSHA(f io.ReaderAt, area region) (string, error) {
	h := sha256.New()
	if _, err := io.CopyN(h, io.NewSectionReader(f, area.Offset, area.Length), area.Length); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func validateReleaseImage(src io.ReaderAt, n int64, l runtimeLayout, m releaseManifest) error {
	return validateReleaseImageWith(src, n, l, m, func() error { return validateRecoveryPayload(src, l) })
}

func validateReleaseImageWith(src io.ReaderAt, n int64, l runtimeLayout, m releaseManifest, checkPayload func() error) error {
	if n != l.Capacity {
		return fmt.Errorf("升级镜像容量不正确")
	}
	var header [4]byte
	if _, err := src.ReadAt(header[:], l.Loader.Offset); err != nil || string(header[:]) != "RKNS" {
		return fmt.Errorf("升级包缺少 RK3588 loader")
	}
	if got, err := rangeSHA(src, region{0, n}); err != nil || got != m.Image.SHA256 {
		return fmt.Errorf("升级镜像 SHA256 不匹配")
	}
	for _, e := range m.Extents {
		if got, err := rangeSHA(src, e.region); err != nil || got != e.SHA256 {
			return fmt.Errorf("升级镜像 %s 区段校验失败", e.Kind)
		}
		if e.Kind == "environment" {
			raw := make([]byte, e.Length)
			if _, err := src.ReadAt(raw, e.Offset); err != nil {
				return err
			}
			env, err := decodeEnvironment(raw, len(l.Environment.Offsets) == 2)
			if err != nil || env.values["rkdev_layout"] != l.LayoutID || env.values["bootcmd"] != "rkdevboot" {
				return fmt.Errorf("升级镜像的环境 ABI 不匹配")
			}
		}
	}
	if l.Profile == "emmc" {
		if err := checkGPTCopies(src, uint64(n/512)); err != nil {
			return err
		}
		parts, err := readGPTReader(src)
		if err != nil {
			return err
		}
		if _, err := l.recoveryPartition(parts); err != nil {
			return err
		}
		if len(parts) != 2 || parts[1].Number != 2 || parts[1].Start != l.startSector() || parts[1].Name != l.System.Label {
			return fmt.Errorf("升级包不是配套的恢复镜像")
		}
	}
	if err := l.checkRecoveryFS(src); err != nil {
		return err
	}
	return checkPayload()
}

func validateRecoveryPayload(src io.ReaderAt, l runtimeLayout) error {
	fs, err := os.CreateTemp("/tmp", "rkdev-ext4-*")
	if err != nil {
		return err
	}
	defer os.Remove(fs.Name())
	_, copyErr := io.CopyN(fs, io.NewSectionReader(src, l.Recovery.Offset, l.Recovery.Length), l.Recovery.Length)
	closeErr := fs.Close()
	if copyErr != nil || closeErr != nil {
		return fmt.Errorf("无法暂存恢复分区: %v %v", copyErr, closeErr)
	}
	return checkRecoveryPayload(context.Background(), fs.Name(), nil, l)
}

func checkRecoveryPayload(ctx context.Context, path string, device *os.File, l runtimeLayout) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	command := func(name string, args ...string) *exec.Cmd {
		cmd := exec.CommandContext(ctx, name, append(args, path)...)
		if device != nil {
			cmd.ExtraFiles = []*os.File{device}
		}
		return cmd
	}
	if err := command("e2fsck", "-fn").Run(); err != nil {
		return fmt.Errorf("恢复文件系统检查失败: %w", err)
	}
	kernel := "/Image"
	if l.Profile == "spi" {
		kernel = "/Image.uimg"
	}
	for _, name := range []string{kernel, "/rkdev.dtb", "/initramfs.cpio.gz", "/extlinux/extlinux.conf"} {
		out, err := command("debugfs", "-R", "stat "+name).Output()
		if err != nil || !bytes.Contains(out, []byte("Inode:")) {
			return fmt.Errorf("恢复文件系统缺少 %s", name)
		}
	}
	out, err := command("debugfs", "-R", "cat /layout.json").Output()
	if err != nil {
		return err
	}
	other, err := decodeLayout(bytes.NewReader(out))
	if err != nil || !sameLayout(l, other) {
		return fmt.Errorf("恢复文件系统内置布局与目标不匹配")
	}
	return nil
}

func (l runtimeLayout) upgradeRegions() []region { return []region{l.Recovery.region, l.Loader.region} }

func (l runtimeLayout) preservedReleaseHash(src io.ReaderAt) (string, error) {
	limit := l.Capacity
	if l.Profile == "emmc" {
		limit = l.System.Offset
	}
	h := sha256.New()
	start := int64(0)
	for _, area := range []region{l.Loader.region, l.Recovery.region, {Offset: limit}} {
		if _, err := io.CopyN(h, io.NewSectionReader(src, start, area.Offset-start), area.Offset-start); err != nil {
			return "", err
		}
		start = area.Offset + area.Length
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func (l runtimeLayout) applyRecoveryUpgrade(src io.ReaderAt) error {
	return l.applyRecoveryUpgradeGuarded(src, nil)
}

func (l runtimeLayout) applyRecoveryUpgradeGuarded(src io.ReaderAt, guard func(*os.File) error) error {
	if err := l.checkMedium(true); err != nil {
		return err
	}
	if _, err := l.loadEnvironment(); err != nil {
		return err
	}
	f, err := os.OpenFile(l.Device, os.O_RDWR|os.O_SYNC, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	if guard != nil {
		if err := guard(f); err != nil {
			return err
		}
	}
	before, err := l.preservedReleaseHash(f)
	if err != nil {
		return err
	}
	for _, area := range l.upgradeRegions() {
		if guard != nil {
			if err := guard(f); err != nil {
				return err
			}
		}
		advLog("info", "[UPDATE] 写入并读回区段 0x%x + 0x%x", area.Offset, area.Length)
		if _, err := f.Seek(area.Offset, io.SeekStart); err != nil {
			return err
		}
		if _, err := io.CopyN(f, io.NewSectionReader(src, area.Offset, area.Length), area.Length); err != nil {
			return err
		}
		if err := f.Sync(); err != nil {
			return err
		}
		want, err := rangeSHA(src, area)
		if err != nil {
			return err
		}
		if got, err := rangeSHA(f, area); err != nil || got != want {
			return fmt.Errorf("更新区段 0x%x 读回校验失败", area.Offset)
		}
	}
	if after, err := l.preservedReleaseHash(f); err != nil || after != before {
		return fmt.Errorf("升级后保留区校验失败")
	}
	return l.checkMedium(false)
}

func handleRecoveryUpgrade(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonErr(w, http.StatusMethodNotAllowed, "仅支持 POST")
		return
	}
	l := currentLayout
	if err := l.checkMedium(false); err != nil {
		jsonErr(w, http.StatusConflict, err.Error())
		return
	}
	if !releaseWriteMu.TryLock() {
		jsonErr(w, http.StatusConflict, "已有写入任务")
		return
	}
	defer releaseWriteMu.Unlock()
	r.Body = http.MaxBytesReader(w, r.Body, l.Capacity+(1<<20))
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		jsonErr(w, http.StatusBadRequest, err.Error())
		return
	}
	defer r.MultipartForm.RemoveAll()
	meta, header, err := r.FormFile("manifest")
	if err != nil || header.Size > 65536 {
		if err == nil {
			meta.Close()
		}
		jsonErr(w, http.StatusBadRequest, "请选择对应的 .json 升级清单")
		return
	}
	defer meta.Close()
	m, err := readReleaseManifest(meta, l)
	if err != nil {
		jsonErr(w, http.StatusBadRequest, err.Error())
		return
	}
	src, image, err := r.FormFile("package")
	if err != nil {
		jsonErr(w, http.StatusBadRequest, "请选择对应的 .img 升级镜像")
		return
	}
	defer src.Close()
	if err := validateReleaseImage(src, image.Size, l, m); err != nil {
		jsonErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if r.Context().Err() != nil {
		return
	}
	if err := l.applyRecoveryUpgrade(src); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	jsonOK(w, map[string]string{"message": "引导与恢复系统已升级并读回验证，保留原系统和环境变量", "sha256": m.Image.SHA256})
}

var releaseHTTP = &http.Client{
	Timeout: 30 * time.Minute,
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 8 || req.URL.Scheme != "https" || req.URL.User != nil {
			return fmt.Errorf("升级下载必须使用 HTTPS")
		}
		return nil
	},
}

func getRelease(ctx context.Context, link string) (*http.Response, error) {
	u, err := url.Parse(link)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
		return nil, fmt.Errorf("升级源未配置有效的 HTTPS 地址")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := releaseHTTP.Do(req)
	if err == nil && resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("升级源返回 %s", resp.Status)
	}
	return resp, err
}

func (l runtimeLayout) fetchRelease(ctx context.Context) (releaseManifest, error) {
	resp, err := getRelease(ctx, l.ReleaseURL)
	if err != nil {
		return releaseManifest{}, err
	}
	defer resp.Body.Close()
	return readReleaseManifest(resp.Body, l)
}

type updateStatus struct {
	Stage   string `json:"stage"`
	Bytes   int64  `json:"bytes"`
	Total   int64  `json:"total"`
	Message string `json:"message"`
}

var onlineUpdate struct {
	sync.Mutex
	status updateStatus
	cancel context.CancelFunc
}

func onlineReady(w http.ResponseWriter) bool {
	if currentLayout.ReleaseURL == "" {
		jsonErr(w, http.StatusNotFound, "此板包未配置在线升级源")
		return false
	}
	if err := currentLayout.checkMedium(false); err != nil {
		jsonErr(w, http.StatusConflict, err.Error())
		return false
	}
	return true
}

func handleUpdateCheck(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		jsonErr(w, http.StatusMethodNotAllowed, "仅支持 GET")
		return
	}
	if !onlineReady(w) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	m, err := currentLayout.fetchRelease(ctx)
	if err != nil {
		jsonErr(w, http.StatusBadGateway, err.Error())
		return
	}
	jsonOK(w, map[string]any{"version": m.Sources.Version, "image": m.Image, "current": buildTime})
}

func handleUpdateDownload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonErr(w, http.StatusMethodNotAllowed, "仅支持 POST")
		return
	}
	if !onlineReady(w) {
		return
	}
	var req struct{ SHA256 string }
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&req); err != nil || !layoutIDRE.MatchString(req.SHA256) {
		jsonErr(w, http.StatusBadRequest, "请先检查并确认要安装的升级版本")
		return
	}
	if !releaseWriteMu.TryLock() {
		jsonErr(w, http.StatusConflict, "已有写入任务")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	onlineUpdate.Lock()
	onlineUpdate.status = updateStatus{Stage: "checking"}
	onlineUpdate.cancel = cancel
	onlineUpdate.Unlock()
	go func(l runtimeLayout) {
		defer releaseWriteMu.Unlock()
		defer cancel()
		err := onlineUpgrade(ctx, l, req.SHA256)
		onlineUpdate.Lock()
		defer onlineUpdate.Unlock()
		onlineUpdate.cancel = nil
		switch {
		case err == nil:
			onlineUpdate.status.Stage, onlineUpdate.status.Message = "done", "升级完成，读回校验通过"
		case ctx.Err() != nil:
			onlineUpdate.status.Stage, onlineUpdate.status.Message = "cancelled", "下载已取消或超时"
		default:
			onlineUpdate.status.Stage, onlineUpdate.status.Message = "error", err.Error()
		}
	}(currentLayout)
	jsonOK(w, map[string]string{"message": "正在下载并验证升级包"})
}

type releaseProgressWriter struct{ io.Writer }

func (p releaseProgressWriter) Write(b []byte) (int, error) {
	n, err := p.Writer.Write(b)
	onlineUpdate.Lock()
	onlineUpdate.status.Bytes += int64(n)
	onlineUpdate.Unlock()
	return n, err
}

func onlineUpgrade(ctx context.Context, l runtimeLayout, confirmedHash string) error {
	m, err := l.fetchRelease(ctx)
	if err != nil {
		return err
	}
	if m.Image.SHA256 != confirmedHash {
		return fmt.Errorf("在线版本已变更，请重新检查")
	}
	u, _ := url.Parse(l.ReleaseURL)
	u.Path = path.Join(path.Dir(u.Path), m.Image.Name)
	u.RawPath, u.RawQuery, u.Fragment = "", "", ""
	resp, err := getRelease(ctx, u.String())
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	f, err := os.CreateTemp("/tmp", "rkdev-update-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	onlineUpdate.Lock()
	onlineUpdate.status.Stage, onlineUpdate.status.Total = "downloading", m.Image.Bytes
	onlineUpdate.Unlock()
	n, err := io.Copy(releaseProgressWriter{f}, io.LimitReader(resp.Body, m.Image.Bytes+1))
	if err != nil || n != m.Image.Bytes {
		return fmt.Errorf("下载不完整或镜像大小不匹配: %v", err)
	}
	onlineUpdate.Lock()
	onlineUpdate.status.Stage = "verifying"
	onlineUpdate.Unlock()
	if err := validateReleaseImage(f, n, l, m); err != nil {
		return err
	}
	onlineUpdate.Lock()
	if err := ctx.Err(); err != nil {
		onlineUpdate.Unlock()
		return err
	}
	onlineUpdate.cancel = nil
	onlineUpdate.status.Stage = "writing"
	onlineUpdate.Unlock()
	return l.applyRecoveryUpgrade(f)
}

func handleUpdateProgress(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		jsonErr(w, http.StatusMethodNotAllowed, "仅支持 GET")
		return
	}
	onlineUpdate.Lock()
	status := onlineUpdate.status
	onlineUpdate.Unlock()
	jsonOK(w, status)
}

func handleUpdateCancel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonErr(w, http.StatusMethodNotAllowed, "仅支持 POST")
		return
	}
	onlineUpdate.Lock()
	defer onlineUpdate.Unlock()
	if onlineUpdate.cancel == nil {
		jsonErr(w, http.StatusConflict, "当前没有可取消的下载")
		return
	}
	onlineUpdate.cancel()
	jsonOK(w, map[string]string{"message": "已请求取消下载"})
}
