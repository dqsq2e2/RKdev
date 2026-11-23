package main

import (
	"archive/zip"
	"bufio"
	"compress/bzip2"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/bodgit/sevenzip"
)

// A device name is not an identity. Diskseq changes when the same kernel
// name or major:minor is reused after hot unplug.
type mediaIdentity struct {
	Name, Node, Parent, Physical, Dev, Seq string
}

var mediaNodeName = regexp.MustCompile(`^(sd[a-z]+[0-9]*|nvme[0-9]+n[0-9]+(p[0-9]+)?|mmcblk[0-9]+(p[0-9]+)?|mtdblock[0-9]+)$`)

func identifyMedia(root, name string) (mediaIdentity, error) {
	if !mediaNodeName.MatchString(name) {
		return mediaIdentity{}, fmt.Errorf("无效的存储设备")
	}
	node, err := filepath.EvalSymlinks(filepath.Join(root, name))
	if err != nil {
		return mediaIdentity{}, fmt.Errorf("存储设备已拔出，请重新选择")
	}
	parent := node
	if sysValue(filepath.Join(node, "partition")) != "" {
		parent = filepath.Dir(node)
	}
	id := mediaIdentity{name, node, parent, parent, sysValue(filepath.Join(node, "dev")), sysValue(filepath.Join(parent, "diskseq"))}
	if strings.HasPrefix(filepath.Base(parent), "nvme") && wholeDiskName.MatchString(filepath.Base(parent)) {
		// Different namespaces can belong to the same drive.
		id.Physical = filepath.Dir(parent)
	}
	for p := parent; filepath.Dir(p) != p; p = filepath.Dir(p) {
		if sysValue(filepath.Join(p, "idVendor")) != "" && sysValue(filepath.Join(p, "idProduct")) != "" {
			id.Physical = p // include other LUNs of the same USB device
			break
		}
	}
	if id.Dev == "" || id.Seq == "" || id.Seq == "0" {
		return mediaIdentity{}, fmt.Errorf("无法确认设备身份，请重新选择")
	}
	return id, nil
}

func (id mediaIdentity) token() string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(id.Node+"\x00"+id.Dev+"\x00"+id.Seq)))
}

func (id mediaIdentity) check(f *os.File) error {
	actual, err := identifyMedia("/sys/class/block", id.Name)
	if err != nil || actual != id {
		return fmt.Errorf("存储设备已拔出或更换，请重新选择")
	}
	if f == nil {
		return nil
	}
	info, err := f.Stat()
	if err != nil || checkBlockDeviceIdentity(info, id.Node) != nil {
		return fmt.Errorf("打开的存储设备和所选设备不一致")
	}
	var seq uint64
	// BLKGETDISKSEQ, from Linux uapi/linux/fs.h.
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), 0x80081280, uintptr(unsafe.Pointer(&seq)))
	if errno != 0 || fmt.Sprint(seq) != id.Seq {
		return fmt.Errorf("打开的存储设备已更换")
	}
	return nil
}

type mediaRef struct {
	Name string `json:"name"`
	ID   string `json:"id"`
}

func (r mediaRef) identity(source bool) (mediaIdentity, error) {
	id, err := identifyMedia("/sys/class/block", r.Name)
	if err != nil || r.ID != id.token() {
		return mediaIdentity{}, fmt.Errorf("存储设备已拔出或更换，请重新选择")
	}
	if source && (!wholeDiskName.MatchString(filepath.Base(id.Parent)) || diskTargetType("/dev/"+filepath.Base(id.Parent)) == "") {
		return mediaIdentity{}, fmt.Errorf("不支持从此设备读取文件")
	}
	return id, nil
}

type mediaVolume struct {
	mediaRef
	Disk string `json:"disk"`
	Size string `json:"size"`
}

func mediaVolumes() []mediaVolume {
	volumes := []mediaVolume{}
	nodes, _ := os.ReadDir("/sys/class/block")
	for _, node := range nodes {
		id, err := identifyMedia("/sys/class/block", node.Name())
		if err != nil {
			continue
		}
		disk := filepath.Base(id.Parent)
		if wholeDiskName.MatchString(disk) && diskTargetType("/dev/"+disk) != "" {
			volumes = append(volumes, mediaVolume{mediaRef{id.Name, id.token()}, disk, formatBytes(sysInt(id.Node+"/size") * 512)})
		}
	}
	return volumes
}

