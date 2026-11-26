package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadSessionSourcesSeedsStoreAndMarksLoaded(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("AGENT_MONITOR_HOME", t.TempDir())
	t.Cleanup(func() { initTraceTables(nil) })
	proj := filepath.Join(home, ".claude", "projects", "-tmp-r1")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	line := `{"type":"user","message":{"role":"user","content":"hello"},"timestamp":"2026-11-16T10:00:00Z","cwd":"/tmp/r1"}` + "\n"
	if err := os.WriteFile(filepath.Join(proj, "11111111-2222-3333-4444-555555555555.jsonl"), []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}
	sessionsLoaded.Store(false)
	s := NewStore()
	if flush := loadSessionSources(s); flush == nil {
		t.Fatal("no shutdown flush with a working sessions.db")
	}
	if !sessionsLoaded.Load() {
		t.Fatal("sessionsLoaded not set")
	}
	found := false
	for _, sess := range s.All() {
		found = found || sess.SessionID == "11111111-2222-3333-4444-555555555555"
	}
	if !found {
		t.Fatalf("transcript not seeded: %d sessions", len(s.All()))
	}
	if !pathExists(filepath.Join(os.Getenv("AGENT_MONITOR_HOME"), "sessions.db")) {
		t.Fatal("sessions.db not opened in the state dir")
	}
}
