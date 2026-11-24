package main

import (
	"fmt"
	"maps"
	"net/url"
	"path"
	"slices"
	"strings"
)

type setupBootState struct {
	base setupBootSnapshot
	edit setupBootEdit
}

func (b *setupBootState) changed() bool {
	return b.edit.Defaults || len(b.edit.Vars) != 0 || !slices.Equal(b.edit.Order, b.base.Order)
}

func (b *setupBootState) reset() {
	b.edit = setupBootEdit{Order: slices.Clone(b.base.Order), Revision: b.base.Revision}
}

func (u *setupUI) loadBoot() {
	u.showBoot()
	var v setupBootSnapshot
	u.bootIO.read(u, "setup/boot", &v, func(err error) {
		if err == nil && !u.boot.changed() && u.dialog == nil {
			u.boot.base = v
			u.boot.reset()
		}
		if u.page == setupPageBoot {
			u.fail(err)
			u.showBoot()
		}
	})
}

const setupEnvGroup = "U-Boot 环境变量"

func (u *setupUI) bootDefaults() {
	b := &u.boot
	if len(b.base.Defaults) == 0 {
		u.message = "未找到板包默认引导设置"
		return
	}
	u.confirm("载入默认引导设置", "替换引导顺序和环境变量草稿。", func() {
		b.reset()
		b.edit.Defaults = true
		b.edit.Order = slices.Clone(b.base.DefaultOrder)
		u.selected, u.scrolled = 0, 0
		u.showBoot()
	})
}

func (u *setupUI) showBoot() {
	b := &u.boot
	defaults := setupRow{"载入默认引导设置", "", u.bootDefaults}
	if b.base.Revision == "" {
		u.define("正在读取引导设置…")
		return
	}
	if b.base.Problem != "" && !b.edit.Defaults {
		u.define(b.base.Problem, defaults)
		return
	}
	if u.group == setupEnvGroup {
		rows := []setupRow{{"添加变量", "", func() {
			u.change("变量名称", "", false, func(name string) {
				name = strings.TrimSpace(name)
				u.editBootVar(name, u.bootVarValue(name))
			})
		}}}
		vars := b.visibleVars()
		for _, name := range slices.Sorted(maps.Keys(vars)) {
			v := vars[name]
			row := setupRow{label: name, detail: v.Value}
			if !v.Protected {
				row.run = func() { u.editBootVar(name, v.Value) }
			} else {
				row.detail = "[只读] " + v.Value
			}
			rows = append(rows, row)
		}
		u.define("", rows...)
		return
	}
	var rows []setupRow
	for i, d := range b.edit.Order {
		rows = append(rows, setupRow{fmt.Sprintf("启动项 #%d", i+1), setupBootLabel(d), func() { u.chooseBootRank(d) }})
	}
	for _, d := range b.base.Available {
		if slices.Contains(b.edit.Order, d) {
			continue
		}
		rows = append(rows, setupRow{"已禁用", setupBootLabel(d), func() { u.chooseBootRank(d) }})
	}
	rows = append(rows, setupRow{setupEnvGroup, "", func() {
		u.group = setupEnvGroup
		u.selected, u.scrolled = 0, 0
		u.showBoot()
	}}, defaults)
	hint := ""
	if b.base.Installing {
		hint = "eMMC 安装尚未完成，U-Boot 将跳过系统空间。"
	}
	u.define(hint, rows...)
}

func setupBootLabel(target string) string {
	switch target {
	case "emmc":
		return "eMMC"
	case "sd":
		return "SD"
	case "usb":
		return "USB"
	case "nvme":
		return "NVMe"
	case "sata":
		return "SATA"
	case "recovery":
		return "恢复系统"
	}
	return target
}

func (u *setupUI) chooseBootRank(device string) {
	order := u.boot.edit.Order
	cur := slices.Index(order, device)
	count := len(order)
	if cur < 0 {
		count++
	}
	var choices []setupOption
	for i := range count {
		choices = append(choices, setupOption{fmt.Sprint(i), fmt.Sprintf("启动项 #%d", i+1)})
	}
	if cur < 0 || len(order) > 1 {
		choices = append(choices, setupOption{"-1", "禁用"})
	}
	if cur < 0 {
		cur = len(choices) - 1
	}
	u.choose(setupBootLabel(device), choices, cur, func(value string) {
		for i, option := range choices {
			if option.key == value {
				if value == "-1" {
					i = -1
				}
				u.setBootRank(device, i)
				return
			}
		}
	})
}

func (u *setupUI) setBootRank(device string, rank int) {
	edit := &u.boot.edit
	if i := slices.Index(edit.Order, device); i >= 0 {
		if rank < 0 && len(edit.Order) == 1 {
			return
		}
		edit.Order = slices.Delete(edit.Order, i, i+1)
	}
	if rank >= 0 {
		rank = min(rank, len(edit.Order))
		edit.Order = slices.Insert(edit.Order, rank, device)
		u.selected = rank
	}
	u.showBoot()
}

