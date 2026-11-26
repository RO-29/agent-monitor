package main

import (
	"context"
	"testing"
	"time"
)

func TestPermWaitDecidedAndCallerGone(t *testing.T) {
	p := NewPermStore()
	adds := 0
	p.Subscribe(func(e PermEvent) {
		if e.Kind == "perm-add" {
			adds++
		}
	})
	d := &PermissionRequest{ToolName: "Bash"}
	p.RegisterDecided(d, PermissionResponse{Behavior: "deny", Reason: "rule"})
	if r, ok := p.Wait(context.Background(), d.ID, time.Second); !ok || r.Behavior != "deny" || len(p.List()) != 0 || adds != 0 {
		t.Fatalf("decided: %+v %v list=%d adds=%d", r, ok, len(p.List()), adds)
	}
	// The MCP helper ends while it waits: the card goes at once.
	q := &PermissionRequest{ToolName: "Bash"}
	p.Register(q)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()
	if r, _ := p.Wait(ctx, q.ID, time.Minute); r.Reason != "caller gone" || len(p.List()) != 0 {
		t.Fatalf("caller gone: %+v list=%d", r, len(p.List()))
	}
}
