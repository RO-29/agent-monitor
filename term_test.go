package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

type termTestEnv struct {
	t     *testing.T
	m     *termManager
	srv   *httptest.Server
	dir   string
	token string
}

func newTermEnv(t *testing.T, idle time.Duration) *termTestEnv {
	t.Helper()
	t.Setenv("SHELL", "/bin/sh")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ENV", "")
	t.Setenv("AGENT_MONITOR_HOME", t.TempDir())
	ds, err := openDeviceStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	_, tok, err := ds.Create("phone")
	if err != nil {
		t.Fatal(err)
	}
	m := newTermManager()
	m.idle = idle
	oldMgr, oldStore, oldPw, oldCwd := termMgr, deviceStore, authPassword, termThreadCwd
	settings.mu.Lock()
	oldSettings := settings.v
	settings.v.RequireDeviceToken, settings.v.RemoteTerminal = false, false
	settings.mu.Unlock()
	termMgr, deviceStore, authPassword = m, ds, "pw"
	dir := t.TempDir()
	allowCwds(t, filepath.Dir(dir))
	termThreadCwd = func(id string) (string, error) {
		switch id {
		case "ot_ok":
			return dir, nil
		case "ot_gone":
			return filepath.Join(dir, "missing"), nil
		}
		return "", errOrchNotFound
	}
	mux := http.NewServeMux()
	registerTermRoutes(mux)
	h := originGuard(authMiddleware(mux))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Test-Remote") != "" {
			r.RemoteAddr = remoteAddr
		}
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(func() {
		srv.Close()
		m.mu.Lock()
		for _, s := range m.sessions {
			_ = s.cmd.Process.Kill()
		}
		m.mu.Unlock()
		termMgr, deviceStore, authPassword, termThreadCwd = oldMgr, oldStore, oldPw, oldCwd
		settings.mu.Lock()
		settings.v = oldSettings
		settings.mu.Unlock()
		ds.Close()
	})
	return &termTestEnv{t: t, m: m, srv: srv, dir: dir, token: tok}
}

func (e *termTestEnv) dial(path string, hdr http.Header) (*websocket.Conn, int) {
	e.t.Helper()
	c, resp, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(e.srv.URL, "http")+path, hdr)
	if err != nil {
		if resp == nil {
			e.t.Fatalf("dial %s: %v", path, err)
		}
		return nil, resp.StatusCode
	}
	e.t.Cleanup(func() { c.Close() })
	return c, http.StatusSwitchingProtocols
}

func (e *termTestEnv) open(path string) *websocket.Conn {
	e.t.Helper()
	c, code := e.dial(path, nil)
	if code != 101 {
		e.t.Fatalf("open %s: %d", path, code)
	}
	return c
}

type termHello struct {
	Type       string `json:"type"`
	New        bool   `json:"new"`
	Cwd        string `json:"cwd"`
	Scrollback int    `json:"scrollback"`
	Code       int    `json:"code"`
}

// readUntil collects output until it contains want; it returns the output and the text frames seen.
func readUntil(t *testing.T, c *websocket.Conn, want string) (string, []termHello) {
	t.Helper()
	var out strings.Builder
	var texts []termHello
	_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
	for {
		mt, data, err := c.ReadMessage()
		if err != nil {
			t.Fatalf("waiting for %q: %v; got %q", want, err, out.String())
		}
		if mt == websocket.TextMessage {
			var h termHello
			_ = json.Unmarshal(data, &h)
			texts = append(texts, h)
			if want == "" || (want == "exit" && h.Type == "exit") {
				return out.String(), texts
			}
			continue
		}
		out.Write(data)
		if want != "" && want != "exit" && strings.Contains(out.String(), want) {
			return out.String(), texts
		}
	}
}

func TestTermEchoRoundTrip(t *testing.T) {
	e := newTermEnv(t, time.Minute)
	c := e.open("/api/orch/threads/ot_ok/term")
	_, texts := readUntil(t, c, "")
	if len(texts) != 1 || texts[0].Type != "hello" || !texts[0].New || texts[0].Cwd != e.dir {
		t.Fatalf("hello %+v", texts)
	}
	_ = c.WriteMessage(websocket.BinaryMessage, []byte("echo bin-$((40+2)) $TERM\n"))
	readUntil(t, c, "bin-42 xterm-256color")
	_ = c.WriteJSON(map[string]any{"type": "input", "data": "pwd; echo txt-$((1+2))\n"})
	out, _ := readUntil(t, c, "txt-3")
	real, _ := filepath.EvalSymlinks(e.dir)
	if !strings.Contains(out, e.dir) && !strings.Contains(out, real) {
		t.Fatalf("pwd not the thread cwd: %q", out)
	}
}