type mountedMedia struct {
	id   mediaIdentity
	root *os.Root
	dev  *os.File
	dir  string
}

func mountMedia(ctx context.Context, ref mediaRef) (*mountedMedia, error) {
	id, err := ref.identity(true)
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile("/dev/"+id.Name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	m := &mountedMedia{id: id, dev: f}
	ok := false
	defer func() {
		if !ok {
			m.close()
		}
	}()
	if err := id.check(f); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "blkid", "-p", "-s", "TYPE", "-o", "value", "/proc/self/fd/3")
	cmd.ExtraFiles = []*os.File{f}
	raw, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("未识别到可读取的文件系统，请选择有文件系统的分区")
	}
	fsType, opts := strings.TrimSpace(string(raw)), ""
	switch fsType {
	case "ext3", "ext4":
		opts = "noload" // even a ro mount must not replay a journal
	case "ntfs":
		fsType = "ntfs3"
	case "vfat":
		opts = "utf8=1"
	case "exfat", "ext2":
	default:
		return nil, fmt.Errorf("不支持的源文件系统: %s", fsType)
	}
	if err := os.MkdirAll("/run/rkdev-media", 0700); err != nil {
		return nil, err
	}
	m.dir, err = os.MkdirTemp("/run/rkdev-media", "volume-")
	if err != nil {
		return nil, err
	}
	if err := syscall.Mount(fmt.Sprintf("/proc/self/fd/%d", f.Fd()), m.dir, fsType,
		syscall.MS_RDONLY|syscall.MS_NOSUID|syscall.MS_NODEV|syscall.MS_NOEXEC, opts); err != nil {
		return nil, fmt.Errorf("无法以只读方式挂载 %s: %w", fsType, err)
	}
	m.root, err = os.OpenRoot(m.dir)
	if err != nil {
		return nil, err
	}
	if err := id.check(f); err != nil {
		return nil, err
	}
	ok = true
	return m, nil
}

func (m *mountedMedia) close() {
	if m.root != nil {
		m.root.Close()
	}
	if m.dir != "" {
		_ = syscall.Unmount(m.dir, syscall.MNT_DETACH)
		_ = os.Remove(m.dir)
	}
	m.dev.Close()
}

func openMediaPath(root *os.Root, path string) (*os.File, os.FileInfo, error) {
	if path != "." && !filepath.IsLocal(path) || strings.ContainsAny(path, "\\\x00") || len(path) > 4096 {
		return nil, nil, fmt.Errorf("文件路径无效")
	}
	f, err := root.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, nil, err
	}
	info, err := f.Stat()
	if err != nil || (!info.Mode().IsRegular() && !info.IsDir()) {
		f.Close()
		return nil, nil, fmt.Errorf("请选择普通文件或目录")
	}
	return f, info, nil
}

func (m *mountedMedia) file(path string) (*os.File, int64, error) {
	f, info, err := openMediaPath(m.root, path)
	if err != nil {
		return nil, 0, err
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 {
		f.Close()
		return nil, 0, fmt.Errorf("请选择非空的普通文件")
	}
	return f, info.Size(), nil
}

type mediaFile struct {
	Name string `json:"name"`
	Dir  bool   `json:"dir"`
}

type mediaTarget struct {
	BlockDevice
	Ref mediaRef `json:"ref"`
}

func handleMedia(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		jsonErr(w, 405, "仅支持 GET")
		return
	}
	jsonOK(w, mediaVolumes())
}

var mediaBrowse = make(chan struct{}, 2)

