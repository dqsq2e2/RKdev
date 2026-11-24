package main

import (
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/base64"
	"fmt"
	"html/template"
	"mime"
	"net/http"
	"sync"
	"time"
)

//go:embed login.html
var loginHTML string

var loginPage = template.Must(template.New("login").Parse(loginHTML))

const (
	webSessionCookie = "__Host-rkdev_session"
	webSessionLife   = 12 * time.Hour
)

type webSession struct {
	username string
	ctx      context.Context
	cancel   context.CancelFunc
}

type webSessions struct {
	mu     sync.Mutex
	active map[string]*webSession
}

func (s *webSessions) issue(username string) (string, error) {
	var secret [32]byte
	if _, err := rand.Read(secret[:]); err != nil {
		return "", err
	}
	token := base64.RawURLEncoding.EncodeToString(secret[:])
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, session := range s.active {
		if session.ctx.Err() != nil {
			delete(s.active, key)
		}
	}
	if len(s.active) >= 64 {
		return "", fmt.Errorf("too many recovery sessions")
	}
	if s.active == nil {
		s.active = make(map[string]*webSession)
	}
	ctx, cancel := context.WithTimeout(context.Background(), webSessionLife)
	s.active[token] = &webSession{username, ctx, cancel}
	return token, nil
}

func (s *webSessions) lookup(token string) *webSession {
	s.mu.Lock()
	defer s.mu.Unlock()
	session := s.active[token]
	if session == nil || session.ctx.Err() != nil {
		return nil
	}
	return session
}

func (s *webSessions) revoke(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if session := s.active[token]; session != nil {
		session.cancel()
		delete(s.active, token)
	}
}

func (s *webSessions) revokeAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, session := range s.active {
		session.cancel()
	}
	clear(s.active)
}

type webSessionKey struct{}

func requestWebSession(r *http.Request) *webSession {
	session, _ := r.Context().Value(webSessionKey{}).(*webSession)
	return session
}

func webCookie(w http.ResponseWriter, token string) {
	cookie := &http.Cookie{
		Name: webSessionCookie, Value: token, Path: "/",
		HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode,
	}
	if token == "" {
		cookie.MaxAge = -1
	}
	http.SetCookie(w, cookie)
}

func webPage(path, view string) string {
	switch view {
	case "boot", "storage", "update", "backup", "network", "management", "terminal":
		return path + "?view=" + view
	default:
		return path
	}
}

func (s *managementController) serveSession(w http.ResponseWriter, r *http.Request, next http.Handler) {
	view := r.URL.Query().Get("view")
	token := ""
	if cookie, err := r.Cookie(webSessionCookie); err == nil {
		token = cookie.Value
	}
	read := r.Method == http.MethodGet || r.Method == http.MethodHead
	switch r.URL.Path {
	case "/login":
		switch {
		case read && s.sessions.lookup(token) != nil:
			http.Redirect(w, r, webPage("/", view), http.StatusSeeOther)
		case read:
			message := ""
			if r.URL.Query().Get("changed") == "1" {
				message = "账号设置已更新，请重新登录。"
			}
			renderLogin(w, webPage("/login", view), http.StatusOK, message)
		case r.Method == http.MethodPost:
			s.loginWeb(w, r, token, view)
		default:
			w.Header().Set("Allow", "GET, HEAD, POST")
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
		return
	case "/logout":
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		s.sessions.revoke(token)
		webCookie(w, "")
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	case "/res/logo.png", "/res/boot-mark.png":
		if read {
			next.ServeHTTP(w, r)
			return
		}
	}
	session := s.sessions.lookup(token)
	if session == nil {
		if read && r.URL.Path == "/" {
			http.Redirect(w, r, webPage("/login", view), http.StatusSeeOther)
			return
		}
		w.Header().Set("X-RKdev-Login", "required")
		jsonErr(w, http.StatusUnauthorized, "请先登录恢复系统")
		return
	}
	ctx := context.WithValue(r.Context(), webSessionKey{}, session)
	if read {
		// Close reading streams on revocation. Let any accepted disk write
		// continue with its original request lifetime.
		stream, cancel := context.WithCancel(ctx)
		stop := context.AfterFunc(session.ctx, cancel)
		defer cancel()
		defer stop()
		ctx = stream
	}
	next.ServeHTTP(w, r.WithContext(ctx))
}

func renderLogin(w http.ResponseWriter, action string, code int, message string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; img-src 'self'; style-src 'unsafe-inline'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'")
	w.WriteHeader(code)
	_ = loginPage.Execute(w, map[string]any{"Action": action, "Message": message, "Error": code >= 400})
}

var loginChecks = make(chan struct{}, 4)

func (s *managementController) loginWeb(w http.ResponseWriter, r *http.Request, oldToken, view string) {
	action := webPage("/login", view)
	content, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	if content != "application/x-www-form-urlencoded" || r.ParseForm() != nil {
		renderLogin(w, action, http.StatusBadRequest, "登录请求无效。")
		return
	}
	select {
	case loginChecks <- struct{}{}:
		defer func() { <-loginChecks }()
	default:
		w.Header().Set("Retry-After", "1")
		renderLogin(w, action, http.StatusTooManyRequests, "登录请求过多，请稍后再试。")
		return
	}
	s.mu.Lock()
	account := s.account
	s.mu.Unlock()
	username, password := r.PostForm.Get("username"), r.PostForm.Get("password")
	if account.authenticates(password) && account.Username == username {
		s.mu.Lock()
		if s.account == account {
			s.sessions.revoke(oldToken)
			token, err := s.sessions.issue(username)
			s.mu.Unlock()
			if err != nil {
				renderLogin(w, action, http.StatusServiceUnavailable, "暂时无法登录，请稍后再试。")
				return
			}
			webCookie(w, token)
			http.Redirect(w, r, webPage("/", view), http.StatusSeeOther)
			return
		}
		s.mu.Unlock()
	}
	time.Sleep(600 * time.Millisecond)
	renderLogin(w, action, http.StatusUnauthorized, "恢复账号或密码不正确。")
}