func TestTermResize(t *testing.T) {
	e := newTermEnv(t, time.Minute)
	c := e.open("/api/term?cwd=" + e.dir + "&cols=90&rows=20")
	readUntil(t, c, "")
	_ = c.WriteMessage(websocket.BinaryMessage, []byte("stty size; echo s-$((1))\n"))
	readUntil(t, c, "20 90")
	_ = c.WriteJSON(map[string]any{"type": "resize", "cols": 132, "rows": 41})
	time.Sleep(100 * time.Millisecond)
	_ = c.WriteMessage(websocket.BinaryMessage, []byte("stty size\n"))
	readUntil(t, c, "41 132")
}

func TestTermScrollbackReplay(t *testing.T) {
	e := newTermEnv(t, time.Minute)
	c := e.open("/api/orch/threads/ot_ok/term")
	_ = c.WriteMessage(websocket.BinaryMessage, []byte("echo mark-$((7*6))\n"))
	readUntil(t, c, "mark-42")
	c.Close()
	time.Sleep(100 * time.Millisecond)
	if n := e.m.Live(); n != 1 {
		t.Fatalf("shell should survive the client: live %d", n)
	}
	c2 := e.open("/api/orch/threads/ot_ok/term")
	out, texts := readUntil(t, c2, "mark-42")
	if len(texts) == 0 || texts[0].New || texts[0].Scrollback == 0 || !strings.Contains(out, "mark-42") {
		t.Fatalf("replay hello %+v out %q", texts, out)
	}
	// A second live client sees the same output.
	c3 := e.open("/api/orch/threads/ot_ok/term")
	readUntil(t, c3, "mark-42")
	_ = c2.WriteMessage(websocket.BinaryMessage, []byte("echo both-$((2+2))\n"))
	readUntil(t, c2, "both-4")
	readUntil(t, c3, "both-4")
}

