package main

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const remoteAddr = "100.101.102.103:51000"

type devEnv struct {
	t     *testing.T
	store *DeviceStore
	h     http.Handler
	clock time.Time
}

func newDevEnv(t *testing.T, password string) *devEnv {
	t.Helper()
	t.Setenv("AGENT_MONITOR_HOME", t.TempDir())
	s, err := openDeviceStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	e := &devEnv{t: t, store: s, clock: time.UnixMilli(1_800_000_000_000)}
	s.now = func() time.Time { return e.clock }
	oldStore, oldPw := deviceStore, authPassword
	settings.mu.Lock()
	oldSettings := settings.v
	settings.v.RequireDeviceToken = false
	settings.mu.Unlock()
	deviceStore, authPassword = s, password
	t.Cleanup(func() {
		deviceStore, authPassword = oldStore, oldPw
		settings.mu.Lock()
		settings.v = oldSettings
		settings.mu.Unlock()
		s.Close()
	})
	mux := http.NewServeMux()
	deviceRoutes(mux, s)
	mux.HandleFunc("GET /api/secret", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, map[string]any{"ok": true}) })
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("<html>shell</html>")) })
	e.h = originGuard(authMiddleware(mux))
	return e
}

func setRequire(on bool) {
	settings.mu.Lock()
	settings.v.RequireDeviceToken = on
	settings.mu.Unlock()
}

func (e *devEnv) do(method, path, body, addr string, mod func(r *http.Request)) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.RemoteAddr = addr
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	if mod != nil {
		mod(r)
	}
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, r)
	return w
}

func bearer(tok string) func(r *http.Request) {
	return func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+tok) }
}

