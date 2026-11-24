package main

import (
	"crypto/rand"
	"crypto/subtle"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"
)

type recoveryAccount struct {
	Username string `json:"username"`
	Hash     string `json:"hash"`
}

type accountEdit struct {
	Username string `json:"username"`
	Password string `json:"password,omitempty"`
}

var accountName = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)
var accountHash = regexp.MustCompile(`^\$(1|5|6)\$([./0-9A-Za-z]{1,16})\$[./0-9A-Za-z]+$`)

const accountGecos = "RKdev recovery administrator"

func validAccountPassword(password string) bool {
	return len(password) > 0 && len(password) <= 128 && strings.IndexFunc(password, unicode.IsControl) < 0
}

func (e accountEdit) validate() error {
	if !accountName.MatchString(e.Username) {
		return fmt.Errorf("账号须以小写字母或下划线开头，可使用小写字母、数字、下划线和短横线，最多 32 字符")
	}
	if e.Password != "" && !validAccountPassword(e.Password) {
		return fmt.Errorf("密码最多 128 字节，且不能含控制字符")
	}
	return nil
}

func cryptAccountPassword(password, method, salt string) (string, error) {
	cmd := exec.Command("/bin/busybox", "cryptpw", "-m", method, "-S", salt, "-P", "0")
	cmd.Stdin = strings.NewReader(password + "\n")
	out, err := cmd.Output()
	hash := strings.TrimSpace(string(out))
	if err != nil || !accountHash.MatchString(hash) {
		return "", fmt.Errorf("无法计算恢复账户密码")
	}
	return hash, nil
}

func hashAccountPassword(password string) (string, error) {
	var salt [8]byte
	if _, err := rand.Read(salt[:]); err != nil {
		return "", fmt.Errorf("无法生成恢复账户密码盐值")
	}
	return cryptAccountPassword(password, "sha512", fmt.Sprintf("%x", salt))
}

func (a recoveryAccount) authenticates(password string) bool {
	m := accountHash.FindStringSubmatch(a.Hash)
	if len(m) == 0 || !validAccountPassword(password) {
		return false
	}
	method := map[string]string{"1": "md5", "5": "sha256", "6": "sha512"}[m[1]]
	hash, err := cryptAccountPassword(password, method, m[2])
	return err == nil && subtle.ConstantTimeCompare([]byte(hash), []byte(a.Hash)) == 1
}

func accountLine(data, name string) (int, []string) {
	for i, line := range strings.Split(data, "\n") {
		if strings.HasPrefix(line, name+":") {
			return i, strings.Split(line, ":")
		}
	}
	return -1, nil
}

func readRecoveryAccount(dir string) (recoveryAccount, error) {
	passwd, err := os.ReadFile(filepath.Join(dir, "passwd"))
	if err != nil {
		return recoveryAccount{}, err
	}
	shadow, err := os.ReadFile(filepath.Join(dir, "shadow"))
	if err != nil {
		return recoveryAccount{}, err
	}
	name := "root"
	for _, line := range strings.Split(string(passwd), "\n") {
		p := strings.Split(line, ":")
		if len(p) == 7 && p[2] == "0" && p[3] == "0" && p[4] == accountGecos && accountName.MatchString(p[0]) {
			name = p[0]
			break
		}
	}
	_, entry := accountLine(string(shadow), name)
	if len(entry) != 9 {
		return recoveryAccount{}, fmt.Errorf("恢复账户文件缺少登录记录")
	}
	return recoveryAccount{name, entry[1]}, nil
}

func replaceAccountFile(path, data string, mode os.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".rkdev-account-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, err = f.WriteString(data)
	if err == nil {
		err = f.Chmod(mode)
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(f.Name(), path)
}

// The internal root identity stays available to system services. Only the
// selected username has a usable password; an alternate name shares UID 0.
func installAccount(dir string, old, next recoveryAccount) error {
	if !accountName.MatchString(next.Username) || !accountHash.MatchString(next.Hash) {
		return fmt.Errorf("恢复账户配置无效")
	}
	passwd, shadow := filepath.Join(dir, "passwd"), filepath.Join(dir, "shadow")
	p, err := os.ReadFile(passwd)
	if err != nil {
		return err
	}
	s, err := os.ReadFile(shadow)
	if err != nil {
		return err
	}
	if next.Username != "root" && next.Username != old.Username {
		_, existing := accountLine(string(p), next.Username)
		_, existingShadow := accountLine(string(s), next.Username)
		if existing != nil || existingShadow != nil {
			return fmt.Errorf("该账号已存在")
		}
	}
	update := func(data string, expected, secret int) (string, error) {
		idx, root := accountLine(data, "root")
		if len(root) != expected || (secret == 0 && (root[2] != "0" || root[3] != "0")) {
			return "", fmt.Errorf("恢复账户文件无效")
		}
		lines := strings.Split(strings.TrimRight(data, "\n"), "\n")
		active := append([]string(nil), root...)
		active[0] = next.Username
		if secret != 0 {
			active[secret] = next.Hash
			root[secret] = "!"
		}
		lines[idx] = strings.Join(root, ":")
		if old.Username != "root" {
			if i, _ := accountLine(data, old.Username); i >= 0 {
				lines = append(lines[:i], lines[i+1:]...)
				if i < idx {
					idx--
				}
			}
		}
		if next.Username == "root" {
			lines[idx] = strings.Join(active, ":")
		} else {
			if secret == 0 {
				active[4] = accountGecos
			}
			lines = append(lines, strings.Join(active, ":"))
		}
		return strings.Join(lines, "\n") + "\n", nil
	}
	newPasswd, err := update(string(p), 7, 0)
	if err != nil {
		return err
	}
	newShadow, err := update(string(s), 9, 1)
	if err != nil {
		return err
	}
	if err := replaceAccountFile(shadow, newShadow, 0600); err != nil {
		return err
	}
	if newPasswd != string(p) {
		if err := replaceAccountFile(passwd, newPasswd, 0644); err != nil {
			if rollback := replaceAccountFile(shadow, string(s), 0600); rollback != nil {
				return fmt.Errorf("更新账号失败，且未能恢复原密码文件")
			}
			return err
		}
	}
	return nil
}
