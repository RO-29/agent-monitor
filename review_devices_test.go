package main

import (
	"crypto/ecdh"
	"crypto/rand"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// F5: revoking a device closes its WebSockets and streams and deletes its push subscriptions.
func TestReviewF5RevokeClosesLiveConnections(t *testing.T) {
	e := newDevEnv(t, "pw")
	d, tok := e.create("phone")
	sseDone := make(chan struct{})
	mux := http.NewServeMux()
	deviceRoutes(mux, e.store)
	mux.HandleFunc("GET /api/stream", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(sseDone)
	})
	mux.HandleFunc("GET /ws", func(w http.ResponseWriter, r *http.Request) {
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		for {
			if _, _, err := c.ReadMessage(); err != nil {
				return
			}
		}
	})
	h := originGuard(authMiddleware(mux))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Test-Remote") != "" {
			r.RemoteAddr = remoteAddr
		}
		h.ServeHTTP(w, r)
	}))
	defer srv.Close()
	hdr := http.Header{"Authorization": {"Bearer " + tok}, "X-Test-Remote": {"1"}}
	ws, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http")+"/ws", hdr)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()
	req, _ := http.NewRequest("GET", srv.URL+"/api/stream", nil)
	req.Header = hdr.Clone()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	n := newTestNotifier(t, &NotifySettings{})
	oldN := notifier
	notifier = n
	t.Cleanup(func() { notifier = oldN })
	sub := testPushKeys(t)
	sub.Endpoint = "https://web.push.apple.com/abc"
	if err := n.addSubFor(sub, d.ID); err != nil {
		t.Fatal(err)
	}
	other := pushSub{Endpoint: "https://fcm.googleapis.com/fcm/send/x", Keys: sub.Keys}
	if err := n.addSubFor(other, ""); err != nil {
		t.Fatal(err)
	}

	if w := e.do("DELETE", "/api/devices/"+d.ID, "", "127.0.0.1:1", nil); w.Code != 200 {
		t.Fatalf("revoke %d", w.Code)
	}
	ws.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, _, err := ws.ReadMessage(); err == nil || strings.Contains(err.Error(), "timeout") {
		t.Fatalf("websocket still open after revoke: %v", err)
	}
	select {
	case <-sseDone:
	case <-time.After(3 * time.Second):
		t.Fatal("stream still open after revoke")
	}
	subs := n.pushSubs()
	if len(subs) != 1 || subs[0].Endpoint != other.Endpoint {
		t.Fatalf("push subscriptions after revoke: %+v", subs)
	}
}

// F5: turning remoteTerminal off closes remote terminals and keeps local ones.
func TestReviewF5RemoteTerminalOffClosesRemoteShells(t *testing.T) {
	e := newTermEnv(t, time.Minute)
	settings.mu.Lock()
	settings.v.RemoteTerminal = true
	settings.mu.Unlock()
	remote, code := e.dial("/api/term?cwd="+e.dir, http.Header{"Authorization": {"Bearer " + e.token}, "X-Test-Remote": {"1"}})
	if code != 101 {
		t.Fatalf("remote terminal %d", code)
	}
	local := e.open("/api/term?cwd=" + e.dir)
	readUntil(t, remote, "")
	readUntil(t, local, "")
	mux := http.NewServeMux()
	registerTraceRoutes(mux)
	r := httptest.NewRequest("POST", "/api/settings", strings.NewReader(`{"remoteTerminal":false}`))
	r.RemoteAddr = "127.0.0.1:1"
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("settings %d %s", w.Code, w.Body)
	}
	remote.SetReadDeadline(time.Now().Add(3 * time.Second))
	for {
		if _, _, err := remote.ReadMessage(); err != nil {
			if strings.Contains(err.Error(), "timeout") {
				t.Fatal("remote terminal still open")
			}
			break
		}
	}
	local.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	if _, _, err := local.ReadMessage(); err != nil && !strings.Contains(err.Error(), "timeout") {
		t.Fatalf("local terminal closed: %v", err)
	}
}

