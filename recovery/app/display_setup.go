//go:build !rkdev_nodisplay

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"
)

var setupPages = []string{"系统概览", "引导设置", "固件刷写", "恢复更新", "网络配置", "SSH 配置", "终端"}

type setupRow struct {
	label, detail string
	run           func()
}

type setupDialog struct {
	title, body string
	input       bool
	secret      bool
	value       string
	finish      func(string)
}

type setupUI struct {
	client                   *http.Client
	tty                      *setupTTY
	page, selected, scrolled int
	group                    string
	hint, message            string
	rows                     []setupRow
	dialog                   *setupDialog
	dirty                    bool
	lastOverview             time.Time
	boot                     setupBootState
	media                    setupMediaState
	trial                    setupNetState
	network                  setupNetworkModel
	firewall                 setupFirewallModel
	ssh                      setupSSHModel
	profile                  struct {
		Board        string
		Profile      string
		Device       string
		OnlineUpdate bool `json:"online_update"`
		Writable     bool
		Message      string
	}
}

func newSetupUI(tty *setupTTY) *setupUI {
	return &setupUI{tty: tty, client: &http.Client{
		Timeout:       3500 * time.Millisecond,
		Transport:     &http.Transport{DisableCompression: true, MaxIdleConns: 2, IdleConnTimeout: 10 * time.Second},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

func (u *setupUI) api(path string, req, dst any) error {
	url := "http://127.0.0.1/api/" + path
	var r *http.Response
	var err error
	if req == nil {
		r, err = u.client.Get(url)
	} else {
		data, _ := json.Marshal(req)
		r, err = u.client.Post(url, "application/json", bytes.NewReader(data))
	}
	if err != nil {
		return fmt.Errorf("恢复服务暂未就绪")
	}
	defer r.Body.Close()
	if r.StatusCode >= 400 {
		var status struct{ Error string }
		_ = json.NewDecoder(r.Body).Decode(&status)
		if status.Error == "" {
			status.Error = r.Status
		}
		return errors.New(status.Error)
	}
	if dst == nil {
		return nil
	}
	return json.NewDecoder(r.Body).Decode(dst)
}

func runSetup() error {
	frame, err := openFramebuffer()
	for range 8 {
		if err == nil {
			break
		}
		time.Sleep(600 * time.Millisecond)
		frame, err = openFramebuffer()
	}
	if err != nil {
		return err
	}
	defer frame.close()
	paint, err := newSetupPainter()
	if err != nil {
		return err
	}
	tty, err := openSetupTTY()
	if err != nil {
		return err
	}
	defer tty.file.Close()
	defer tty.text()
	u := newSetupUI(tty)
	u.enter(0)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	poll := time.NewTicker(35 * time.Millisecond)
	defer poll.Stop()
	update := time.NewTicker(time.Second)
	defer update.Stop()
	var input []byte
	var since time.Time
	for {
		if u.dirty && tty.active() {
			if err := frame.refresh(); err != nil {
				return err
			}
			u.paint(paint)
			frame.paint(paint.image)
			u.dirty = false
		}
		select {
		case <-ctx.Done():
			return nil
		case <-update.C:
			u.update()
			u.dirty = true
		case <-poll.C:
			var buf [256]byte
			n, _ := syscall.Read(int(tty.file.Fd()), buf[:])
			if n > 0 {
				input = append(input, buf[:n]...)
				since = time.Now()
			}
			for len(input) > 0 {
				k, consumed := setupKey(input, time.Since(since) >= 65*time.Millisecond)
				if consumed == 0 {
					break
				}
				input = input[consumed:]
				u.key(k)
				u.dirty = true
			}
		}
	}
}

func setupKey(b []byte, elapsed bool) (string, int) {
	if b[0] == 27 {
		if len(b) == 1 {
			if !elapsed {
				return "", 0
			}
			return "Escape", 1
		}
		if b[1] != '[' && b[1] != 'O' {
			return "Escape", 1
		}
		for i := 2; i < min(len(b), 16); i++ {
			x := b[i]
			if b[1] == '[' && i == 2 && x == '[' {
				continue
			} // Linux-console F1–F5
			if x >= 0x40 && x <= 0x7e {
				key := map[string]string{
					"[A": "Up", "[B": "Down", "[C": "Right", "[D": "Left", "OA": "Up", "OB": "Down", "OC": "Right", "OD": "Left",
					"[[A": "F1", "[[E": "F5", "[15~": "F5", "[5~": "PageUp", "[6~": "PageDown", "[H": "Home", "[F": "End",
				}[string(b[1:i+1])]
				return key, i + 1
			}
		}
		if elapsed || len(b) >= 16 {
			return "", min(len(b), 16)
		}
		return "", 0
	}
	switch b[0] {
	case '\r', '\n':
		return "Enter", 1
	case 8, 127:
		return "Backspace", 1
	case '\t':
		return "Tab", 1
	case 3:
		return "Escape", 1
	}
	if !utf8.FullRune(b) && !elapsed {
		return "", 0
	}
	r, n := utf8.DecodeRune(b)
	if unicode.IsPrint(r) && r != utf8.RuneError {
		return string(r), n
	}
	return "", n
}

func (u *setupUI) key(key string) {
	if u.dialog != nil {
		d := u.dialog
		switch key {
		case "Escape":
			u.dialog = nil
		case "Enter":
			u.dialog = nil
			d.finish(d.value)
		case "Backspace":
			_, n := utf8.DecodeLastRuneInString(d.value)
			d.value = d.value[:len(d.value)-n]
		default:
			if d.input && utf8.RuneCountInString(key) == 1 && len(d.value)+len(key) <= 8192 {
				d.value += key
			}
		}
		return
	}
	switch key {
	case "Left":
		u.enter((u.page + len(setupPages) - 1) % len(setupPages))
	case "Right", "Tab":
		u.enter((u.page + 1) % len(setupPages))
	case "Up", "Down", "PageUp", "PageDown", "Home", "End":
		offset := map[string]int{"Up": -1, "Down": 1, "PageUp": -8, "PageDown": 8, "Home": -len(u.rows), "End": len(u.rows)}[key]
		u.selected = min(max(0, u.selected+offset), max(0, len(u.rows)-1))
	case "Enter":
		if u.selected < len(u.rows) && u.rows[u.selected].run != nil {
			u.rows[u.selected].run()
		}
	case "F5":
		u.refresh()
	case "+", "=", "-":
		if u.page == 1 && u.group == "" {
			u.moveBoot(key != "-")
		}
	case "Escape":
		u.back()
	}
}

func (u *setupUI) enter(page int) {
	u.page, u.group, u.selected, u.scrolled = page, "", 0, 0
	u.message = ""
	u.refresh()
}

func (u *setupUI) define(hint string, rows ...setupRow) {
	u.hint, u.rows = hint, rows
	u.selected = min(u.selected, max(0, len(rows)-1))
	u.dirty = true
}

func (u *setupUI) back() {
	switch u.page {
	case 1:
		u.group = ""
		u.showBoot()
	case 2, 3:
		u.mediaBack()
	case 4:
		if u.group == "fw-add" {
			u.group = "防火墙"
		} else {
			u.group = ""
		}
		u.showNetwork()
	}
}

func (u *setupUI) refresh() {
	u.message = ""
	switch u.page {
	case 0:
		u.loadOverview()
	case 1:
		u.loadBoot()
	case 2, 3:
		u.loadMediaPage()
	case 4:
		u.loadNetworkPage()
	case 5:
		u.loadSSH()
	case 6:
		u.define("进入本地登录，退出登录后返回恢复界面。", setupRow{"进入本地终端", "沿用本机账号密码", u.terminal})
	}
}

func (u *setupUI) update() {
	switch u.page {
	case 0:
		if time.Since(u.lastOverview) >= 5*time.Second {
			u.loadOverview()
		}
	case 2, 3:
		u.updateMedia()
	case 4:
		u.updateNetwork()
	}
}

func (u *setupUI) change(label, old string, secret bool, fn func(string)) {
	u.dialog = &setupDialog{title: label, value: old, input: true, secret: secret, finish: fn}
}
func (u *setupUI) confirm(label, body string, fn func()) {
	u.dialog = &setupDialog{title: label, body: body, finish: func(string) { fn() }}
}
func (u *setupUI) fail(err error) {
	if err != nil {
		u.message = err.Error()
	}
}
func (u *setupUI) terminal() {
	if u.tty == nil {
		return
	}
	u.tty.text()
	fmt.Fprint(os.Stdout, "\x1b[2J\x1b[H")
	cmd := exec.Command("/bin/login")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	_ = cmd.Run()
	u.fail(u.tty.graphics())
}

func (u *setupUI) loadOverview() {
	u.lastOverview = time.Now()
	u.message = ""
	var cpu CpuInfo
	var network NetworkInfo
	var emmc struct{ Devices []EmmcHealth }
	err := u.api("profile", nil, &u.profile)
	if err == nil {
		err = u.api("cpu", nil, &cpu)
	}
	if err == nil {
		err = u.api("network", nil, &network)
	}
	emmcErr := u.api("emmc", nil, &emmc)
	u.fail(err)
	board := u.profile.Board
	if board == "" {
		board = "RKdev"
	}
	rows := []setupRow{
		{"本机", board + "  ·  " + strings.ToUpper(u.profile.Profile), nil},
		{"处理器", fmt.Sprintf("%s  /  %d 核  /  %s", cpu.Model, cpu.Cores, cpu.Temp), nil},
		{"运行内存", formatBytes(cpu.MemTotal) + " · 可用 " + formatBytes(cpu.MemFree), nil},
	}
	if emmcErr != nil {
		rows = append(rows, setupRow{"eMMC 容量", "暂不可用", nil})
	} else if len(emmc.Devices) == 0 {
		rows = append(rows, setupRow{"eMMC 容量", "未检测到 eMMC", nil})
	}
	for _, disk := range emmc.Devices {
		detail := "容量未知"
		if disk.Capacity > 0 {
			detail = fmt.Sprintf("%.2f GiB", float64(disk.Capacity)/(1<<30))
		}
		rows = append(rows, setupRow{"eMMC 容量", detail + " · " + disk.Device, nil})
	}
	rows = append(rows, setupRow{"恢复系统", "v" + buildTime + "  ·  Linux " + cpu.Kernel, nil})
	if lan := setupAddress(runtimeIPv4("br0")); lan != "" {
		rows = append(rows, setupRow{"LAN / Wi-Fi", "http://" + lan + "/", nil})
	}
	for _, n := range network.Interfaces {
		if n.Ip != "" && n.Ip != "-" && n.Name != "lo" {
			addr, err := netip.ParseAddr(n.Ip)
			detail := n.Ip
			if err == nil && addr.Is4() {
				detail = "http://" + addr.String() + "/"
			} else if err == nil && addr.Is6() && !addr.IsLinkLocalUnicast() {
				detail = "http://[" + addr.String() + "]/"
			}
			rows = append(rows, setupRow{"网络 · " + n.Name, detail, nil})
		}
	}
	if u.profile.Message != "" && u.message == "" {
		u.message = u.profile.Message
	}
	rows = append(rows, setupRow{"重新启动", "", func() {
		u.confirm("重新启动设备", "恢复系统将重新启动。", func() {
			u.fail(u.api("reboot", map[string]string{}, nil))
		})
	}})
	u.define("查看机器信息与 Web 管理地址。", rows...)
}

func (u *setupUI) paint(p *setupPainter) {
	p.box(0, 0, 1024, 576, 0x0e1422)
	p.box(0, 0, 1024, 67, 0x192940)
	p.box(0, 65, 1024, 2, 0x47b7e6)
	p.box(0, 67, 220, 449, 0x141f31)
	p.box(219, 66, 1, 449, 0x2c4059)
	p.mark()
	p.text(119, 11, 700, "RKdev  /  恢复设置", 0xeef8ff)
	p.text(119, 36, 714, "Recovery Setup Utility", 0x8eabc9)
	p.text(779, 23, 1000, "v"+buildTime, 0xb6d2e9)
	for i, title := range setupPages {
		y := 90 + i*49
		if i == u.page {
			p.box(15, y, 190, 39, 0x254264)
			p.box(15, y, 3, 39, 0x53d7ff)
		}
		p.text(26, y+8, 52, fmt.Sprintf("%02d", i+1), 0x6ab9e1)
		p.text(58, y+8, 204, title, 0xe1edfa)
	}
	p.text(243, 81, 1000, setupPages[u.page]+func() string {
		if u.group == "" {
			return ""
		}
		return "  /  " + u.group
	}(), 0x73d8fe)
	p.text(243, 113, 1000, u.hint, 0x9bb4cc)
	const visible = 9
	if u.selected-u.scrolled >= visible {
		u.scrolled = u.selected - visible + 1
	}
	if u.selected < u.scrolled {
		u.scrolled = u.selected
	}
	for i := u.scrolled; i < len(u.rows) && i < u.scrolled+visible; i++ {
		y := 146 + 34*(i-u.scrolled)
		bg, fg := uint32(0x162238), uint32(0xc3d9ed)
		if i%2 == 1 {
			bg = 0x19273c
		}
		if i == u.selected {
			bg, fg = 0x185281, 0xffffff
		}
		p.box(241, y, 760, 32, bg)
		row := u.rows[i]
		if row.detail == "" {
			p.text(251, y+4, 990, row.label, fg)
		} else {
			p.text(251, y+4, 462, row.label, fg)
			p.text(471, y+4, 990, row.detail, fg)
		}
		if row.run != nil {
			p.text(976, y+4, 998, "→", 0x8ad4f9)
		}
	}
	p.text(244, 461, 1000, u.message, 0xf1c380)
	if len(u.rows) > visible {
		p.text(927, 491, 1000, fmt.Sprintf("%d/%d", u.selected+1, len(u.rows)), 0x90aecb)
	}
	p.box(0, 516, 1024, 60, 0x192940)
	p.box(0, 516, 1024, 1, 0x385974)
	p.text(24, 532, 1000, "← →  菜单    ↑ ↓  选择    Enter  打开 / 编辑    Esc  返回    F5  刷新", 0xd7e9fa)
	if d := u.dialog; d != nil {
		p.box(206, 175, 612, 228, 0x061320)
		p.box(210, 179, 604, 220, 0x223b57)
		p.text(234, 197, 794, d.title, 0xa0e7ff)
		p.text(234, 234, 794, d.body, 0xe4eef9)
		if d.input {
			p.box(233, 271, 556, 33, 0x111d2e)
			value := d.value
			if d.secret {
				value = strings.Repeat("*", utf8.RuneCountInString(value))
			}
			r := []rune(value)
			if len(r) > 27 {
				value = "…" + string(r[len(r)-27:])
			}
			p.text(241, 274, 785, value+"_", 0xf4fbff)
		}
		p.text(234, 335, 794, "Enter  确认                         Esc  取消", 0xd4eafb)
	}
}
