package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// F4: a cross-site GET must not reach an /api/ handler or /ws; navigations to pages still work.
func TestReviewF4CrossSiteGetRefused(t *testing.T) {
	var hits atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/orch/git", func(w http.ResponseWriter, r *http.Request) { hits.Add(1) })
	mux.HandleFunc("GET /ws", func(w http.ResponseWriter, r *http.Request) { hits.Add(1) })
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("<html>")) })
	h := originGuard(authMiddleware(mux))
	for _, c := range []struct {
		path string
		hdr  map[string]string
		want int
	}{
		{"/api/orch/git?cwd=/tmp/x", map[string]string{"Sec-Fetch-Site": "cross-site", "Sec-Fetch-Mode": "no-cors", "Sec-Fetch-Dest": "image"}, 403},
		{"/api/orch/git?cwd=/tmp/x", map[string]string{"Sec-Fetch-Site": "same-site"}, 403},
		{"/ws", map[string]string{"Origin": "http://evil.example", "Sec-Fetch-Site": "cross-site"}, 403},
		{"/api/orch/git?cwd=/tmp/x", nil, 200},
		{"/api/orch/git?cwd=/tmp/x", map[string]string{"Sec-Fetch-Site": "same-origin"}, 200},
		{"/w", map[string]string{"Sec-Fetch-Site": "cross-site", "Sec-Fetch-Mode": "navigate"}, 200},
	} {
		before := hits.Load()
		w, _ := rvDo(h, "GET", c.path, "", "127.0.0.1:5", c.hdr)
		if w.Code != c.want {
			t.Errorf("%s %v: %d want %d", c.path, c.hdr, w.Code, c.want)
		}
		if c.want == 403 && hits.Load() != before {
			t.Errorf("%s %v reached the handler", c.path, c.hdr)
		}
	}
}

// F4: cwd must be a folder the daemon knows; a repo with a hostile core.fsmonitor never runs.
func TestReviewF4UnknownCwdRefused(t *testing.T) {
	e := newOrchEnv(t)
	srv := e.server()
	repo := gitRepo(t)
	marker := filepath.Join(t.TempDir(), "MARKER")
	if out, err := exec.Command("git", "-C", repo, "config", "core.fsmonitor", "touch "+marker+"; false").CombinedOutput(); err != nil {
		t.Fatal(string(out))
	}
	for _, p := range []string{"/api/orch/git?cwd=", "/api/orch/files?q=&cwd=", "/api/orch/git/diff?cwd="} {
		var out map[string]any
		if code := getJSONURL(t, srv.URL+p+url.QueryEscape(repo), &out); code != http.StatusForbidden {
			t.Errorf("%s unknown cwd: %d %v", p, code, out)
		}
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the repo's core.fsmonitor command ran")
	}
	allowCwds(t, repo)
	var hits []orchFileHit
	if code := getJSONURL(t, srv.URL+"/api/orch/files?q=&cwd="+url.QueryEscape(filepath.Join(repo)), &hits); code != 200 {
		t.Fatalf("known cwd refused: %d", code)
	}
}

func TestReviewF4TermUnknownCwdRefused(t *testing.T) {
	e := newTermEnv(t, time.Minute)
	other, _ := os.MkdirTemp("", "am-unknown-")
	defer os.RemoveAll(other)
	if _, code := e.dial("/api/term?cwd="+other, nil); code != http.StatusForbidden {
		t.Fatalf("terminal in unknown cwd: %d", code)
	}
}

func TestKnownCwd(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "sub", "deep"), 0o755)
	out := t.TempDir()
	os.Symlink(out, filepath.Join(root, "escape"))
	allowCwds(t, root, home)
	for p, want := range map[string]bool{
		root: true, filepath.Join(root, "sub", "deep"): true, root + "/sub/../sub": true,
		root + "x": false, out: false, filepath.Join(root, "escape"): false, "/": false, "relative": false, "": false,
		home: true, filepath.Join(home, "Downloads"): false,
	} {
		if got := knownCwd(p); got != want {
			t.Errorf("knownCwd(%q) = %v, want %v", p, got, want)
		}
	}
}

func rvDo(h http.Handler, method, path, body, addr string, hdr map[string]string) (*httptest.ResponseRecorder, *http.Request) {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.RemoteAddr = addr
	for k, v := range hdr {
		if k == "Host" {
			r.Host = v
		} else {
			r.Header.Set(k, v)
		}
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w, r
}
