//go:build !rkdev_nodisplay

package main

import (
	"fmt"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"time"
)

type setupNetState struct {
	Supported, Ready, Station, Secured, Persistent bool
	Config                                         networkConfig
	Ports                                          []networkPort
	Names, Addresses                               map[string]string
	APBands                                        []string `json:"ap_bands"`
	LANActual                                      string   `json:"lan_actual"`
	Message, Warning                               string
	Trial                                          *networkTrial
}
type setupNetworkModel struct {
	draft networkConfig
}
type setupFirewallModel struct {
	Enabled                            bool
	InputRules, ForwardRules, NatRules []firewallRule
	Trial                              *firewallTrial
	newRule                            firewallRule
	Message                            string
}
type setupSSHModel struct {
	Supported, Persistent bool
	Config                sshConfig
	Message               string
}

func setupYes(v bool) string {
	if v {
		return "启用"
	}
	return "关闭"
}

func editSetupString(u *setupUI, label string, text *string, updated func()) setupRow {
	return setupRow{label, *text, func() { u.change(label, *text, false, func(s string) { *text = strings.TrimSpace(s); updated() }) }}
}

type setupOption struct{ key, label string }

func setupChoice(label string, value *string, options []setupOption, updated func()) setupRow {
	i := slices.IndexFunc(options, func(o setupOption) bool { return o.key == *value })
	desc := *value
	if i >= 0 {
		desc = options[i].label
	}
	return setupRow{label, desc, func() {
		if len(options) > 0 {
			*value = options[(i+1)%len(options)].key
			updated()
		}
	}}
}

func setupToggle(label string, value *bool, updated func()) setupRow {
	return setupRow{label, setupYes(*value), func() { *value = !*value; updated() }}
}

func (u *setupUI) loadNetworkPage() {
	var state setupNetState
	if err := u.api("network/config", nil, &state); err != nil {
		u.fail(err)
	} else {
		u.trial = state
		u.network.draft = state.Config
	}
	if u.group == "防火墙" {
		u.loadFirewall()
	}
	u.showNetwork()
}
func (u *setupUI) loadFirewall() {
	r := u.firewall.newRule
	var fresh setupFirewallModel
	if err := u.api("firewall/status", nil, &fresh); err != nil {
		u.fail(err)
		return
	}
	u.firewall = fresh
	u.firewall.newRule = r
}

func (u *setupUI) updateNetwork() {
	var state setupNetState
	if err := u.api("network/config", nil, &state); err == nil {
		was := u.trial.Trial
		if was != nil && state.Trial == nil {
			u.network.draft = state.Config
		}
		u.trial = state
	}
	if u.group == "防火墙" {
		u.loadFirewall()
	}
	u.showNetwork()
}

func (u *setupUI) applyNet(op string) {
	req := map[string]any{"action": op}
	if op == "apply" {
		req["config"] = u.network.draft
	}
	var t networkTrial
	if err := u.api("network/config", req, &t); err != nil {
		u.fail(err)
		return
	}
	u.trial.Trial = &t
	u.showNetwork()
}

func (u *setupUI) trialNet(op string) {
	if u.trial.Trial == nil {
		return
	}
	if err := u.api("network/config", map[string]string{"action": op, "id": u.trial.Trial.ID}, nil); err != nil {
		u.fail(err)
	} else {
		u.loadNetworkPage()
	}
}

