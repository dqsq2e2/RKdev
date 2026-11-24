package main

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testAccountHash = "$6$testsalt$abcdefghijk"

func testManagement(t *testing.T) *managementController {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "passwd"), []byte("root:x:0:0:root:/root:/bin/sh\nnobody:x:65534:65534:nobody:/:/sbin/nologin\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "shadow"), []byte("root:"+testAccountHash+":1:0:99999:7:::\nnobody:!:1:0:99999:7:::\n"), 0600); err != nil {
		t.Fatal(err)
	}
	s := &managementController{accountDir: dir}
	if err := s.startAccount(nil); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestRecoveryAccountRenameAndReload(t *testing.T) {
	s := testManagement(t)
	change := managementEdit{Account: &accountEdit{Username: "operator"}}
	if err := s.commit(change); err != nil {
		t.Fatal(err)
	}
	shadow, _ := os.ReadFile(filepath.Join(s.accountDir, "shadow"))
	_, root := accountLine(string(shadow), "root")
	if len(root) < 2 || root[1] != "!" {
		t.Fatal("old username still allows password login")
	}
	got, err := readRecoveryAccount(s.accountDir)
	if err != nil || got != (recoveryAccount{"operator", testAccountHash}) {
		t.Fatal("active account could not be read back")
	}
	var resumed = managementController{accountDir: s.accountDir}
	if err := resumed.startAccount(nil); err != nil || resumed.account != got {
		t.Fatal("service restart lost the active alias")
	}
	fresh := testManagement(t)
	if err := fresh.startAccount(&got); err != nil || fresh.account != got {
		t.Fatal("saved account could not be restored on a fresh rootfs")
	}
	for _, name := range []string{"nobody", "root"} {
		err := s.commit(managementEdit{Account: &accountEdit{Username: name}})
		if (err != nil) != (name != "root") {
			t.Fatalf("rename to %s: %v", name, err)
		}
	}
	passwd, _ := os.ReadFile(filepath.Join(s.accountDir, "passwd"))
	if _, alias := accountLine(string(passwd), "operator"); alias != nil {
		t.Fatal("former alias still exists")
	}
}

func TestManagementRollsBackFailedStoreWrite(t *testing.T) {
	s := testManagement(t)
	s.store = &recoverySettingsStore{} // Layout has no validated write target.
	if err := s.commit(managementEdit{Account: &accountEdit{Username: "operator"}}); err == nil {
		t.Fatal("accepted a save with no settings medium")
	}
	got, err := readRecoveryAccount(s.accountDir)
	if err != nil || got != (recoveryAccount{"root", testAccountHash}) {
		t.Fatal("failed save changed live credentials")
	}
}

func TestManagementTransportAndSecrets(t *testing.T) {
	settingsJS, err := resFS.ReadFile("res/settings.js")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(settingsJS), "current_password") || strings.Contains(string(settingsJS), "当前密码") {
		t.Fatal("web management still asks for the current password")
	}
	s := testManagement(t)
	token, err := s.sessions.issue("root")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.sessions.revokeAll)
	session := s.sessions.lookup(token)
	handler := recoveryWeb(s, http.HandlerFunc(s.handle))
	for _, tc := range []struct {
		method, scheme, origin, body string
		local, authenticated         bool
		want                         int
	}{
		{"GET", "https", "", "", false, true, 200},
		{"POST", "http", "http://192.168.88.1", `{"account":{"username":"operator"}}`, false, true, 403},
		{"POST", "https", "http://192.168.88.1", `{"account":{"username":"operator"}}`, false, true, 403},
		{"POST", "https", "https://192.168.88.1", `{"account":{"username":"operator"}}`, false, false, 401},
		{"POST", "https", "https://192.168.88.1", `{"account":{"username":"operator"}}`, false, true, 200},
		{"GET", "https", "", "", false, true, 401},
		{"POST", "http", "", `{"account":{"username":"operator"}}`, true, false, 200},
	} {
		var addr net.Addr = &net.TCPAddr{IP: net.ParseIP("192.168.88.1"), Port: 443}
		if tc.local {
			addr = &net.UnixAddr{Name: setupSocket, Net: "unix"}
		}
		ctx := context.WithValue(context.Background(), http.LocalAddrContextKey, addr)
		r := httptest.NewRequestWithContext(ctx, tc.method, tc.scheme+"://192.168.88.1/api/management", strings.NewReader(tc.body))
		r.RemoteAddr = "192.168.88.100:1234"
		r.Header.Set("Origin", tc.origin)
		if tc.authenticated {
			r.AddCookie(&http.Cookie{Name: webSessionCookie, Value: token})
		}
		w := httptest.NewRecorder()
		if tc.local {
			s.handle(w, r)
		} else {
			handler.ServeHTTP(w, r)
		}
		if w.Code != tc.want {
			t.Errorf("%s via %s: got %d, want %d", tc.method, tc.scheme, w.Code, tc.want)
		}
		if strings.Contains(w.Body.String(), testAccountHash) {
			t.Error("web response exposed a password hash")
		}
		if tc.method == http.MethodPost && tc.scheme == "https" && tc.want == 200 && !tc.local {
			if !strings.Contains(w.Body.String(), `"login_required":true`) {
				t.Error("account update did not require a fresh login")
			}
		}
	}
	if session.ctx.Err() == nil {
		t.Fatal("the HDMI account change did not close old sessions")
	}
}