func (u *setupUI) moveBoot(delta int) {
	pos := u.selected
	order := u.boot.edit.Order
	if pos >= len(order) || pos+delta < 0 || pos+delta >= len(order) {
		return
	}
	u.setBootRank(order[pos], pos+delta)
}

func (b *setupBootState) sourceVars() []UbootVar {
	if b.edit.Defaults {
		return b.base.Defaults
	}
	return b.base.Vars
}

func (b *setupBootState) visibleVars() map[string]UbootVar {
	vars := make(map[string]UbootVar)
	for _, v := range b.sourceVars() {
		vars[v.Name] = v
	}
	for name, value := range b.edit.Vars {
		vars[name] = UbootVar{Name: name, Value: value}
	}
	vars["rkdev_targets"] = UbootVar{"rkdev_targets", strings.Join(b.edit.Order, " "), true}
	return vars
}

func (u *setupUI) bootVarValue(name string) string {
	return u.boot.visibleVars()[name].Value
}

func (u *setupUI) editBootVar(name, old string) {
	if !envName.MatchString(name) || protectedEnv(name) {
		u.message = "环境变量名称无效或受保护"
		return
	}
	u.change(name, old, false, func(value string) {
		b := &u.boot
		for _, v := range b.sourceVars() {
			if name == v.Name && value == v.Value {
				delete(b.edit.Vars, name)
				u.showBoot()
				return
			}
		}
		if b.edit.Vars == nil {
			b.edit.Vars = make(map[string]string)
		}
		b.edit.Vars[name] = value
		u.showBoot()
	})
}

type setupMediaState struct {
	volumes              []mediaVolume
	source               mediaRef
	dir, image, manifest string
	files                []mediaFile
	targets              []mediaTarget
	target               mediaTarget
	job                  mediaJobState
	jobDetail            string
	upgradeBlocked       string
	loading              bool
}

func (u *setupUI) upgradePage() bool { return u.page == setupPageUpdate }

func (u *setupUI) mediaPage() bool { return u.page == setupPageFlash || u.upgradePage() }

func (u *setupUI) loadMediaPage() {
	u.mediaRevision++
	u.media = setupMediaState{}
	u.define("正在读取…")
	var volumes []mediaVolume
	u.mediaListIO.read(u, "media", &volumes, func(err error) {
		if u.mediaPage() {
			u.fail(err)
			u.media.volumes = volumes
			u.showMedia()
		}
	})
	u.updateMedia()
}

func (u *setupUI) updateMedia() {
	if u.mediaJobIO.reading || u.mediaStarting {
		return
	}
	u.mediaJobIO.reading = true
	revision := u.mediaJobIO.revision
	var j mediaJobState
	detail := ""
	u.async(func() error {
		if err := u.api("media/job", nil, &j); err != nil {
			return err
		}
		if j.TaskID != "" && j.Stage != "done" && j.Stage != "error" {
			var p uploadState
			if u.api("upload_status", nil, &p) == nil && p.TaskID == j.TaskID && p.Written > 0 {
				detail = " · " + formatBytes(p.Written)
				if p.ExpandedTotal > 0 {
					detail += " / " + formatBytes(p.ExpandedTotal)
				}
			}
		}
		return nil
	}, func(err error) {
		u.mediaJobIO.reading = false
		if err != nil || revision != u.mediaJobIO.revision || !u.mediaPage() ||
			(u.upgradePage() && j.Kind != "upgrade") || (!u.upgradePage() && j.Kind != "flash") {
			return
		}
		m := &u.media
		if m.job != j || m.jobDetail != detail {
			m.job, m.jobDetail = j, detail
			u.showMedia()
		}
	})
}

