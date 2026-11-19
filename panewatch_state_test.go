package main

import "testing"

func TestPaneScreenLeavesFinishedTurnAlone(t *testing.T) {
	s := NewStore()
	prompt := "Do you want to proceed?\n❯ 1. Yes\n  2. No\n\nEsc to cancel\n"
	idle := "✻ Brewed for 29s\n────────────────────\n❯ \n────────────────────\n"
	up := func(st State) *Session {
		return s.Upsert(UpsertInput{Tool: ToolClaude, SessionID: "a", State: st, HasState: true})
	}
	// Turn ended, no prompt: stays awaiting-input (it went to running before).
	applyPaneScreen(s, up(StateAwaitingInput), idle)
	if got := s.Get("claude:a").State; got != StateAwaitingInput {
		t.Fatalf("finished turn: %s", got)
	}
	// A prompt on screen is the prompt state, whatever came before.
	applyPaneScreen(s, s.Get("claude:a"), prompt)
	if got := s.Get("claude:a").State; got != StateAwaitingPermission {
		t.Fatalf("prompt: %s", got)
	}
	// The prompt is gone: the agent goes on.
	applyPaneScreen(s, s.Get("claude:a"), idle)
	if got := s.Get("claude:a").State; got != StateRunning {
		t.Fatalf("answered: %s", got)
	}
}

func TestSubagentStopKeepsParentState(t *testing.T) {
	s := NewStore()
	s.Upsert(UpsertInput{Tool: ToolClaude, SessionID: "p", State: StateRunning, HasState: true})
	handleHook(s, ToolClaude, ClaudeHookPayload{SessionID: "p", Cwd: "/w", HookEventName: "SubagentStop"})
	if got := s.Get("claude:p").State; got != StateRunning {
		t.Fatalf("parent after SubagentStop: %s", got)
	}
	handleHook(s, ToolClaude, ClaudeHookPayload{SessionID: "p", Cwd: "/w", HookEventName: "Stop"})
	if got := s.Get("claude:p").State; got != StateAwaitingInput {
		t.Fatalf("after Stop: %s", got)
	}
}

// Output that only looks like a prompt ("(y/n)" in a log) is not a prompt: no card, no state change.
func TestPaneScreenIgnoresPromptLookalikes(t *testing.T) {
	s := NewStore()
	sess := s.Upsert(UpsertInput{Tool: ToolCodex, SessionID: "c1", State: StateRunning, HasState: true})
	applyPaneScreen(s, sess, "• Ran ./install.sh\n  └ Overwrite config? (y/n) y\n    done\n")
	if got := s.Get("codex:c1").State; got != StateRunning {
		t.Fatalf("lookalike: %s", got)
	}
}