func handleMediaFiles(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		jsonErr(w, 405, "仅支持 GET")
		return
	}
	select {
	case mediaBrowse <- struct{}{}:
		defer func() { <-mediaBrowse }()
	default:
		jsonErr(w, 409, "正在读取目录，请稍后刷新")
		return
	}
	q := r.URL.Query()
	m, err := mountMedia(r.Context(), mediaRef{q.Get("name"), q.Get("id")})
	if err != nil {
		jsonErr(w, 400, err.Error())
		return
	}
	defer m.close()
	path := q.Get("dir")
	if path == "" {
		path = "."
	}
	f, info, err := openMediaPath(m.root, path)
	if err != nil || !info.IsDir() {
		if f != nil {
			f.Close()
		}
		jsonErr(w, 400, "目录不可读")
		return
	}
	defer f.Close()
	entries, err := f.ReadDir(4096)
	if err != nil && err != io.EOF {
		jsonErr(w, 400, err.Error())
		return
	}
	files := []mediaFile{}
	for _, entry := range entries {
		if entry.IsDir() || entry.Type().IsRegular() && (mediaImageName(entry.Name()) || strings.EqualFold(filepath.Ext(entry.Name()), ".json")) {
			files = append(files, mediaFile{entry.Name(), entry.IsDir()})
		}
	}
	slices.SortFunc(files, func(a, b mediaFile) int {
		if a.Dir != b.Dir {
			if a.Dir {
				return -1
			}
			return 1
		}
		return strings.Compare(a.Name, b.Name)
	})
	jsonOK(w, files)
}

func mediaImageName(name string) bool {
	ext := strings.ToLower(filepath.Ext(name))
	lower := strings.ToLower(name)
	lower = strings.TrimSuffix(strings.TrimSuffix(lower, ".gz"), ".bz2")
	return ext == ".zip" || ext == ".7z" || strings.HasSuffix(lower, ".img") || strings.HasSuffix(lower, ".bin")
}

// The on-device file is already seekable. Read it in place, including random
// access archives, without creating a copy or unpacking to /tmp.
func openMediaImage(name string, size int64, input interface {
	io.Reader
	io.ReaderAt
}) (io.ReadCloser, int64, error) {
	archiveError := fmt.Errorf("设备存储压缩包须只包含一个 .img/.bin；多文件刷机包请从电脑上传")
	if !mediaImageName(name) {
		return nil, 0, archiveError
	}
	switch strings.ToLower(filepath.Ext(name)) {
	case ".gz":
		r, err := gzip.NewReader(input)
		return r, -1, err
	case ".bz2":
		return io.NopCloser(bzip2.NewReader(input)), -1, nil
	case ".zip":
		archive, err := zip.NewReader(input, size)
		if err != nil {
			return nil, 0, err
		}
		var image *zip.File
		for _, file := range archive.File {
			if file.FileInfo().IsDir() {
				continue
			}
			if _, err := checkFirmwareName(file.Name); err != nil || !file.Mode().IsRegular() ||
				image != nil || file.UncompressedSize64 > math.MaxInt64 {
				return nil, 0, archiveError
			}
			image = file
		}
		if image == nil {
			return nil, 0, archiveError
		}
		r, err := image.Open()
		return r, int64(image.UncompressedSize64), err
	case ".7z":
		archive, err := sevenzip.NewReader(input, size)
		if err != nil {
			return nil, 0, err
		}
		var image *sevenzip.File
		for _, file := range archive.File {
			if file.FileInfo().IsDir() {
				continue
			}
			if _, err := checkFirmwareName(file.Name); err != nil || !file.Mode().IsRegular() ||
				image != nil || file.UncompressedSize > math.MaxInt64 {
				return nil, 0, archiveError
			}
			image = file
		}
		if image == nil {
			return nil, 0, archiveError
		}
		r, err := image.Open()
		return r, int64(image.UncompressedSize), err
	default:
		return io.NopCloser(input), size, nil
	}
}

func mediaTargetAllowed(source, target mediaIdentity) error {
	if source.Physical == target.Physical || source.Parent == target.Parent {
		return fmt.Errorf("文件来源所在的整块物理盘不能用作写入目标")
	}
	return nil
}

