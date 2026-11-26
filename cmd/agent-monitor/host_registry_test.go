package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// ── daemon side ──

// withHostDaemon serves the host routes on a loopback test server and points daemonURL at it.
func withHostDaemon(t *testing.T) *httptest.Server {
	t.Helper()
	prevHR, prevPR, prevStore := hostRegistry, paneRegistry, store
	t.Setenv("AGENT_MONITOR_HOME", t.TempDir())
	hostRegistry = NewHostRegistry(shortTempDir(t) + "/hosts.json")
	paneRegistry = NewPaneRegistry()
	store = nil
	stubPaneProbe(t, nil)
	mux := http.NewServeMux()
	registerHostRoutes(mux)
	srv := httptest.NewServer(mux)
	u, _ := url.Parse(srv.URL)
	t.Setenv("AGENT_MONITOR_PORT", u.Port())
	t.Cleanup(func() {
		srv.Close()
		hostRegistry, paneRegistry, store = prevHR, prevPR, prevStore
	})
	return srv
}

type tmuxSpy struct {
	mu    sync.Mutex
	calls []string
}

func stubTmux(t *testing.T, panes []TmuxPane) *tmuxSpy {
	t.Helper()
	spy := &tmuxSpy{}
	prevText, prevKeys, prevCap, prevList := tmuxSendTextFn, tmuxSendKeysFn, tmuxCaptureFn, listPanesFn
	tmuxSendTextFn = func(p, text string, enter bool) error {
		spy.mu.Lock()
		spy.calls = append(spy.calls, "text "+p+" "+text)
		spy.mu.Unlock()
		return nil
	}
	tmuxSendKeysFn = func(p string, keys []string) error {
		spy.mu.Lock()
		spy.calls = append(spy.calls, "keys "+p+" "+strings.Join(keys, ","))
		spy.mu.Unlock()
		return nil
	}
	tmuxCaptureFn = func(p string, lines int, plain bool) (string, error) {
		spy.mu.Lock()
		spy.calls = append(spy.calls, "capture "+p)
		spy.mu.Unlock()
		return "tmux screen", nil
	}
	listPanesFn = func() ([]TmuxPane, error) { return panes, nil }
	t.Cleanup(func() {
		tmuxSendTextFn, tmuxSendKeysFn, tmuxCaptureFn, listPanesFn = prevText, prevKeys, prevCap, prevList
	})
	return spy
}

func (s *tmuxSpy) Calls() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.calls...)
}