func TestTermIdleKill(t *testing.T) {
	e := newTermEnv(t, 300*time.Millisecond)
	c := e.open("/api/orch/threads/ot_ok/term")
	readUntil(t, c, "")
	time.Sleep(500 * time.Millisecond)
	if e.m.Live() != 1 {
		t.Fatal("killed while a client was attached")
	}
	c.Close()
	deadline := time.Now().Add(20 * time.Second)
	for e.m.Live() != 0 {
		if time.Now().After(deadline) {
			t.Fatal("shell not killed after the idle timeout")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestTermExitNotice(t *testing.T) {
	e := newTermEnv(t, time.Minute)
	c := e.open("/api/orch/threads/ot_ok/term")
	_ = c.WriteMessage(websocket.BinaryMessage, []byte("exit 3\n"))
	_, texts := readUntil(t, c, "exit")
	if last := texts[len(texts)-1]; last.Code != 3 {
		t.Fatalf("exit %+v", last)
	}
	if e.m.Live() != 0 {
		t.Fatal("exited shell still listed")
	}
}

func TestTermMaxShells(t *testing.T) {
	e := newTermEnv(t, time.Minute)
	var first string
	for i := 0; i < 6; i++ {
		d := t.TempDir()
		if i == 0 {
			first = d
		}
		readUntil(t, e.open("/api/term?cwd="+d), "")
	}
	if _, code := e.dial("/api/term?cwd="+t.TempDir(), nil); code != http.StatusTooManyRequests {
		t.Fatalf("7th shell %d", code)
	}
	if _, code := e.dial("/api/term?cwd="+first, nil); code != 101 {
		t.Fatalf("reattach at the limit %d", code)
	}
}

func TestTermAccess(t *testing.T) {
	e := newTermEnv(t, time.Minute)
	remote := func(auth string) http.Header {
		h := http.Header{"X-Test-Remote": {"1"}}
		if auth != "" {
			h.Set("Authorization", "Bearer "+auth)
		}
		return h
	}
	path := "/api/orch/threads/ot_ok/term"
	if _, code := e.dial(path, remote("pw")); code != 403 {
		t.Fatalf("remote password, setting off %d", code)
	}
	if _, code := e.dial(path, remote(e.token)); code != 403 {
		t.Fatalf("remote device, setting off %d", code)
	}
	settings.mu.Lock()
	settings.v.RemoteTerminal = true
	settings.mu.Unlock()
	if _, code := e.dial(path, remote("pw")); code != 403 {
		t.Fatalf("remote password only, setting on %d", code)
	}
	if _, code := e.dial(path, remote("")); code != 401 {
		t.Fatalf("remote no credential %d", code)
	}
	if _, code := e.dial(path, remote(e.token)); code != 101 {
		t.Fatalf("remote device, setting on %d", code)
	}
	ck := remote("")
	ck.Set("Cookie", deviceCookieName+"="+e.token)
	if _, code := e.dial(path, ck); code != 101 {
		t.Fatalf("remote device cookie %d", code)
	}
	fwd := http.Header{"X-Forwarded-For": {"100.1.1.1"}}
	if _, code := e.dial(path, fwd); code != 401 {
		t.Fatalf("proxied loopback without a device %d", code)
	}
	settings.mu.Lock()
	settings.v.RemoteTerminal = false
	settings.mu.Unlock()
	if _, code := e.dial(path, nil); code != 101 {
		t.Fatalf("loopback %d", code)
	}
}

func TestTermBadCwd(t *testing.T) {
	e := newTermEnv(t, time.Minute)
	file := filepath.Join(e.dir, "f")
	_ = os.WriteFile(file, []byte("x"), 0o600)
	for path, want := range map[string]int{
		"/api/term?cwd=relative/dir":         400,
		"/api/term":                          400,
		"/api/term?cwd=/no/such/dir/am-test": 403,
		"/api/term?cwd=" + file:              400,
		"/api/orch/threads/ot_gone/term":     400,
		"/api/orch/threads/ot_unknown/term":  404,
	} {
		if _, code := e.dial(path, nil); code != want {
			t.Fatalf("%s: %d want %d", path, code, want)
		}
	}
	if e.m.Live() != 0 {
		t.Fatal("a refused call started a shell")
	}
}

func TestTermEnvDropsSecrets(t *testing.T) {
	env := termEnv([]string{"PATH=/bin", "TERM=dumb", "AGENT_MONITOR_PASSWORD=x", "AGENT_MONITOR_PORT=7777", "AM_ORCH_THREAD=ot_1"})
	got := strings.Join(env, " ")
	if got != "PATH=/bin AGENT_MONITOR_PORT=7777 TERM=xterm-256color" {
		t.Fatalf("env %q", got)
	}
}

func TestSettingsRemoteTerminal(t *testing.T) {
	t.Setenv("AGENT_MONITOR_HOME", t.TempDir())
	settings.mu.Lock()
	saved := settings.v
	settings.v = Settings{EnrichModel: "haiku", DailyCapUSD: 2}
	settings.mu.Unlock()
	defer func() { settings.mu.Lock(); settings.v = saved; settings.mu.Unlock() }()
	mux := http.NewServeMux()
	registerTraceRoutes(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	code, out := postJSON(t, srv.URL+"/api/settings", `{"remoteTerminal":true}`)
	if code != 200 || out["remoteTerminal"] != true || !getSettings().RemoteTerminal {
		t.Fatalf("post %d %v", code, out)
	}
	code, out = postJSON(t, srv.URL+"/api/settings", `{"acpTurnTimeoutMin":5}`)
	if code != 200 || out["remoteTerminal"] != true {
		t.Fatalf("merge lost remoteTerminal %v", out)
	}
}

func TestTermNoShellWithoutUpgrade(t *testing.T) {
	e := newTermEnv(t, time.Minute)
	resp, err := http.Get(e.srv.URL + "/api/term?cwd=" + e.dir)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUpgradeRequired || e.m.Live() != 0 {
		t.Fatalf("plain GET: status %d, shells %d; want 426 and 0", resp.StatusCode, e.m.Live())
	}
	if _, code := e.dial("/api/term?cwd="+e.dir, http.Header{"Origin": {"https://evil.example"}}); code < 400 || e.m.Live() != 0 {
		t.Fatalf("cross-origin upgrade: status %d, shells %d; want refused and 0", code, e.m.Live())
	}
}
