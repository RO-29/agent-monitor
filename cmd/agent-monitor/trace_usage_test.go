package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func jsonl(t *testing.T, path string, recs ...any) {
	t.Helper()
	var b strings.Builder
	for _, r := range recs {
		x, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		b.Write(x)
		b.WriteByte('\n')
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}

func tsAt(sec int) string {
	return time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC).Add(time.Duration(sec) * time.Second).Format(time.RFC3339Nano)
}

func TestClaudeTurnUsageCountsEachMessageOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	usage := map[string]any{"input_tokens": 10, "output_tokens": 100, "cache_read_input_tokens": 1000, "cache_creation_input_tokens": 0}
	asst := func(sec int, part any, u map[string]any) map[string]any {
		return map[string]any{"type": "assistant", "timestamp": tsAt(sec), "message": map[string]any{
			"id": "msg_1", "role": "assistant", "model": "claude-sonnet-5", "content": []any{part}, "usage": u}}
	}
	jsonl(t, path,
		map[string]any{"type": "user", "timestamp": tsAt(0), "message": map[string]any{"role": "user", "content": "hello"}},
		asst(1, map[string]any{"type": "thinking", "thinking": "x"}, usage),
		asst(2, map[string]any{"type": "text", "text": "hi"}, usage),
		asst(3, map[string]any{"type": "tool_use", "id": "t1", "name": "Bash", "input": map[string]any{"command": "ls"}}, usage),
	)
	tr := buildClaudeTrace(&Session{ID: "claude:s", SessionID: "s", Tool: ToolClaude, TranscriptPath: path})
	var turn *Span
	for i := range tr.Spans {
		if tr.Spans[i].Kind == "turn" {
			turn = &tr.Spans[i]
		}
	}
	if turn == nil || turn.Tokens == nil {
		t.Fatal("no turn tokens")
	}
	if turn.Tokens.Output != 100 || turn.Tokens.CacheRead != 1000 || turn.Tokens.Input != 10 {
		t.Fatalf("usage counted per line, not per message: %+v", *turn.Tokens)
	}
	if want := estimateCost("claude-sonnet-5", *turn.Tokens); turn.Usd != want || want == 0 {
		t.Fatalf("turn usd %v, want %v", turn.Usd, want)
	}
}

func TestCodexTurnUsageFromTotals(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout.jsonl")
	tok := func(sec, totIn, totCached, totOut, lastIn, lastCached, lastOut int) map[string]any {
		return map[string]any{"type": "event_msg", "timestamp": tsAt(sec), "payload": map[string]any{"type": "token_count", "info": map[string]any{
			"total_token_usage": map[string]any{"input_tokens": totIn, "cached_input_tokens": totCached, "output_tokens": totOut},
			"last_token_usage":  map[string]any{"input_tokens": lastIn, "cached_input_tokens": lastCached, "output_tokens": lastOut}}}}
	}
	jsonl(t, path,
		map[string]any{"type": "turn_context", "timestamp": tsAt(0), "payload": map[string]any{"model": "gpt-6"}},
		map[string]any{"type": "event_msg", "timestamp": tsAt(0), "payload": map[string]any{"type": "user_message", "message": "go"}},
		tok(1, 1000, 800, 50, 1000, 800, 50),
		tok(2, 1000, 800, 50, 1000, 800, 50), // repeated event
		tok(3, 2500, 2000, 80, 1500, 1200, 30),
		map[string]any{"type": "event_msg", "timestamp": tsAt(4), "payload": map[string]any{"type": "task_complete"}},
	)
	tr := buildCodexTrace(&Session{ID: "codex:c", SessionID: "c", Tool: ToolCodex, TranscriptPath: path})
	var turn *Span
	for i := range tr.Spans {
		if tr.Spans[i].Kind == "turn" {
			turn = &tr.Spans[i]
		}
	}
	if turn == nil || turn.Tokens == nil {
		t.Fatal("no turn tokens")
	}
	if got := *turn.Tokens; got.Input != 500 || got.CacheRead != 2000 || got.Output != 80 {
		t.Fatalf("codex turn tokens %+v", got)
	}
	if turn.Usd <= 0 {
		t.Fatal("no turn cost")
	}
}
