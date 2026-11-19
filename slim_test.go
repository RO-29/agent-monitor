package main

import (
	"testing"
	"time"
)

func TestSlimSessionsKeepsLiveEvents(t *testing.T) {
	now := time.Now().UnixMilli()
	ev := []RecentEvent{{Kind: "Stop"}}
	live := &Session{ID: "a", State: StateRunning, LastActivityAt: now, RecentEvents: ev}
	ended := &Session{ID: "b", State: StateCompleted, LastActivityAt: now, RecentEvents: ev}
	old := &Session{ID: "c", State: StateIdle, LastActivityAt: now - (7 * time.Hour).Milliseconds(), RecentEvents: ev}
	out := slimSessions([]*Session{live, ended, old})
	if len(out[0].RecentEvents) != 1 || len(out[1].RecentEvents) != 0 || out[1].RecentEvents == nil || len(out[2].RecentEvents) != 0 {
		t.Fatalf("slim: %v %v %v", out[0].RecentEvents, out[1].RecentEvents, out[2].RecentEvents)
	}
	if len(ended.RecentEvents) != 1 {
		t.Fatal("the original session must not change")
	}
}
