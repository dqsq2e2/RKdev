package main

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"os"
	"os/exec"
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

type managementController struct {
	mu         sync.Mutex
	cfg        sshConfig
	supported  bool
	account    recoveryAccount
	sessions   webSessions
	accountDir string
	store      *recoverySettingsStore
	proc       *netRuntime
	message    string
}

var management managementController
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

func (s *managementController) applySSH(c sshConfig, username string) error {
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
	for _, dir := range []string{"/run/sshd", "/var/empty"} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return err
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := exec.CommandContext(ctx, "ssh-keygen", "-A").Run(); err != nil {
		return fmt.Errorf("SSH 主机密钥初始化失败：%w", err)
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
	if username != "root" {
		args = append(args, "-o", "DenyUsers=root")
	}
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
