//go:build !rkdev_nodisplay

package main

import (
	"fmt"
	"net/url"
	"path"
	"slices"
	"strings"
)

type setupBootState struct {
	Order      []string
	Available  []string
	Vars       []UbootVar
	Installing bool
}

func (u *setupUI) loadBoot() {
	u.fail(u.api("boot_order", nil, &u.boot))
	u.showBoot()
}

func (u *setupUI) showBoot() {
	b := &u.boot
	if u.group == "U-Boot 环境变量" {
		rows := []setupRow{
			{"添加变量", "", func() {
				u.change("变量名称", "", false, func(name string) { u.editBootVar(name, "") })
			}},
			{"恢复默认环境", "", func() {
				u.confirm("恢复出厂环境变量", "此操作将重置引导顺序与自定义变量。", func() {
					u.fail(u.api("uboot_reset", map[string]any{}, nil))
					u.loadBoot()
				})
			}},
		}
		for _, v := range b.Vars {
			var row setupRow
			row.label, row.detail = v.Name, v.Value
			if !v.Protected {
				row.run = func() { u.editBootVar(v.Name, v.Value) }
			} else {
				row.detail = "[只读] " + v.Value
			}
			rows = append(rows, row)
		}
		u.define("受保护的变量仅供查看。", rows...)
		return
	}
	rows := []setupRow{}
	for i, d := range b.Order {
		rows = append(rows, setupRow{d, fmt.Sprintf("启动优先级 %d", i+1), func() {
			if len(b.Order) > 1 {
				b.Order = slices.Delete(b.Order, i, i+1)
				u.showBoot()
			}
		}})
	}
	for _, d := range b.Available {
		if slices.Contains(b.Order, d) {
			continue
		}
		rows = append(rows, setupRow{d, "已禁用", func() {
			b.Order = append(b.Order, d)
			u.showBoot()
		}})
	}
	rows = append(rows, setupRow{"保存引导顺序", "重启后生效", func() {
		u.confirm("保存引导顺序", strings.Join(b.Order, "  →  "), func() {
			if err := u.api("boot_order", map[string]any{"order": b.Order}, nil); err != nil {
				u.fail(err)
			} else {
				u.message = "引导顺序已保存"
			}
		})
	}}, setupRow{"U-Boot 环境变量", "查看与编辑", func() {
		u.group = "U-Boot 环境变量"
		u.selected = 0
		u.showBoot()
	}})
	hint := "Enter 启用或禁用引导项；+ / - 调整启用项的顺序。"
	if b.Installing {
		hint = "eMMC 安装尚未完成，U-Boot 将跳过系统空间。"
	}
	u.define(hint, rows...)
}

func (u *setupUI) moveBoot(higher bool) {
	pos := u.selected
	next := pos + 1
	if higher {
		next = pos - 1
	}
	if pos >= len(u.boot.Order) || next < 0 || next >= len(u.boot.Order) {
		return
	}
	u.boot.Order[pos], u.boot.Order[next] = u.boot.Order[next], u.boot.Order[pos]
	u.selected = next
	u.showBoot()
}

func (u *setupUI) editBootVar(name, old string) {
	if name == "" {
		return
	}
	u.change(name, old, false, func(value string) {
		err := u.api("uboot_setenv", map[string]string{"name": name, "value": value}, nil)
		u.fail(err)
		if err == nil {
			u.message = "环境变量已保存"
			u.loadBoot()
		}
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
}

func (u *setupUI) upgradePage() bool { return u.page == 3 }

func (u *setupUI) loadMediaPage() {
	if u.upgradePage() {
		u.fail(u.api("profile", nil, &u.profile))
	}
	m := &u.media
	*m = setupMediaState{}
	u.fail(u.api("media", nil, &m.volumes))
	u.updateMedia()
	u.showMedia()
}

func (u *setupUI) updateMedia() {
	m := &u.media
	var j mediaJobState
	if u.api("media/job", nil, &j) != nil {
		return
	}
	if (u.upgradePage() && j.Kind != "upgrade") || (!u.upgradePage() && j.Kind != "flash") {
		return
	}
	detail := ""
	if j.TaskID != "" && j.Stage != "done" && j.Stage != "error" {
		var p uploadState
		if u.api("upload_status", nil, &p) == nil && p.TaskID == j.TaskID && p.Written > 0 {
			detail = " · " + formatBytes(p.Written)
			if p.ExpandedTotal > 0 {
				detail += " / " + formatBytes(p.ExpandedTotal)
			}
		}
	}
	if m.job != j || m.jobDetail != detail {
		m.job, m.jobDetail = j, detail
		u.showMedia()
	}
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
				q := url.Values{"name": {v.Name}, "id": {v.ID}}
				if u.upgradePage() {
					q.Set("kind", "upgrade")
					if err := u.api("media/targets?"+q.Encode(), nil, nil); err != nil {
						m.upgradeBlocked = err.Error()
					}
				} else {
					u.fail(u.api("media/targets?"+q.Encode(), nil, &m.targets))
				}
				u.browseMedia(".")
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
			} else if m.image != "" && m.manifest != "" {
				rows = append(rows, setupRow{"从设备升级", "保留系统与环境变量", u.startMedia})
			}
		} else {
			rows = append(rows, setupRow{"写入目标", m.target.Path, func() {
				u.group = "写入目标"
				u.selected = 0
				u.showMedia()
			}})
			if m.image != "" && m.target.Ref.ID != "" {
				rows = append(rows, setupRow{"从设备刷写", "直接写入 " + m.target.Path, u.startMedia})
			}
		}
		rows = append(rows, setupRow{"返回上一级", "", u.mediaBack})
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
					u.browseMedia(file)
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

func (u *setupUI) browseMedia(dir string) {
	m := &u.media
	q := url.Values{"name": {m.source.Name}, "id": {m.source.ID}, "dir": {dir}}
	m.dir, m.files = dir, nil
	u.fail(u.api("media/files?"+q.Encode(), nil, &m.files))
	u.selected = 0
	u.showMedia()
}

func (u *setupUI) mediaBack() {
	m := &u.media
	u.message = ""
	if u.group == "写入目标" {
		u.group = "设备文件"
		u.showMedia()
	} else if m.source.ID != "" && m.dir != "." {
		u.browseMedia(path.Dir(m.dir))
	} else {
		u.group = ""
		u.loadMediaPage()
	}
}

func (u *setupUI) startMedia() {
	m := &u.media
	if m.job.ID != "" && m.job.Stage != "done" && m.job.Stage != "error" {
		return
	}
	job := mediaJob{Volume: m.source, Image: m.image, Manifest: m.manifest, Target: m.target.Ref}
	op, title, body := "flash", "确认写入 "+m.target.Path, "目标上的系统或数据将被覆盖。"
	if u.upgradePage() {
		op, title, body = "upgrade", "确认从设备升级", "恢复区和引导程序将被更新。"
	}
	u.confirm(title, body, func() {
		u.message = ""
		u.fail(u.api("media/"+op, job, &m.job))
		u.showMedia()
	})
}
