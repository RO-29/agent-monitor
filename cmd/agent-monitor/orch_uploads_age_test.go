package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestUploadAgeSettingAndPrune(t *testing.T) {
	e := newOrchEnv(t)
	srv := e.server()
	prev := getSettings()
	t.Cleanup(func() { saveSettings(prev) })
	_, a := uploadFile(t, srv.URL, "old.png", onePixelPNG, "")
	_, b := uploadFile(t, srv.URL, "new.png", onePixelPNG, "")
	old := time.Now().Add(-3 * time.Hour)
	if err := os.Chtimes(a["path"].(string), old, old); err != nil {
		t.Fatal(err)
	}
	stats := func(method, path, body string) UploadStats {
		t.Helper()
		resp, err := http.Post(srv.URL+path, "application/json", strings.NewReader(body))
		if method == "GET" {
			resp, err = http.Get(srv.URL + path)
		}
		if err != nil || resp.StatusCode != 200 {
			t.Fatalf("%s %s: %v %v", method, path, err, resp)
		}
		defer resp.Body.Close()
		var st UploadStats
		_ = json.NewDecoder(resp.Body).Decode(&st)
		return st
	}
	if st := stats("GET", "/api/orch/uploads/stats", ""); st.Files != 2 || st.Deleted != 0 {
		t.Fatalf("stats: %+v", st)
	}
	// The 30-day default keeps a 3-hour-old file; a 2-hour setting deletes it.
	if st := stats("POST", "/api/orch/uploads/prune", ""); st.Files != 2 {
		t.Fatalf("default prune: %+v", st)
	}
	s := getSettings()
	s.UploadMaxAgeHours = 2
	saveSettings(s)
	if st := stats("POST", "/api/orch/uploads/prune", ""); st.Files != 1 || st.Deleted != 1 || st.MaxAgeHours != 2 {
		t.Fatalf("2h prune: %+v", st)
	}
	if _, err := os.Stat(b["path"].(string)); err != nil {
		t.Fatalf("new file gone: %v", err)
	}
	s.UploadMaxAgeHours = -1
	saveSettings(s)
	if uploadMaxAge() != 0 {
		t.Fatal("keep setting still has an age")
	}
	// Delete all keeps a file from the last 2 minutes, which a message may be sending now.
	if st := stats("POST", "/api/orch/uploads/prune", `{"all":true}`); st.Files != 1 || st.Deleted != 0 {
		t.Fatalf("all, fresh: %+v", st)
	}
	five := time.Now().Add(-5 * time.Minute)
	_ = os.Chtimes(b["path"].(string), five, five)
	if st := stats("POST", "/api/orch/uploads/prune", `{"all":true}`); st.Files != 0 || st.Deleted != 1 {
		t.Fatalf("all: %+v", st)
	}
	if left, _ := filepath.Glob(filepath.Join(e.dir, "uploads", "*", "*")); len(left) != 0 {
		t.Fatalf("files left: %v", left)
	}
}