func handleMediaTargets(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		jsonErr(w, 405, "仅支持 GET")
		return
	}
	q := r.URL.Query()
	source, err := (mediaRef{q.Get("name"), q.Get("id")}).identity(true)
	if err != nil {
		jsonErr(w, 400, err.Error())
		return
	}
	if q.Get("kind") == "upgrade" {
		if err := currentLayout.checkMedium(false); err != nil {
			jsonErr(w, 409, err.Error())
			return
		}
		target, err := identifyMedia("/sys/class/block", filepath.Base(currentLayout.Device))
		if err == nil {
			err = mediaTargetAllowed(source, target)
		}
		if err != nil {
			jsonErr(w, 409, err.Error())
			return
		}
		jsonOK(w, map[string]string{"target": currentLayout.Device})
		return
	}
	targets := []mediaTarget{}
	for _, d := range getFilteredDevices() {
		id, err := identifyMedia("/sys/class/block", d.Name)
		if err == nil && mediaTargetAllowed(source, id) == nil && checkFlashDiskIdle(d.Path) == nil {
			targets = append(targets, mediaTarget{d, mediaRef{id.Name, id.token()}})
		}
	}
	jsonOK(w, targets)
}

type mediaJob struct {
	Volume   mediaRef `json:"volume"`
	Target   mediaRef `json:"target"`
	Image    string   `json:"image"`
	Manifest string   `json:"manifest"`
}

type mediaJobState struct {
	ID      string `json:"id"`
	Kind    string `json:"kind"`
	Stage   string `json:"stage"`
	Image   string `json:"image"`
	Message string `json:"message,omitempty"`
	TaskID  string `json:"task_id,omitempty"`
}

var mediaStatus = struct {
	sync.Mutex
	state mediaJobState
}{}

func setMediaStatus(stage, message, task string) {
	mediaStatus.Lock()
	defer mediaStatus.Unlock()
	mediaStatus.state.Stage, mediaStatus.state.Message = stage, message
	if task != "" {
		mediaStatus.state.TaskID = task
	}
}

func handleMediaJob(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		jsonErr(w, 405, "仅支持 GET")
		return
	}
	mediaStatus.Lock()
	st := mediaStatus.state
	mediaStatus.Unlock()
	jsonOK(w, st)
}

func mediaRequest(w http.ResponseWriter, r *http.Request, kind string, work func(context.Context, mediaJob) (string, error)) {
	if r.Method != http.MethodPost {
		jsonErr(w, 405, "仅支持 POST")
		return
	}
	var job mediaJob
	if err := decodeSettingsRequest(w, r, &job, 12<<10); err != nil || job.Image == "" {
		jsonErr(w, 400, "请选择有效的源文件和目标")
		return
	}
	id := fmt.Sprintf("%x", time.Now().UnixNano())
	if !releaseWriteMu.TryLock() {
		jsonErr(w, 409, "已有刷写、备份或升级任务")
		return
	}
	st := mediaJobState{ID: id, Kind: kind, Stage: "preparing", Image: filepath.Base(job.Image)}
	mediaStatus.Lock()
	mediaStatus.state = st
	mediaStatus.Unlock()
	go func() {
		defer releaseWriteMu.Unlock()
		message, err := work(context.Background(), job)
		if err != nil {
			advLog("error", "设备存储任务失败: %v", err)
			setMediaStatus("error", err.Error(), "")
		} else {
			setMediaStatus("done", message, "")
		}
	}()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	jsonOK(w, st)
}

// Check a pinned source during streaming as well as immediately before writes.
type mediaReader struct {
	file *os.File
	src  *mountedMedia
	ctx  context.Context
	next time.Time
}

func (m *mediaReader) check() error {
	if err := m.ctx.Err(); err != nil {
		return err
	}
	if now := time.Now(); !now.Before(m.next) {
		m.next = now.Add(time.Second)
		return m.src.id.check(m.src.dev)
	}
	return nil
}

func (m *mediaReader) Read(p []byte) (int, error) {
	if err := m.check(); err != nil {
		return 0, err
	}
	n, err := m.file.Read(p)
	upAddBytes(n)
	return n, err
}

func (m *mediaReader) ReadAt(p []byte, off int64) (int, error) {
	if err := m.check(); err != nil {
		return 0, err
	}
	return m.file.ReadAt(p, off)
}

