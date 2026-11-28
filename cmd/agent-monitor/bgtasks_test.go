package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRunningBgTracksStartAndFinish(t *testing.T) {
	p := filepath.Join(t.TempDir(), "s.jsonl")
	start := `{"type":"assistant","timestamp":"2026-11-28T10:00:00Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"tu_a","name":"Bash","input":{"command":"sleep 99","description":"Wait for build","run_in_background":true}},{"type":"tool_use","id":"tu_b","name":"Bash","input":{"command":"make","run_in_background":true}}]}}` + "\n"
	if err := os.WriteFile(p, []byte(start), 0o644); err != nil {
		t.Fatal(err)
	}
	got := runningBg(p)
	if len(got) != 2 || got[0].Label != "Wait for build" {
		t.Fatalf("after start: %+v", got)
	}
	f, _ := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0o644)
	f.WriteString(`{"type":"queue-operation","operation":"enqueue","content":"<task-notification> <task-id>x</task-id> <tool-use-id>tu_a</tool-use-id> <status>completed</status></task-notification>"}` + "\n")
	f.WriteString(`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu_b","is_error":true,"content":"denied"}]}}` + "\n")
	f.WriteString(`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"partial`)
	f.Close()
	if got := runningBg(p); len(got) != 0 {
		t.Fatalf("after finish: %+v", got)
	}
}

func TestRunningBgDropsStoppedTask(t *testing.T) {
	p := filepath.Join(t.TempDir(), "s.jsonl")
	lines := `{"type":"assistant","timestamp":"2026-11-28T10:00:00Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"tu_s","name":"Bash","input":{"command":"sleep 600","run_in_background":true}},{"type":"tool_use","id":"tu_g","name":"Agent","input":{"prompt":"x","description":"Review","run_in_background":true}}]}}
{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu_s","content":"Command running in background with ID: b123abc. Output is being written to: /tmp/x"},{"type":"tool_result","tool_use_id":"tu_g","content":[{"type":"text","text":"Async agent launched successfully. (note)\nagentId: a9f8e7 (internal ID)"}]}]}}
{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu_x","content":"{\"message\":\"Successfully stopped task: b123abc (sleep 600)\",\"task_id\":\"b123abc\"}"}]}}
`
	if err := os.WriteFile(p, []byte(lines), 0o644); err != nil {
		t.Fatal(err)
	}
	got := runningBg(p)
	if len(got) != 1 || got[0].ID != "tu_g" {
		t.Fatalf("after stop: %+v", got)
	}
	f, _ := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0o644)
	f.WriteString(`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu_y","content":"{\"message\":\"Successfully stopped task: a9f8e7\",\"task_id\":\"a9f8e7\"}"}]}}` + "\n")
	f.Close()
	if got := runningBg(p); len(got) != 0 {
		t.Fatalf("after agent stop: %+v", got)
	}
}
