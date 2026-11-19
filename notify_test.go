package main

import (
	"bufio"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func intPtr(n int) *int { return &n }

func newTestNotifier(t *testing.T, cfg *NotifySettings) *Notifier {
	t.Helper()
	n, err := openNotifier(t.TempDir(), func() NotifySettings { return *cfg })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { n.db.Close() })
	return n
}

func TestNotifyQuietHours(t *testing.T) {
	at := func(h, m int) time.Time { return time.Date(2026, 11, 16, h, m, 0, 0, time.Local) }
	night := NotifySettings{QuietStart: "22:30", QuietEnd: "07:00"}
	for _, c := range []struct {
		t    time.Time
		want bool
	}{{at(23, 0), true}, {at(3, 0), true}, {at(7, 0), false}, {at(12, 0), false}, {at(22, 30), true}} {
		if got := night.quiet(c.t); got != c.want {
			t.Errorf("quiet at %s = %v", c.t.Format("15:04"), got)
		}
	}
	day := NotifySettings{QuietStart: "12:00", QuietEnd: "13:00"}
	if !day.quiet(at(12, 30)) || day.quiet(at(13, 30)) {
		t.Fatal("same-day window")
	}
	if (NotifySettings{QuietStart: "25:00", QuietEnd: "07:00"}).quiet(at(3, 0)) {
		t.Fatal("bad clock must turn quiet hours off")
	}
	if validNotifySettings(NotifySettings{QuietStart: "7:5"}) == nil || validNotifySettings(NotifySettings{BatchSec: intPtr(-1)}) == nil ||
		validNotifySettings(NotifySettings{QuietStart: "07:05", QuietEnd: ""}) != nil {
		t.Fatal("validation")
	}
}

func TestNotifierBatchQuietAndList(t *testing.T) {
	off := false
	cfg := &NotifySettings{BatchSec: intPtr(3), Error: &off}
	n := newTestNotifier(t, cfg)
	n.unit = 200 * time.Millisecond
	live, cancel := n.subscribe()
	defer cancel()

	// The first event goes out at once; the next two fold into one at the end of the window.
	n.Emit(notifyEvent{Kind: "error", Title: "dropped"})
	n.Emit(notifyEvent{Kind: "needs-you", ThreadID: "ot_a", Title: "A needs approval"})
	n.Emit(notifyEvent{Kind: "needs-you", ThreadID: "ot_b", Title: "B needs approval"})
	n.Emit(notifyEvent{Kind: "needs-you", ThreadID: "ot_c", Title: "C needs approval"})
	first := recvNote(t, live)
	if first.Title != "A needs approval" || first.ThreadID != "ot_a" || first.ID == "" {
		t.Fatalf("first %+v", first)
	}
	var folded ONotification
	select {
	case folded = <-live:
	case <-time.After(15 * time.Second):
		t.Fatal("no folded notification")
	}
	if folded.Count != 2 || folded.Title != "2 requests need you" || folded.ThreadID != "" || !strings.Contains(folded.Body, "B needs approval; C needs approval") {
		t.Fatalf("folded %+v", folded)
	}

	cfg.BatchSec = intPtr(0)
	for deadline := time.Now().Add(15 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		n.mu.Lock()
		open := len(n.batches)
		n.mu.Unlock()
		if open == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("batch window never closed")
		}
	}
	now := time.Now()
	cfg.QuietStart, cfg.QuietEnd = now.Add(-time.Hour).Format("15:04"), now.Add(time.Hour).Format("15:04")
	if now.Hour() == 0 || now.Hour() == 23 {
		t.Skip("quiet window would cross midnight in an unexpected way")
	}
	n.Emit(notifyEvent{Kind: "finished", Title: "quiet one"})
	n.Emit(notifyEvent{Kind: "needs-you", Title: "loud one"})
	if q := recvNote(t, live); !q.Quiet || q.Title != "quiet one" {
		t.Fatalf("quiet %+v", q)
	}
	if l := recvNote(t, live); l.Quiet {
		t.Fatalf("needs-you during quiet hours must stay loud: %+v", l)
	}

	items, unread, err := n.List(0, 10)
	if err != nil || len(items) != 4 || unread != 4 || items[0].Title != "loud one" {
		t.Fatalf("list %d %d %v %+v", len(items), unread, err, items)
	}
	if err := n.MarkRead([]string{items[0].ID}, false); err != nil {
		t.Fatal(err)
	}
	if items, unread, _ := n.List(0, 10); unread != 3 || !items[0].Read {
		t.Fatalf("after read: %d %+v", unread, items[0])
	}
	after := items[1].ID
	newer, _, _ := n.List(mustInt(t, after), 10)
	if len(newer) != 1 || newer[0].ID != items[0].ID {
		t.Fatalf("after=%s: %+v", after, newer)
	}
	_ = n.MarkRead(nil, true)
	if n.unread() != 0 {
		t.Fatal("mark all")
	}
}

