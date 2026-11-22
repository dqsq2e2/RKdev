package main

import (
	"crypto/tls"
	"log"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strings"
	"time"
)

func recoveryWeb(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		// Recovery intentionally serves the local network without a web login.
		// Keep the shell and disk APIs off globally routable connections.
		peer, err := netip.ParseAddrPort(r.RemoteAddr)
		if err != nil || !localRecoveryAddr(peer.Addr().Unmap()) {
			http.Error(w, "恢复网页仅供本地网络访问", http.StatusForbidden)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/") || (r.Method != http.MethodGet && r.Method != http.MethodHead) {
			if !recoveryHost(r) {
				http.Error(w, "请使用本机地址访问恢复系统", http.StatusForbidden)
				return
			}
			if fetch := r.Header.Get("Sec-Fetch-Site"); fetch != "" && fetch != "same-origin" && fetch != "none" {
				http.Error(w, "拒绝跨站请求", http.StatusForbidden)
				return
			}
			origin := r.Header.Get("Origin")
			if origin != "" {
				u, err := url.Parse(origin)
				if err != nil || u.Scheme != "http" || !strings.EqualFold(u.Host, r.Host) || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
					http.Error(w, "拒绝跨站请求", http.StatusForbidden)
					return
				}
			}
		}
		next.ServeHTTP(w, r)
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

func redirectToHTTP(w http.ResponseWriter, r *http.Request) {
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
		if strings.Contains(host, ":") {
			host = "[" + host + "]"
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, (&url.URL{Scheme: "http", Host: host, Path: "/"}).String(), http.StatusMovedPermanently)
}

func serveWeb() error {
	web := &http.Server{
		Addr: ":80", Handler: recoveryWeb(logRequests(http.DefaultServeMux)),
		ReadHeaderTimeout: 10 * time.Second, IdleTimeout: time.Minute,
	}
	go func() {
		cert, err := generateSelfSignedCert()
		if err != nil {
			log.Printf("[HTTPS redirect] %v", err)
			return
		}
		redirect := &http.Server{
			Addr: ":443", Handler: http.HandlerFunc(redirectToHTTP), ReadHeaderTimeout: 5 * time.Second,
			TLSConfig: &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12},
		}
		log.Printf("[HTTPS redirect] %v", redirect.ListenAndServeTLS("", ""))
	}()
	log.Printf("RKdev %s: HTTP :80", buildTime)
	return web.ListenAndServe()
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