func (u *setupUI) showNetwork() {
	n := &u.trial
	c := &u.network.draft
	var rows []setupRow
	hint := "WAN、LAN、Wi-Fi 与防火墙使用相同的恢复配置。"
	if n.Trial != nil {
		t := n.Trial
		remaining := max(0, int(time.Until(t.Deadline).Seconds()+1))
		hint = "正在试用网络，新管理地址：" + t.LANURL
		rows = append(rows, setupRow{"自动回退", fmt.Sprintf("%d 秒", remaining), nil})
		if t.Status == "ready" && remaining > 0 {
			if n.Warning == "" {
				rows = append(rows, setupRow{"确认配置", "", func() { u.trialNet("confirm") }})
			}
			rows = append(rows, setupRow{"回退网络配置", "", func() { u.trialNet("revert") }})
		}
		if n.Warning != "" {
			rows = append(rows, setupRow{n.Warning, "", nil})
		}
		u.define(hint, rows...)
		return
	}
	if u.group == "" {
		for _, part := range []struct{ name, value string }{
			{"WAN", n.Addresses[n.Config.WAN.Port]}, {"LAN", n.LANActual}, {"Wi-Fi", n.Config.WiFi.SSID}, {"防火墙", "规则与地址转换"},
		} {
			rows = append(rows, setupRow{part.name, part.value, func() {
				u.group = part.name
				u.selected = 0
				if u.group == "防火墙" {
					u.loadFirewall()
				}
				u.showNetwork()
			}})
		}
		u.define(hint, rows...)
		return
	}
	if u.group == "防火墙" || u.group == "fw-add" {
		u.showFirewall()
		return
	}
	if !n.Supported || !n.Ready {
		desc := n.Message
		if desc == "" {
			desc = "设备网络尚未就绪"
		}
		u.define("", setupRow{desc, "", nil})
		return
	}
	refresh := u.showNetwork
	switch u.group {
	case "WAN":
		var ports []setupOption
		for _, p := range n.Ports {
			if p.Wireless && !n.Station {
				continue
			}
			ports = append(ports, setupOption{p.ID, p.Label + "  " + n.Names[p.ID]})
		}
		rows = append(rows,
			setupChoice("物理网口", &c.WAN.Port, ports, refresh),
			setupChoice("连接方式", &c.WAN.Mode, []setupOption{{"dhcp", "自动获取 DHCP"}, {"static", "静态 IPv4"}, {"off", "关闭"}}, refresh))
		if c.WAN.Mode == "static" {
			rows = append(rows, editSetupString(u, "IPv4 / 前缀", &c.WAN.Address, refresh), editSetupString(u, "默认网关", &c.WAN.Gateway, refresh))
		}
		dns := strings.Join(c.WAN.DNS, " ")
		rows = append(rows, setupRow{"DNS 地址", dns, func() {
			u.change("DNS（空格分隔）", dns, false, func(s string) { c.WAN.DNS = strings.Fields(s); refresh() })
		}})
		hint = "当前 WAN 地址：" + n.Addresses[n.Config.WAN.Port]
	case "LAN":
		rows = append(rows, editSetupString(u, "管理 IPv4 / 前缀", &c.LAN.Address, refresh))
		for _, p := range n.Ports {
			if p.Wireless {
				continue
			}
			in := slices.Contains(c.LAN.Ports, p.ID)
			rows = append(rows, setupRow{"网桥端口 · " + p.Label, setupYes(in), func() {
				c.LAN.Ports = slices.DeleteFunc(c.LAN.Ports, func(id string) bool { return id == p.ID })
				if !in {
					c.LAN.Ports = append(c.LAN.Ports, p.ID)
				}
				refresh()
			}})
		}
		rows = append(rows, setupToggle("DHCP 服务", &c.LAN.DHCP, refresh), setupToggle("共享 WAN (NAT)", &c.LAN.NAT, refresh))
		if c.LAN.DHCP {
			rows = append(rows, editSetupString(u, "地址池起点", &c.LAN.Start, refresh), editSetupString(u, "地址池终点", &c.LAN.End, refresh))
		}
		hint = "当前 LAN 地址：" + n.LANActual
	case "Wi-Fi":
		modes := []setupOption{{"off", "关闭"}, {"ap", "恢复热点"}}
		if n.Station {
			modes = append(modes, setupOption{"station", "无线客户端"})
		}
		rows = append(rows, setupChoice("工作模式", &c.WiFi.Mode, modes, refresh))
		if c.WiFi.Mode != "off" {
			rows = append(rows, editSetupString(u, "SSID", &c.WiFi.SSID, refresh))
			if c.WiFi.Mode == "ap" {
				var options []setupOption
				for _, b := range n.APBands {
					options = append(options, setupOption{b, b + " GHz"})
				}
				rows = append(rows, setupChoice("频段", &c.WiFi.Band, options, func() {
					c.WiFi.Channel = 6
					if c.WiFi.Band == "5" {
						c.WiFi.Channel = 36
					}
					refresh()
				}), setupRow{"信道", strconv.Itoa(c.WiFi.Channel), func() {
					u.change("信道", strconv.Itoa(c.WiFi.Channel), false, func(s string) { c.WiFi.Channel, _ = strconv.Atoi(s); refresh() })
				}})
			} else {
				hint = "无线客户端需在 WAN 页选中 Wi-Fi。"
			}
			passwordLabel := "开放网络"
			if n.Secured {
				passwordLabel = "已有密码"
			}
			if c.WiFi.Password != nil {
				passwordLabel = "下次试用时设为开放"
				if *c.WiFi.Password != "" {
					passwordLabel = "下次试用时更换密码"
				}
			}
			rows = append(rows, setupRow{"设置或清空密码", passwordLabel, func() {
				u.change("新密码，留空设为开放", "", true, func(s string) { c.WiFi.Password = &s; refresh() })
			}})
		}
	}
	rows = append(rows, setupRow{"试用配置", "90 秒后自动回退", func() { u.applyNet("apply") }},
		setupRow{"试用板包默认配置", "", func() { u.applyNet("defaults") }})
	u.define(hint, rows...)
}

