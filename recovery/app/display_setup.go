package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
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

const (
	setupPageMain = iota
	setupPageBoot
	setupPageNetwork
	setupPageManagement
	setupPageFlash
	setupPageUpdate
	setupPageTerminal
	setupPageExit
)

var setupPages = [...]string{"概览", "引导", "网络", "管理", "刷写", "更新", "终端", "退出"}

type setupRow struct {
	label, detail string
	run           func()
}

type setupDialog struct {
	title, body string
	input       bool
	secret      bool
	value       string
	cursor      int
	options     []setupOption
	selected    int
	finish      func(string)
}

type setupProfile struct {
	Board        string
	Profile      string
	Device       string
	OnlineUpdate bool `json:"online_update"`
	Writable     bool
	Message      string
}

type setupOverview struct {
	profile setupProfile
	cpu     CpuInfo
	network NetworkInfo
	emmc    struct{ Devices []EmmcHealth }
	emmcErr error
	lan     string
}

type setupUI struct {
	ctx                      context.Context
	completed                chan func()
	client                   *http.Client
	tty                      *setupTTY
	page, selected, scrolled int
	group                    string
	hint, message            string
	rows                     []setupRow
	dialog                   *setupDialog
	dirty                    bool
	visit                    uint64
	lastOverview             time.Time
	overviewPending          bool
	boot                     setupBootState
	bootIO                   setupSettingIO
	media                    setupMediaState
	mediaListIO              setupSettingIO
	mediaJobIO               setupSettingIO
	mediaRevision            uint64
	mediaStarting            bool
	trial                    setupNetState
	network                  setupNetworkModel
	firewall                 setupFirewallModel
	firewallIO               setupSettingIO
	management               setupManagementModel
	profile                  setupProfile
	save                     *setupSave
	rebooting                bool
}

