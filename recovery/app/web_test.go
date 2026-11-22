package main

import (
	"html/template"
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

func TestWebAuth(t *testing.T) {
	const token = "0123456789abcdef01234567"
	handler := webAuth(token, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	for _, test := range []struct {
		name, path, origin, password, method string
		code                                 int
	}{
		{"missing password", "/", "", "", "GET", 401},
		{"authenticated reader", "/", "", token, "GET", 204},
		{"same origin", "/upload", "https://rkdev.local", token, "POST", 204},
		{"cross origin writer", "/upload", "https://foreign.example", token, "POST", 403},
		{"cross origin terminal", "/api/terminal", "https://foreign.example", token, "GET", 403},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := httptest.NewRequest(test.method, "https://rkdev.local"+test.path, nil)
			if test.password != "" {
				r.SetBasicAuth("rkdev", test.password)
			}
			r.Header.Set("Origin", test.origin)
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			if w.Code != test.code {
				t.Fatalf("status %d, want %d", w.Code, test.code)
			}
		})
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
		for _, marker := range []string{"Example Board", "vtest", "flash-panel", "boot-panel", "storage-panel", "update-panel", "backup-panel", "firewall-panel", "terminal-panel", "recovery-manifest", "/api/update_check", "/api/update_download", "/api/update_cancel"} {
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
