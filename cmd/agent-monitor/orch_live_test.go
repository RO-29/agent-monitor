package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestOrchLiveCodex drives the real codex app-server; it spends tokens, so it runs only with AM_LIVE_CODEX=1.
func TestOrchLiveCodex(t *testing.T) {
	if os.Getenv("AM_LIVE_CODEX") != "1" {
		t.Skip("set AM_LIVE_CODEX=1 to run against the real codex app-server")
	}
	dir := t.TempDir()
	store, err := openOrchStore(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	appr := newApprovalStore(dir)
	appr.focused = func(string) bool { return false }
	events, _ := store.hub.subscribe()
	m := newOrchManager(store, appr)
	defer m.Shutdown()
	var mu sync.Mutex
	methods := map[string]int{}
	m.trace = func(line []byte) {
		var msg rpcMsg
		if json.Unmarshal(line, &msg) == nil && msg.Method != "" {
			mu.Lock()
			methods[msg.Method]++
			mu.Unlock()
		}
	}
	// User hooks stay off so the run does not reach the live daemon.
	m.mcp = nil
	m.command = func(th *OThread, extra []string) *exec.Cmd {
		cmd := exec.Command(codexBinary(), "app-server", "--disable", "hooks")
		cmd.Env = append(os.Environ(), "AGENT_MONITOR_URL=http://127.0.0.1:9")
		return cmd
	}
	cwd := t.TempDir()
	th, err := m.CreateThread(orchCreate{Agent: "codex", Cwd: cwd, Prompt: "reply with the word pong", Sandbox: "read-only", ApprovalPolicy: "never"})
	if err != nil {
		t.Fatal(err)
	}
	timeout := time.After(3 * time.Minute)
	var run map[string]any
	for run == nil {
		select {
		case ev := <-events:
			if ev.Type == "run.ended" {
				_ = json.Unmarshal(ev.Data, &run)
			}
		case <-timeout:
			d, _ := store.ThreadDetail(th.ID)
			t.Fatalf("no run.ended in time: %+v", d)
		}
	}
	d, err := store.ThreadDetail(th.ID)
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	var seen []string
	for k, n := range methods {
		seen = append(seen, k+"="+itoa(n))
	}
	mu.Unlock()
	sort.Strings(seen)
	t.Logf("codex methods: %s", strings.Join(seen, " "))
	t.Logf("thread: status=%s native=%s model=%s error=%q", d.Thread.Status, d.Thread.NativeID, d.Thread.Model, d.Thread.Error)
	for _, r := range d.Runs {
		t.Logf("run %d: %s turn=%s usage=%+v", r.Index, r.Status, r.TurnID, r.Usage)
	}
	agentText := ""
	for _, it := range d.Items {
		t.Logf("item %s %s %q", it.Kind, it.Status, clipString(it.Text, 120))
		if it.Kind == "agent" {
			agentText += it.Text
		}
	}
	if st, _ := run["run"].(map[string]any)["status"].(string); st != "completed" {
		t.Fatalf("run status %q", st)
	}
	if !strings.Contains(strings.ToLower(agentText), "pong") {
		t.Fatalf("no agent item with pong: %q", agentText)
	}
	if d.Thread.NativeID == "" {
		t.Fatal("no native thread id")
	}
	matches, _ := filepath.Glob(filepath.Join(homeDir(), ".codex", "sessions", "*", "*", "*", "*"+d.Thread.NativeID+".jsonl"))
	t.Logf("rollout files named by nativeId: %v", matches)
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

// TestOrchLiveClaude drives the real claude CLI; it spends tokens, so it runs only with AM_LIVE_CLAUDE=1.
func TestOrchLiveClaude(t *testing.T) {
	if os.Getenv("AM_LIVE_CLAUDE") != "1" {
		t.Skip("set AM_LIVE_CLAUDE=1 to run against the real claude CLI")
	}
	dir := t.TempDir()
	store, err := openOrchStore(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	appr := newApprovalStore(dir)
	appr.focused = func(string) bool { return false }
	events, _ := store.hub.subscribe()
	m := newOrchManager(store, appr)
	defer m.Shutdown()
	var mu sync.Mutex
	types := map[string]int{}
	m.trace = func(line []byte) {
		var l struct {
			Type    string `json:"type"`
			Subtype string `json:"subtype"`
		}
		if json.Unmarshal(line, &l) == nil {
			mu.Lock()
			types[strings.TrimSuffix(l.Type+"/"+l.Subtype, "/")]++
			mu.Unlock()
		}
	}
	// User settings (hooks, plugins) and MCP servers stay off so the run does not reach the live daemon.
	m.claude.ExtraArgs = []string{"--setting-sources", "", "--strict-mcp-config"}
	m.mcp = nil
	base := m.claude.Command
	m.claude.Command = func(args []string) *exec.Cmd {
		cmd := base(args)
		cmd.Env = append(os.Environ(), "AGENT_MONITOR_URL=http://127.0.0.1:9")
		return cmd
	}
	cwd := t.TempDir()
	th, err := m.CreateThread(orchCreate{Agent: "claude", Cwd: cwd, Prompt: "reply with the word pong", Model: "haiku"})
	if err != nil {
		t.Fatal(err)
	}
	timeout := time.After(3 * time.Minute)
	var run map[string]any
	for run == nil {
		select {
		case ev := <-events:
			if ev.Type == "run.ended" {
				_ = json.Unmarshal(ev.Data, &run)
			}
		case <-timeout:
			d, _ := store.ThreadDetail(th.ID)
			t.Fatalf("no run.ended in time: %+v", d)
		}
	}
	d, err := store.ThreadDetail(th.ID)
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	var seen []string
	for k, n := range types {
		seen = append(seen, k+"="+itoa(n))
	}
	mu.Unlock()
	sort.Strings(seen)
	t.Logf("claude lines: %s", strings.Join(seen, " "))
	t.Logf("thread: status=%s native=%s model=%s mode=%s error=%q", d.Thread.Status, d.Thread.NativeID, d.Thread.Model, d.Thread.PermissionMode, d.Thread.Error)
	for _, r := range d.Runs {
		t.Logf("run %d: %s usage=%+v", r.Index, r.Status, r.Usage)
	}
	agentText := ""
	for _, it := range d.Items {
		t.Logf("item %s %s %q", it.Kind, it.Status, clipString(it.Text, 120))
		if it.Kind == "agent" {
			agentText += it.Text
		}
	}
	if st, _ := run["run"].(map[string]any)["status"].(string); st != "completed" {
		t.Fatalf("run status %q", st)
	}
	if !strings.Contains(strings.ToLower(agentText), "pong") {
		t.Fatalf("no agent item with pong: %q", agentText)
	}
	if u := d.Runs[0].Usage; u == nil || u.CostUSD <= 0 {
		t.Fatalf("no cost in usage: %+v", u)
	}
	t.Logf("transcript exists for nativeId: %v", claudeSessionExists(d.Thread.NativeID))
}

// TestOrchMCPServerProcess is not a test: it is agent-monitor's MCP server, re-executed by the live delegate test.
func TestOrchMCPServerProcess(t *testing.T) {
	if os.Getenv("ORCH_MCP_SERVER") != "1" {
		t.Skip("helper process")
	}
	runMCPPermServer()
	os.Exit(0)
}

// TestOrchLiveDelegate: a driven Claude (haiku) delegates to a read-only Codex child through the MCP tools.
// It spends tokens, so it runs only with AM_LIVE_DELEGATE=1; the daemon is an httptest server.
func TestOrchLiveDelegate(t *testing.T) {
	if os.Getenv("AM_LIVE_DELEGATE") != "1" {
		t.Skip("set AM_LIVE_DELEGATE=1 to run a real Claude to Codex delegation")
	}
	dir := t.TempDir()
	store, err := openOrchStore(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	appr := newApprovalStore(dir)
	appr.focused = func(string) bool { return false }
	m := newOrchManager(store, appr)
	defer m.Shutdown()
	mux := http.NewServeMux()
	m.routes(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	m.daemonURL, m.stateDir = srv.URL, dir
	m.mcp = func() *orchMCPServer {
		return &orchMCPServer{Command: os.Args[0], Args: []string{"-test.run=^TestOrchMCPServerProcess$"}, Env: []string{"ORCH_MCP_SERVER=1"}}
	}
	// User settings, hooks and MCP servers stay off; only the test's agent-monitor MCP server runs.
	m.claude.ExtraArgs = []string{"--setting-sources", "", "--strict-mcp-config"}
	base := m.claude.Command
	m.claude.Command = func(args []string) *exec.Cmd {
		cmd := base(args)
		cmd.Env = append(os.Environ(), "AGENT_MONITOR_URL=http://127.0.0.1:9")
		return cmd
	}
	var off []string
	if out, err := exec.Command(codexBinary(), "mcp", "list", "--json").Output(); err == nil {
		var servers []struct {
			Name string `json:"name"`
		}
		_ = json.Unmarshal(out, &servers)
		for _, s := range servers {
			if s.Name != "agent-monitor" {
				off = append(off, "-c", "mcp_servers."+s.Name+".enabled=false")
			}
		}
	}
	m.command = func(th *OThread, extra []string) *exec.Cmd {
		args := append([]string{"app-server", "--disable", "hooks"}, off...)
		cmd := exec.Command(codexBinary(), append(args, extra...)...)
		cmd.Env = append(os.Environ(), "AGENT_MONITOR_URL=http://127.0.0.1:9")
		return cmd
	}
	cwd := gitRepo(t)
	prompt := `Call the delegate_task tool with agent "codex", workspace "read-only", title "pong" and prompt "Reply with the single word pong. Do not run any command." ` +
		`Then call the wait_tasks tool with no arguments. Finally reply with exactly the result text that wait_tasks returned for that task.`
	th, err := m.CreateThread(orchCreate{Agent: "claude", Cwd: cwd, Prompt: prompt, Model: "haiku"})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(6 * time.Minute)
	var d *OThreadDetail
	for {
		d, _ = store.ThreadDetail(th.ID)
		if d != nil && len(d.Runs) > 0 && d.Runs[0].Status != "running" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("parent run did not end: %+v", d)
		}
		time.Sleep(500 * time.Millisecond)
	}
	for _, r := range d.Runs {
		t.Logf("parent run %d: %s usage=%+v", r.Index, r.Status, r.Usage)
	}
	for _, it := range d.Items {
		t.Logf("parent item %s %s %s %q out=%q", it.Kind, it.Status, it.Command, clipString(it.Text, 160), clipString(it.Output, 300))
	}
	if len(d.Children) != 1 {
		t.Fatalf("children %+v", d.Children)
	}
	c := d.Children[0]
	cd, _ := store.ThreadDetail(c.ID)
	t.Logf("child: agent=%s status=%s sandbox=%s workspace=%s depth=%d group=%s model=%s", c.Agent, c.Status, c.Sandbox, c.Workspace, c.Depth, c.Group, c.Model)
	for _, it := range cd.Items {
		t.Logf("child item %s %s %q", it.Kind, it.Status, clipString(it.Text, 160))
	}
	t.Logf("parent: status=%s group=%s", d.Thread.Status, d.Thread.Group)
	if c.Agent != "codex" || c.Sandbox != "read-only" || !strings.Contains(strings.ToLower(lastAgentText(cd)), "pong") {
		t.Fatalf("child did not answer pong: %+v", c)
	}
	if !strings.Contains(strings.ToLower(lastAgentText(d)), "pong") {
		t.Fatalf("parent did not relay pong: %q", lastAgentText(d))
	}
}

// TestOrchLiveACP drives the real `opencode acp`; it spends tokens, so it runs only with AM_LIVE_ACP=1.
func TestOrchLiveACP(t *testing.T) {
	if os.Getenv("AM_LIVE_ACP") != "1" {
		t.Skip("set AM_LIVE_ACP=1 to run against the real opencode acp")
	}
	dir := t.TempDir()
	store, err := openOrchStore(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	appr := newApprovalStore(dir)
	appr.focused = func(string) bool { return false }
	events, _ := store.hub.subscribe()
	m := newOrchManager(store, appr)
	m.stateDir = dir
	defer m.Shutdown()
	var mu sync.Mutex
	methods := map[string]int{}
	m.trace = func(line []byte) {
		var msg rpcMsg
		if json.Unmarshal(line, &msg) == nil && msg.Method != "" {
			mu.Lock()
			methods[msg.Method]++
			mu.Unlock()
		}
	}
	m.mcp = nil
	th, err := m.CreateThread(orchCreate{Agent: "opencode", Cwd: t.TempDir(), Prompt: "reply with the word pong", Model: os.Getenv("AM_LIVE_ACP_MODEL")})
	if err != nil {
		t.Fatal(err)
	}
	timeout := time.After(3 * time.Minute)
	var run map[string]any
	for run == nil {
		select {
		case ev := <-events:
			if ev.Type == "run.ended" {
				_ = json.Unmarshal(ev.Data, &run)
			}
		case <-timeout:
			_ = m.Interrupt(th.ID)
			d, _ := store.ThreadDetail(th.ID)
			t.Fatalf("no run.ended in 3 min (opencode hangs when its provider fails): %+v", d.Thread)
		}
	}
	d, _ := store.ThreadDetail(th.ID)
	mu.Lock()
	t.Logf("acp methods from agent: %v", methods)
	mu.Unlock()
	t.Logf("thread: status=%s native=%s model=%s error=%q", d.Thread.Status, d.Thread.NativeID, d.Thread.Model, d.Thread.Error)
	text := ""
	for _, it := range d.Items {
		t.Logf("item %s %s %q", it.Kind, it.Status, clipString(it.Text, 160))
		if it.Kind == "agent" {
			text += it.Text
		}
	}
	for _, r := range d.Runs {
		t.Logf("run %d: %s usage=%+v", r.Index, r.Status, r.Usage)
	}
	if !strings.Contains(strings.ToLower(text), "pong") {
		t.Fatalf("no agent item with pong: %q", text)
	}
}