func newSetupUI(ctx context.Context, tty *setupTTY) *setupUI {
	return &setupUI{ctx: ctx, tty: tty, completed: make(chan func()), client: &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", setupSocket)
			},
			DisableCompression: true, MaxIdleConns: 2, IdleConnTimeout: 10 * time.Second,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

func (u *setupUI) api(path string, req, dst any) error {
	return u.call(u.ctx, path, req, dst)
}

func (u *setupUI) call(ctx context.Context, path string, req, dst any) error {
	url := "http://localhost/api/" + path
	method := http.MethodGet
	var data []byte
	budget := 3500 * time.Millisecond
	if req != nil {
		var err error
		data, err = json.Marshal(req)
		if err != nil {
			return err
		}
		method = http.MethodPost
		budget = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(data))
	if err != nil {
		return err
	}
	if req != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	r, err := u.client.Do(request)
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return fmt.Errorf("恢复服务请求超时")
		}
		return fmt.Errorf("恢复服务暂未就绪")
	}
	defer r.Body.Close()
	if r.StatusCode < 200 || r.StatusCode >= 300 {
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

// Work reads into private snapshots; only the display loop changes the UI.
func (u *setupUI) async(work func() error, done func(error)) {
	go func() {
		err := work()
		select {
		case u.completed <- func() { done(err) }:
		case <-u.ctx.Done():
		}
	}()
}

func runSetup() error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	frame, err := waitFramebuffer(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return err
	}
	defer frame.close()
	paint, err := newSetupPainter()
	if err != nil {
		return err
	}
	u := newSetupUI(ctx, nil)
	defer u.client.CloseIdleConnections()
	overview, err := u.awaitOverview()
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return err
	}
	tty, err := openSetupTTY()
	if err != nil {
		return err
	}
	defer tty.file.Close()
	defer tty.text()
	u.tty = tty
	u.lastOverview = time.Now()
	u.showOverview(overview)
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
			if err := frame.paint(paint.image); err != nil {
				return err
			}
			u.dirty = false
		}
		select {
		case <-ctx.Done():
			return nil
		case <-update.C:
			u.update()
			u.dirty = true
		case done := <-u.completed:
			done()
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

func waitFramebuffer(ctx context.Context) (*framebuffer, error) {
	retry := time.NewTicker(600 * time.Millisecond)
	defer retry.Stop()
	for {
		frame, err := openFramebuffer()
		if err == nil || !(errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ENODEV)) {
			return frame, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-retry.C:
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
					"[[A": "F1", "[[E": "F5", "[15~": "F5", "[5~": "PageUp", "[6~": "PageDown",
					"[H": "Home", "[1~": "Home", "[7~": "Home", "[F": "End", "[4~": "End", "[8~": "End",
					"[21~": "F10", "[3~": "Delete", "OM": "Enter", "Ok": "+", "Om": "-",
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
	case 21:
		return "Clear", 1
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
		u.dialogKey(key)
		return
	}
	if u.rebooting {
		return
	}
	if u.save != nil {
		switch key {
		case "Up", "Down", "Home", "End":
			u.moveSelection(key)
		case "Enter":
			u.runSelection()
		case "Escape":
			u.finishSetupTrial(false)
		}
		return
	}
	switch key {
	case "Left":
		u.enter((u.page + len(setupPages) - 1) % len(setupPages))
	case "Right", "Tab":
		u.enter((u.page + 1) % len(setupPages))
	case "Up", "Down", "PageUp", "PageDown", "Home", "End":
		u.moveSelection(key)
	case "Enter":
		u.runSelection()
	case "F5":
		u.refresh()
	case "F10":
		u.savePrompt(true)
	case "+", "=", "-":
		if u.page == setupPageBoot && u.group == "" {
			delta := -1
			if key == "-" {
				delta = 1
			}
			u.moveBoot(delta)
		}
	case "Escape":
		u.back()
	}
}

func (u *setupUI) runSelection() {
	if u.selected < len(u.rows) && u.rows[u.selected].run != nil {
		u.rows[u.selected].run()
	}
}

func (u *setupUI) moveSelection(key string) {
	offset := map[string]int{"Up": -1, "Down": 1, "PageUp": -9, "PageDown": 9, "Home": -len(u.rows), "End": len(u.rows)}[key]
	u.selected = min(max(0, u.selected+offset), max(0, len(u.rows)-1))
}

func (u *setupUI) dialogKey(key string) {
	d := u.dialog
	switch key {
	case "Escape":
		u.dialog = nil
		return
	case "Enter":
		u.dialog = nil
		if len(d.options) > 0 {
			d.value = d.options[d.selected].key
		}
		d.finish(d.value)
		return
	}
	if len(d.options) > 0 {
		switch key {
		case "Up":
			d.selected = max(0, d.selected-1)
		case "Down":
			d.selected = min(len(d.options)-1, d.selected+1)
		case "Home":
			d.selected = 0
		case "End":
			d.selected = len(d.options) - 1
		}
		return
	}
	if !d.input {
		return
	}
	switch key {
	case "Home":
		d.cursor = 0
	case "End":
		d.cursor = len(d.value)
	case "Clear":
		d.value, d.cursor = "", 0
	case "Left", "Backspace":
		_, n := utf8.DecodeLastRuneInString(d.value[:d.cursor])
		d.cursor -= n
		if key == "Backspace" {
			d.value = d.value[:d.cursor] + d.value[d.cursor+n:]
		}
	case "Right", "Delete":
		_, n := utf8.DecodeRuneInString(d.value[d.cursor:])
		if key == "Right" {
			d.cursor += n
		} else {
			d.value = d.value[:d.cursor] + d.value[d.cursor+n:]
		}
	default:
		if utf8.RuneCountInString(key) == 1 && len(d.value)+len(key) <= 8192 {
			d.value = d.value[:d.cursor] + key + d.value[d.cursor:]
			d.cursor += len(key)
		}
	}
}

func (u *setupUI) enter(page int) {
	u.visit++
	u.page, u.group, u.selected, u.scrolled = page, "", 0, 0
	u.define("正在读取…")
	u.message = ""
	u.refresh()
}

func (u *setupUI) define(hint string, rows ...setupRow) {
	u.hint, u.rows = hint, rows
	u.selected = min(u.selected, max(0, len(rows)-1))
	u.dirty = true
}

func (u *setupUI) back() {
	if u.group == "" && !(u.page == setupPageFlash || u.page == setupPageUpdate) {
		if u.page == setupPageExit {
			u.enter(setupPageMain)
		} else {
			u.exitPage()
		}
		return
	}
	switch u.page {
	case setupPageBoot:
		u.group = ""
		u.selected, u.scrolled = len(u.boot.base.Available), 0
		u.showBoot()
	case setupPageFlash, setupPageUpdate:
		if u.media.source.ID == "" {
			u.exitPage()
		} else {
			u.mediaBack()
		}
	case setupPageNetwork:
		if u.group == "fw-add" {
			u.group = "防火墙"
			u.selected = 1
		} else {
			u.selected = map[string]int{"WAN": 0, "LAN": 1, "Wi-Fi": 2, "防火墙": 3}[u.group]
			u.group = ""
		}
		u.scrolled = 0
		u.showNetwork()
	}
}

func (u *setupUI) refresh() {
	u.message = ""
	switch u.page {
	case setupPageMain:
		u.loadOverview()
	case setupPageBoot:
		u.loadBoot()
	case setupPageFlash, setupPageUpdate:
		u.loadMediaPage()
	case setupPageNetwork:
		u.loadNetworkPage()
	case setupPageManagement:
		u.loadManagement()
	case setupPageTerminal:
		u.define("", setupRow{"进入本地终端", "", u.terminal})
	case setupPageExit:
		u.showExit()
	}
}

func (u *setupUI) update() {
	if u.save != nil {
		u.updateSave()
		return
	}
	switch u.page {
	case setupPageMain:
		if time.Since(u.lastOverview) >= 5*time.Second {
			u.loadOverview()
		}
	case setupPageFlash, setupPageUpdate:
		u.updateMedia()
	case setupPageNetwork:
		u.updateNetwork()
	}
}

func (u *setupUI) change(label, old string, secret bool, fn func(string)) {
	u.dialog = &setupDialog{title: label, value: old, cursor: len(old), input: true, secret: secret, finish: fn}
}
func (u *setupUI) choose(label string, options []setupOption, selected int, fn func(string)) {
	u.dialog = &setupDialog{title: label, options: options, selected: selected, finish: fn}
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

func (u *setupUI) readOverview(ctx context.Context, v *setupOverview) error {
	for _, req := range []struct {
		path string
		dst  any
	}{{"profile", &v.profile}, {"cpu", &v.cpu}, {"network", &v.network}} {
		if err := u.call(ctx, req.path, nil, req.dst); err != nil {
			return err
		}
	}
	v.emmcErr = u.call(ctx, "emmc", nil, &v.emmc)
	if carrier, _ := os.ReadFile("/sys/class/net/br0/carrier"); strings.TrimSpace(string(carrier)) == "1" {
		v.lan = setupAddress(runtimeIPv4("br0"))
	}
	return nil
}

func (u *setupUI) awaitOverview() (setupOverview, error) {
	ctx := u.ctx
	retry := time.NewTicker(250 * time.Millisecond)
	defer retry.Stop()
	for {
		var v setupOverview
		if err := u.readOverview(ctx, &v); err == nil && ctx.Err() == nil {
			return v, nil
		}
		select {
		case <-ctx.Done():
			return setupOverview{}, ctx.Err()
		case <-retry.C:
		}
	}
}

func (u *setupUI) loadOverview() {
	if u.overviewPending {
		return
	}
	u.overviewPending = true
	u.lastOverview = time.Now()
	var v setupOverview
	u.async(func() error { return u.readOverview(u.ctx, &v) }, func(err error) {
		u.overviewPending = false
		if u.page != setupPageMain {
			return
		}
		if err != nil {
			u.fail(err)
			return
		}
		u.message = ""
		u.showOverview(v)
	})
}

func (u *setupUI) showOverview(v setupOverview) {
	u.profile = v.profile
	cpu := v.cpu
	board := u.profile.Board
	if board == "" {
		board = "RKdev"
	}
	rows := []setupRow{
		{"本机", board + "  ·  " + strings.ToUpper(u.profile.Profile), nil},
		{"处理器", fmt.Sprintf("%s  /  %d 核", cpu.Model, cpu.Cores), nil},
		{"处理器温度", cpu.Temp, nil},
		{"运行内存", formatBytes(cpu.MemTotal) + " · 可用 " + formatBytes(cpu.MemFree), nil},
	}
	if v.emmcErr != nil {
		rows = append(rows, setupRow{"eMMC 容量", "暂不可用", nil})
	} else if len(v.emmc.Devices) == 0 {
		rows = append(rows, setupRow{"eMMC 容量", "未检测到 eMMC", nil})
	}
	for _, disk := range v.emmc.Devices {
		detail := "容量未知"
		if disk.Capacity > 0 {
			detail = fmt.Sprintf("%.2f GiB", float64(disk.Capacity)/(1<<30))
		}
		rows = append(rows, setupRow{"eMMC 容量", detail + " · " + disk.Device, nil})
	}
	rows = append(rows, setupRow{"恢复系统", "v" + buildTime + "  ·  Linux " + cpu.Kernel, nil})
	if v.lan != "" {
		rows = append(rows, setupRow{"LAN / Wi-Fi", "https://" + v.lan + "/", nil})
	}
	for _, n := range v.network.Interfaces {
		if n.Ip != "" && n.Ip != "-" && n.Name != "lo" && n.Name != "br0" {
			addr, err := netip.ParseAddr(n.Ip)
			detail := n.Ip
			if err == nil && addr.Is4() {
				detail = "https://" + addr.String() + "/"
			} else if err == nil && addr.Is6() && !addr.IsLinkLocalUnicast() {
				detail = "https://[" + addr.String() + "]/"
			}
			rows = append(rows, setupRow{"网络 · " + n.Name, detail, nil})
		}
	}
	if u.profile.Message != "" && u.message == "" {
		u.message = u.profile.Message
	}
	u.define("", rows...)
}

func (u *setupUI) paint(p *setupPainter) {
	const navy, ink, muted = 0x152840, 0x172438, 0x455268
	const panel, line, white = 0xeef0f3, 0xa4aebd, 0xffffff
	p.box(0, 0, 1024, 576, 0xdadde4)
	p.box(0, 0, 1024, 105, navy)
	p.mark()
	p.text(119, 12, 690, "RKdev Setup Utility", white)
	p.text(119, 36, 690, u.profile.Board, 0xb7c9df)
	p.text(794, 24, 1002, "v"+buildTime, 0xb7c9df)
	changes := u.changes()
	for i, title := range setupPages {
		x := 22 + i*123
		fg := uint32(0xc8d8ec)
		p.box(x, 68, 119, 35, 0x223c5a)
		if i == u.page {
			p.box(x, 68, 119, 37, panel)
			p.box(x, 68, 119, 3, 0xf0cd79)
			fg = ink
		}
		p.text(x+14, 75, x+105, title, fg)
		if (i == setupPageBoot && u.boot.changed()) ||
			(i == setupPageNetwork && (u.network.changed() || u.firewall.changed())) ||
			(i == setupPageManagement && u.management.changed()) {
			p.text(x+98, 75, x+116, "*", fg)
		}
	}
	p.box(22, 105, 980, 379, panel)
	p.box(722, 117, 1, 355, line)
	p.box(35, 148, 672, 1, line)
	title := setupPages[u.page]
	if u.group != "" {
		group := u.group
		if group == "fw-add" {
			group = "防火墙 / 添加规则"
		}
		title += "  /  " + group
	}
	p.text(39, 119, 705, title, ink)
	const visible = 9
	if u.selected-u.scrolled >= visible {
		u.scrolled = u.selected - visible + 1
	}
	if u.selected < u.scrolled {
		u.scrolled = u.selected
	}
	for i := u.scrolled; i < len(u.rows) && i < u.scrolled+visible; i++ {
		y := 155 + 34*(i-u.scrolled)
		bg, fg, value := uint32(panel), uint32(ink), uint32(muted)
		if i%2 == 1 {
			bg = 0xe4e8ef
		}
		if i == u.selected {
			bg, fg, value = 0x23517e, white, 0xe7f2ff
		}
		p.box(34, y, 674, 32, bg)
		row := u.rows[i]
		if row.run != nil {
			p.text(42, y+4, 62, ">", fg)
		}
		limit := 699
		if row.detail != "" {
			limit = 300
		}
		p.text(66, y+4, limit, row.label, fg)
		p.text(314, y+4, 699, row.detail, value)
	}
	if len(u.rows) > visible {
		p.text(643, 459, 705, fmt.Sprintf("%d/%d", u.selected+1, len(u.rows)), muted)
	}
	p.text(743, 119, 987, "设置状态", ink)
	p.box(740, 148, 245, 1, line)
	if len(changes) == 0 {
		p.text(743, 163, 987, "没有未保存的更改", muted)
	} else {
		p.text(743, 163, 987, "未保存的更改", 0x8a4a04)
		for i, name := range changes {
			p.text(744, 193+27*i, 987, "*  "+name, ink)
		}
	}
	p.wrap(743, 321, 985, 5, u.hint, muted)
	p.wrap(31, 486, 996, 2, u.message, 0x773b00)
	p.box(0, 538, 1024, 38, navy)
	if u.save != nil {
		p.text(24, 545, 1002, "↑ ↓ 选择     Enter 执行     Esc 回退", 0xd9e8fa)
	} else {
		p.text(24, 545, 1002, "← → 菜单     ↑ ↓ 选择     Enter 编辑     Esc 返回     F5 刷新", 0xd9e8fa)
		if u.page == setupPageBoot && u.group == "" {
			p.text(696, 545, 840, "+ / -  优先级", 0xd9e8fa)
		}
		p.text(851, 545, 1002, "F10 保存并退出", 0xffdf92)
	}
	if d := u.dialog; d != nil {
		height := 177
		if len(d.options) > 0 {
			height = 86 + 33*min(8, len(d.options))
		}
		y := (576 - height) / 2
		p.box(210, y+4, 614, height, 0x848f9f)
		p.box(205, y, 614, height, navy)
		p.box(207, y+36, 610, height-38, panel)
		p.text(223, y+7, 797, d.title, white)
		p.wrap(223, y+48, 797, 3, d.body, ink)
		if len(d.options) > 0 {
			start := max(0, d.selected-7)
			for i := start; i < min(len(d.options), start+8); i++ {
				rowY := y + 43 + 33*(i-start)
				fg := uint32(ink)
				if i == d.selected {
					p.box(223, rowY, 576, 31, 0x23517e)
					fg = white
				}
				p.text(237, rowY+4, 783, d.options[i].label, fg)
			}
		}
		if d.input {
			p.box(222, y+64, 577, 37, white)
			left, right := d.value[:d.cursor], d.value[d.cursor:]
			if d.secret {
				left = strings.Repeat("*", utf8.RuneCountInString(left))
				right = strings.Repeat("*", utf8.RuneCountInString(right))
			}
			width := p.width(left)
			for width > 510 {
				_, n := utf8.DecodeRuneInString(left)
				width -= p.width(left[:n])
				left = left[n:]
			}
			p.text(233, y+70, 791, left+right, ink)
			p.box(233+width, y+72, 2, 22, 0x23517e)
		}
		p.text(223, y+height-33, 797, "Enter 确认             Esc 取消", muted)
	}
}
