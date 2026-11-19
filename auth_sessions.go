package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Password logins get a random session id in am_auth; only its sha256 is kept on disk.
const authSessionTTL = 30 * 24 * time.Hour

type authSession struct {
	Exp   int64  `json:"exp"`
	PwTag string `json:"pw"`
}

var authSessions struct {
	mu   sync.Mutex
	path string
	m    map[string]authSession
}

func passwordTag() string {
	h := sha256.Sum256([]byte("am_auth\x00" + authPassword))
	return hex.EncodeToString(h[:8])
}

func sessionKey(id string) string {
	h := sha256.Sum256([]byte(id))
	return hex.EncodeToString(h[:])
}

// loadSessionsLocked reads the session file once per state folder.
func loadSessionsLocked() {
	p := filepath.Join(stateDir(), "auth_sessions.json")
	if authSessions.path == p && authSessions.m != nil {
		return
	}
	authSessions.path, authSessions.m = p, map[string]authSession{}
	if b, err := os.ReadFile(p); err == nil {
		_ = json.Unmarshal(b, &authSessions.m)
	}
}

func saveSessionsLocked() {
	now := time.Now().UnixMilli()
	for k, s := range authSessions.m {
		if s.Exp < now {
			delete(authSessions.m, k)
		}
	}
	b, _ := json.Marshal(authSessions.m)
	_ = os.MkdirAll(filepath.Dir(authSessions.path), 0o700)
	_ = os.WriteFile(authSessions.path, b, 0o600)
}

func newAuthSession() string {
	raw := make([]byte, 32)
	_, _ = rand.Read(raw)
	id := "ams_" + base64.RawURLEncoding.EncodeToString(raw)
	authSessions.mu.Lock()
	defer authSessions.mu.Unlock()
	loadSessionsLocked()
	authSessions.m[sessionKey(id)] = authSession{Exp: time.Now().Add(authSessionTTL).UnixMilli(), PwTag: passwordTag()}
	saveSessionsLocked()
	return id
}

// authSessionValid is true for a live session made with the current password.
func authSessionValid(id string) bool {
	if authPassword == "" || !strings.HasPrefix(id, "ams_") {
		return false
	}
	authSessions.mu.Lock()
	defer authSessions.mu.Unlock()
	loadSessionsLocked()
	s, ok := authSessions.m[sessionKey(id)]
	return ok && s.Exp > time.Now().UnixMilli() && s.PwTag == passwordTag()
}

// passwordAuthed accepts the password as a Bearer header (native apps) or a session cookie (browser).
func passwordAuthed(r *http.Request) bool {
	if h := r.Header.Get("Authorization"); len(h) > 7 && strings.EqualFold(h[:7], "Bearer ") {
		if tokenMatches(strings.TrimSpace(h[7:])) {
			return true
		}
	}
	if c, err := r.Cookie(authCookieName); err == nil {
		return authSessionValid(c.Value)
	}
	return false
}

const (
	loginMaxFailures = 10
	loginWindow      = 10 * time.Minute
)

var loginFails struct {
	mu sync.Mutex
	m  map[string][]time.Time
}

func resetLoginLimiter() {
	loginFails.mu.Lock()
	loginFails.m = nil
	loginFails.mu.Unlock()
}

// loginClientIP is the peer address, or the first X-Forwarded-For hop from a local proxy.
func loginClientIP(r *http.Request) string {
	if isLoopbackRemote(r.RemoteAddr) {
		if f := strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-For"), ",")[0]); f != "" {
			return f
		}
	}
	return remoteIP(r)
}

func recentLoginFails(ip string, now time.Time) []time.Time {
	var keep []time.Time
	for _, t := range loginFails.m[ip] {
		if now.Sub(t) < loginWindow {
			keep = append(keep, t)
		}
	}
	return keep
}

// loginBlocked reports whether ip used up its failed attempts in the window.
func loginBlocked(ip string) bool {
	loginFails.mu.Lock()
	defer loginFails.mu.Unlock()
	return len(recentLoginFails(ip, time.Now())) >= loginMaxFailures
}

func loginFailed(ip string) {
	now := time.Now()
	loginFails.mu.Lock()
	defer loginFails.mu.Unlock()
	if loginFails.m == nil || len(loginFails.m) > 10000 {
		loginFails.m = map[string][]time.Time{}
	}
	loginFails.m[ip] = append(recentLoginFails(ip, now), now)
}
