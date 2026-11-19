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
	"sync"
)

// authPassword is the secret for non-loopback clients; loopback is never challenged. Empty disables auth.
var authPassword string

// authCookieName holds a random session id the browser presents after a
// successful login (so the user types the password once per device).
const authCookieName = "am_auth"

// loadAuthPassword reads $AGENT_MONITOR_PASSWORD, else the first line of ~/.agent-monitor/password, else "".
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

// wantsHTML is true for browser navigations (so we can serve a login page
// instead of a JSON 401).
func wantsHTML(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept"), "text/html")
}

// authMiddleware gates non-loopback requests behind a device token or the password
// (the password stops working remotely when requireDeviceToken is on).
func authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if localCaller(r) || authPublicPath(r) {
			next.ServeHTTP(w, r)
			return
		}
		if d := requestDevice(r); d != nil {
			deviceStore.Touch(d.ID, remoteIP(r))
			noteDeviceAgent(d.ID, d.Name, r.UserAgent())
			liveConns.track(d.ID, w, r, next)
			return
		}
		require := requireDeviceToken()
		if authPassword == "" && !require {
			next.ServeHTTP(w, r)
			return
		}
		if r.URL.Path == "/api/login" {
			if require {
				writeJSONStatus(w, http.StatusForbidden, map[string]any{"error": "this server requires a paired device token; the password does not work remotely"})
				return
			}
			handleLogin(w, r)
			return
		}
		if !require {
			// A Bearer guess counts against the same limit as the login form.
			ip, bearer := loginClientIP(r), strings.HasPrefix(strings.ToLower(r.Header.Get("Authorization")), "bearer ")
			if bearer && loginBlocked(ip) {
				writeJSONStatus(w, http.StatusTooManyRequests, map[string]any{"error": "too many failed attempts; try again later"})
				return
			}
			if passwordAuthed(r) {
				next.ServeHTTP(w, r)
				return
			}
			if bearer {
				loginFailed(ip)
			}
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
	ip := loginClientIP(r)
	if loginBlocked(ip) {
		writeJSONStatus(w, http.StatusTooManyRequests, map[string]any{"error": "too many failed sign-ins; try again in 10 minutes"})
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
		loginFailed(ip)
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
		Value:    newAuthSession(),
		Path:     "/",
		HttpOnly: true,
		Secure:   secureRequest(r),
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

// noteDeviceAgent logs a paired device's User-Agent when it changes; the iPhone app's carries its build number.
var deviceAgents sync.Map

func noteDeviceAgent(id, name, ua string) {
	if ua == "" {
		return
	}
	if prev, ok := deviceAgents.Load(id); ok && prev.(string) == ua {
		return
	}
	deviceAgents.Store(id, ua)
	log.Printf("device %q user agent: %s", name, clipString(ua, 120))
}
