package main

import (
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

type sshConfig struct {
	Enabled  bool   `json:"enabled"`
	Port     int    `json:"port"`
	Password bool   `json:"password"`
	Keys     string `json:"keys"`
}

type sshController struct {
	mu        sync.Mutex
	cfg       sshConfig
	supported bool
	store     *recoverySettingsStore
	proc      *netRuntime
	message   string
}

var recoverySSH sshController
var sshType = regexp.MustCompile(`^(ssh-(ed25519|rsa)|ecdsa-sha2-nistp(256|384|521)|sk-(ssh-ed25519|ecdsa-sha2-nistp256)@openssh.com)$`)

func (c sshConfig) validate() error {
	if c.Port < 1 || c.Port > 65535 || c.Port == 80 || c.Port == 443 {
		return fmt.Errorf("无效 SSH 端口，80 和 443 用于恢复网页")
	}
	if c.Enabled && !c.Password && strings.TrimSpace(c.Keys) == "" {
		return fmt.Errorf("关闭密码登录前请添加至少一个 SSH 公钥")
	}
	if len(c.Keys) > 4096 {
		return fmt.Errorf("SSH 公钥超出容量")
	}
	for _, line := range strings.Split(c.Keys, "\n") {
		p := strings.Fields(line)
		if len(p) == 0 {
			continue
		}
		if len(p) < 2 || !sshType.MatchString(p[0]) || strings.ContainsAny(line, "\r\x00") {
			return fmt.Errorf("每行填写一个 OpenSSH 公钥，例如 ssh-ed25519")
		}
		b, err := base64.StdEncoding.DecodeString(p[1])
		if err != nil || len(b) < len(p[0])+4 || int(binary.BigEndian.Uint32(b)) != len(p[0]) || string(b[4:4+len(p[0])]) != p[0] {
			return fmt.Errorf("SSH 公钥内容不匹配")
		}
	}
	return nil
}

func startSettingsControls(n *netController) {
	var store *recoverySettingsStore
	var saved recoverySettings
	backend := "iptables"
	if n != nil {
		backend, store = n.defaults.Firewall, n.store
		if store != nil {
			saved, _ = store.read()
		}
	}
	startFirewall(store, backend, saved.Firewall)
	s := &recoverySSH
	if _, err := os.Stat("/etc/rkdev-managed-ssh"); err != nil {
		return
	}
	s.cfg, s.supported, s.store = sshConfig{Enabled: true, Port: 22, Password: true}, true, store
	if c := saved.SSH; c != nil && c.validate() == nil {
		if err := s.apply(*c); err == nil {
			s.cfg = *c
			return
		} else {
			s.message = "未能加载保存的 SSH，尝试恢复默认：" + err.Error()
		}
	}
	if err := s.apply(s.cfg); err != nil {
		s.message = "SSH 启动失败：" + err.Error()
	}
}

func (s *sshController) apply(c sshConfig) error {
	const keys = netRunDir + "/ssh-authorized-keys"
	if err := os.MkdirAll(netRunDir, 0700); err != nil {
		return err
	}
	if s.proc != nil {
		for _, p := range s.proc.procs {
			_ = p.cmd.Process.Signal(syscall.SIGTERM)
			select {
			case <-p.done:
			case <-time.After(time.Second):
				_ = p.cmd.Process.Kill()
				<-p.done
			}
		}
		s.proc = nil
	}
	if !c.Enabled {
		return nil
	}
	if err := os.WriteFile(keys, []byte(strings.TrimSpace(c.Keys)+"\n"), 0600); err != nil {
		return err
	}
	if strings.TrimSpace(c.Keys) != "" {
		if err := netExec("ssh-keygen", "-lf", keys); err != nil {
			return fmt.Errorf("sshd 无法识别公钥")
		}
	}
	pw := "no"
	if c.Password {
		pw = "yes"
	}
	args := []string{"-p", strconv.Itoa(c.Port), "-o", "PermitRootLogin=yes", "-o", "PasswordAuthentication=" + pw,
		"-o", "KbdInteractiveAuthentication=no", "-o", "AuthorizedKeysFile=" + keys + " .ssh/authorized_keys"}
	if err := netExec("/usr/sbin/sshd", append([]string{"-t"}, args...)...); err != nil {
		return err
	}
	p := &netRuntime{}
	if err := p.spawn("/usr/sbin/sshd", nil, append([]string{"-D", "-e"}, args...)...); err != nil {
		return err
	}
	s.proc = p
	return nil
}

func handleSSHConfig(w http.ResponseWriter, r *http.Request) {
	s := &recoverySSH
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.Method == http.MethodGet {
		jsonOK(w, map[string]any{"supported": s.supported, "config": s.cfg, "persistent": s.store != nil, "message": s.message})
		return
	}
	if r.Method != http.MethodPost {
		jsonErr(w, 405, "仅支持 GET/POST")
		return
	}
	if !s.supported || !configTrialMu.TryLock() {
		jsonErr(w, 409, "SSH 不可配置或已有配置正在应用")
		return
	}
	defer configTrialMu.Unlock()
	var c sshConfig
	if err := decodeSettingsRequest(w, r, &c, 6<<10); err != nil {
		jsonErr(w, 400, "无效 SSH 配置")
		return
	}
	if err := c.validate(); err != nil {
		jsonErr(w, 400, err.Error())
		return
	}
	if err := s.apply(c); err != nil {
		_ = s.apply(s.cfg)
		jsonErr(w, 409, err.Error())
		return
	}
	message := "已生效，仅本次启动"
	if s.store != nil {
		if err := s.store.save(func(saved *recoverySettings) { saved.SSH = &c }); err != nil {
			_ = s.apply(s.cfg)
			jsonErr(w, 409, "未保存 SSH："+err.Error())
			return
		}
		message = "SSH 已保存并生效"
	}
	s.cfg, s.message = c, message
	jsonOK(w, map[string]string{"message": message})
}
