package main

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
)

type managementState struct {
	SSH           *sshConfig `json:"ssh,omitempty"`
	Username      string     `json:"username"`
	Persistent    bool       `json:"persistent"`
	Message       string     `json:"message"`
	LoginRequired bool       `json:"login_required,omitempty"`
}

type managementEdit struct {
	SSH     *sshConfig   `json:"ssh,omitempty"`
	Account *accountEdit `json:"account,omitempty"`
}

func startSettingsControls(n *netController) {
	var store *recoverySettingsStore
	backend := "iptables"
	if n != nil {
		backend, store = n.defaults.Firewall, n.store
	} else if identityError == nil {
		var err error
		store, err = configuredRecoverySettingsStore(currentLayout, 0)
		if err != nil {
			log.Printf("settings store: %v", err)
		}
	}
	var saved recoverySettings
	if store != nil {
		saved, _ = store.read()
	}
	startFirewall(store, backend, saved.Firewall)
	s := &management
	s.accountDir, s.store = "/etc", store
	if err := s.startAccount(saved.Account); err != nil {
		s.message = "恢复账户初始化失败：" + err.Error()
	}
	if _, err := os.Stat("/usr/sbin/sshd"); err != nil {
		return
	}
	_, enabled := os.Stat("/etc/rkdev-managed-ssh")
	s.cfg, s.supported = sshConfig{Enabled: enabled == nil, Port: 22, Password: true}, true
	c := s.cfg
	if saved.SSH != nil && saved.SSH.validate() == nil {
		c = *saved.SSH
	}
	if err := s.applySSH(c, s.account.Username); err == nil {
		s.cfg = c
	} else {
		s.message = "SSH 启动失败：" + err.Error()
		if c != s.cfg {
			_ = s.applySSH(s.cfg, s.account.Username)
		}
	}
}

func (s *managementController) startAccount(saved *recoveryAccount) error {
	current, err := readRecoveryAccount(s.accountDir)
	if err != nil {
		return err
	}
	if accountHash.MatchString(current.Hash) {
		s.account = current
	}
	if saved != nil {
		if *saved == s.account {
			return nil
		}
		if err = installAccount(s.accountDir, current, *saved); err == nil {
			s.account = *saved
			return nil
		}
	}
	if s.account.Username == "" {
		hash, hashErr := hashAccountPassword("root")
		if hashErr != nil {
			return hashErr
		}
		root := recoveryAccount{"root", hash}
		if bootstrapErr := installAccount(s.accountDir, current, root); bootstrapErr != nil {
			return bootstrapErr
		}
		s.account = root
	}
	return err
}

func (s *managementController) state() managementState {
	v := managementState{Username: s.account.Username, Persistent: s.store != nil, Message: s.message}
	if s.supported {
		cfg := s.cfg
		v.SSH = &cfg
	}
	return v
}

// Validate and save both changes as one record. If applying the SSH listener
// or writing the record fails, keep the previous live settings.
func (s *managementController) commit(edit managementEdit) error {
	account, ssh := s.account, s.cfg
	if e := edit.Account; e != nil {
		account.Username = e.Username
		if e.Password != "" {
			hash, err := hashAccountPassword(e.Password)
			if err != nil {
				return err
			}
			account.Hash = hash
		}
	}
	if edit.SSH != nil {
		ssh = *edit.SSH
	}
	accountChanged := account != s.account
	sshChanged := s.supported && (ssh != s.cfg || account.Username != s.account.Username)
	if accountChanged {
		if err := installAccount(s.accountDir, s.account, account); err != nil {
			return err
		}
	}
	rollback := func(cause error) error {
		var revert error
		if accountChanged {
			revert = installAccount(s.accountDir, account, s.account)
		}
		if sshChanged {
			revert = errors.Join(revert, s.applySSH(s.cfg, s.account.Username))
		}
		if revert != nil {
			return fmt.Errorf("%w；恢复原管理设置失败：%v", cause, revert)
		}
		return cause
	}
	if sshChanged {
		if err := s.applySSH(ssh, account.Username); err != nil {
			return rollback(err)
		}
	}
	if s.store != nil && (accountChanged || sshChanged) {
		if err := s.store.save(func(saved *recoverySettings) {
			if accountChanged {
				saved.Account = &account
			}
			if edit.SSH != nil {
				saved.SSH = &ssh
			}
		}); err != nil {
			return rollback(err)
		}
	}
	s.account, s.cfg, s.message = account, ssh, "管理设置已保存并生效"
	if accountChanged {
		s.sessions.revokeAll()
	}
	if s.store == nil {
		s.message = "已生效，仅本次启动"
	}
	return nil
}

func (s *managementController) handle(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		s.mu.Lock()
		v := s.state()
		s.mu.Unlock()
		jsonOK(w, v)
		return
	}
	if r.Method != http.MethodPost {
		jsonErr(w, 405, "仅支持 GET/POST")
		return
	}
	var edit managementEdit
	if err := decodeSettingsRequest(w, r, &edit, 8<<10); err != nil || (edit.Account == nil && edit.SSH == nil) {
		jsonErr(w, 400, "无效管理设置")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	local := localSetup(r)
	if !local {
		if session := requestWebSession(r); session == nil || session.ctx.Err() != nil {
			w.Header().Set("X-RKdev-Login", "required")
			jsonErr(w, http.StatusUnauthorized, "请先登录恢复系统")
			return
		}
	}
	if edit.Account != nil {
		if s.account.Username == "" {
			jsonErr(w, 409, "恢复账户不可配置")
			return
		}
		if err := edit.Account.validate(); err != nil {
			jsonErr(w, 400, err.Error())
			return
		}
	}
	if edit.SSH != nil {
		if !s.supported {
			jsonErr(w, 409, "SSH 未安装")
			return
		}
		if err := edit.SSH.validate(); err != nil {
			jsonErr(w, 400, err.Error())
			return
		}
	}
	if !configTrialMu.TryLock() {
		jsonErr(w, 409, "已有配置正在应用")
		return
	}
	defer configTrialMu.Unlock()
	old := s.account
	if err := s.commit(edit); err != nil {
		jsonErr(w, 409, err.Error())
		return
	}
	state := s.state()
	if !local && old != s.account {
		webCookie(w, "")
		state.LoginRequired = true
	}
	jsonOK(w, state)
}
