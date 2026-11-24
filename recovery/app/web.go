package main

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strings"
	"time"
)

func decodeSettingsRequest(w http.ResponseWriter, r *http.Request, dst any, limit int64) error {
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, limit))
	d.DisallowUnknownFields()
	if err := d.Decode(dst); err != nil {
		return err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("JSON 请求包含额外数据")
	}
	return nil
}

func setupLocal(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !localSetup(r) {
			jsonErr(w, http.StatusForbidden, "仅供本机设置使用")
			return
		}
		next(w, r)
	}
}

const setupSocket = "/run/rkdev/setup.sock"

func localSetup(r *http.Request) bool {
	addr, ok := r.Context().Value(http.LocalAddrContextKey).(*net.UnixAddr)
	return ok && addr.Name == setupSocket
}

func recoveryWeb(s *managementController, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		peer, err := netip.ParseAddrPort(r.RemoteAddr)
		if err != nil || !localRecoveryAddr(peer.Addr().Unmap()) {
			http.Error(w, "恢复网页仅供本地网络访问", http.StatusForbidden)
			return
		}
		if !recoveryHost(r) {
			http.Error(w, "请使用本机地址访问恢复系统", http.StatusForbidden)
			return
		}
		read := r.Method == http.MethodGet || r.Method == http.MethodHead
		if strings.HasPrefix(r.URL.Path, "/api/") || !read {
			if fetch := r.Header.Get("Sec-Fetch-Site"); fetch != "" && fetch != "same-origin" && fetch != "none" {
				http.Error(w, "拒绝跨站请求", http.StatusForbidden)
				return
			}
			origin := r.Header.Get("Origin")
			if origin != "" {
				u, err := url.Parse(origin)
				scheme := "http"
				if r.TLS != nil {
					scheme = "https"
				}
				if err != nil || u.Scheme != scheme || !strings.EqualFold(u.Host, r.Host) || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
					http.Error(w, "拒绝跨站请求", http.StatusForbidden)
					return
				}
			}
		}
		if r.TLS == nil {
			if read && (r.URL.Path == "/" || r.URL.Path == "/login") {
				target, _ := url.Parse(webPage(r.URL.Path, r.URL.Query().Get("view")))
				target.Scheme, target.Host = "https", r.Host
				if host, _, err := net.SplitHostPort(r.Host); err == nil {
					target.Host = host
					if strings.Contains(host, ":") {
						target.Host = "[" + host + "]"
					}
				}
				http.Redirect(w, r, target.String(), http.StatusSeeOther)
				return
			}
			jsonErr(w, http.StatusForbidden, "请通过 HTTPS 访问恢复系统")
			return
		}
		s.serveSession(w, r, next)
	})
}

func localRecoveryAddr(ip netip.Addr) bool {
	return ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast()
}

func recoveryHost(r *http.Request) bool {
	host := r.Host
	if addr, _, err := net.SplitHostPort(host); err == nil {
		host = addr
	}
	if ip, err := netip.ParseAddr(strings.Trim(host, "[]")); err == nil {
		addr, ok := r.Context().Value(http.LocalAddrContextKey).(*net.TCPAddr)
		return ok && ip.Unmap().WithZone("") == addr.AddrPort().Addr().Unmap().WithZone("")
	}
	name, _ := os.Hostname()
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	name = strings.ToLower(name)
	if host == "localhost" {
		peer, err := netip.ParseAddrPort(r.RemoteAddr)
		return err == nil && peer.Addr().Unmap().IsLoopback()
	}
	return name != "" && (host == name || host == name+".local")
}

func serveWeb() error {
	app := logRequests(http.DefaultServeMux)
	setup, err := listenSetup()
	if err != nil {
		return fmt.Errorf("HDMI setup service: %w", err)
	}
	defer setup.Close()
	go func() {
		server := &http.Server{Handler: app, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: time.Minute}
		log.Printf("[HDMI setup] %v", server.Serve(setup))
	}()
	handler := recoveryWeb(&management, app)
	web := &http.Server{
		Addr: ":80", Handler: handler,
		ReadHeaderTimeout: 10 * time.Second, IdleTimeout: time.Minute,
	}
	go func() {
		cert, err := generateSelfSignedCert()
		if err != nil {
			log.Printf("[HTTPS] %v", err)
			return
		}
		secure := &http.Server{
			Addr: ":443", Handler: handler, ReadHeaderTimeout: web.ReadHeaderTimeout, IdleTimeout: web.IdleTimeout,
			TLSConfig: &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12},
			// The Web terminal upgrades an HTTP/1.1 connection to WebSocket.
			Protocols: new(http.Protocols),
		}
		secure.Protocols.SetHTTP1(true)
		log.Printf("[HTTPS] %v", secure.ListenAndServeTLS("", ""))
	}()
	log.Printf("RKdev %s: HTTP :80", buildTime)
	return web.ListenAndServe()
}

func listenSetup() (net.Listener, error) {
	// Both processes run as root. A private socket keeps the physical HDMI
	// console independent of browser sessions without trusting TCP loopback.
	const dir = "/run/rkdev"
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	if err := os.Chmod(dir, 0700); err != nil {
		return nil, err
	}
	if err := os.Remove(setupSocket); err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	return net.Listen("unix", setupSocket)
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
		"device": currentLayout.Device,
	})
}