func (e *devEnv) create(name string) (Device, string) {
	e.t.Helper()
	w := e.do("POST", "/api/devices", `{"name":"`+name+`"}`, "127.0.0.1:1", nil)
	if w.Code != http.StatusCreated {
		e.t.Fatalf("create %d %s", w.Code, w.Body)
	}
	var out struct {
		Device  Device `json:"device"`
		Token   string `json:"token"`
		PairURL string `json:"pairUrl"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	if !strings.HasPrefix(out.Token, "amd_") || len(out.Token) != 4+43 || !strings.Contains(out.PairURL, "/pair#") || strings.Contains(out.PairURL, out.Token) {
		e.t.Fatalf("create out %+v", out)
	}
	return out.Device, out.Token
}

func TestDevicesCreateListRevoke(t *testing.T) {
	e := newDevEnv(t, "pw")
	d1, tok1 := e.create("phone")
	d2, _ := e.create("tablet")
	w := e.do("GET", "/api/devices", "", remoteAddr, bearer(tok1))
	var list []Device
	_ = json.Unmarshal(w.Body.Bytes(), &list)
	if w.Code != 200 || len(list) != 2 || !list[0].Current || list[1].Current || list[0].ID != d1.ID {
		t.Fatalf("list %d %+v", w.Code, list)
	}
	if strings.Contains(w.Body.String(), "hash") || strings.Contains(w.Body.String(), tok1) {
		t.Fatal("list leaks token material")
	}
	if w := e.do("DELETE", "/api/devices/"+d2.ID, "", "127.0.0.1:1", nil); w.Code != 200 {
		t.Fatalf("revoke %d", w.Code)
	}
	if w := e.do("DELETE", "/api/devices/dv_nope", "", "127.0.0.1:1", nil); w.Code != 404 {
		t.Fatalf("revoke unknown %d", w.Code)
	}
	// The current device may revoke itself; its token stops working at once.
	if w := e.do("DELETE", "/api/devices/"+d1.ID, "", remoteAddr, bearer(tok1)); w.Code != 200 {
		t.Fatalf("self revoke %d %s", w.Code, w.Body)
	}
	if w := e.do("GET", "/api/secret", "", remoteAddr, bearer(tok1)); w.Code != 401 {
		t.Fatalf("revoked token %d", w.Code)
	}
	// Tokens survive a reopen of the store.
	_, tok3 := e.create("laptop")
	s2, err := openDeviceStore(filepathDir(e))
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if s2.Lookup(tok3) == nil || s2.Lookup(tok1) != nil {
		t.Fatal("reopened store lookup")
	}
}

func filepathDir(e *devEnv) string {
	var dir string
	_ = e.store.db.QueryRow(`SELECT file FROM pragma_database_list WHERE name='main'`).Scan(&dir)
	return dir[:strings.LastIndex(dir, "/")]
}

func TestDevicesAuth(t *testing.T) {
	e := newDevEnv(t, "pw")
	_, tok := e.create("phone")
	if w := e.do("GET", "/api/secret", "", remoteAddr, nil); w.Code != 401 {
		t.Fatalf("no credential %d", w.Code)
	}
	if w := e.do("GET", "/api/secret", "", remoteAddr, bearer(tok)); w.Code != 200 {
		t.Fatalf("bearer %d", w.Code)
	}
	if w := e.do("GET", "/api/secret?token="+tok, "", remoteAddr, nil); w.Code != 401 {
		t.Fatalf("device token in URL accepted: %d", w.Code)
	}
	if w := e.do("GET", "/api/secret", "", remoteAddr, bearer("amd_wrong")); w.Code != 401 {
		t.Fatalf("bad token %d", w.Code)
	}
	// Pairing sets the cookie; a wrong token is 401.
	if w := e.do("POST", "/api/devices/session", `{"token":"amd_bad"}`, remoteAddr, nil); w.Code != 401 {
		t.Fatalf("bad session %d", w.Code)
	}
	w := e.do("POST", "/api/devices/session", `{"token":"`+tok+`"}`, remoteAddr, nil)
	var cookie *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == deviceCookieName {
			cookie = c
		}
	}
	if w.Code != 200 || cookie == nil || !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode || cookie.Secure {
		t.Fatalf("session %d %+v", w.Code, cookie)
	}
	w = e.do("POST", "/api/devices/session", `{"token":"`+tok+`"}`, "127.0.0.1:9", func(r *http.Request) { r.Header.Set("X-Forwarded-Proto", "https") })
	if c := w.Result().Cookies(); len(c) != 1 || !c[0].Secure {
		t.Fatalf("https cookie %+v", c)
	}
	if w := e.do("GET", "/api/secret", "", remoteAddr, func(r *http.Request) { r.AddCookie(cookie) }); w.Code != 200 {
		t.Fatalf("cookie %d", w.Code)
	}
	// Password works while the requirement is off, and stops when it is on.
	pw := func(r *http.Request) { r.Header.Set("Authorization", "Bearer pw") }
	if w := e.do("GET", "/api/secret", "", remoteAddr, pw); w.Code != 200 {
		t.Fatalf("password %d", w.Code)
	}
	setRequire(true)
	if w := e.do("GET", "/api/secret", "", remoteAddr, pw); w.Code != 401 {
		t.Fatalf("password with require on %d", w.Code)
	}
	if w := e.do("POST", "/api/login", `{"password":"pw"}`, remoteAddr, nil); w.Code != 403 {
		t.Fatalf("login with require on %d", w.Code)
	}
	if w := e.do("GET", "/api/secret", "", remoteAddr, bearer(tok)); w.Code != 200 {
		t.Fatalf("device with require on %d", w.Code)
	}
	// Loopback stays exempt.
	if w := e.do("GET", "/api/secret", "", "127.0.0.1:4000", nil); w.Code != 200 {
		t.Fatalf("loopback %d", w.Code)
	}
	if w := e.do("GET", "/api/secret", "", "[::1]:4000", nil); w.Code != 200 {
		t.Fatalf("loopback v6 %d", w.Code)
	}
}

func TestDevicesNoPasswordRequire(t *testing.T) {
	e := newDevEnv(t, "")
	// Secure default: with no password and no device requirement, another machine is still refused.
	if w := e.do("GET", "/api/secret", "", remoteAddr, nil); w.Code != 401 {
		t.Fatalf("no password, no requirement: %d, want 401", w.Code)
	}
	_, tok := e.create("phone")
	setRequire(true)
	if w := e.do("GET", "/api/secret", "", remoteAddr, nil); w.Code != 401 {
		t.Fatalf("require without password %d", w.Code)
	}
	if w := e.do("GET", "/api/secret", "", remoteAddr, bearer(tok)); w.Code != 200 {
		t.Fatalf("device %d", w.Code)
	}
}

func TestDevicesPairingPathsArePublic(t *testing.T) {
	e := newDevEnv(t, "pw")
	setRequire(true)
	html := func(r *http.Request) { r.Header.Set("Accept", "text/html") }
	if w := e.do("GET", "/pair", "", remoteAddr, html); w.Code != 200 || !strings.Contains(w.Body.String(), "shell") {
		t.Fatalf("/pair %d %s", w.Code, w.Body)
	}
	if w := e.do("GET", "/assets/index-abc.js", "", remoteAddr, nil); w.Code != 200 {
		t.Fatalf("/assets %d", w.Code)
	}
	if w := e.do("POST", "/api/devices/session", `{"token":"amd_x"}`, remoteAddr, nil); w.Code != 401 {
		t.Fatalf("session reachable without auth should answer 401 for a bad token, got %d", w.Code)
	}
	for _, p := range []string{"/", "/w", "/api/devices", "/pairx", "/assets/../api/devices"} {
		if w := e.do("GET", p, "", remoteAddr, nil); w.Code != 401 {
			t.Fatalf("%s without credentials: %d", p, w.Code)
		}
	}
	if w := e.do("POST", "/api/devices", `{"name":"x"}`, remoteAddr, nil); w.Code != 401 {
		t.Fatalf("create without credentials %d", w.Code)
	}
}

func TestDevicesRequireGuard(t *testing.T) {
	e := newDevEnv(t, "pw")
	req := func(addr string, mod func(*http.Request)) *http.Request {
		r := httptest.NewRequest("POST", "/api/settings", nil)
		r.RemoteAddr = addr
		if mod != nil {
			mod(r)
		}
		return r
	}
	if err := checkRequireDeviceToken(req("127.0.0.1:1", nil), true); err == nil {
		t.Fatal("enabled with no device")
	}
	if err := checkRequireDeviceToken(req("127.0.0.1:1", nil), false); err != nil {
		t.Fatal("turning off must always work")
	}
	d1, tok1 := e.create("phone")
	if err := checkRequireDeviceToken(req("127.0.0.1:1", nil), true); err != nil {
		t.Fatalf("loopback with one device: %v", err)
	}
	pw := func(r *http.Request) { r.Header.Set("Authorization", "Bearer pw") }
	if err := checkRequireDeviceToken(req(remoteAddr, pw), true); err == nil {
		t.Fatal("remote password caller allowed to lock itself out")
	}
	if err := checkRequireDeviceToken(req(remoteAddr, bearer(tok1)), true); err == nil {
		t.Fatal("only device is the caller: no other device")
	}
	_, tok2 := e.create("tablet")
	if err := checkRequireDeviceToken(req(remoteAddr, bearer(tok1)), true); err != nil {
		t.Fatalf("caller plus another device: %v", err)
	}
	// With the requirement on, a remote caller cannot revoke the last active device.
	setRequire(true)
	if w := e.do("DELETE", "/api/devices/"+d1.ID, "", "127.0.0.1:1", nil); w.Code != 200 {
		t.Fatalf("revoke one of two %d", w.Code)
	}
	var list []Device
	_ = json.Unmarshal(e.do("GET", "/api/devices", "", remoteAddr, bearer(tok2)).Body.Bytes(), &list)
	last := ""
	for _, d := range list {
		if d.RevokedAt == 0 {
			last = d.ID
		}
	}
	if w := e.do("DELETE", "/api/devices/"+last, "", remoteAddr, bearer(tok2)); w.Code != 409 {
		t.Fatalf("revoke last remote %d", w.Code)
	}
	if w := e.do("DELETE", "/api/devices/"+last, "", "127.0.0.1:1", nil); w.Code != 200 {
		t.Fatalf("revoke last from loopback %d", w.Code)
	}
}

func TestDevicesLastUsedThrottle(t *testing.T) {
	e := newDevEnv(t, "pw")
	d, tok := e.create("phone")
	get := func() Device {
		for _, x := range e.store.List("") {
			if x.ID == d.ID {
				return x
			}
		}
		t.Fatal("device missing")
		return Device{}
	}
	e.do("GET", "/api/secret", "", remoteAddr, bearer(tok))
	first := get()
	if first.LastUsedAt != e.clock.UnixMilli() || first.LastIP != "100.101.102.103" {
		t.Fatalf("first touch %+v", first)
	}
	e.clock = e.clock.Add(30 * time.Second)
	e.do("GET", "/api/secret", "", "100.1.1.1:2", bearer(tok))
	if got := get(); got.LastUsedAt != first.LastUsedAt || got.LastIP != first.LastIP {
		t.Fatalf("touched inside a minute %+v", got)
	}
	e.clock = e.clock.Add(31 * time.Second)
	e.do("GET", "/api/secret", "", "100.1.1.1:2", bearer(tok))
	if got := get(); got.LastUsedAt != e.clock.UnixMilli() || got.LastIP != "100.1.1.1" {
		t.Fatalf("no touch after a minute %+v", got)
	}
	var stored int64
	_ = e.store.db.QueryRow(`SELECT last_used_at FROM devices WHERE id=?`, d.ID).Scan(&stored)
	if stored != e.clock.UnixMilli() {
		t.Fatalf("db last_used_at %d", stored)
	}
}

func TestDevicesPairBase(t *testing.T) {
	t.Setenv("AGENT_MONITOR_PAIR_BASE", "")
	t.Setenv("AGENT_MONITOR_ALLOWED_HOSTS", "")
	r := httptest.NewRequest("POST", "/api/devices", nil)
	r.Host = "mac.local:7777"
	ips := func() []net.IP { return []net.IP{net.ParseIP("192.168.1.5"), net.ParseIP("100.90.1.2")} }
	cases := []struct{ bind, want string }{
		{"", "http://127.0.0.1:7777"},
		{"100.80.2.3", "http://100.80.2.3:7777"},
		{"0.0.0.0", "http://100.90.1.2:7777"},
		{"192.168.1.5", "http://192.168.1.5:7777"},
	}
	for _, c := range cases {
		if got := pairBase(r, c.bind, 7777, ips); got != c.want {
			t.Fatalf("bind %q: %s", c.bind, got)
		}
	}
	t.Setenv("AGENT_MONITOR_ALLOWED_HOSTS", "mac.tail1.ts.net, other")
	if got := pairBase(r, "", 7777, ips); got != "https://mac.tail1.ts.net" {
		t.Fatalf("allowed host: %s", got)
	}
	t.Setenv("AGENT_MONITOR_PAIR_BASE", "https://pair.example/")
	if got := pairBase(r, "100.80.2.3", 7777, ips); got != "https://pair.example" {
		t.Fatalf("pair base: %s", got)
	}
}

// A Bearer guess counts against the login limit; the right password from that address is then refused too.
func TestBearerGuessesAreLimited(t *testing.T) {
	e := newDevEnv(t, "hunter2-long-pw")
	resetLoginLimiter()
	t.Cleanup(resetLoginLimiter)
	bearer := func(pw string) func(r *http.Request) {
		return func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+pw) }
	}
	for i := 0; i < loginMaxFailures; i++ {
		if w := e.do("GET", "/api/secret", "", "100.64.0.9:5000", bearer("guess")); w.Code != 401 {
			t.Fatalf("guess %d: %d", i, w.Code)
		}
	}
	if w := e.do("GET", "/api/secret", "", "100.64.0.9:5000", bearer("hunter2-long-pw")); w.Code != 429 {
		t.Fatalf("after the limit: %d", w.Code)
	}
	if w := e.do("GET", "/api/secret", "", "100.64.0.10:5000", bearer("hunter2-long-pw")); w.Code != 200 {
		t.Fatalf("another address: %d", w.Code)
	}
}