func (u *setupUI) showFirewall() {
	f := &u.firewall
	if u.group == "fw-add" {
		u.showFirewallRule()
		return
	}
	if t := f.Trial; t != nil {
		left := max(0, int(time.Until(t.Deadline).Seconds()+1))
		rows := []setupRow{{"自动回退", fmt.Sprintf("%d 秒", left), nil}}
		if left > 0 && t.Status == "ready" {
			for _, a := range []setupOption{{"confirm", "确认规则"}, {"revert", "回退规则"}} {
				rows = append(rows, setupRow{a.label, "", func() {
					u.firewallEdit("trial", map[string]string{"trial": t.ID, "operation": a.key})
				}})
			}
		}
		u.define("试用防火墙设置。", rows...)
		return
	}
	rows := []setupRow{
		{"防火墙", setupYes(f.Enabled), func() { u.firewallEdit("toggle", map[string]bool{"enabled": !f.Enabled}) }},
		{"添加规则", "", func() {
			u.group = "fw-add"
			u.selected = 0
			f.newRule = firewallRule{Chain: "input", Action: "ACCEPT", Protocol: "all"}
			u.showFirewallRule()
		}},
	}
	for _, group := range [][]firewallRule{f.InputRules, f.ForwardRules, f.NatRules} {
		for _, r := range group {
			rows = append(rows, setupRow{r.Chain + "  " + r.Action, strings.Join([]string{r.Protocol, r.SrcIP, r.DstIP, r.DstPort, r.NatTarget}, "  "), func() {
				u.confirm("删除防火墙规则", r.Chain+"  "+r.Action, func() {
					u.firewallEdit("delete", map[string]any{"id": r.ID, "chain": r.Chain})
				})
			}})
		}
	}
	if u.message == "" {
		u.message = f.Message
	}
	u.define("管理入站、转发与地址转换规则。", rows...)
}
func (u *setupUI) showFirewallRule() {
	r := &u.firewall.newRule
	refresh := u.showFirewallRule
	chains := []setupOption{{"input", "入站"}, {"forward", "转发"}, {"nat", "地址转换"}}
	actions := []setupOption{{"ACCEPT", "允许"}, {"DROP", "丢弃"}, {"REJECT", "拒绝"}}
	if r.Chain == "nat" {
		actions = []setupOption{{"dnat", "端口转发"}, {"masquerade", "IP 伪装"}}
	}
	rows := []setupRow{
		setupChoice("规则类型", &r.Chain, chains, func() {
			r.Action, r.Protocol, r.SrcIP, r.DstIP, r.DstPort, r.NatTarget = "ACCEPT", "all", "", "", "", ""
			if r.Chain == "nat" {
				r.Action, r.Protocol = "dnat", "tcp"
			}
			refresh()
		}),
		setupChoice("动作", &r.Action, actions, func() {
			if r.Action == "masquerade" {
				r.Protocol, r.DstPort, r.NatTarget = "all", "", ""
			}
			refresh()
		}),
	}
	if r.Action != "masquerade" {
		protos := []setupOption{{"tcp", "TCP"}, {"udp", "UDP"}}
		if r.Chain != "nat" {
			protos = append(protos, setupOption{"all", "全部"}, setupOption{"icmp", "ICMP"})
		}
		rows = append(rows, setupChoice("协议", &r.Protocol, protos, func() {
			if r.Protocol != "tcp" && r.Protocol != "udp" {
				r.DstPort = ""
			}
			refresh()
		}))
		if r.Protocol == "tcp" || r.Protocol == "udp" {
			rows = append(rows, editSetupString(u, "目标端口", &r.DstPort, refresh))
		}
	}
	rows = append(rows, editSetupString(u, "来源 IPv4 / CIDR", &r.SrcIP, refresh), editSetupString(u, "目标 IPv4 / CIDR", &r.DstIP, refresh))
	if r.Action == "dnat" {
		rows = append(rows, editSetupString(u, "转发至 IPv4:端口", &r.NatTarget, refresh))
	}
	rows = append(rows, setupRow{"添加并试用", "", func() { u.firewallEdit("apply", *r) }})
	u.define("Enter 切换选项或编辑字段。", rows...)
}

