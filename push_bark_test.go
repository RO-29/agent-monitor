package main

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type barkFake struct {
	mu   sync.Mutex
	got  []map[string]any
	code int
}

func setupBark(t *testing.T) (*barkFake, *httptest.Server) {
	t.Setenv("AGENT_MONITOR_HOME", t.TempDir())
	f := &barkFake{code: 200}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var m map[string]any
		_ = json.NewDecoder(r.Body).Decode(&m)
		m["_path"] = r.URL.Path
		f.mu.Lock()
		f.got = append(f.got, m)
		code := f.code
		f.mu.Unlock()
		w.WriteHeader(code)
		_, _ = w.Write([]byte(`{"code":` + map[bool]string{true: "200", false: "400"}[code == 200] + `,"message":"x"}`))
	}))
	t.Cleanup(srv.Close)
	bark.mu.Lock()
	bark.cfg, bark.loaded, bark.phoneActive = BarkConfig{}, false, time.Time{}
	bark.mu.Unlock()
	return f, srv
}

func barkOpen(t *testing.T, key string, m map[string]any) map[string]string {
	t.Helper()
	ct, _ := base64.StdEncoding.DecodeString(m["ciphertext"].(string))
	block, _ := aes.NewCipher([]byte(key))
	plain := make([]byte, len(ct))
	cipher.NewCBCDecrypter(block, []byte(m["iv"].(string))).CryptBlocks(plain, ct)
	plain = plain[:len(plain)-int(plain[len(plain)-1])]
	var out map[string]string
	if err := json.Unmarshal(plain, &out); err != nil {
		t.Fatalf("open: %v %q", err, plain)
	}
	return out
}

// The example in Bark's encryption docs (AES-128-CBC, openssl) must come out the same.
func TestBarkSealMatchesBarkDocs(t *testing.T) {
	got, err := barkSealBytes("1234567890123456", "1111111111111111", []byte(`{"body": "test", "sound": "birdsong"}`))
	if err != nil || got != "d3QhjQjP5majvNt5CjsvFWwqqj2gKl96RFj5OO+u6ynTt7lkyigDYNA3abnnCLpr" {
		t.Fatalf("%v %q", err, got)
	}
}

func TestBarkPushEncryptsAndRoutes(t *testing.T) {
	f, srv := setupBark(t)
	mux := http.NewServeMux()
	registerPushRoutes(mux)
	do := func(method, path, body string) (int, map[string]any) {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(method, path, strings.NewReader(body)))
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}
	// A Bark URL pasted whole gives the device key and the server.
	code, v := do("POST", "/api/push/bark", `{"enabled":true,"deviceKey":"https://api.day.app/AbCdEfGh12345678/"}`)
	if code != 200 || v["deviceKeySet"] != true || v["deviceKeyHint"] != "…5678" || v["server"] != "https://api.day.app" || len(v["key"].(string)) != 32 || len(v["iv"].(string)) != 16 {
		t.Fatalf("set: %d %v", code, v)
	}
	if _, ok := v["deviceKey"]; ok {
		t.Fatal("the view shows the whole device key")
	}
	if code, _ := do("POST", "/api/push/bark", `{"deviceKey":"bad key!"}`); code != 400 {
		t.Fatalf("bad key: %d", code)
	}
	if code, _ := do("POST", "/api/push/bark", `{"deviceKey":"http://api.day.app/AbCdEfGh12345678/"}`); code != 400 {
		t.Fatalf("http URL: %d", code)
	}
	bark.mu.Lock()
	bark.cfg.Server = srv.URL // the fake server; real ones must be https
	bark.mu.Unlock()
	key := v["key"].(string)

	no := ONotification{ID: "42", Kind: "needs-you", Title: "Approve Bash", Body: "rm -rf build", ThreadID: "ot_0123456789abcdef"}
	if err := barkSend(no, false); err != nil {
		t.Fatal(err)
	}
	m := f.got[0]
	if m["_path"] != "/push" || m["device_key"] != "AbCdEfGh12345678" || m["id"] != "am-42" || m["title"] != nil || m["body"] != nil {
		t.Fatalf("request: %v", m)
	}
	p := barkOpen(t, key, m)
	if p["title"] != "Approve Bash" || p["body"] != "rm -rf build" || p["url"] != "agentmonitor://thread/ot_0123456789abcdef" || p["level"] != "timeSensitive" {
		t.Fatalf("payload: %v", p)
	}

	// The iPhone app in the foreground alerts by itself; quiet and resolved alerts never push.
	do("POST", "/api/push/presence", `{"active":true}`)
	_ = barkSend(no, false)
	no.Quiet = true
	do("POST", "/api/push/presence", `{"active":false}`)
	_ = barkSend(no, false)
	if len(f.got) != 1 {
		t.Fatalf("pushed while open or quiet: %d", len(f.got))
	}
	// The worker sends, then deletes in order; a delete for an alert never sent goes nowhere.
	no.Quiet = false
	cur := func(string) (ONotification, bool) { return no, true }
	barkQueue(no, cur)
	barkDelete("42")
	barkDelete("never-sent")
	waitFor(t, func() bool { f.mu.Lock(); defer f.mu.Unlock(); return len(f.got) == 3 })
	time.Sleep(50 * time.Millisecond)
	f.mu.Lock()
	if len(f.got) != 3 || f.got[1]["ciphertext"] == nil || f.got[2]["id"] != "am-42" || f.got[2]["delete"] != "1" || f.got[2]["ciphertext"] != nil {
		t.Fatalf("send then delete: %v", f.got)
	}
	f.mu.Unlock()
	if code, out := do("POST", "/api/push/bark/test", ``); code != 200 || len(f.got) != 4 {
		t.Fatalf("test: %d %v", code, out)
	}
	f.code = 400
	if code, out := do("POST", "/api/push/bark/test", ``); code != 502 || !strings.Contains(out["error"].(string), "400") {
		t.Fatalf("test error: %d %v", code, out)
	}
	// A new key replaces both values; the device key stays.
	_, v2 := do("POST", "/api/push/bark", `{"newKey":true}`)
	if v2["key"] == key || v2["deviceKeySet"] != true {
		t.Fatalf("new key: %v", v2)
	}
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if ok() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out")
}

func TestBarkLinkOpensInstallPage(t *testing.T) {
	if got := barkLink(ONotification{Kind: "finished", URL: "https://mac.ts.net/ios-ota/x/"}); got != "https://mac.ts.net/ios-ota/x/" {
		t.Fatalf("install link: %s", got)
	}
	if got := barkLink(ONotification{Kind: "spend", URL: "/settings/limits"}); got != "agentmonitor://alerts" {
		t.Fatalf("a relative url stays in the app: %s", got)
	}
}
