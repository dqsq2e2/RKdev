package main

import (
	"fmt"
	"strconv"
	"strings"
)

type setupManagementModel struct {
	base    managementState
	ssh     sshConfig
	account accountEdit
	io      setupSettingIO
	loaded  bool
}

func (m *setupManagementModel) reset(state managementState) {
	m.base, m.account = state, accountEdit{Username: state.Username}
	if state.SSH != nil {
		m.ssh = *state.SSH
	}
}

func (m *setupManagementModel) changed() bool {
	return m.account.Username != m.base.Username || m.account.Password != "" ||
		(m.base.SSH != nil && m.ssh != *m.base.SSH)
}

func (m *setupManagementModel) edit() managementEdit {
	var e managementEdit
	if m.account.Username != m.base.Username || m.account.Password != "" {
		account := m.account
		e.Account = &account
	}
	if m.base.SSH != nil && m.ssh != *m.base.SSH {
		ssh := m.ssh
		e.SSH = &ssh
	}
	return e
}

func (u *setupUI) loadManagement() {
	u.showManagement()
	var state managementState
	m := &u.management
	m.io.read(u, "management", &state, func(err error) {
		if err == nil && !m.changed() && u.dialog == nil {
			m.reset(state)
			m.loaded = true
		}
		if u.page == setupPageManagement {
			u.showManagement()
			u.fail(err)
		}
	})
}

func (u *setupUI) showManagement() {
	m := &u.management
	if !m.loaded {
		u.define("正在读取管理设置…")
		return
	}
	var rows []setupRow
	if m.base.Username != "" {
		rows = append(rows, editSetupString(u, "恢复系统账号", &m.account.Username, u.showManagement))
		detail := "未更改"
		if m.account.Password != "" {
			detail = "已暂存"
		}
		rows = append(rows, setupRow{"恢复系统密码", detail, func() {
			u.change("新密码（留空保留现有密码）", "", true, func(password string) {
				if password == "" {
					m.account.Password = ""
					u.showManagement()
					return
				}
				if !validAccountPassword(password) {
					u.message = "密码最多 128 字节，且不能含控制字符"
					return
				}
				u.change("确认新密码", "", true, func(confirmation string) {
					if password != confirmation {
						u.message = "两次输入的密码不一致"
						return
					}
					m.account.Password = password
					u.message = ""
					u.showManagement()
				})
			})
		}})
	}
	if m.base.SSH != nil {
		c := &m.ssh
		keyLines := strings.FieldsFunc(c.Keys, func(r rune) bool { return r == '\n' })
		rows = append(rows,
			setupToggle("SSH 监听", &c.Enabled, u.showManagement),
			setupRow{"SSH 监听端口", strconv.Itoa(c.Port), func() {
				u.change("SSH 监听端口", strconv.Itoa(c.Port), false, func(p string) { c.Port, _ = strconv.Atoi(p); u.showManagement() })
			}},
			setupToggle("SSH 密码登录", &c.Password, u.showManagement),
			setupRow{"添加 SSH 公钥", fmt.Sprintf("%d 条", len(keyLines)), func() {
				u.change("输入一条 OpenSSH 公钥", "", false, func(key string) {
					if key = strings.TrimSpace(key); key != "" {
						c.Keys = strings.Join(append(keyLines, key), "\n")
					}
					u.showManagement()
				})
			}},
			setupRow{"清空 SSH 公钥", "", func() {
				u.confirm("清空 SSH 公钥", "保存后从恢复系统移除已配置公钥。", func() { c.Keys = ""; u.showManagement() })
			}})
	}
	if len(rows) == 0 {
		rows = append(rows, setupRow{"管理设置不可用", "", nil})
	}
	hint := m.base.Message
	if hint == "" && !m.base.Persistent {
		hint = "设置仅在本次启动中生效。"
	}
	u.define(hint, rows...)
}
