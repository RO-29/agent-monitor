package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestTailLinesAfterStopKeepTurnFinished(t *testing.T) {
	s := NewStore()
	path := filepath.Join(t.TempDir(), "t.jsonl")
	if err := os.WriteFile(path, []byte(`{"type":"user","message":{"role":"user","content":"hi"}}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	const id = "tail-stop-1"
	ensureClaudeTail(s, path, id, "/w")
	t.Cleanup(func() { claudeTailMu.Lock(); claudeTails[id](); delete(claudeTails, id); claudeTailMu.Unlock() })
	handleHook(s, ToolClaude, ClaudeHookPayload{SessionID: id, Cwd: "/w", HookEventName: "Stop"})
	add := func(line string) {
		f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
		f.WriteString(line + "\n")
		f.Close()
	}
	add(`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Red"}]}}`)
	add(`{"type":"system","subtype":"turn_duration"}`)
	wait := func(want State) {
		t.Helper()
		for end := time.Now().Add(4 * time.Second); time.Now().Before(end); time.Sleep(100 * time.Millisecond) {
			if s.Get("claude:"+id).State == want {
				return
			}
		}
		t.Fatalf("state %s, want %s", s.Get("claude:"+id).State, want)
	}
	time.Sleep(2500 * time.Millisecond) // the tail polls each second
	wait(StateAwaitingInput)
	add(`{"type":"user","message":{"role":"user","content":"next question"}}`)
	wait(StateRunning)
}

// Claude writes cost-state after SessionEnd; an ended session must not look live again.
func TestTailLinesAfterSessionEndKeepCompleted(t *testing.T) {
	s := NewStore()
	path := filepath.Join(t.TempDir(), "t.jsonl")
	if err := os.WriteFile(path, []byte(`{"type":"user","message":{"role":"user","content":"hi"}}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	const id = "tail-end-1"
	ensureClaudeTail(s, path, id, "/w")
	t.Cleanup(func() { claudeTailMu.Lock(); claudeTails[id](); delete(claudeTails, id); claudeTailMu.Unlock() })
	handleHook(s, ToolClaude, ClaudeHookPayload{SessionID: id, Cwd: "/w", HookEventName: "UserPromptSubmit"})
	time.Sleep(1500 * time.Millisecond)
	handleHook(s, ToolClaude, ClaudeHookPayload{SessionID: id, Cwd: "/w", HookEventName: "SessionEnd", Reason: "other"})
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	f.WriteString(`{"type":"cost-state","totalCostUSD":0.01}` + "\n" + `{"type":"last-prompt"}` + "\n")
	f.Close()
	time.Sleep(2500 * time.Millisecond) // the tail polls each second
	if st := s.Get("claude:" + id).State; st != StateCompleted {
		t.Fatalf("state %s, want completed", st)
	}
}

// A ! command writes user lines and no Stop follows: the finished turn stays finished.
func TestTailBangRecordsKeepState(t *testing.T) {
	s := NewStore()
	path := filepath.Join(t.TempDir(), "t.jsonl")
	_ = os.WriteFile(path, []byte(`{"type":"user","message":{"role":"user","content":"hi"}}`+"\n"), 0o644)
	const id = "tail-bang-1"
	ensureClaudeTail(s, path, id, "/w")
	t.Cleanup(func() { claudeTailMu.Lock(); claudeTails[id](); delete(claudeTails, id); claudeTailMu.Unlock() })
	handleHook(s, ToolClaude, ClaudeHookPayload{SessionID: id, Cwd: "/w", HookEventName: "Stop"})
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	f.WriteString(`{"type":"user","message":{"role":"user","content":"<bash-input>ls</bash-input>"}}` + "\n")
	f.WriteString(`{"type":"user","message":{"role":"user","content":"<bash-stdout>a\nb</bash-stdout><bash-stderr></bash-stderr>"}}` + "\n")
	f.Close()
	time.Sleep(2500 * time.Millisecond)
	if got := s.Get("claude:" + id).State; got != StateAwaitingInput {
		t.Fatalf("after a ! command: %s", got)
	}
}

// A session seeded or decayed to completed while the agent still works comes back when content arrives.
func TestTailContentRevivesDecayedSession(t *testing.T) {
	s := NewStore()
	path := filepath.Join(t.TempDir(), "t.jsonl")
	if err := os.WriteFile(path, []byte(`{"type":"user","message":{"role":"user","content":"hi"}}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	const id = "tail-decay-1"
	ensureClaudeTail(s, path, id, "/w")
	t.Cleanup(func() { claudeTailMu.Lock(); claudeTails[id](); delete(claudeTails, id); claudeTailMu.Unlock() })
	handleHook(s, ToolClaude, ClaudeHookPayload{SessionID: id, Cwd: "/w", HookEventName: "UserPromptSubmit"})
	time.Sleep(1500 * time.Millisecond)
	s.SetState(ToolClaude, id, StateCompleted, "")
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	f.WriteString(`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"still working"}]}}` + "\n")
	f.Close()
	time.Sleep(2500 * time.Millisecond)
	if st := s.Get("claude:" + id).State; st != StateRunning {
		t.Fatalf("state %s, want running", st)
	}
}