func mustInt(t *testing.T, s string) int64 {
	t.Helper()
	var v int64
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestNotifyRoutesAndStream(t *testing.T) {
	cfg := &NotifySettings{BatchSec: intPtr(0)}
	n := newTestNotifier(t, cfg)
	mux := http.NewServeMux()
	n.routes(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	n.Emit(notifyEvent{Kind: "finished", ThreadID: "ot_1", Title: "one"})

	resp, err := http.Get(srv.URL + "/api/notify/stream?after=0")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content type %q", ct)
	}
	lines := make(chan string, 16)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			if strings.HasPrefix(sc.Text(), "data: ") {
				lines <- strings.TrimPrefix(sc.Text(), "data: ")
			}
		}
	}()
	time.Sleep(100 * time.Millisecond)
	n.Emit(notifyEvent{Kind: "spend", Title: "day limit"})
	select {
	case l := <-lines:
		var no ONotification
		if json.Unmarshal([]byte(l), &no) != nil || no.Title != "day limit" || no.URL != "/settings/limits" || no.ID != "2" {
			t.Fatalf("stream %s", l)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no stream data")
	}
	resp2, err := http.Get(srv.URL + "/api/notify/stream?after=0")
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()

	code, out := getJSON2(t, srv.URL+"/api/notify?after=0&limit=10")
	if code != 200 || out["unread"].(float64) != 2 || len(out["items"].([]any)) != 2 {
		t.Fatalf("list %d %v", code, out)
	}
	if code, out := postJSON(t, srv.URL+"/api/notify/read", `{"ids":["1"]}`); code != 200 || out["unread"].(float64) != 1 {
		t.Fatalf("read %d %v", code, out)
	}
	if code, out := getJSON2(t, srv.URL+"/api/notify/vapid"); code != 200 || len(out["publicKey"].(string)) != 87 {
		t.Fatalf("vapid %d %v", code, out)
	}
	if code, _ := postJSON(t, srv.URL+"/api/notify/subscribe", `{"endpoint":"http://x/y","keys":{"p256dh":"AA","auth":"AA"}}`); code != 400 {
		t.Fatalf("bad subscription accepted: %d", code)
	}
}

func getJSON2(t *testing.T, url string) (int, map[string]any) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// decryptPush is the user agent side of RFC 8291, for the round-trip test.
func decryptPush(t *testing.T, ua *ecdh.PrivateKey, auth, body []byte) []byte {
	t.Helper()
	salt, rs, idlen := body[:16], binary.BigEndian.Uint32(body[16:20]), int(body[20])
	if rs != pushRecordSize || idlen != 65 {
		t.Fatalf("header rs=%d idlen=%d", rs, idlen)
	}
	asPub := body[21 : 21+idlen]
	as, err := ecdh.P256().NewPublicKey(asPub)
	if err != nil {
		t.Fatal(err)
	}
	secret, err := ua.ECDH(as)
	if err != nil {
		t.Fatal(err)
	}
	cek, nonce, err := pushKeys(secret, auth, ua.PublicKey().Bytes(), asPub, salt)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := aes.NewCipher(cek)
	gcm, _ := cipher.NewGCM(block)
	plain, err := gcm.Open(nil, nonce, body[21+idlen:], nil)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if plain[len(plain)-1] != 0x02 {
		t.Fatal("missing last-record delimiter")
	}
	return plain[:len(plain)-1]
}

