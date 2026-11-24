package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCursorTranscriptItems(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "agent-transcripts", "abc")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "abc.jsonl")
	body := `{"role":"user","message":{"content":[{"type":"text","text":"<timestamp>Wednesday, Nov 18, 2026, 11:55 AM (UTC+5:30)</timestamp>\n<user_query>\nfix the bug\n</user_query>"}]}}
{"role":"assistant","message":{"content":[{"type":"text","text":"<think>look first</think>Reading it."},{"type":"tool_use","name":"Read","input":{"path":"/x/a.go"}},{"type":"tool_use","name":"StrReplace","input":{"path":"/x/a.go"}}]}}
{"role":"assistant","message":{"content":[{"type":"tool_use","name":"Shell","input":{"command":"go test ./..."}}]}}
{"type":"turn_ended","status":"success"}
{"role":"user","message":{"content":[{"type":"text","text":"context only"}]}}
`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if !isCursorTranscript(path) {
		t.Fatal("path not seen as a transcript")
	}
	items, runs, err := cursorTranscriptItems("cursor:abc", path)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].Prompt != "fix the bug" || runs[0].Status != "completed" || runs[0].StartedAt != 1794983100000 {
		t.Fatalf("runs %+v", runs[0])
	}
	want := []string{"user", "reasoning", "agent", "other", "fileChange", "command"}
	if len(items) != len(want) {
		t.Fatalf("got %d items, want %d", len(items), len(want))
	}
	for i, k := range want {
		if items[i].Kind != k || items[i].RunID != runs[0].ID || items[i].Status != "completed" {
			t.Fatalf("item %d = %+v, want kind %s", i, items[i], k)
		}
	}
	if items[5].Command != "go test ./..." {
		t.Fatalf("command %q", items[5].Command)
	}
}
