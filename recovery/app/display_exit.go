package main

import (
	"fmt"
	"strings"
	"time"
)

const (
	setupSaveNetwork  = "网络"
	setupSaveFirewall = "防火墙"
	setupSaveManage   = "管理"
	setupSaveBoot     = "引导"
)

type setupSave struct {
	reboot        bool
	step          string
	saved         []string
	sessionOnly   []string
	trial         setupTrial
	busy, polling bool
	problem       string
}

type setupTrial struct {
	ID, Status string
	Deadline   time.Time
}

func (u *setupUI) changes() []string {
	var names []string
	for _, s := range []struct {
		name    string
		changed bool
	}{{setupSaveBoot, u.boot.changed()}, {setupSaveNetwork, u.network.changed()},
		{setupSaveFirewall, u.firewall.changed()}, {setupSaveManage, u.management.changed()}} {
		if s.changed {
			names = append(names, s.name)
		}
	}
	return names
}

func (u *setupUI) showExit() {
	if u.save != nil {
		u.showSave()
		return
	}
	var save func()
	if len(u.changes()) > 0 {
		save = func() { u.savePrompt(false) }
	}
	u.define("", setupRow{"保存并退出", "重新启动设备", func() { u.savePrompt(true) }},
		setupRow{"保存更改", "留在设置界面", save},
		setupRow{"放弃更改并退出", "重新启动设备", func() { u.discardPrompt(true) }},
		setupRow{"放弃更改", "留在设置界面", func() { u.discardPrompt(false) }})
}

func (u *setupUI) exitPage() {
	if u.page != setupPageExit {
		u.enter(setupPageExit)
	}
}

func (u *setupUI) discardPrompt(reboot bool) {
	u.confirm("放弃更改", "撤销尚未保存的设置。", func() {
		u.boot.reset()
		u.resetNetworkDraft()
		u.firewall.draft = cloneSetupFirewall(u.firewall.base)
		u.firewall.newRule = firewallRule{}
		u.management.reset(u.management.base)
		u.message = "已放弃未保存的更改"
		u.showExit()
		if reboot {
			u.rebootSetup()
		}
	})
}

func (u *setupUI) savePrompt(reboot bool) {
	changes := u.changes()
	title, body := "保存更改", strings.Join(changes, " · ")
	if reboot {
		title = "保存并退出"
	}
	if len(changes) == 0 {
		if reboot {
			u.confirm("重新启动设备", "没有待保存的更改。", u.rebootSetup)
		}
		return
	}
	u.confirm(title, body, func() {
		management := u.management.edit()
		if management.SSH != nil {
			if err := management.SSH.validate(); err != nil {
				u.fail(err)
				return
			}
		}
		if management.Account != nil {
			if err := management.Account.validate(); err != nil {
				u.fail(err)
				return
			}
		}
		u.exitPage()
		u.bootIO.revision++
		u.network.io.revision++
		u.firewallIO.revision++
		u.management.io.revision++
		u.save = &setupSave{reboot: reboot}
		u.advanceSave()
	})
}

// Network and firewall trials share one backend lock. Each trial is explicitly
// confirmed before the next step starts; failed or reverted steps keep drafts.
func (u *setupUI) advanceSave() {
	s := u.save
	s.trial, s.problem, s.busy = setupTrial{}, "", true
	switch {
	case u.network.changed():
		s.step = setupSaveNetwork
		var t setupTrial
		config := cloneSetupNetwork(u.network.draft)
		u.saveRequest("network/config", map[string]any{"action": "apply", "config": config}, &t, func() {
			s.trial = t
			u.updateSave()
		})
	case u.firewall.changed():
		s.step = setupSaveFirewall
		var t setupTrial
		edit := setupFirewallEdit{cloneSetupFirewall(u.firewall.base), cloneSetupFirewall(u.firewall.draft)}
		u.saveRequest("setup/firewall", edit, &t, func() {
			s.trial = t
			u.updateSave()
		})
	case u.management.changed():
		s.step = setupSaveManage
		var state managementState
		u.saveRequest("management", u.management.edit(), &state, func() {
			u.management.reset(state)
			u.savedStep(state.Persistent)
		})
	case u.boot.changed():
		s.step = setupSaveBoot
		u.saveRequest("setup/boot", u.boot.edit, nil, func() {
			// Read back the new revision and values next time the page is opened.
			u.boot = setupBootState{}
			u.savedStep(true)
		})
	default:
		u.save = nil
		u.message = "设置已保存"
		if len(s.sessionOnly) > 0 {
			u.message = "仅本次启动有效：" + strings.Join(s.sessionOnly, "、")
			if len(s.sessionOnly) < len(s.saved) {
				u.message = "已保存部分设置；" + u.message
			}
		}
		u.showExit()
		if s.reboot && len(s.sessionOnly) == 0 {
			u.rebootSetup()
		}
		return
	}
	u.selected, u.scrolled = 0, 0
	u.message = ""
	u.showSave()
}

func (u *setupUI) savedStep(persistent bool) {
	s := u.save
	s.saved = append(s.saved, s.step)
	if !persistent {
		s.sessionOnly = append(s.sessionOnly, s.step)
	}
	u.advanceSave()
}