func (u *setupUI) firewallEdit(path string, request any) {
	if err := u.api("firewall/"+path, request, nil); err != nil {
		u.fail(err)
		return
	}
	u.message = ""
	u.group = "防火墙"
	u.selected = 0
	u.loadFirewall()
	u.showFirewall()
}

func (u *setupUI) loadSSH() {
	u.fail(u.api("ssh/config", nil, &u.ssh))
	u.showSSH()
}
func (u *setupUI) showSSH() {
	s := &u.ssh
	c := &s.Config
	if !s.Supported {
		u.define("", setupRow{"此板包未启用可配置 SSH", "", nil})
		return
	}
	keyLines := strings.FieldsFunc(c.Keys, func(r rune) bool { return r == '\n' })
	rows := []setupRow{
		setupToggle("SSH 监听", &c.Enabled, u.showSSH),
		{"监听端口", strconv.Itoa(c.Port), func() {
			u.change("监听端口", strconv.Itoa(c.Port), false, func(p string) { c.Port, _ = strconv.Atoi(p); u.showSSH() })
		}},
		setupToggle("允许密码登录", &c.Password, u.showSSH),
		{"添加公钥", fmt.Sprintf("%d 条", len(keyLines)), func() {
			u.change("输入一条 OpenSSH 公钥", "", false, func(key string) {
				key = strings.TrimSpace(key)
				if key != "" {
					c.Keys = strings.Join(append(keyLines, key), "\n")
				}
				u.showSSH()
			})
		}},
		{"清空公钥", "", func() {
			u.confirm("清空公钥", "保存后从恢复系统移除已配置公钥。", func() { c.Keys = ""; u.showSSH() })
		}},
		{"保存并应用", "", func() {
			var r struct{ Message string }
			if err := u.api("ssh/config", *c, &r); err != nil {
				u.fail(err)
			} else {
				u.message = r.Message
			}
		}},
	}
	hint := "配置远程登录。物理终端会继续使用本地账号。"
	if !s.Persistent {
		hint = "这些设置仅在本次启动中生效。"
	}
	u.define(hint, rows...)
}

func setupAddress(address string) string {
	if p, err := netip.ParsePrefix(address); err == nil {
		return p.Addr().String()
	}
	return address
}
