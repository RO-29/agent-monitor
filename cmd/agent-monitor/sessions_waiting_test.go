package main

import "testing"

func TestWaitingSessions(t *testing.T) {
	in := []*Session{{ID: "a", State: StateAwaitingInput}, {ID: "b", State: StateRunning}, {ID: "c", State: StateAwaitingPermission}, {ID: "d", State: StateIdle}}
	got := waitingSessions(in)
	if len(got) != 2 || got[0].ID != "a" || got[1].ID != "c" {
		t.Fatalf("got %v", got)
	}
}
