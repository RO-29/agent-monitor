package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestResolveTargetIn(t *testing.T) {
	list := []sessionTarget{
		{AgentID: "claude:aaaaaaaa-1", Tool: ToolClaude, SessionID: "aaaaaaaa-1", PaneID: "%3", Alias: "rev"},
		{AgentID: "codex:bbbbbbbb-2", Tool: ToolCodex, SessionID: "bbbbbbbb-2", HostID: "0123456789abcdef"},
		{AgentID: "codex:bbbbbbcc-3", Tool: ToolCodex, SessionID: "bbbbbbcc-3", HostID: "fedcba9876543210"},
	}
	for q, want := range map[string]string{
		"REV": "claude:aaaaaaaa-1", "claude:aaaaaaaa-1": "claude:aaaaaaaa-1", "%3": "claude:aaaaaaaa-1",
		"0123456789abcdef": "codex:bbbbbbbb-2", "bbbbbbbb": "codex:bbbbbbbb-2", "claude": "claude:aaaaaaaa-1",
		"bbbbbbcc-3": "codex:bbbbbbcc-3",
	} {
		got, err := resolveTargetIn(list, q)
		if err != nil || got.AgentID != want {
			t.Fatalf("%q: %+v %v", q, got, err)
		}
	}
	for _, q := range []string{"codex", "bbbbbb", "nobody", ""} {
		if _, err := resolveTargetIn(list, q); err == nil {
			t.Fatalf("%q resolved", q)
		}
	}
}

func TestCLIResolvesHostedSession(t *testing.T) {
	withHostDaemon(t)
	stubTmux(t, nil)
	u := newUserTerm(t)
	h, _ := startHost(t, hostOptions{
		Argv: fakeAgentArgv(), Env: fakeAgentEnv(), SockDir: shortTempDir(t),
		In: u.slave, Out: u.slave, TTY: u.slave, Register: postHostRegistration, Remove: deleteHostRegistration,
	})
	u.waitFor(t, "READY")
	info := waitHost(t, h.opt.HostID)
	if _, err, _ := resolveViaTargets("claude"); err == nil {
		t.Fatal("unlinked host resolved")
	}
	if !hostRegistry.LinkFromHook(h.opt.HostID, ToolClaude, testClaudeSID, info.AgentPid) {
		t.Fatal("link refused")
	}
	want := "claude:" + testClaudeSID
	for _, q := range []string{"claude", want, testClaudeSID[:8], h.opt.HostID} {
		if id, err := resolveRecipientToAgentID(q); err != nil || id != want {
			t.Fatalf("%q: %q %v", q, id, err)
		}
	}
	list, ok := fetchTargets()
	if !ok || len(list) != 1 || list[0].HostID != h.opt.HostID || list[0].PaneID != "" {
		t.Fatalf("targets %+v %v", list, ok)
	}
	var b bytes.Buffer
	printTargets(&b, list)
	if !strings.Contains(b.String(), "HOST") || !strings.Contains(b.String(), h.opt.HostID) {
		t.Fatalf("list output %q", b.String())
	}
	t.Setenv("AM_HOST_ID", h.opt.HostID)
	if got := hostSelfAgentID(); got != want {
		t.Fatalf("self %q", got)
	}
	if got := hostRegistry.Get(h.opt.HostID); got.AgentID != want {
		t.Fatalf("host agentId %q", got.AgentID)
	}
	_, _ = u.master.Write([]byte("exit 0\r"))
}