func (u *setupUI) showMedia() {
	m := &u.media
	var rows []setupRow
	var hint string
	if m.source.ID == "" {
		hint = "选择保存镜像的 U 盘或其他设备分区。"
		for _, v := range m.volumes {
			rows = append(rows, setupRow{"/dev/" + v.Name, v.Size, func() {
				m.source = v.mediaRef
				m.dir = "."
				m.targets = nil
				m.target = mediaTarget{}
				m.image, m.manifest, m.upgradeBlocked = "", "", ""
				u.group = "设备文件"
				u.browseMedia(".", true)
			}})
		}
		if len(m.volumes) == 0 {
			rows = append(rows, setupRow{"未发现可读取的存储", "插入后按 F5 刷新", nil})
		}
	} else if u.group == "写入目标" {
		hint = "来源所在整块盘已排除；选择另一块设备。"
		for _, t := range m.targets {
			rows = append(rows, setupRow{t.Path, t.Type + " · " + t.Size, func() {
				m.target = t
				u.group = "设备文件"
				u.selected = 0
				u.showMedia()
			}})
		}
		if len(rows) == 0 {
			rows = append(rows, setupRow{"没有可用的异盘目标", "", nil})
		}
	} else {
		dir := m.dir
		if dir == "." {
			dir = ""
		}
		hint = "/dev/" + m.source.Name + "   /" + dir
		rows = append(rows, setupRow{"镜像", path.Base(m.image), nil})
		if u.upgradePage() {
			rows = append(rows, setupRow{"配套 JSON", path.Base(m.manifest), nil})
			if m.upgradeBlocked != "" {
				rows = append(rows, setupRow{m.upgradeBlocked, "", nil})
			} else if m.image != "" && m.manifest != "" && !u.mediaStarting {
				rows = append(rows, setupRow{"从设备升级", "保留系统与环境变量", u.startMedia})
			}
		} else {
			rows = append(rows, setupRow{"写入目标", m.target.Path, func() {
				u.group = "写入目标"
				u.selected = 0
				u.showMedia()
			}})
			if m.image != "" && m.target.Ref.ID != "" && !u.mediaStarting {
				rows = append(rows, setupRow{"从设备刷写", "直接写入 " + m.target.Path, u.startMedia})
			}
		}
		rows = append(rows, setupRow{"返回上一级", "", u.mediaBack})
		if m.loading {
			rows = append(rows, setupRow{"正在读取设备…", "", nil})
		}
		for _, f := range m.files {
			ext := strings.ToLower(path.Ext(f.Name))
			if !f.Dir && (u.upgradePage() && ext != ".img" && ext != ".json" || !u.upgradePage() && ext == ".json") {
				continue
			}
			name := f.Name
			if f.Dir {
				name += "/"
			}
			rows = append(rows, setupRow{name, "", func() {
				file := path.Join(m.dir, f.Name)
				switch {
				case f.Dir:
					u.browseMedia(file, false)
				case ext == ".json":
					m.manifest = file
				default:
					m.image = file
					if u.upgradePage() {
						meta := strings.TrimSuffix(f.Name, path.Ext(f.Name)) + ".json"
						if slices.Contains(m.files, mediaFile{Name: meta}) {
							m.manifest = path.Join(m.dir, meta)
						}
					}
				}
				u.showMedia()
			}})
		}
	}
	if m.job.ID != "" {
		phase := map[string]string{"preparing": "正在打开镜像", "checking": "正在校验包", "writing": "正在写入并读回校验", "done": "已完成", "error": "未完成"}[m.job.Stage]
		rows = append([]setupRow{{phase + m.jobDetail, m.job.Image, nil}}, rows...)
		if m.job.Message != "" && u.message == "" {
			u.message = m.job.Message
		}
	}
	u.define(hint, rows...)
}

func (u *setupUI) browseMedia(dir string, checkTargets bool) {
	m := &u.media
	ref, upgrade := m.source, u.upgradePage()
	u.mediaRevision++
	rev, visit := u.mediaRevision, u.visit
	m.dir, m.files, m.loading = dir, nil, true
	u.selected, u.scrolled = 0, 0
	u.showMedia()
	var files []mediaFile
	var targets []mediaTarget
	var targetErr error
	u.async(func() error {
		q := url.Values{"name": {ref.Name}, "id": {ref.ID}}
		if checkTargets {
			var dst any = &targets
			if upgrade {
				q.Set("kind", "upgrade")
				dst = nil
			}
			targetErr = u.api("media/targets?"+q.Encode(), nil, dst)
			q.Del("kind")
		}
		q.Set("dir", dir)
		return u.api("media/files?"+q.Encode(), nil, &files)
	}, func(err error) {
		if visit != u.visit || rev != u.mediaRevision {
			return
		}
		m.loading, m.files = false, files
		u.fail(err)
		if checkTargets {
			m.targets = targets
			if targetErr != nil {
				if upgrade {
					m.upgradeBlocked = targetErr.Error()
				} else {
					u.fail(targetErr)
				}
			}
		}
		u.showMedia()
	})
}

func (u *setupUI) mediaBack() {
	m := &u.media
	u.message = ""
	if u.group == "写入目标" {
		u.group = "设备文件"
		u.showMedia()
	} else if m.source.ID != "" && m.dir != "." {
		u.browseMedia(path.Dir(m.dir), false)
	} else {
		u.group = ""
		u.loadMediaPage()
	}
}

func (u *setupUI) startMedia() {
	m := &u.media
	if u.mediaStarting || (m.job.ID != "" && m.job.Stage != "done" && m.job.Stage != "error") {
		return
	}
	job := mediaJob{Volume: m.source, Image: m.image, Manifest: m.manifest, Target: m.target.Ref}
	op, title, body := "flash", "确认写入 "+m.target.Path, "目标上的系统或数据将被覆盖。"
	if u.upgradePage() {
		op, title, body = "upgrade", "确认从设备升级", "恢复区和引导程序将被更新。"
	}
	u.confirm(title, body, func() {
		u.message, u.mediaStarting = "", true
		u.mediaJobIO.revision++
		var result mediaJobState
		u.showMedia()
		u.async(func() error { return u.api("media/"+op, job, &result) }, func(err error) {
			u.mediaStarting = false
			if u.mediaPage() && u.upgradePage() == (op == "upgrade") {
				u.fail(err)
				if err == nil {
					m.job = result
				}
				u.showMedia()
			}
		})
	})
}
