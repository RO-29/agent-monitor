package main

import (
	"testing"
	"time"
)

func TestMarkDoneHidesUntilNewActivity(t *testing.T) {
	t.Setenv("AGENT_MONITOR_HOME", t.TempDir())
	marks.Lock()
	marks.m, marks.loaded = map[string]threadMark{}, true
	marks.Unlock()
	now := time.Now().UnixMilli()
	s := &Session{ID: "claude:x", State: StateIdle, LastActivityAt: now - 1000}
	today := startOfDay(time.Now())
	if p := sessionPhase(s, nil, today); p != phaseReady {
		t.Fatalf("before mark: %s", p)
	}
	yes := true
	applyMarks(markReq{IDs: []string{"claude:x"}, Settled: &yes}, now)
	if p := sessionPhase(s, nil, today); p != phaseDone {
		t.Fatalf("after done: %s", p)
	}
	s.LastActivityAt = now + 1000
	if p := sessionPhase(s, nil, today); p != phaseReady {
		t.Fatalf("new activity must clear done: %s", p)
	}
	until := now + 3600_000
	applyMarks(markReq{IDs: []string{"claude:x"}, SnoozedUntil: &until}, now)
	if p := sessionPhase(s, nil, today); p != phaseDone {
		t.Fatalf("snoozed: %s", p)
	}
	s.State = StateRunning
	if p := sessionPhase(s, nil, today); p != phaseWorking {
		t.Fatalf("working wins over snooze: %s", p)
	}
}
