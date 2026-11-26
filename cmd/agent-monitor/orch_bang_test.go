package main

import (
	"strings"
	"testing"
)

func TestOrchBangRunsAShellCommand(t *testing.T) {
	e := newOrchEnv(t)
	th := e.create("hello")
	e.waitRunEnd()

	mode, err := e.m.Send(th.ID, "! echo out-line; echo err-line 1>&2; exit 3")
	if err != nil || mode != "bash" {
		t.Fatalf("Send = %q, %v", mode, err)
	}
	done := e.wait("item.completed", func(d map[string]any) bool { return field(d, "item", "by") == "user" })
	it := done["item"].(map[string]any)
	if it["status"] != "failed" || it["exitCode"] != float64(3) {
		t.Fatalf("item = %v", it)
	}
	if out, _ := it["output"].(string); !strings.Contains(out, "out-line") || !strings.Contains(out, "err-line") {
		t.Fatalf("output = %q", out)
	}
	e.wait("run.ended", nil)

	ctx := e.m.store.takeBangContext(th.ID)
	for _, want := range []string{"<bash-input>echo out-line; echo err-line 1>&2; exit 3</bash-input>", "<bash-stdout>out-line</bash-stdout>", "<bash-stderr>err-line</bash-stderr>"} {
		if !strings.Contains(ctx, want) {
			t.Fatalf("context %q lacks %q", ctx, want)
		}
	}
	if again := e.m.store.takeBangContext(th.ID); again != "" {
		t.Fatalf("context sent twice: %q", again)
	}
}

func TestOrchBangStopEndsTheCommand(t *testing.T) {
	e := newOrchEnv(t)
	th := e.create("hello")
	e.waitRunEnd()

	if _, err := e.m.Send(th.ID, "! sleep 30"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.m.Send(th.ID, "! echo second"); err == nil {
		t.Fatal("a second command started while the first runs")
	}
	if err := e.m.Interrupt(th.ID); err != nil {
		t.Fatalf("Interrupt = %v", err)
	}
	ended := e.wait("run.ended", func(d map[string]any) bool { return field(d, "run", "status") == "interrupted" })
	if ended == nil {
		t.Fatal("no interrupted run")
	}
	if bangRunning(th.ID) {
		t.Fatal("command still listed as running")
	}
	if ctx := e.m.store.takeBangContext(th.ID); !strings.Contains(ctx, "stopped by the user") {
		t.Fatalf("context = %q", ctx)
	}
}

func TestBangCommand(t *testing.T) {
	for in, want := range map[string]string{"!ls -la": "ls -la", "  ! git status ": "git status", "!": "", "hello !x": "", "": ""} {
		if got := bangCommand(in); got != want {
			t.Errorf("bangCommand(%q) = %q, want %q", in, got, want)
		}
	}
}
