package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func voiceServer(t *testing.T, script string) *httptest.Server {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "clean.sh")
	if script != "" {
		if err := os.WriteFile(path, []byte(script), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("AM_VOICE_CLEAN", path)
	t.Setenv("AM_VOICE_CONF", filepath.Join(dir, "dictate.conf"))
	mux := http.NewServeMux()
	registerVoiceRoutes(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func voicePost(t *testing.T, srv *httptest.Server, body string) (int, voiceCleanResp) {
	t.Helper()
	res, err := http.Post(srv.URL+"/api/voice/clean", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var out voiceCleanResp
	_ = json.NewDecoder(res.Body).Decode(&out)
	return res.StatusCode, out
}

func voiceBody(text, mode string) string {
	b, _ := json.Marshal(map[string]string{"text": text, "mode": mode})
	return string(b)
}

func TestVoiceCleanPipesStdin(t *testing.T) {
	srv := voiceServer(t, "#!/bin/bash\nin=\"$(cat)\"\nprintf 'clean[%s|%s]' \"$in\" \"$DICTATE_MODE\"\n")
	// Shell metacharacters reach the script as stdin data only.
	raw := "um hello $(touch /tmp/x) `id` ; world"
	code, out := voicePost(t, srv, voiceBody(raw, "prompt"))
	if code != 200 || !out.Cleaned || out.Text != "clean["+raw+"|prompt]" || out.Error != "" {
		t.Fatalf("%d %+v", code, out)
	}
}

func TestVoiceCleanFailuresReturnRaw(t *testing.T) {
	for name, script := range map[string]string{
		"exit1":   "#!/bin/bash\necho boom >&2\nexit 3\n",
		"empty":   "#!/bin/bash\ncat >/dev/null\n",
		"missing": "",
	} {
		t.Run(name, func(t *testing.T) {
			srv := voiceServer(t, script)
			code, out := voicePost(t, srv, voiceBody("raw words", ""))
			if code != 200 || out.Cleaned || out.Text != "raw words" || out.Error == "" {
				t.Fatalf("%d %+v", code, out)
			}
			if name == "exit1" && !strings.Contains(out.Error, "boom") {
				t.Fatalf("stderr not in error: %q", out.Error)
			}
		})
	}
}

func TestVoiceCleanTimeoutKillsChildren(t *testing.T) {
	old := voiceTimeout
	voiceTimeout = time.Second
	t.Cleanup(func() { voiceTimeout = old })
	srv := voiceServer(t, "#!/bin/bash\ncat >/dev/null\nsleep 60 &\nwait\n")
	start := time.Now()
	code, out := voicePost(t, srv, voiceBody("slow", ""))
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("took %v", d)
	}
	if code != 200 || out.Cleaned || out.Text != "slow" || !strings.Contains(out.Error, "timed out") {
		t.Fatalf("%d %+v", code, out)
	}
}

func TestVoiceCleanLimits(t *testing.T) {
	srv := voiceServer(t, "#!/bin/bash\ncat\n")
	if code, _ := voicePost(t, srv, voiceBody(strings.Repeat("a", voiceMaxInput+1), "")); code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize: %d", code)
	}
	if code, out := voicePost(t, srv, voiceBody(strings.Repeat("a", voiceMaxInput), "")); code != 200 || !out.Cleaned {
		t.Fatalf("8 KB exactly: %d %+v", code, out)
	}
	if code, _ := voicePost(t, srv, voiceBody("x", "bad mode;rm")); code != 400 {
		t.Fatalf("bad mode: %d", code)
	}
	if code, _ := voicePost(t, srv, "{"); code != 400 {
		t.Fatalf("bad json: %d", code)
	}
}

func TestVoiceCleanConcurrencyCap(t *testing.T) {
	dir := t.TempDir()
	// Each run records itself while it holds a slot; the third request must wait.
	script := "#!/bin/bash\ncat >/dev/null\nn=$(ls " + dir + " | wc -l)\ntouch " + dir + "/$$\necho \"$n\"\nsleep 0.5\nrm " + dir + "/$$\n"
	srv := voiceServer(t, script)
	var wg sync.WaitGroup
	var mu sync.Mutex
	maxSeen := 0
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, out := voicePost(t, srv, voiceBody("x", ""))
			mu.Lock()
			defer mu.Unlock()
			if n, err := strconv.Atoi(strings.TrimSpace(out.Text)); out.Cleaned && err == nil {
				maxSeen = max(maxSeen, n+1)
			}
		}()
	}
	wg.Wait()
	if maxSeen > 2 || maxSeen == 0 {
		t.Fatalf("concurrent runs %d", maxSeen)
	}
}

func TestVoiceStatus(t *testing.T) {
	srv := voiceServer(t, "#!/bin/bash\ncat\n")
	conf := os.Getenv("AM_VOICE_CONF")
	get := func() map[string]any {
		res, err := http.Get(srv.URL + "/api/voice/status")
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var m map[string]any
		_ = json.NewDecoder(res.Body).Decode(&m)
		return m
	}
	if m := get(); m["available"] != true || m["engine"] != "claude" {
		t.Fatalf("default %v", m)
	}
	if err := os.WriteFile(conf, []byte("engine=claude\ncodex_model=x\nengine=codex\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if m := get(); m["engine"] != "codex" || len(m) != 2 {
		t.Fatalf("conf %v", m)
	}
	t.Setenv("AM_VOICE_CLEAN", filepath.Join(t.TempDir(), "none.sh"))
	if m := get(); m["available"] != false {
		t.Fatalf("missing %v", m)
	}
}
