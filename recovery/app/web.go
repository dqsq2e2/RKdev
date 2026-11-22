package main

import (
	"crypto/subtle"
	"crypto/tls"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

var loginTokenRE = regexp.MustCompile(`^[a-f0-9]{24}$`)

func webAuth(token string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		user, password, ok := r.BasicAuth()
		if !ok || user != "rkdev" || subtle.ConstantTimeCompare([]byte(password), []byte(token)) != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="RKdev", charset="UTF-8"`)
			http.Error(w, "需要串口显示的 RKdev 登录口令", http.StatusUnauthorized)
			return
		}
		if r.Method != http.MethodGet || r.URL.Path == "/api/terminal" {
			if fetch := r.Header.Get("Sec-Fetch-Site"); fetch != "" && fetch != "same-origin" && fetch != "none" {
				http.Error(w, "拒绝跨站请求", http.StatusForbidden)
				return
			}
			origin := r.Header.Get("Origin")
			if origin != "" {
				u, err := url.Parse(origin)
				if err != nil || u.Scheme != "https" || !strings.EqualFold(u.Host, r.Host) || u.User != nil || u.Path != "" {
					http.Error(w, "拒绝跨站请求", http.StatusForbidden)
					return
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}

func serveWeb() error {
	secret, err := os.ReadFile("/run/rkdev-token")
	token := strings.TrimSpace(string(secret))
	if err != nil || !loginTokenRE.MatchString(token) {
		return fmt.Errorf("没有有效的本次开机登录口令")
	}
	cert, err := generateSelfSignedCert()
	if err != nil {
		return err
	}
	secure := &http.Server{
		Addr: ":443", Handler: webAuth(token, logRequests(http.DefaultServeMux)),
		ReadHeaderTimeout: 10 * time.Second, IdleTimeout: time.Minute,
		TLSConfig: &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12},
	}
	// HTTP only directs a browser to HTTPS. It never handles credentials or API requests.
	redirect := &http.Server{Addr: ":80", ReadHeaderTimeout: 5 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/" {
			http.Error(w, "请使用 HTTPS 访问恢复系统", http.StatusBadRequest)
			return
		}
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
			if strings.Contains(host, ":") {
				host = "[" + host + "]"
			}
		}
		http.Redirect(w, r, (&url.URL{Scheme: "https", Host: host}).String(), http.StatusTemporaryRedirect)
	})}
	go func() { log.Printf("[HTTP] %v", redirect.ListenAndServe()) }()
	log.Printf("RKdev %s: HTTPS :443", buildTime)
	return secure.ListenAndServeTLS("", "")
}

func handleProfile(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		jsonErr(w, http.StatusMethodNotAllowed, "仅支持 GET")
		return
	}
	var message string
	if err := currentLayout.checkMedium(false); err != nil {
		message = err.Error()
	}
	jsonOK(w, map[string]any{
		"board": currentLayout.Name, "profile": currentLayout.Profile, "writable": message == "",
		"online_update": message == "" && currentLayout.ReleaseURL != "", "message": message,
	})
}
