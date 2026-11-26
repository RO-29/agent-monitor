package main

import (
	"path/filepath"
	"testing"
	"time"
)

func captureTurnNotices(t *testing.T) *[]notifyEvent {
	got := &[]notifyEvent{}
	oldE, oldR := turnNotifyEmit, turnNotifyResolve
	turnNotifyEmit = func(ev notifyEvent) { *got = append(*got, ev) }
	turnNotifyResolve = func(k string) { *got = append(*got, notifyEvent{Kind: "resolved", Keys: []string{k}}) }
	turnWatch.seen, turnWatch.prompts, turnWatch.primed = map[string]*turnWatchState{}, nil, false
	t.Cleanup(func() {
		turnNotifyEmit, turnNotifyResolve = oldE, oldR
		turnWatch.seen, turnWatch.prompts, turnWatch.primed = map[string]*turnWatchState{}, nil, false
	})
	return got
}

func TestTurnNotifierAnnouncesEachNewTurnEndOnce(t *testing.T) {
	got := captureTurnNotices(t)
	path := filepath.Join(t.TempDir(), "s1.jsonl")
	jsonl(t, path, cUser(1, "Old work"), cAsst(2, "m0", "end_turn", map[string]any{"type": "text", "text": "Old answer."}))
	now := time.Now()
	s := &Session{ID: "claude:s1", Tool: ToolClaude, SessionID: "s1", TranscriptPath: path, Title: "Build fix", LastActivityAt: now.UnixMilli()}

	turnNotifyTick([]*Session{s}, now)
	if len(*got) != 0 {
		t.Fatalf("a turn that ended before the watcher saw the session must stay quiet: %v", *got)
	}

	appendJSONL(t, path, cUser(3, "Fix the build"), cAsst(4, "m1", "end_turn", map[string]any{"type": "text", "text": "Fixed.\nShould I also push the branch?"}))
	s.LastActivityAt++
	turnNotifyTick([]*Session{s}, now)
	if len(*got) != 1 || (*got)[0].Kind != "needs-you" || (*got)[0].Title != "Build fix asks you" || (*got)[0].Body != "Should I also push the branch?" || (*got)[0].ThreadID != "claude:s1" {
		t.Fatalf("a turn ending on a question: %+v", *got)
	}
	turnNotifyTick([]*Session{s}, now)
	if len(*got) != 1 {
		t.Fatal("an unchanged session must not repeat the notice")
	}

	appendJSONL(t, path, cUser(5, "Push it"), cAsst(6, "m2", "end_turn", map[string]any{"type": "text", "text": "Pushed."}))
	s.LastActivityAt++
	turnNotifyTick([]*Session{s}, now)
	if len(*got) != 2 || (*got)[1].Kind != "finished" || (*got)[1].Title != "Build fix finished" || (*got)[1].Body != "Pushed." {
		t.Fatalf("a finished turn: %+v", *got)
	}

	appendJSONL(t, path, cUser(7, "Now refactor everything"),
		claudeRec("user", 8, map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "[Request interrupted by user]"}}}, nil))
	s.LastActivityAt++
	turnNotifyTick([]*Session{s}, now)
	if len(*got) != 3 || (*got)[2].Kind != "error" || (*got)[2].Title != "Build fix was interrupted" {
		t.Fatalf("an interrupted turn: %+v", *got)
	}
}

func TestPromptNotifierAnnouncesNewPromptsAndSettlesClosedOnes(t *testing.T) {
	got := captureTurnNotices(t)
	old := NeedsYouItem{Kind: "prompt", Key: "tp:claude:a", SessionID: "claude:a", Title: "A", Message: "Old prompt"}
	promptNotifyTick([]NeedsYouItem{old})
	if len(*got) != 0 {
		t.Fatal("prompts open when the daemon starts must stay quiet")
	}
	exit := NeedsYouItem{Kind: "prompt", Key: "tp:claude:b", SessionID: "claude:b", Title: "B", Message: "Keep the worktree?"}
	held := NeedsYouItem{Kind: "approval", Key: "ap_1", SessionID: "claude:c"}
	promptNotifyTick([]NeedsYouItem{old, exit, held})
	if len(*got) != 1 || (*got)[0].Title != "B needs you" || (*got)[0].Body != "Keep the worktree?" || (*got)[0].Keys[0] != "tp:claude:b" {
		t.Fatalf("a new terminal prompt: %+v", *got)
	}
	promptNotifyTick([]NeedsYouItem{exit})
	if len(*got) != 2 || (*got)[1].Kind != "resolved" || (*got)[1].Keys[0] != "tp:claude:a" {
		t.Fatalf("a closed prompt settles its notice: %+v", *got)
	}
}

func TestPromptNotifierAlertsANewPromptOfTheSameSession(t *testing.T) {
	got := captureTurnNotices(t)
	promptNotifyTick(nil)
	a := NeedsYouItem{Kind: "prompt", Key: "tp:claude:a", SessionID: "claude:a", Title: "A", Message: "Edit x.go?", PromptKey: "k1"}
	promptNotifyTick([]NeedsYouItem{a})
	promptNotifyTick([]NeedsYouItem{a})
	if len(*got) != 1 {
		t.Fatalf("one prompt alerts once: %+v", *got)
	}
	b := a
	b.Message, b.PromptKey = "Edit y.go?", "k2"
	promptNotifyTick([]NeedsYouItem{b})
	if len(*got) != 3 || (*got)[1].Kind != "resolved" || (*got)[2].Body != "Edit y.go?" {
		t.Fatalf("prompt B after A in one tick settles A and alerts B: %+v", *got)
	}
	c := b
	c.PromptKey = ""
	promptNotifyTick([]NeedsYouItem{c})
	promptNotifyTick([]NeedsYouItem{b})
	if len(*got) != 3 {
		t.Fatalf("a tick with no fingerprint is not a new prompt: %+v", *got)
	}
}

func TestEndingQuestion(t *testing.T) {
	cases := map[string]string{
		"Done.":                              "",
		"Done. Want me to open a PR?":        "Done. Want me to open a PR?",
		"All fixed.\n\n**Should I deploy?**": "Should I deploy?",
		"Is it? No, it works.":               "",
		"":                                   "",
	}
	for in, want := range cases {
		if got := endingQuestion(in); got != want {
			t.Errorf("%q: got %q, want %q", in, got, want)
		}
	}
}
