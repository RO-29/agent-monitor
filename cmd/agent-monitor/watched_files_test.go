package main

import (
	"path/filepath"
	"slices"
	"testing"
)

func TestWatchedSessionFilesReadsTurnsBeyondThePage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "files.jsonl")
	recs := []any{
		cUser(1, "Fix the build"),
		cAsst(2, "m1", "tool_use", map[string]any{"type": "tool_use", "id": "t1", "name": "Edit", "input": map[string]any{"file_path": "/w/a.go", "old_string": "a", "new_string": "b"}}),
		cResult(3, "t1", "ok", false),
		cAsst(4, "m2", "tool_use", map[string]any{"type": "tool_use", "id": "t2", "name": "Write", "input": map[string]any{"file_path": "/w/new.go", "content": "x"}}),
		cResult(5, "t2", "ok", false),
		cAsst(6, "m3", "end_turn", map[string]any{"type": "text", "text": "Done."}),
	}
	for i := range watchedPageTurns + 3 {
		recs = append(recs, cUser(10+i*2, "next"), cAsst(11+i*2, "n", "end_turn", map[string]any{"type": "text", "text": "ok"}))
	}
	jsonl(t, path, recs...)
	ref := claudeRef(path)
	page, err := watchedItemsFor(ref, -1, -1)
	if err != nil {
		t.Fatal(err)
	}
	if len(editedPaths(page.Items)) != 0 || !page.More {
		t.Fatal("the fixture must put the edits outside the latest page")
	}
	got, err := watchedSessionFiles(ref)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, []string{"/w/a.go", "/w/new.go"}) {
		t.Fatalf("paths %v", got)
	}
}
