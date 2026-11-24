package main

import (
	"context"
	"html/template"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWholeDiskNames(t *testing.T) {
	for name, want := range map[string]bool{
		"mmcblk0": true, "mmcblk12": true, "nvme0n1": true, "sda": true,
		"mmcblk0boot0": false, "mmcblk0boot1": false, "mmcblk0rpmb": false,
		"mmcblk0p1": false, "nvme0n1p1": false, "sda1": false, "mtdblock0": false,
	} {
		if got := wholeDiskName.MatchString(name); got != want {
			t.Errorf("disk %q = %v, want %v", name, got, want)
		}
	}
}

func TestRecoveryWeb(t *testing.T) {
	s := testManagement(t)
	t.Cleanup(s.sessions.revokeAll)
	token, err := s.sessions.issue("root")
	if err != nil {
		t.Fatal(err)
	}
	handler := recoveryWeb(s, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	for _, test := range []struct {
		name, path, origin, method, peer, host, scheme string
		cookie                                         bool
		code                                           int
		location                                       string
	}{
		{"HTTP entry redirects", "/?view=management", "", "GET", "192.168.88.100:1234", "192.168.88.1:80", "http", false, 303, "https://192.168.88.1/?view=management"},
		{"HTTP cannot accept credentials", "/login", "", "POST", "192.168.88.100:1234", "192.168.88.1", "http", false, 403, ""},
		{"HTTPS entry requires login", "/?view=terminal", "", "GET", "192.168.88.100:1234", "192.168.88.1", "https", false, 303, "/login?view=terminal"},
		{"public login", "/login", "", "GET", "192.168.88.100:1234", "192.168.88.1", "https", false, 200, ""},
		{"public logo", "/res/logo.png", "", "GET", "192.168.88.100:1234", "192.168.88.1", "https", false, 204, ""},
		{"anonymous upload", "/upload", "", "POST", "192.168.88.100:1234", "192.168.88.1", "https", false, 401, ""},
		{"anonymous disk backup", "/api/backup/stream", "", "GET", "192.168.88.100:1234", "192.168.88.1", "https", false, 401, ""},
		{"anonymous log stream", "/api/logs", "", "GET", "192.168.88.100:1234", "192.168.88.1", "https", false, 401, ""},
		{"anonymous terminal", "/api/terminal", "", "GET", "192.168.88.100:1234", "192.168.88.1", "https", false, 401, ""},
		{"TCP loopback requires login", "/api/management", "", "GET", "127.0.0.1:1234", "localhost", "https", false, 401, ""},
		{"authorized upload", "/upload", "https://192.168.88.1", "POST", "192.168.88.100:1234", "192.168.88.1", "https", true, 204, ""},
		{"cross origin writer", "/upload", "https://foreign.example", "POST", "192.168.88.100:1234", "192.168.88.1", "https", true, 403, ""},
		{"cross origin terminal", "/api/terminal", "https://foreign.example", "GET", "192.168.88.100:1234", "192.168.88.1", "https", true, 403, ""},
		{"cross origin logout", "/logout", "https://foreign.example", "POST", "192.168.88.100:1234", "192.168.88.1", "https", true, 403, ""},
		{"foreign host", "/api/terminal", "https://foreign.example", "GET", "192.168.88.100:1234", "foreign.example", "https", true, 403, ""},
		{"remote peer", "/api/terminal", "", "GET", "198.51.100.123:1234", "192.168.88.1", "https", true, 403, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.WithValue(context.Background(), http.LocalAddrContextKey, &net.TCPAddr{IP: net.ParseIP("192.168.88.1"), Port: 80})
			r := httptest.NewRequestWithContext(ctx, test.method, test.scheme+"://"+test.host+test.path, nil)
			r.RemoteAddr = test.peer
			r.Header.Set("Origin", test.origin)
			if test.cookie {
				r.AddCookie(&http.Cookie{Name: webSessionCookie, Value: token})
			}
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			if got := w.Header().Get("Location"); w.Code != test.code || got != test.location {
				t.Fatalf("got %d %q, want %d %q", w.Code, got, test.code, test.location)
			}
		})
	}
}

func TestWebLogoutRevokesSession(t *testing.T) {
	s := testManagement(t)
	t.Cleanup(s.sessions.revokeAll)
	token, err := s.sessions.issue("root")
	if err != nil {
		t.Fatal(err)
	}
	session := s.sessions.lookup(token)
	handler := recoveryWeb(s, http.NotFoundHandler())
	r := httptest.NewRequest("POST", "https://localhost/logout", nil)
	r.RemoteAddr = "127.0.0.1:1234"
	r.AddCookie(&http.Cookie{Name: webSessionCookie, Value: token})
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	cookies := w.Result().Cookies()
	if w.Code != http.StatusSeeOther || len(cookies) != 1 || cookies[0].MaxAge != -1 ||
		!cookies[0].Secure || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteStrictMode {
		t.Fatal("logout did not expire the HTTPS cookie")
	}
	if s.sessions.lookup(token) != nil || session.ctx.Err() == nil {
		t.Fatal("logout did not revoke the token and existing connections")
	}
}

func TestWebTemplate(t *testing.T) {
	tmpl, err := template.New("index").Parse(indexHTML)
	if err != nil {
		t.Fatal(err)
	}
	for _, profile := range []string{"emmc", "spi"} {
		var output strings.Builder
		err := tmpl.Execute(&output, map[string]string{"BuildTime": "test", "BoardName": "Example Board", "Profile": profile})
		if err != nil {
			t.Fatal(err)
		}
		html := output.String()
		for _, marker := range []string{"Example Board", "vtest", "flash-panel", "boot-panel", "storage-panel", "update-panel", "backup-panel", "firewall-panel", "terminal-panel", "management-panel", ".boot-type-sata .boot-type-badge", "recovery-manifest", "/api/update_check", "/api/update_download", "/api/update_cancel"} {
			if !strings.Contains(html, marker) {
				t.Errorf("%s missing %s", profile, marker)
			}
		}
		if strings.Contains(html, "PHX-R3588") || strings.Contains(html, "iflytek_rk3588_aiui") ||
			strings.Contains(html, "eMMC 系统空间</strong>") != (profile == "emmc") {
			t.Errorf("incorrect %s profile rendering", profile)
		}
	}
}