func TestWebPushEncryptAndSend(t *testing.T) {
	ua, _ := ecdh.P256().GenerateKey(rand.Reader)
	auth := make([]byte, 16)
	_, _ = rand.Read(auth)
	body, err := encryptPush(ua.PublicKey().Bytes(), auth, []byte(`{"title":"hi"}`), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if got := decryptPush(t, ua, auth, body); string(got) != `{"title":"hi"}` {
		t.Fatalf("round trip %q", got)
	}

	dir := t.TempDir()
	keys, err := loadVAPID(dir)
	if err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(filepath.Join(dir, "vapid.json"))
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("vapid.json mode %v", st.Mode())
	}
	again, err := loadVAPID(dir)
	if err != nil || again.public != keys.public {
		t.Fatal("keys not reused")
	}

	var mu sync.Mutex
	var got []byte
	var hdr http.Header
	status := http.StatusCreated
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		got, hdr = b, r.Header.Clone()
		code := status
		mu.Unlock()
		w.WriteHeader(code)
	}))
	defer srv.Close()

	cfg := &NotifySettings{BatchSec: intPtr(0)}
	n := newTestNotifier(t, cfg)
	oldAllowed := pushHostAllowed
	pushHostAllowed = func(*url.URL) bool { return true }
	t.Cleanup(func() { pushHostAllowed = oldAllowed })
	n.push = &webPusher{keys: keys, sub: "mailto:test@example.com", client: srv.Client()}
	sub := pushSub{Endpoint: srv.URL + "/push/abc"}
	sub.Keys.P256dh, sub.Keys.Auth = b64u.EncodeToString(ua.PublicKey().Bytes()), b64u.EncodeToString(auth)
	if err := n.addSub(sub); err != nil {
		t.Fatal(err)
	}
	n.pushAll(ONotification{ID: "7", Kind: "needs-you", Title: "Approve?", ThreadID: "ot_x"})
	mu.Lock()
	payload := decryptPush(t, ua, auth, got)
	h := hdr
	mu.Unlock()
	var msg map[string]any
	if json.Unmarshal(payload, &msg) != nil || msg["title"] != "Approve?" || msg["threadId"] != "ot_x" || msg["id"] != "7" {
		t.Fatalf("payload %s", payload)
	}
	if h.Get("Content-Encoding") != "aes128gcm" || h.Get("Urgency") != "high" || h.Get("TTL") == "" {
		t.Fatalf("headers %v", h)
	}
	auths := strings.TrimPrefix(h.Get("Authorization"), "vapid ")
	parts := map[string]string{}
	for _, kv := range strings.Split(auths, ", ") {
		k, v, _ := strings.Cut(kv, "=")
		parts[k] = v
	}
	if parts["k"] != keys.public {
		t.Fatal("vapid k")
	}
	seg := strings.Split(parts["t"], ".")
	claims, _ := b64u.DecodeString(seg[1])
	var c map[string]any
	_ = json.Unmarshal(claims, &c)
	if c["aud"] != srv.URL || c["sub"] != "mailto:test@example.com" {
		t.Fatalf("claims %s", claims)
	}
	sig, _ := b64u.DecodeString(seg[2])
	hsum := sha256.Sum256([]byte(seg[0] + "." + seg[1]))
	r, s := new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])
	if !ecdsa.Verify(&keys.priv.PublicKey, hsum[:], r, s) {
		t.Fatal("jwt signature does not verify")
	}

	mu.Lock()
	status = http.StatusGone
	mu.Unlock()
	n.pushAll(ONotification{ID: "8", Kind: "finished", Title: "x"})
	if len(n.pushSubs()) != 0 {
		t.Fatal("gone subscription kept")
	}
}

func TestOrchNotifyEvents(t *testing.T) {
	e := newOrchEnv(t)
	sink := &notifySink{}
	e.m.notifier = sink.add

	root := e.create("hello")
	e.idle(root.ID)
	e.until("finished", func() bool { return len(sink.titles("finished")) == 1 })

	th, err := e.m.CreateThread(orchCreate{Agent: "codex", Cwd: e.cwd, Prompt: "hello", Children: []orchChildSpec{
		{Title: "one", Prompt: "hello", Agent: "codex"}, {Title: "two", Prompt: "hello", Agent: "codex"}}})
	if err != nil {
		t.Fatal(err)
	}
	e.until("group notice", func() bool {
		for _, ti := range sink.titles("finished") {
			if ti == "2 subagents finished" {
				return true
			}
		}
		return false
	})
	e.until("parent notified", func() bool { return notifyRuns(e.runs(th.ID)) == 1 })
	e.idle(th.ID)
	time.Sleep(time.Second) // room for a duplicate group notice to show up
	groups := 0
	for _, ti := range sink.titles("finished") {
		if ti == "2 subagents finished" {
			groups++
		}
	}
	if groups != 1 {
		t.Fatalf("group notices %d: %v", groups, sink.titles("finished"))
	}

	cl := e.createClaude("approve", "")
	e.until("needs-you", func() bool { return len(sink.titles("needs-you")) == 1 })
	if got := sink.titles("needs-you")[0]; !strings.Contains(got, "Bash needs approval") {
		t.Fatalf("needs-you %q", got)
	}
	e.until("request", func() bool { return len(e.detail(cl.ID).Requests) == 1 })
	e.decide(e.detail(cl.ID).Requests[0].ApprovalID, approvalDecision{Behavior: "allow"})
	e.idle(cl.ID)

	bad := e.create("fail")
	e.until("error", func() bool { return len(sink.titles("error")) == 1 })
	if got := sink.titles("error")[0]; got != bad.Title+" stopped with an error" {
		t.Fatalf("error %q", got)
	}
}

