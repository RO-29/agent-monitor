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

func TestPaneScreenShowsCursorAndOpenCodePrompts(t *testing.T) {
	s := NewStore()
	for _, c := range []struct {
		tool   Tool
		prompt string
	}{
		{ToolCursorAgent, "  $ rm -rf build\n\n  Run this command?\n  → Run (once) (y)\n    Skip (esc or n)\n"},
		{ToolOpencode, "△ Permission required\n  bash: make test\n  Allow once   Allow always   Reject\n"},
	} {
		sess := s.Upsert(UpsertInput{Tool: c.tool, SessionID: "x", State: StateRunning, HasState: true})
		applyPaneScreen(s, sess, c.prompt)
		got := s.Get(sess.ID)
		if got.State != StateAwaitingPermission || got.PermissionMessage == "" && got.LastMessage == "" {
			t.Fatalf("%s prompt: %s %+v", c.tool, got.State, got)
		}
		applyPaneScreen(s, got, "  done.\n> \n")
		if got := s.Get(sess.ID).State; got != StateRunning {
			t.Fatalf("%s answered: %s", c.tool, got)
		}
	}
}

func TestPaneActivityReadsTheWorkingLine(t *testing.T) {
	for in, want := range map[string]string{
		"✻ Thinking… (12s · ↓ 300 tokens · esc to interrupt)\n\n────\n❯ \n────\n": "Thinking… (12s · ↓ 300 tokens)",
		"• Working (8s • esc to interrupt)\n› \n":                                 "Working (8s)",
		"✻ Brewed for 29s\n────\n❯ \n────\n":                                      "",
	} {
		if got := paneActivity(in); got != want {
			t.Errorf("paneActivity(%q) = %q, want %q", in, got, want)
		}
	}
}