// F6: a device may revoke only itself and may not mint devices.
func TestReviewF6DeviceCannotAdministerOthers(t *testing.T) {
	e := newDevEnv(t, "pw")
	d1, tok1 := e.create("phone")
	d2, _ := e.create("tablet")
	if w := e.do("POST", "/api/devices", `{"name":"evil"}`, remoteAddr, bearer(tok1)); w.Code != http.StatusForbidden {
		t.Fatalf("device minted a device: %d", w.Code)
	}
	if w := e.do("DELETE", "/api/devices/"+d2.ID, "", remoteAddr, bearer(tok1)); w.Code != http.StatusForbidden {
		t.Fatalf("device revoked another device: %d", w.Code)
	}
	pw := func(r *http.Request) { r.Header.Set("Authorization", "Bearer pw") }
	if w := e.do("DELETE", "/api/devices/"+d2.ID, "", remoteAddr, pw); w.Code != http.StatusForbidden {
		t.Fatalf("password caller revoked a device: %d", w.Code)
	}
	mux := http.NewServeMux()
	registerTraceRoutes(mux)
	h := authMiddleware(mux)
	setRequire(true)
	w, _ := rvDo(h, "POST", "/api/settings", `{"requireDeviceToken":false}`, remoteAddr, map[string]string{"Authorization": "Bearer " + tok1, "Content-Type": "application/json"})
	if w.Code != http.StatusForbidden || !getSettings().RequireDeviceToken {
		t.Fatalf("device turned requireDeviceToken off: %d", w.Code)
	}
	if w := e.do("DELETE", "/api/devices/"+d1.ID, "", remoteAddr, bearer(tok1)); w.Code != 200 {
		t.Fatalf("self revoke %d %s", w.Code, w.Body)
	}
}

// F7: the pair link carries a single-use code that expires; the token never appears in it.
func TestReviewF7PairCodeIsOneTime(t *testing.T) {
	e := newDevEnv(t, "pw")
	w := e.do("POST", "/api/devices", `{"name":"phone"}`, "127.0.0.1:1", nil)
	var out struct {
		Token   string `json:"token"`
		PairURL string `json:"pairUrl"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	if w.Code != 201 || out.Token == "" || strings.Contains(out.PairURL, out.Token) {
		t.Fatalf("pairUrl leaks the token or create failed: %d %s", w.Code, out.PairURL)
	}
	code := strings.TrimPrefix(out.PairURL[strings.Index(out.PairURL, "#")+1:], "code=")
	if code == "" || strings.HasPrefix(code, deviceTokenPrefix) {
		t.Fatalf("no pairing code in %s", out.PairURL)
	}
	w = e.do("POST", "/api/devices/session", `{"code":"`+code+`"}`, remoteAddr, nil)
	var cookie *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == deviceCookieName {
			cookie = c
		}
	}
	if w.Code != 200 || cookie == nil || cookie.Value != out.Token {
		t.Fatalf("code exchange %d %+v", w.Code, cookie)
	}
	if w := e.do("POST", "/api/devices/session", `{"code":"`+code+`"}`, remoteAddr, nil); w.Code != 401 {
		t.Fatalf("code reused: %d", w.Code)
	}
	w = e.do("POST", "/api/devices", `{"name":"tablet"}`, "127.0.0.1:1", nil)
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	code = strings.TrimPrefix(out.PairURL[strings.Index(out.PairURL, "#")+1:], "code=")
	e.clock = e.clock.Add(6 * time.Minute)
	if w := e.do("POST", "/api/devices/session", `{"code":"`+code+`"}`, remoteAddr, nil); w.Code != 401 {
		t.Fatalf("expired code accepted: %d", w.Code)
	}
	if w := e.do("POST", "/api/devices/session", `{"token":"`+out.Token+`"}`, remoteAddr, nil); w.Code != 200 {
		t.Fatalf("token body (AgentBar) refused: %d", w.Code)
	}
	// An older Pair page sends the code in the token field.
	w = e.do("POST", "/api/devices", `{"name":"old"}`, "127.0.0.1:1", nil)
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	code = out.PairURL[strings.Index(out.PairURL, "#")+1:]
	if w := e.do("POST", "/api/devices/session", `{"token":"`+code+`"}`, remoteAddr, nil); w.Code != 200 {
		t.Fatalf("code in token field refused: %d", w.Code)
	}
}

// F9: pairUrl never uses the request Host; X-Forwarded-Proto counts only from a local proxy.
func TestReviewF9PairBaseAndSecureFlag(t *testing.T) {
	t.Setenv("AGENT_MONITOR_PAIR_BASE", "")
	t.Setenv("AGENT_MONITOR_ALLOWED_HOSTS", "")
	r := httptest.NewRequest("POST", "/api/devices", nil)
	r.Host = "evil.example:7777"
	r.Header.Set("X-Forwarded-Proto", "https")
	none := func() []net.IP { return nil }
	for _, bind := range []string{"", "127.0.0.1", "192.168.1.5"} {
		if got := pairBase(r, bind, 7777, none); strings.Contains(got, "evil.example") {
			t.Errorf("bind %q: pairUrl uses the request Host: %s", bind, got)
		}
	}
	r.RemoteAddr = remoteAddr
	if secureRequest(r) {
		t.Error("remote client header made the request secure")
	}
	r.RemoteAddr = "127.0.0.1:1"
	if !secureRequest(r) {
		t.Error("local proxy https header ignored")
	}
}

func testPushKeys(t *testing.T) pushSub {
	k, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	var s pushSub
	s.Keys.P256dh = b64u.EncodeToString(k.PublicKey().Bytes())
	s.Keys.Auth = b64u.EncodeToString(make([]byte, 16))
	return s
}