func (u *setupUI) saveRequest(path string, req, dst any, ok func()) {
	s := u.save
	u.async(func() error { return u.api(path, req, dst) }, func(err error) {
		s.busy = false
		if err != nil {
			if s.trial.ID == "" {
				u.stopSave(err.Error())
			} else {
				s.problem = err.Error()
				u.showSave()
			}
			return
		}
		ok()
		if u.save == s {
			u.showSave()
		}
	})
}

func (u *setupUI) stopSave(problem string) {
	s := u.save
	u.save = nil
	u.message = s.step + "未保存：" + problem
	if len(s.saved) > 0 {
		u.message = "已确认 " + strings.Join(s.saved, "、") + "；" + u.message
	}
	u.selected, u.scrolled = 0, 0
	u.showExit()
}

func (u *setupUI) updateSave() {
	s := u.save
	if s.busy || s.polling || s.trial.ID == "" {
		u.showSave()
		return
	}
	s.polling = true
	step, id := s.step, s.trial.ID
	var network setupNetState
	var firewall setupFirewallSnapshot
	u.async(func() error {
		if step == setupSaveNetwork {
			return u.api("network/config", nil, &network)
		}
		return u.api("setup/firewall", nil, &firewall)
	}, func(err error) {
		s.polling = false
		if u.save != s || s.trial.ID != id || s.busy {
			return
		}
		if err != nil {
			u.showSave()
			return
		}
		message := network.Message
		var trial setupTrial
		if step == setupSaveNetwork {
			u.trial = network
			if t := network.Trial; t != nil {
				trial = setupTrial{t.ID, t.Status, t.Deadline}
			}
		} else {
			u.firewall.live = firewall
			message = firewall.Message
			if t := firewall.Trial; t != nil {
				trial = setupTrial{t.ID, t.Status, t.Deadline}
			}
		}
		if trial.ID != id {
			if message == "" {
				message = "试用已结束"
			}
			u.stopSave(message)
			return
		}
		s.trial = trial
		u.showSave()
	})
}

func (u *setupUI) finishSetupTrial(confirm bool) {
	s := u.save
	if s.busy || s.trial.Status != "ready" || !time.Now().Before(s.trial.Deadline) {
		return
	}
	var accepted struct{ Message string }
	act := "revert"
	if confirm {
		act = "confirm"
	}
	s.busy, s.problem = true, ""
	if s.step == setupSaveNetwork {
		u.saveRequest("network/config", map[string]string{"action": act, "id": s.trial.ID}, &accepted, func() {
			if !confirm {
				s.trial.Status = "reverting"
				return
			}
			u.network.base = cloneSetupNetwork(u.network.draft)
			u.network.base.WiFi.Password = nil
			u.resetNetworkDraft()
			u.trial.Trial = nil
			u.savedStep(u.trial.Persistent)
		})
	} else {
		u.saveRequest("firewall/trial", map[string]string{"operation": act, "trial": s.trial.ID}, &accepted, func() {
			u.firewall.live.Trial = nil
			if !confirm {
				u.stopSave(accepted.Message)
				return
			}
			u.firewall.base = cloneSetupFirewall(u.firewall.draft)
			u.savedStep(u.firewall.live.Persistent)
		})
	}
	u.showSave()
}

func (u *setupUI) showSave() {
	s := u.save
	hint := "正在保存 " + s.step + "…"
	var rows []setupRow
	if s.trial.ID != "" {
		switch s.trial.Status {
		case "applying":
			hint = "正在应用" + s.step + "试用配置…"
		case "reverting":
			hint = "正在恢复先前" + s.step + "配置…"
		case "ready":
			hint = s.step + "配置正在试用"
		}
		left := trialSeconds(s.trial.Deadline)
		if s.step == setupSaveNetwork && s.trial.Status == "ready" {
			n := u.trial
			addr := ""
			switch {
			case n.Config.WAN.Mode != "off" && len(n.Config.LAN.Ports) == 0 && n.Config.WiFi.Mode != "ap":
				addr = setupAddress(n.Addresses[n.Config.WAN.Port])
			case len(n.Config.LAN.Ports) > 0 || n.Config.WiFi.Mode == "ap":
				addr = setupAddress(n.LANActual)
			}
			if addr != "" {
				rows = append(rows, setupRow{"试用管理地址", "https://" + addr + "/", nil})
			} else if n.Config.WAN.Mode == "dhcp" {
				rows = append(rows, setupRow{"WAN", "等待 DHCP 地址", nil})
			}
		}
		rows = append(rows, setupRow{"自动回退", fmt.Sprintf("%d 秒", left), nil})
		if !s.busy && s.trial.Status == "ready" && left > 0 {
			if s.step != setupSaveNetwork || u.trial.Warning == "" {
				rows = append(rows, setupRow{"确认试用配置", "", func() { u.finishSetupTrial(true) }})
			}
			rows = append(rows, setupRow{"回退试用配置", "", func() { u.finishSetupTrial(false) }})
		}
	}
	if len(s.saved) > 0 {
		rows = append(rows, setupRow{"本次已确认", strings.Join(s.saved, "、"), nil})
	}
	u.message = s.problem
	if u.message == "" && s.step == setupSaveNetwork && s.trial.Status == "ready" {
		u.message = u.trial.Warning
	}
	u.define(hint, rows...)
}

func (u *setupUI) rebootSetup() {
	if u.rebooting {
		return
	}
	u.rebooting = true
	u.message = "正在重新启动…"
	u.async(func() error { return u.api("reboot", struct{}{}, nil) }, func(err error) {
		u.rebooting = false
		u.fail(err)
	})
}