func waitHost(t *testing.T, id string) *HostInfo {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if h := hostRegistry.Get(id); h != nil {
			return h
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("host %s never registered", id)
	return nil
}

func TestHostRegistersAndSendPathsPreferHost(t *testing.T) {
	srv := withHostDaemon(t)
	spy := stubTmux(t, []TmuxPane{{PaneID: "%9", SessionName: "work", CurrentCommand: "claude"}})
	sess := "claude:" + testClaudeSID
	paneRegistry.Register(&PaneRegistration{AgentID: sess, Tool: ToolClaude, SessionID: testClaudeSID, PaneID: "%9", Source: "hook"})

	u := newUserTerm(t)
	h, done := startHost(t, hostOptions{
		Argv: fakeAgentArgv(), Env: fakeAgentEnv(), SockDir: shortTempDir(t),
		In: u.slave, Out: u.slave, TTY: u.slave, Register: postHostRegistration, Remove: deleteHostRegistration,
	})
	u.waitFor(t, "READY")
	info := waitHost(t, h.opt.HostID)

	// Before the hook links the session, the tmux pane is used.
	if _, _, err := withTerminal(sess, func(tg string) error { return targetText(tg, "to tmux", true) }); err != nil {
		t.Fatal(err)
	}
	if c := spy.Calls(); len(c) != 1 || c[0] != "text %9 to tmux" {
		t.Fatalf("tmux calls: %v", c)
	}

	if !hostRegistry.LinkFromHook(h.opt.HostID, ToolClaude, testClaudeSID, info.AgentPid) {
		t.Fatal("link refused")
	}
	rt, _, err := withTerminal(sess, func(tg string) error { return targetText(tg, "to host", true) })
	if err != nil || rt.HostID != h.opt.HostID {
		t.Fatalf("route %+v %v", rt, err)
	}
	u.waitFor(t, "GOT:to host")
	if _, _, err := withTerminal(sess, func(tg string) error { return targetKeys(tg, []string{"k", "Enter"}) }); err != nil {
		t.Fatal(err)
	}
	u.waitFor(t, "GOT:k")
	var screen string
	if _, _, err := withTerminal(sess, func(tg string) error {
		var err error
		screen, err = targetCapture(tg, 0, false)
		return err
	}); err != nil || !strings.Contains(screen, "GOT:to host") {
		t.Fatalf("capture %q %v", screen, err)
	}
	if c := spy.Calls(); len(c) != 1 {
		t.Fatalf("tmux used while a host serves the session: %v", c)
	}

	// The question and queue paths see the host as the target.
	wp := watchedPane(sess)
	if wp.HostID != h.opt.HostID || !wp.Sendable || wp.Target() != hostTargetPrefix+h.opt.HostID {
		t.Fatalf("watched pane %+v", wp)
	}
	if err := (targetKeySender{}).Text(wp.Target(), "from question\r"); err != nil {
		t.Fatal(err)
	}
	u.waitFor(t, "GOT:from question")
	if err := (paneQueueWatched{}).send(sess, "queued"); err != nil {
		t.Fatal(err)
	}
	u.waitFor(t, "GOT:queued")

	// Local callers see the socket; remote callers never do.
	for _, remote := range []bool{false, true} {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/hosts", nil)
		if remote {
			req.Header.Set("X-Forwarded-For", "203.0.113.9")
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var body struct{ Hosts []HostInfo }
		_ = json.NewDecoder(resp.Body).Decode(&body)
		resp.Body.Close()
		if len(body.Hosts) != 1 || body.Hosts[0].SessionID != testClaudeSID {
			t.Fatalf("hosts: %+v", body.Hosts)
		}
		if remote == (body.Hosts[0].Socket != "") {
			t.Fatalf("remote=%v socket=%q", remote, body.Hosts[0].Socket)
		}
	}

	_, _ = u.master.Write([]byte("exit 0\r"))
	waitExit(t, done)
	deadline := time.Now().Add(5 * time.Second)
	for hostRegistry.Get(h.opt.HostID) != nil && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if hostRegistry.Get(h.opt.HostID) != nil {
		t.Fatal("host still registered after exit")
	}
	// With the host gone, the pane takes over again.
	if _, _, err := withTerminal(sess, func(tg string) error { return targetKeys(tg, []string{"Enter"}) }); err != nil {
		t.Fatal(err)
	}
	if c := spy.Calls(); c[len(c)-1] != "keys %9 Enter" {
		t.Fatalf("tmux calls: %v", c)
	}
}

func TestHostRegisterRejectsBadSocket(t *testing.T) {
	withHostDaemon(t)
	if _, err := hostRegistry.Register(hostRegistration{HostID: "abcd1234", Pid: os.Getpid(), Socket: "/etc/passwd"}); err == nil {
		t.Fatal("non-socket accepted")
	}
	if _, err := hostRegistry.Register(hostRegistration{HostID: "../x", Pid: os.Getpid()}); err == nil {
		t.Fatal("bad id accepted")
	}
}

func TestHookLinkRefusesNestedAgent(t *testing.T) {
	prev := procParentComm
	// 100 host agent (claude) <- 101 sh <- hook; 200 nested claude <- 150 zsh <- 100.
	tree := map[int][2]string{101: {"100", "sh"}, 100: {"50", "claude"}, 200: {"150", "claude"}, 150: {"100", "zsh"}}
	procParentComm = func(pid int) (int, string, bool) {
		e, ok := tree[pid]
		if !ok {
			return 0, "", false
		}
		p, _ := strconv.Atoi(e[0])
		return p, e[1], true
	}
	t.Cleanup(func() { procParentComm = prev })
	if !hookFromAgent(100, 101) || !hookFromAgent(100, 100) {
		t.Fatal("direct hook refused")
	}
	if hookFromAgent(100, 200) {
		t.Fatal("nested agent accepted")
	}
	if hookFromAgent(100, 999) {
		t.Fatal("unrelated process accepted")
	}
}

func TestOrchTerminalStartsTmuxAndTypesPrompt(t *testing.T) {
	srv := withHostDaemon(t)
	prevNew, prevExe, prevQuiet := tmuxNewSessionFn, agentMonitorExe, hostReadyQuietFor
	hostReadyQuietFor = 100 * time.Millisecond
	agentMonitorExe = func() (string, error) { return "/opt/it's/agent-monitor", nil }
	t.Cleanup(func() { tmuxNewSessionFn, agentMonitorExe, hostReadyQuietFor = prevNew, prevExe, prevQuiet })

	u := newUserTerm(t)
	sockDir := shortTempDir(t)
	var gotName, gotCwd, gotCmd string
	done := make(chan runResult, 1)
	tmuxNewSessionFn = func(name, cwd, command string) error {
		gotName, gotCwd, gotCmd = name, cwd, command
		id := strings.TrimPrefix(strings.Fields(command)[0], "AM_HOST_ID_ASSIGN=")
		_, d := startHost(t, hostOptions{
			Argv: fakeAgentArgv(), Env: fakeAgentEnv(), HostID: id, SockDir: sockDir,
			In: u.slave, Out: u.slave, TTY: u.slave, Register: postHostRegistration, Remove: deleteHostRegistration,
		})
		go func() { done <- <-d }()
		return nil
	}
	cwd := t.TempDir()
	body := fmt.Sprintf(`{"agent":"claude","cwd":%q,"args":["--model","x y"],"prompt":"do the thing"}`, cwd)
	resp, err := http.Post(srv.URL+"/api/orch/terminal", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Session, HostID string
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()
	if resp.StatusCode != 200 || out.Session != "am-"+out.HostID[:8] || gotName != out.Session || gotCwd != cwd {
		t.Fatalf("status %d out %+v name %q cwd %q", resp.StatusCode, out, gotName, gotCwd)
	}
	want := "AM_HOST_ID_ASSIGN=" + out.HostID + ` exec '/opt/it'\''s/agent-monitor' run 'claude' '--model' 'x y'`
	if gotCmd != want {
		t.Fatalf("command\n got %s\nwant %s", gotCmd, want)
	}
	u.waitFor(t, "GOT:do the thing")
	_, _ = u.master.Write([]byte("exit 0\r"))
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("host did not exit")
	}

	for _, bad := range []string{`{"agent":"bash","cwd":"/"}`, `{"agent":"claude","cwd":"relative"}`} {
		resp, _ := http.Post(srv.URL+"/api/orch/terminal", "application/json", strings.NewReader(bad))
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("%s: status %d", bad, resp.StatusCode)
		}
	}
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/orch/terminal", strings.NewReader(fmt.Sprintf(`{"agent":"codex","cwd":%q,"args":["--yolo"]}`, cwd)))
	req.Header.Set("X-Forwarded-For", "203.0.113.9")
	resp, _ = http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("remote args: status %d", resp.StatusCode)
	}
}
