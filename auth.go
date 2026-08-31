package main

import (
	"crypto/subtle"
	"encoding/json"
	"html"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// authPassword is the shared secret required for NON-loopback (remote) access.
// Loopback clients — the MCP permission server, the `agent-monitor` CLI wrapper,
// the Claude/Codex SessionStart hooks, and register_self — always connect to
// 127.0.0.1 and are never challenged, so local functionality is unaffected even
// when a password is set. Empty string => auth disabled entirely (the classic
// localhost-only deployment).
var authPassword string

// authCookieName holds the credential the browser dashboard presents after a
// successful login (so the user types the password once per device).
const authCookieName = "am_auth"

// loadAuthPassword resolves the remote-access password, in priority order:
//  1. $AGENT_MONITOR_PASSWORD
//  2. ~/.agent-monitor/password  (first line, trimmed)
//
// Returns "" when neither is set, which disables authentication.
func loadAuthPassword() string {
	if v := strings.TrimSpace(os.Getenv("AGENT_MONITOR_PASSWORD")); v != "" {
		return v
	}
	b, err := os.ReadFile(filepath.Join(stateDir(), "password"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// isLoopbackRemote reports whether an http.Request.RemoteAddr ("ip:port") came
// in over the loopback interface.
func isLoopbackRemote(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// tokenMatches compares a presented credential against the configured password
// in constant time.
func tokenMatches(got string) bool {
	if authPassword == "" || got == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(authPassword)) == 1
}

// presentedToken pulls a candidate credential from, in order: the
// `Authorization: Bearer <pw>` header (the iOS app + any API client), a `token`
// query param (a fallback for clients that can't set headers), or the auth
// cookie (the browser dashboard after login).
func presentedToken(r *http.Request) string {
	if h := r.Header.Get("Authorization"); len(h) > 7 && strings.EqualFold(h[:7], "Bearer ") {
		return strings.TrimSpace(h[7:])
	}
	if t := r.URL.Query().Get("token"); t != "" {
		return t
	}
	if c, err := r.Cookie(authCookieName); err == nil {
		return c.Value
	}
	return ""
}

// wantsHTML is true for browser navigations (so we can serve a login page
// instead of a JSON 401).
func wantsHTML(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept"), "text/html")
}

// authMiddleware gates every non-loopback request behind the password. Loopback
// requests and the /api/login endpoint always pass; the latter validates the
// password itself.
func authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if authPassword == "" || isLoopbackRemote(r.RemoteAddr) {
			next.ServeHTTP(w, r)
			return
		}
		if r.URL.Path == "/api/login" {
			handleLogin(w, r)
			return
		}
		if tokenMatches(presentedToken(r)) {
			next.ServeHTTP(w, r)
			return
		}
		if wantsHTML(r) && r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(loginPageHTML("")))
			return
		}
		w.Header().Set("WWW-Authenticate", `Bearer realm="agent-monitor"`)
		writeJSONStatus(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
	})
}

// handleLogin validates a password (JSON body or HTML form) and, on success,
// sets a long-lived HttpOnly cookie so the browser stays authenticated.
func handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var pw string
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		var body struct {
			Password string `json:"password"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		pw = body.Password
	} else {
		_ = r.ParseForm()
		pw = r.FormValue("password")
	}
	if !tokenMatches(strings.TrimSpace(pw)) {
		if wantsHTML(r) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(loginPageHTML("Incorrect password")))
			return
		}
		writeJSONStatus(w, http.StatusUnauthorized, map[string]any{"error": "incorrect password"})
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     authCookieName,
		Value:    authPassword,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   60 * 60 * 24 * 30, // 30 days
	})
	if wantsHTML(r) {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// initAuth loads the password once at startup and logs the resulting posture.
// remoteExposed indicates whether a non-loopback listener is active.
func initAuth(remoteExposed bool) {
	authPassword = loadAuthPassword()
	switch {
	case authPassword != "":
		log.Printf("🔒 remote access is password-protected (loopback clients exempt)")
	case remoteExposed:
		log.Printf("⚠ NO password set — remote access is UNAUTHENTICATED. Set AGENT_MONITOR_PASSWORD or write ~/.agent-monitor/password")
	}
}

func loginPageHTML(errMsg string) string {
	banner := ""
	if errMsg != "" {
		banner = `<p class="err">` + html.EscapeString(errMsg) + `</p>`
	}
	return `<!doctype html><html><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Agent Monitor — Sign in</title>
<style>
  :root{color-scheme:dark light}
  *{box-sizing:border-box}
  body{margin:0;min-height:100vh;display:flex;align-items:center;justify-content:center;
       font:16px/1.5 -apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif;
       background:#0b0e14;color:#e6e6e6}
  .card{width:min(360px,92vw);padding:28px;border-radius:14px;background:#151a24;
        box-shadow:0 10px 40px rgba(0,0,0,.4)}
  h1{margin:0 0 4px;font-size:19px}
  p.sub{margin:0 0 20px;color:#8a93a6;font-size:13px}
  label{display:block;font-size:12px;color:#8a93a6;margin-bottom:6px}
  input{width:100%;padding:11px 12px;border-radius:9px;border:1px solid #2a3140;
        background:#0e131c;color:#fff;font-size:15px}
  button{width:100%;margin-top:16px;padding:11px;border:0;border-radius:9px;
         background:#3b82f6;color:#fff;font-size:15px;font-weight:600;cursor:pointer}
  button:hover{background:#2f6fe0}
  .err{margin:0 0 14px;padding:9px 11px;border-radius:8px;background:#3b1d1d;
       color:#ff9d9d;font-size:13px}
</style></head>
<body><form class="card" method="post" action="/api/login">
  <h1>Agent Monitor</h1>
  <p class="sub">This dashboard is password-protected.</p>
  ` + banner + `
  <label for="pw">Password</label>
  <input id="pw" name="password" type="password" autocomplete="current-password" autofocus>
  <button type="submit">Sign in</button>
</form></body></html>`
}
