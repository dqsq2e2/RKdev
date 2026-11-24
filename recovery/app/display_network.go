package main

import (
	"fmt"
	"net/netip"
	"reflect"
	"slices"
	"strconv"
	"strings"
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
	base, draft     networkConfig
	loaded          bool
	loadingDefaults bool
	io              setupSettingIO
}

func (n *setupNetworkModel) changed() bool {
	return !reflect.DeepEqual(n.draft, n.base)
}

type setupSettingIO struct {
	reading  bool
	revision uint64
}

func (op *setupSettingIO) read(u *setupUI, path string, dst any, done func(error)) {
	if op.reading {
		return
	}
	op.reading = true
	rev := op.revision
	u.async(func() error { return u.api(path, nil, dst) }, func(err error) {
		op.reading = false
		if rev == op.revision {
			done(err)
		}
	})
}

type setupFirewallModel struct {
	live        setupFirewallSnapshot
	base, draft firewallSave
	loaded      bool
	newRule     firewallRule
}

func (f *setupFirewallModel) changed() bool {
	return f.draft.Enabled != f.base.Enabled || !slices.Equal(f.draft.Rules, f.base.Rules)
}

func cloneSetupFirewall(c firewallSave) firewallSave {
	c.Rules = slices.Clone(c.Rules)
	return c
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

func (u *setupUI) choice(label string, value *string, options []setupOption, updated func()) setupRow {
	i := slices.IndexFunc(options, func(o setupOption) bool { return o.key == *value })
	desc := *value
	if i >= 0 {
		desc = options[i].label
	}
	if len(options) == 0 {
		return setupRow{label, desc, nil}
	}
	return setupRow{label, desc, func() {
		u.choose(label, options, max(0, i), func(key string) { *value = key; updated() })
	}}
}

func setupToggle(label string, value *bool, updated func()) setupRow {
	return setupRow{label, setupYes(*value), func() { *value = !*value; updated() }}
}

func (u *setupUI) loadNetworkPage() {
	u.showNetwork()
	u.refreshNetwork(true)
}

func cloneSetupNetwork(c networkConfig) networkConfig {
	c.WAN.DNS = slices.Clone(c.WAN.DNS)
	c.LAN.Ports = slices.Clone(c.LAN.Ports)
	return c
}

func (u *setupUI) resetNetworkDraft() {
	u.network.draft = cloneSetupNetwork(u.network.base)
}

func (u *setupUI) loadFirewall(reportError bool) {
	var fresh setupFirewallSnapshot
	u.firewallIO.read(u, "setup/firewall", &fresh, func(err error) {
		inGroup := u.page == setupPageNetwork && u.group == "防火墙"
		if err != nil {
			if reportError && inGroup {
				u.fail(err)
			}
		} else {
			f := &u.firewall
			f.live = fresh
			if !f.changed() && u.dialog == nil {
				f.loaded, f.base, f.draft = true, fresh.Config, cloneSetupFirewall(fresh.Config)
			}
		}
		if inGroup {
			u.showNetwork()
		}
	})
}

func (u *setupUI) updateNetwork() {
	u.refreshNetwork(false)
	u.showNetwork()
}

func (u *setupUI) refreshNetwork(reportError bool) {
	var state setupNetState
	u.network.io.read(u, "network/config", &state, func(err error) {
		if err != nil {
			if reportError && u.page == setupPageNetwork {
				u.fail(err)
			}
		} else {
			u.trial = state
			if state.Trial == nil && !u.network.changed() && u.dialog == nil {
				u.network.loaded, u.network.base = true, state.Config
				u.resetNetworkDraft()
			}
		}
		if u.page == setupPageNetwork {
			u.showNetwork()
		}
	})
	if u.group == "防火墙" {
		u.loadFirewall(reportError)
	}
}

func (u *setupUI) networkDefaults() {
	u.confirm("载入默认网络设置", "替换 WAN、LAN 和 Wi-Fi 草稿。", func() {
		var c networkConfig
		visit := u.visit
		u.network.loadingDefaults = true
		u.showNetwork()
		u.async(func() error { return u.api("setup/network_defaults", nil, &c) }, func(err error) {
			u.network.loadingDefaults = false
			if u.visit != visit {
				return
			}
			u.fail(err)
			if err == nil {
				u.network.draft = c
			}
			u.showNetwork()
		})
	})
}

func (u *setupUI) showNetwork() {
	n := &u.trial
	c := &u.network.draft
	var rows []setupRow
	hint := ""
	if u.network.loadingDefaults {
		u.define("正在读取默认配置…")
		return
	}
	if n.Trial != nil {
		u.define("已有网络配置正在试用", setupRow{"自动回退", fmt.Sprintf("%d 秒", trialSeconds(n.Trial.Deadline)), nil})
		return
	}
	if u.group == "" {
		for _, part := range []struct{ name, value string }{
			{"WAN", n.Addresses[n.Config.WAN.Port]}, {"LAN", n.LANActual}, {"Wi-Fi", c.WiFi.SSID}, {"防火墙", ""},
		} {
			rows = append(rows, setupRow{part.name, part.value, func() {
				u.group = part.name
				u.selected = 0
				if u.group == "防火墙" {
					u.loadFirewall(true)
				}
				u.showNetwork()
			}})
		}
		if u.network.loaded && n.Supported && n.Ready {
			rows = append(rows, setupRow{"载入默认网络设置", "", u.networkDefaults})
		}
		u.define(hint, rows...)
		return
	}
	if u.group == "防火墙" || u.group == "fw-add" {
		u.showFirewall()
		return
	}
	if !u.network.loaded || !n.Supported || !n.Ready {
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
			u.choice("物理网口", &c.WAN.Port, ports, refresh),
			u.choice("连接方式", &c.WAN.Mode, []setupOption{{"dhcp", "自动获取 DHCP"}, {"static", "静态 IPv4"}, {"off", "关闭"}}, refresh))
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
		rows = append(rows, u.choice("工作模式", &c.WiFi.Mode, modes, refresh))
		if c.WiFi.Mode != "off" {
			rows = append(rows, editSetupString(u, "SSID", &c.WiFi.SSID, refresh))
			if c.WiFi.Mode == "ap" {
				var options []setupOption
				for _, b := range n.APBands {
					options = append(options, setupOption{b, b + " GHz"})
				}
				rows = append(rows, u.choice("频段", &c.WiFi.Band, options, func() {
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
	u.define(hint, rows...)
}

func (u *setupUI) showFirewall() {
	f := &u.firewall
	if !f.loaded {
		u.define("正在读取防火墙设置…")
		return
	}
	if u.group == "fw-add" {
		u.showFirewallRule()
		return
	}
	if t := f.live.Trial; t != nil {
		u.define("已有防火墙配置正在试用", setupRow{"自动回退", fmt.Sprintf("%d 秒", trialSeconds(t.Deadline)), nil})
		return
	}
	rows := []setupRow{
		setupToggle("防火墙", &f.draft.Enabled, u.showFirewall),
		{"添加规则", "", func() {
			u.group = "fw-add"
			u.selected = 0
			if f.newRule.Chain == "" {
				f.newRule = firewallRule{Chain: "input", Action: "ACCEPT", Protocol: "all"}
			}
			u.showFirewallRule()
		}},
	}
	for i, r := range f.draft.Rules {
		rows = append(rows, setupRow{r.Chain + "  " + r.Action, strings.Join([]string{r.Protocol, r.SrcIP, r.DstIP, r.DstPort, r.NatTarget}, "  "), func() {
			u.confirm("删除防火墙规则", r.Chain+"  "+r.Action, func() {
				f.draft.Rules = slices.Delete(f.draft.Rules, i, i+1)
				u.showFirewall()
			})
		}})
	}
	u.define("", rows...)
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
		u.choice("规则类型", &r.Chain, chains, func() {
			r.Action, r.Protocol, r.SrcIP, r.DstIP, r.DstPort, r.NatTarget = "ACCEPT", "all", "", "", "", ""
			if r.Chain == "nat" {
				r.Action, r.Protocol = "dnat", "tcp"
			}
			refresh()
		}),
		u.choice("动作", &r.Action, actions, func() {
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
		rows = append(rows, u.choice("协议", &r.Protocol, protos, func() {
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
	rows = append(rows, setupRow{"添加规则", "", func() {
		if _, _, err := ruleArgs(*r); err != nil {
			u.fail(err)
			return
		}
		f := &u.firewall
		if len(f.draft.Rules) >= 48 {
			u.message = "最多可保存 48 条防火墙规则"
			return
		}
		f.draft.Rules = append(f.draft.Rules, *r)
		f.newRule = firewallRule{}
		u.group, u.message = "防火墙", ""
		u.selected = len(f.draft.Rules) + 1
		u.showFirewall()
	}})
	u.define("", rows...)
}

func setupAddress(address string) string {
	if p, err := netip.ParsePrefix(address); err == nil {
		return p.Addr().String()
	}
	return address
}