func TestSettingsNotifyAndSpend(t *testing.T) {
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
	code, out := postJSON(t, srv.URL+"/api/settings", `{"notify":{"needsYou":false,"quietStart":"22:00","quietEnd":"07:00","batchSec":30},"spend":{"perThreadUsd":5,"codexRates":{"*":{"in":1.25,"out":10}}},"acpTurnTimeoutMin":7}`)
	if code != 200 {
		t.Fatalf("post %d %v", code, out)
	}
	s := getSettings()
	if s.Notify.enabled("needs-you") || !s.Notify.enabled("finished") || s.Notify.batchSecs() != 30 || !s.Notify.quiet(time.Date(2026, 1, 1, 23, 0, 0, 0, time.Local)) ||
		s.Spend.PerThreadUSD != 5 || s.Spend.PerDayUSD != 0 || s.Spend.CodexRates["*"].Out != 10 || s.AcpTurnTimeoutMin != 7 {
		t.Fatalf("settings %+v", s)
	}
	n := out["notify"].(map[string]any)
	if n["needsYou"] != false || n["finished"] != nil || n["quietStart"] != "22:00" {
		t.Fatalf("notify json %v", n)
	}
	if code, _ := postJSON(t, srv.URL+"/api/settings", `{"notify":{"quietStart":"9pm"}}`); code != 400 {
		t.Fatalf("bad clock %d", code)
	}
	if code, _ := postJSON(t, srv.URL+"/api/settings", `{"spend":{"perDayUsd":-1}}`); code != 400 {
		t.Fatalf("negative limit %d", code)
	}
	if code, _ := postJSON(t, srv.URL+"/api/settings", `{"spend":{"perDayUsd":20}}`); code != 200 || getSettings().Spend.PerThreadUSD != 5 || getSettings().Spend.PerDayUSD != 20 {
		t.Fatal("partial spend update")
	}
}

func TestNotifyWatchedApproval(t *testing.T) {
	n := newTestNotifier(t, &NotifySettings{BatchSec: intPtr(0)})
	prev := notifier
	notifier = n
	defer func() { notifier = prev }()
	live, cancel := n.subscribe()
	defer cancel()
	notifyWatchedApproval(&Approval{Agent: "claude", SessionID: "claude:s1", Cwd: "/x/home", Kind: "question", Summary: "Yes or no?"})
	select {
	case got := <-live:
		if got.Kind != "needs-you" || got.ThreadID != "claude:s1" || got.Title != "home: a question for you" || got.Body != "Yes or no?" {
			t.Fatalf("got %+v", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no notification for a terminal question")
	}
}

func TestNotifyResolveOnApprovalEnd(t *testing.T) {
	n := newTestNotifier(t, &NotifySettings{BatchSec: intPtr(0)})
	live, cancel := n.subscribe()
	defer cancel()
	next := func() ONotification {
		select {
		case no := <-live:
			return no
		case <-time.After(2 * time.Second):
			t.Fatal("no notification")
		}
		return ONotification{}
	}

	n.Emit(notifyEvent{Kind: "needs-you", Title: "q", Keys: []string{"ap_1"}})
	first := next()
	if first.Resolved || first.Read {
		t.Fatalf("new alert already resolved: %+v", first)
	}
	n.Resolve("ap_1")
	if got := next(); got.ID != first.ID || !got.Resolved || !got.Read {
		t.Fatalf("resolution %+v", got)
	}
	if n.unread() != 0 {
		t.Fatalf("unread %d after resolve", n.unread())
	}

	// Answered before the alert was stored: it is stored already resolved.
	n.Resolve("ap_2")
	n.Emit(notifyEvent{Kind: "needs-you", Title: "fast", Keys: []string{"ap_2"}})
	if got := next(); !got.Resolved || !got.Read {
		t.Fatalf("early answer %+v", got)
	}
}

func TestNotifyResolveDropsHeldEvent(t *testing.T) {
	n := newTestNotifier(t, &NotifySettings{BatchSec: intPtr(3)})
	n.unit = 50 * time.Millisecond
	live, cancel := n.subscribe()
	defer cancel()
	n.Emit(notifyEvent{Kind: "needs-you", Title: "a", Keys: []string{"ap_a"}})
	<-live
	n.Emit(notifyEvent{Kind: "needs-you", Title: "b", Keys: []string{"ap_b"}})
	n.Resolve("ap_b")
	select {
	case got := <-live:
		t.Fatalf("held alert still sent: %+v", got)
	case <-time.After(400 * time.Millisecond):
	}
}

func recvNote(t *testing.T, ch <-chan ONotification) ONotification {
	t.Helper()
	select {
	case n := <-ch:
		return n
	case <-time.After(15 * time.Second):
		t.Fatal("no notification")
	}
	return ONotification{}
}
