package main

import (
	"reflect"
	"strings"
	"sync"
	"testing"
)

func TestTerminalFlagsAllowList(t *testing.T) {
	ok := []struct {
		in   terminalStart
		want []string
	}{
		{terminalStart{Agent: "claude"}, nil},
		{terminalStart{Agent: "claude", Model: "opus", PermissionMode: "acceptEdits"}, []string{"--model", "opus", "--permission-mode", "acceptEdits"}},
		{terminalStart{Agent: "claude", PermissionMode: "manual"}, nil},
		{terminalStart{Agent: "codex", Model: "gpt-5.5", Sandbox: "read-only", ApprovalPolicy: "never"}, []string{"--model", "gpt-5.5", "-s", "read-only", "-a", "never"}},
	}
	for _, c := range ok {
		got, err := terminalFlags(c.in)
		if err != nil || !reflect.DeepEqual(got, c.want) {
			t.Fatalf("%+v: %v %v", c.in, got, err)
		}
	}
	bad := []terminalStart{
		{Agent: "bash"},
		{Agent: "claude", Model: "--dangerously-skip-permissions"},
		{Agent: "claude", Model: "a b"},
		{Agent: "claude", PermissionMode: "bypassPermissions"},
		{Agent: "claude", Sandbox: "read-only"},
		{Agent: "codex", Sandbox: "danger-full-access"},
		{Agent: "codex", ApprovalPolicy: "on-failure"},
		{Agent: "codex", PermissionMode: "plan"},
	}
	for _, b := range bad {
		if _, err := terminalFlags(b); err == nil {
			t.Fatalf("%+v accepted", b)
		}
	}
}

// stubTerminalStart records tmux starts instead of running tmux.
func stubTerminalStart(t *testing.T) func() []string {
	prevNew, prevExe, prevType, prevWatch := tmuxNewSessionFn, agentMonitorExe, typeWhenReadyFn, watchStartMenuFn
	t.Cleanup(func() {
		tmuxNewSessionFn, agentMonitorExe, typeWhenReadyFn, watchStartMenuFn = prevNew, prevExe, prevType, prevWatch
	})
	typeWhenReadyFn = func(string, string) error { return nil }
	watchStartMenuFn = func(string) {}
	agentMonitorExe = func() (string, error) { return "/x/agent-monitor", nil }
	var mu sync.Mutex
	var cmds []string
	tmuxNewSessionFn = func(name, cwd, command string) error {
		mu.Lock()
		defer mu.Unlock()
		cmds = append(cmds, name+" "+cwd+" "+command)
		return nil
	}
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), cmds...)
	}
}

func TestOrchTerminalRemoteFieldsMapToFlags(t *testing.T) {
	srv := withHostDaemon(t)
	cmds := stubTerminalStart(t)
	cwd := t.TempDir()
	code, out := postJSON(t, srv.URL+"/api/orch/terminal", `{"agent":"codex","cwd":"`+cwd+`","model":"gpt-5.5","sandbox":"read-only","approvalPolicy":"untrusted"}`)
	if code != 200 || out["attach"] != "tmux attach -t "+out["session"].(string) {
		t.Fatalf("%d %v", code, out)
	}
	if c := cmds(); len(c) != 1 || !strings.HasSuffix(c[0], `run 'codex' '--model' 'gpt-5.5' '-s' 'read-only' '-a' 'untrusted'`) {
		t.Fatalf("cmds %q", c)
	}
	if code, _ := postJSON(t, srv.URL+"/api/orch/terminal", `{"agent":"claude","cwd":"`+cwd+`","permissionMode":"bypassPermissions"}`); code != 400 {
		t.Fatalf("bad mode: %d", code)
	}
}

func TestTemplateAndScheduleWhereTerminal(t *testing.T) {
	e := newOrchEnv(t)
	cmds := stubTerminalStart(t)
	if _, err := e.m.SaveTemplate("", OTemplate{Name: "X", Agent: "codex", Prompt: "p", Where: "nowhere"}); err == nil {
		t.Fatal("bad where accepted")
	}
	tpl, err := e.m.SaveTemplate("", OTemplate{Name: "T", Agent: "claude", Prompt: "look at {{cwd}}", Where: "terminal", Workspace: "read-only"})
	if err != nil {
		t.Fatal(err)
	}
	l, err := e.m.LaunchTemplate(tpl.ID, orchTemplateRun{Cwd: e.cwd})
	if err != nil || l.Terminal == nil || l.Thread != nil {
		t.Fatalf("launch %+v %v", l, err)
	}
	if c := cmds(); len(c) != 1 || !strings.Contains(c[0], `run 'claude' '--permission-mode' 'plan'`) {
		t.Fatalf("cmds %q", c)
	}

	s, err := e.m.SaveSchedule("", OSchedule{TemplateID: tpl.ID, Cwd: e.cwd, Cron: "0 9 * * *", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	th, got, err := e.m.RunScheduleNow(s.ID)
	if err != nil || th != nil || got.LastSession == "" || got.LastHostID == "" || got.LastThreadID != "" {
		t.Fatalf("run-now %v %+v %v", th, got, err)
	}
	s.Where = "headless"
	if _, err := e.m.SaveSchedule(s.ID, *s); err != nil {
		t.Fatal(err)
	}
	th, got, err = e.m.RunScheduleNow(s.ID)
	if err != nil || th == nil || got.LastThreadID != th.ID || got.LastSession != "" {
		t.Fatalf("headless override %v %+v %v", th, got, err)
	}
	e.idle(th.ID)
	if n := len(cmds()); n != 2 {
		t.Fatalf("tmux starts %d", n)
	}
}