func handleMediaFlash(w http.ResponseWriter, r *http.Request) {
	mediaRequest(w, r, "flash", func(ctx context.Context, job mediaJob) (string, error) {
		source, err := mountMedia(ctx, job.Volume)
		if err != nil {
			return "", err
		}
		defer source.close()
		disk, err := job.Target.identity(false)
		if err != nil {
			return "", err
		}
		if err := mediaTargetAllowed(source.id, disk); err != nil {
			return "", err
		}
		file, size, err := source.file(job.Image)
		if err != nil {
			return "", err
		}
		defer file.Close()
		target, err := openStreamTargetPinned("/dev/"+disk.Name, &disk)
		if err != nil {
			return "", err
		}
		defer target.file.Close()
		guard := func(f *os.File) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := source.id.check(source.dev); err != nil {
				return err
			}
			return disk.check(f)
		}
		target.guard = func() error { return guard(target.file) }
		if err := target.check(); err != nil {
			return "", err
		}
		upBegin(size)
		defer upEnd()
		name := filepath.Base(job.Image)
		upSet(name, "streaming")
		image, expanded, err := openMediaImage(name, size, &mediaReader{file: file, src: source, ctx: ctx})
		if err != nil {
			return "", err
		}
		defer image.Close()
		if expanded == 0 || expanded > target.limit || (expanded > 0 && expanded%512 != 0) {
			return "", fmt.Errorf("镜像为空、未按扇区对齐或超过目标容量")
		}
		reader := bufio.NewReaderSize(contextReader{ctx, image}, streamProbeBytes)
		prefix, err := reader.Peek(streamProbeBytes)
		if err != nil && err != io.EOF {
			return "", err
		}
		if len(prefix) < 512 {
			return "", fmt.Errorf("镜像数据不足")
		}
		s, err := beginStreamImage(target, prefix, expanded)
		if err != nil {
			return "", err
		}
		setMediaStatus("writing", "", s.task.ID)
		err = s.copyFrom(reader)
		if err == nil {
			err = guard(target.file)
		}
		if err == nil {
			err = s.verify(ctx)
		}
		if err == nil {
			err = s.commit(ctx)
		}
		if err != nil {
			err = fmt.Errorf("流式刷写中断，目标可能不完整：%w", err)
			updateTask(s.task.ID, func(t *FlashTask) { t.Status, t.Error = "error", err.Error() })
			return "", err
		}
		updateTask(s.task.ID, func(t *FlashTask) {
			t.Status, t.Progress, t.Written, t.Total = "done", 100, s.written, s.written
		})
		return "刷写完成，SHA256 读回校验通过", nil
	})
}

func handleMediaUpgrade(w http.ResponseWriter, r *http.Request) {
	mediaRequest(w, r, "upgrade", func(ctx context.Context, job mediaJob) (string, error) {
		l := currentLayout
		if err := l.checkMedium(false); err != nil {
			return "", err
		}
		src, err := mountMedia(ctx, job.Volume)
		if err != nil {
			return "", err
		}
		defer src.close()
		disk, err := identifyMedia("/sys/class/block", filepath.Base(l.Device))
		if err != nil {
			return "", err
		}
		if err := mediaTargetAllowed(src.id, disk); err != nil {
			return "", fmt.Errorf("%w；请换一个文件来源", err)
		}
		meta, size, err := src.file(job.Manifest)
		if err != nil {
			return "", fmt.Errorf("请选择配套的 .json 升级清单：%w", err)
		}
		defer meta.Close()
		if size > 65536 || !strings.EqualFold(filepath.Ext(job.Manifest), ".json") || !strings.EqualFold(filepath.Ext(job.Image), ".img") {
			return "", fmt.Errorf("请选择配套的 .img 和 .json")
		}
		manifest, err := readReleaseManifest(meta, l)
		if err != nil {
			return "", err
		}
		image, size, err := src.file(job.Image)
		if err != nil {
			return "", err
		}
		defer image.Close()
		reader := &mediaReader{file: image, src: src, ctx: ctx}
		setMediaStatus("checking", "", "")
		if err := validateReleaseImageWith(reader, size, l, manifest, func() error {
			return checkMediaRecoveryPayload(ctx, image, l)
		}); err != nil {
			return "", err
		}
		setMediaStatus("writing", "", "")
		if err := l.applyRecoveryUpgradeGuarded(reader, func(f *os.File) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := src.id.check(src.dev); err != nil {
				return err
			}
			return disk.check(f)
		}); err != nil {
			return "", err
		}
		return "引导与恢复系统已升级并读回验证", nil
	})
}
