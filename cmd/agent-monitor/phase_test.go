package main

import (
	"testing"
	"time"
)

func TestSessionPhase(t *testing.T) {
	now := time.Now()
	today := startOfDay(now)
	ms := now.UnixMilli()
	cases := []struct {
		name string
		s    Session
		need map[string]bool
		want string
	}{
		{"held approval wins over running", Session{ID: "claude:a", State: StateRunning, LastActivityAt: ms}, map[string]bool{"claude:a": true}, phaseNeedsYou},
		{"terminal prompt", Session{ID: "claude:b", State: StateAwaitingPermission, LastActivityAt: ms}, nil, phaseNeedsYou},
		{"open question by transcript", Session{ID: "claude:c", State: StateIdle, TranscriptPath: "/t.jsonl", LastActivityAt: ms}, map[string]bool{"path:/t.jsonl": true}, phaseNeedsYou},
		{"running", Session{ID: "claude:d", State: StateRunning, LastActivityAt: ms}, nil, phaseWorking},
		{"turn ended today", Session{ID: "claude:e", State: StateAwaitingInput, LastActivityAt: ms}, nil, phaseReady},
		{"idle yesterday", Session{ID: "claude:f", State: StateIdle, LastActivityAt: today - 1}, nil, phaseDone},
		{"completed", Session{ID: "claude:g", State: StateCompleted, LastActivityAt: ms}, nil, phaseDone},
	}
	for _, c := range cases {
		if got := sessionPhase(&c.s, c.need, today); got != c.want {
			t.Errorf("%s: got %s, want %s", c.name, got, c.want)
		}
	}
}

func TestThreadPhaseMostUrgentWins(t *testing.T) {
	ms := time.Now().UnixMilli()
	byID := map[string]*Session{
		"claude:old": {ID: "claude:old", State: StateCompleted, LastActivityAt: ms},
		"claude:new": {ID: "claude:new", State: StateRunning, LastActivityAt: ms},
	}
	tv := ThreadV3{Thread: Thread{Sessions: []string{"claude:old", "claude:new"}}}
	if p := threadPhase(&tv, byID, nil, startOfDay(time.Now())); p != phaseWorking {
		t.Fatalf("got %s", p)
	}
	tv.Approvals = 1
	if p := threadPhase(&tv, byID, nil, startOfDay(time.Now())); p != phaseNeedsYou {
		t.Fatalf("with an approval got %s", p)
	}
	d := ThreadV3{Kind: "driven", Group: "settled"}
	if p := threadPhase(&d, nil, nil, 0); p != phaseDone {
		t.Fatalf("settled driven got %s", p)
	}
}

func TestToolCallOpenKeepsRunning(t *testing.T) {
	old := time.Now().Add(-10 * time.Minute).UnixMilli()
	st := NewStore()
	st.sessions["claude:x"] = &Session{ID: "claude:x", State: StateRunning, LastActivityAt: old,
		RecentEvents: []RecentEvent{{Kind: "UserPromptSubmit"}, {Kind: "PreToolUse", Text: "Bash"}}}
	st.sessions["claude:y"] = &Session{ID: "claude:y", State: StateRunning, LastActivityAt: old,
		RecentEvents: []RecentEvent{{Kind: "PreToolUse"}, {Kind: "PostToolUse"}}}
	st.tick()
	if s := st.Get("claude:x"); s.State != StateRunning {
		t.Fatalf("open tool call decayed to %s", s.State)
	}
	if s := st.Get("claude:y"); s.State != StateIdle {
		t.Fatalf("finished tool call stayed %s", s.State)
	}
}
