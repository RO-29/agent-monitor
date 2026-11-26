package main

import (
	"path/filepath"
	"testing"
)

func TestSessionTitleOverride(t *testing.T) {
	s := NewStore()
	path := filepath.Join(t.TempDir(), "titles.json")
	s.LoadTitles(path)
	s.Upsert(UpsertInput{Tool: "cursor", SessionID: "a", Title: "Goal: same", HasTitle: true})

	if _, err := s.SetTitle("cursor:a", cleanTitle("  Disk report\nsecond line")); err != nil {
		t.Fatal(err)
	}
	if got := s.Get("cursor:a").Title; got != "Disk report" {
		t.Fatalf("title = %q", got)
	}
	s.Upsert(UpsertInput{Tool: "cursor", SessionID: "a", Title: "Goal: newer", HasTitle: true})
	if got := s.Get("cursor:a").Title; got != "Disk report" {
		t.Fatalf("agent update replaced the user title: %q", got)
	}

	again := NewStore()
	again.Upsert(UpsertInput{Tool: "cursor", SessionID: "a", Title: "Goal: same", HasTitle: true})
	again.LoadTitles(path)
	if got := again.Get("cursor:a").Title; got != "Disk report" {
		t.Fatalf("title after reload = %q", got)
	}

	if _, err := s.SetTitle("cursor:a", ""); err != nil {
		t.Fatal(err)
	}
	if got := s.Get("cursor:a").Title; got != "Goal: newer" {
		t.Fatalf("reset title = %q, want the agent's latest", got)
	}
	if _, err := s.SetTitle("cursor:missing", "x"); err == nil {
		t.Fatal("rename of a missing session passed")
	}
}
