package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeBigClaude writes a synthetic Claude transcript of about size bytes.
func writeBigClaude(b *testing.B, path string, size int) int {
	f, err := os.Create(path)
	if err != nil {
		b.Fatal(err)
	}
	defer f.Close()
	out := strings.Repeat("output line of a tool result\n", 150)
	n, turns := 0, 0
	w := func(v any) {
		x, _ := json.Marshal(v)
		x = append(x, '\n')
		n += len(x)
		_, _ = f.Write(x)
	}
	for sec := 0; n < size; sec += 30 {
		turns++
		w(cUser(sec, fmt.Sprintf("prompt %d: make the change and run the tests", turns)))
		for k := 0; k < 6; k++ {
			id := fmt.Sprintf("t%d_%d", turns, k)
			w(cAsst(sec+k, "m"+id, "tool_use", map[string]any{"type": "tool_use", "id": id, "name": "Bash", "input": map[string]any{"command": "go test ./..."}}))
			w(cResult(sec+k, id, out, false))
		}
		w(cAsst(sec+20, fmt.Sprintf("e%d", turns), "end_turn", map[string]any{"type": "text", "text": "Done. " + strings.Repeat("words ", 80)}))
	}
	return turns
}

func writeBigCodex(b *testing.B, path string, size int) {
	f, err := os.Create(path)
	if err != nil {
		b.Fatal(err)
	}
	defer f.Close()
	out := strings.Repeat("output line of a command\n", 150)
	n := 0
	w := func(v any) {
		x, _ := json.Marshal(v)
		x = append(x, '\n')
		n += len(x)
		_, _ = f.Write(x)
	}
	for sec, turn := 0, 0; n < size; sec += 30 {
		turn++
		text := []any{map[string]any{"type": "text", "text": fmt.Sprintf("prompt %d", turn)}}
		w(codexResp(sec, map[string]any{"type": "message", "role": "user", "content": text}))
		w(codexDone(sec, map[string]any{"type": "UserMessage", "id": fmt.Sprintf("u%d", turn), "content": text}, 0))
		for k := 0; k < 6; k++ {
			w(codexDone(sec+k, map[string]any{"type": "CommandExecution", "id": fmt.Sprintf("c%d_%d", turn, k), "command": []any{"/bin/zsh", "-lc", "go test"},
				"status": "completed", "aggregated_output": out, "exit_code": 0, "duration": map[string]any{"secs": 1}}, 0))
		}
		w(codexEv(sec+20, map[string]any{"type": "task_complete"}))
	}
}

func benchCold(b *testing.B, ref watchedRef) {
	st, _ := os.Stat(ref.Path)
	b.SetBytes(st.Size())
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		watchedCache.mu.Lock()
		delete(watchedCache.m, ref.ID)
		watchedCache.mu.Unlock()
		res, err := watchedItemsFor(ref, -1, -1)
		if err != nil || len(res.Runs) != watchedPageTurns {
			b.Fatalf("runs %d err %v", len(res.Runs), err)
		}
	}
}

func BenchmarkWatchedItemsClaudeCold20MB(b *testing.B) {
	path := filepath.Join(b.TempDir(), "big.jsonl")
	writeBigClaude(b, path, 20<<20)
	benchCold(b, watchedRef{ID: "claude:bench-cold", Tool: ToolClaude, Path: path, Resolved: true})
}

func BenchmarkWatchedItemsCodexCold20MB(b *testing.B) {
	path := filepath.Join(b.TempDir(), "rollout-big.jsonl")
	writeBigCodex(b, path, 20<<20)
	benchCold(b, watchedRef{ID: "codex:bench-cold", Tool: ToolCodex, Path: path, Resolved: true})
}

// Each iteration appends one tool call and its result, then polls with the last cursor.
func BenchmarkWatchedItemsClaudePoll20MB(b *testing.B) {
	path := filepath.Join(b.TempDir(), "big.jsonl")
	writeBigClaude(b, path, 20<<20)
	ref := watchedRef{ID: "claude:bench-poll", Tool: ToolClaude, Path: path, Resolved: true}
	res, err := watchedItemsFor(ref, -1, -1)
	if err != nil {
		b.Fatal(err)
	}
	cursor := res.Cursor
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		b.Fatal(err)
	}
	defer f.Close()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		id := fmt.Sprintf("p%d", i)
		x, _ := json.Marshal(cAsst(i, "mp"+id, "tool_use", map[string]any{"type": "tool_use", "id": id, "name": "Bash", "input": map[string]any{"command": "ls"}}))
		y, _ := json.Marshal(cResult(i, id, "a\nb", false))
		_, _ = f.Write(append(append(append(x, '\n'), y...), '\n'))
		res, err := watchedItemsFor(ref, cursor, -1)
		if err != nil || len(res.Items) != 1 {
			b.Fatalf("items %d err %v", len(res.Items), err)
		}
		cursor = res.Cursor
	}
}
