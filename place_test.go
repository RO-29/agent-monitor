package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestJourneyOfReadsChangesIncrementally(t *testing.T) {
	p := filepath.Join(t.TempDir(), "s.jsonl")
	w := func(s string) {
		f, _ := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		f.WriteString(s)
		f.Close()
	}
	w(`{"type":"user","entrypoint":"claude-desktop","timestamp":"2026-11-19T09:00:00Z"}` + "\n" +
		`{"type":"assistant","entrypoint":"claude-desktop","timestamp":"2026-11-19T09:00:05Z"}` + "\n")
	if j := journeyOf(p); len(j) != 1 || j[0].Entry != "claude-desktop" || j[0].Label != "Claude desktop app" || j[0].At == 0 {
		t.Fatalf("first read %+v", j)
	}
	w(`{"type":"user","entrypoint":"cli","timestamp":"2026-11-19T10:00:00Z"}` + "\n" + `{"type":"user","entrypoint":"cli"`)
	j := journeyOf(p)
	if len(j) != 2 || j[1].Entry != "cli" || j[1].At <= j[0].At {
		t.Fatalf("second read %+v", j)
	}
}

func TestStorePlacesFollowEntrypoints(t *testing.T) {
	s := NewStore()
	for _, e := range []string{"sdk-cli", "sdk-cli", "cli", "claude-desktop", "claude-desktop"} {
		s.Upsert(UpsertInput{Tool: ToolClaude, SessionID: "p1", Entrypoint: e, HasEntrypoint: true})
	}
	got := s.Get("claude:p1")
	if got.Entrypoint != "claude-desktop" || len(got.Places) != 3 || got.Places[0] != "sdk-cli" || got.Places[2] != "claude-desktop" {
		t.Fatalf("entrypoint %q places %v", got.Entrypoint, got.Places)
	}
	s.Upsert(UpsertInput{Tool: ToolClaude, SessionID: "p1"})
	if g := s.Get("claude:p1"); g.Entrypoint != "claude-desktop" || len(g.Places) != 3 {
		t.Fatalf("an update without entrypoint kept %q %v", g.Entrypoint, g.Places)
	}
}

func TestPlaceNowDesktopAndEnded(t *testing.T) {
	prev := store
	store = NewStore()
	t.Cleanup(func() { store = prev })
	store.Upsert(UpsertInput{Tool: ToolClaude, SessionID: "d1", Entrypoint: "claude-desktop", HasEntrypoint: true, State: StateRunning, HasState: true})
	if k, _, _ := placeNow("claude:d1", store.Get("claude:d1")); k != "desktop" {
		t.Fatalf("live desktop session: %s", k)
	}
	store.Upsert(UpsertInput{Tool: ToolClaude, SessionID: "d2", Entrypoint: "cli", HasEntrypoint: true, State: StateCompleted, HasState: true})
	if k, _, d := placeNow("claude:d2", store.Get("claude:d2")); k != "ended" || d != "last in Terminal" {
		t.Fatalf("ended: %s %q", k, d)
	}
}
