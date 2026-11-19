package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type hookReq struct {
	Method, URI, CT, UA string
	Body                string
}

func (r hookReq) key() string { return r.Method + " " + r.URI + " ct=" + r.CT + " body=" + r.Body }

type hookRec struct {
	mu       sync.Mutex
	reqs     []hookReq
	approval http.HandlerFunc
}

func (h *hookRec) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	h.mu.Lock()
	h.reqs = append(h.reqs, hookReq{r.Method, r.RequestURI, r.Header.Get("Content-Type"), r.Header.Get("User-Agent"), string(body)})
	fn := h.approval
	h.mu.Unlock()
	if r.URL.Path == "/api/approvals/hook/codex" && fn != nil {
		fn(w, r)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (h *hookRec) take() []hookReq {
	h.mu.Lock()
	defer h.mu.Unlock()
	r := h.reqs
	h.reqs = nil
	return r
}

type hookEnv struct {
	dir, fake, home string
}

// newHookEnv lays out <dir>/bin/{claude,codex}-hook.sh with am-helper in <dir>, as in ~/agent-monitor.
func newHookEnv(t *testing.T, helper string) *hookEnv {
	t.Helper()
	d := t.TempDir()
	e := &hookEnv{dir: d, fake: filepath.Join(d, "fake"), home: filepath.Join(d, "home")}
	for _, p := range []string{filepath.Join(d, "bin"), e.fake, filepath.Join(e.home, ".am")} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cp := func(src, dst string) {
		b, err := os.ReadFile(src)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dst, b, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cp("bin/claude-hook.sh", filepath.Join(d, "bin", "claude-hook.sh"))
	cp("bin/codex-hook.sh", filepath.Join(d, "bin", "codex-hook.sh"))
	cp(helper, filepath.Join(d, rustHelperName))
	if err := os.WriteFile(filepath.Join(d, rustHelperName+".hooks"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	// Fake tmux: answers from files in $FAKE_TMUX_DIR.
	tmux := `#!/bin/bash
case "$1" in
  list-panes)
    if [ "$4" = '#{pane_id} #{pane_pid}' ]; then cat "$FAKE_TMUX_DIR/pidpanes" 2>/dev/null; else cat "$FAKE_TMUX_DIR/panes" 2>/dev/null; fi
    exit 0 ;;
  capture-pane) cat "$FAKE_TMUX_DIR/cap-${6#%}" 2>/dev/null; exit 0 ;;
esac
exit 1
`
	if err := os.WriteFile(filepath.Join(e.fake, "tmux"), []byte(tmux), 0o755); err != nil {
		t.Fatal(err)
	}
	return e
}

type hookRun struct {
	out  string
	code int
	dur  time.Duration
	reqs []hookReq
}

// run starts the hook the way an agent does: through `sh -c <command>` with the payload on stdin.
func (e *hookEnv) run(t *testing.T, rec *hookRec, script string, rust bool, payload string, env map[string]string) hookRun {
	t.Helper()
	cmd := exec.Command("/bin/sh", "-c", filepath.Join(e.dir, "bin", script))
	cmd.Env = []string{
		"PATH=" + e.fake + ":/usr/bin:/bin:/usr/sbin:/sbin",
		"HOME=" + e.home,
		"LANG=en_US.UTF-8",
		"AGENT_MONITOR_HOME=" + filepath.Join(e.home, ".am"),
		"AGENT_MONITOR_PANE_REGISTRY=" + filepath.Join(e.home, "no-registry.json"),
		"FAKE_TMUX_DIR=" + e.fake,
	}
	if !rust {
		cmd.Env = append(cmd.Env, "AGENT_MONITOR_RUST_HELPERS=0")
	}
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	cmd.Stdin = strings.NewReader(payload)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	start := time.Now()
	_ = cmd.Run()
	r := hookRun{out: out.String(), code: cmd.ProcessState.ExitCode(), dur: time.Since(start)}
	if rec != nil {
		r.reqs = rec.take()
	}
	return r
}

func compareHookRuns(t *testing.T, name string, sh, rs hookRun, shCode int) {
	t.Helper()
	if sh.code != shCode {
		t.Errorf("%s: shell exit %d, want %d", name, sh.code, shCode)
	}
	if rs.code != 0 {
		t.Errorf("%s: rust exit %d, want 0", name, rs.code)
	}
	if sh.out != rs.out {
		t.Errorf("%s: stdout differs\nshell: %q\nrust:  %q", name, sh.out, rs.out)
	}
	if len(sh.reqs) != len(rs.reqs) {
		t.Errorf("%s: %d shell requests, %d rust requests\nshell: %v\nrust:  %v", name, len(sh.reqs), len(rs.reqs), sh.reqs, rs.reqs)
		return
	}
	for i := range sh.reqs {
		if sh.reqs[i].key() != rs.reqs[i].key() {
			t.Errorf("%s: request %d differs\nshell: %s\nrust:  %s", name, i, sh.reqs[i].key(), rs.reqs[i].key())
		}
		if !strings.HasPrefix(sh.reqs[i].UA, "curl/") || !strings.HasPrefix(rs.reqs[i].UA, "Go-http-client") {
			t.Errorf("%s: request %d user agents %q / %q: the shim did not switch", name, i, sh.reqs[i].UA, rs.reqs[i].UA)
		}
	}
}

func claudePayload(event, sid string, extra string) string {
	return fmt.Sprintf(`{"session_id":"%s","transcript_path":"/Users/u/.claude/projects/p/%s.jsonl","cwd":"/Users/u/work dir","hook_event_name":"%s"%s}`, sid, sid, event, extra)
}

func TestRustClaudeHookMatchesShell(t *testing.T) {
	helper := rustHelperBin(t)
	rec := &hookRec{}
	srv := httptest.NewServer(rec)
	defer srv.Close()
	e := newHookEnv(t, helper)
	sid := "3f2a9c1e-8b7d-4e6f-a1b2-c3d4e5f60789"
	pane := map[string]string{"TMUX_PANE": "%12", "TMUX": "/private/tmp/tmux-501/default,4242,0"}
	with := func(m map[string]string, kv ...string) map[string]string {
		out := map[string]string{"AGENT_MONITOR_URL": srv.URL}
		for k, v := range m {
			out[k] = v
		}
		for i := 0; i+1 < len(kv); i += 2 {
			out[kv[i]] = kv[i+1]
		}
		return out
	}
	cases := []struct {
		name, payload string
		env           map[string]string
		shCode        int
	}{
		{"SessionStart in tmux", claudePayload("SessionStart", sid, `,"source":"startup"`), with(pane), 0},
		{"SessionStart outside tmux", claudePayload("SessionStart", sid, `,"source":"resume"`), with(nil), 0},
		{"SessionStart under host", claudePayload("SessionStart", sid, `,"source":"startup"`), with(pane, "AM_HOST_ID", "a1b2c3d4e5f60718"), 0},
		{"SessionStart bad host id", claudePayload("SessionStart", sid, ""), with(pane, "AM_HOST_ID", "bad id!"), 0},
		{"SessionEnd", claudePayload("SessionEnd", sid, `,"reason":"exit"`), with(pane, "AM_HOST_ID", "host_1-x"), 0},
		{"Notification", claudePayload("Notification", sid, `,"message":"Claude needs your permission to use Bash","notification_type":"permission_prompt"`), with(pane), 0},
		{"Stop", claudePayload("Stop", sid, `,"stop_hook_active":false`), with(pane), 0},
		{"SubagentStop", claudePayload("SubagentStop", sid, `,"stop_hook_active":false`), with(nil), 0},
		{"UserPromptSubmit", claudePayload("UserPromptSubmit", sid, `,"prompt":"fix \"the\" bug: café ✓\nline2"`), with(pane, "AM_HOST_ID", "h1"), 0},
		{"pretty printed", "{\n  \"session_id\" : \"" + sid + "\",\n  \"cwd\":\t\"/w\",\n  \"hook_event_name\": \"SessionStart\"\n}\n\n", with(pane), 0},
		{"cwd ends with colon space", `{"session_id":"s1","cwd":"/a: ","hook_event_name":"SessionStart"}`, with(pane), 0},
		{"port only", claudePayload("SessionEnd", sid, ""), map[string]string{"AGENT_MONITOR_PORT": strings.TrimPrefix(srv.URL, "http://127.0.0.1:")}, 0},
		{"dot segments in id", `{"session_id":"x/../../y","hook_event_name":"SessionEnd"}`, with(nil), 0},
		{"no cwd", `{"session_id":"s2","hook_event_name":"SessionStart"}`, with(pane), 1},
		{"no session id", `{"hook_event_name":"SessionEnd"}`, with(pane), 1},
		{"no event", `{"session_id":"s3"}`, with(pane), 1},
		{"empty payload", "", with(pane), 1},
	}
	for _, c := range cases {
		sh := e.run(t, rec, "claude-hook.sh", false, c.payload, c.env)
		rs := e.run(t, rec, "claude-hook.sh", true, c.payload, c.env)
		compareHookRuns(t, c.name, sh, rs, c.shCode)
		if len(rs.reqs) == 0 {
			t.Errorf("%s: no requests", c.name)
		}
		if c.env["AM_HOST_ID"] == "a1b2c3d4e5f60718" {
			// sh -c execs the hook, so $PPID is the agent itself: this test process.
			want := fmt.Sprintf("/events/claude?host=a1b2c3d4e5f60718&ppid=%d", os.Getpid())
			if rs.reqs[0].URI != want {
				t.Errorf("ppid: got %s, want %s", rs.reqs[0].URI, want)
			}
		}
	}
}

func TestRustClaudeHookDaemonDown(t *testing.T) {
	helper := rustHelperBin(t)
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	url := "http://" + l.Addr().String()
	l.Close()
	e := newHookEnv(t, helper)
	env := map[string]string{"AGENT_MONITOR_URL": url, "TMUX_PANE": "%1"}
	for _, ev := range []string{"SessionStart", "SessionEnd", "Stop"} {
		p := claudePayload(ev, "s", "")
		sh := e.run(t, nil, "claude-hook.sh", false, p, env)
		rs := e.run(t, nil, "claude-hook.sh", true, p, env)
		compareHookRuns(t, ev+" daemon down", sh, rs, 0)
		if rs.dur > 2*time.Second {
			t.Errorf("%s: rust took %v with the daemon down", ev, rs.dur)
		}
	}
}

func goHookBody(d approvalDecision) string {
	b, _ := json.Marshal(hookResponse(hookCodex, &Approval{}, d))
	return string(b) + "\n"
}

func TestRustCodexHookMatchesShell(t *testing.T) {
	helper := rustHelperBin(t)
	rec := &hookRec{}
	srv := httptest.NewServer(rec)
	defer srv.Close()
	e := newHookEnv(t, helper)
	sid := "019a1b2c-3d4e-7f60-8a9b-0c1d2e3f4a5b"
	writeFake := func(files map[string]string) {
		ents, _ := os.ReadDir(e.fake)
		for _, en := range ents {
			if en.Name() != "tmux" {
				os.Remove(filepath.Join(e.fake, en.Name()))
			}
		}
		for k, v := range files {
			if err := os.WriteFile(filepath.Join(e.fake, k), []byte(v), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	setHold := func(s string) {
		if err := os.WriteFile(filepath.Join(e.home, ".am", "approval-settings.json"), []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	tmuxEnv := map[string]string{"AGENT_MONITOR_URL": srv.URL, "TMUX": "/private/tmp/tmux-501/default,4242,0"}
	plain := map[string]string{"AGENT_MONITOR_URL": srv.URL}
	perm := func(input string) string {
		return fmt.Sprintf(`{"session_id":"%s","turn_id":"t1","transcript_path":"/x/rollout.jsonl","cwd":"/Users/u/w","hook_event_name":"PermissionRequest","model":"gpt-5","permission_mode":"default","tool_name":"Bash"%s}`, sid, input)
	}
	allow := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, goHookBody(approvalDecision{Behavior: "allow"}))
	}
	cases := []struct {
		name, payload string
		env           map[string]string
		fake          map[string]string
		hold          string
		approval      http.HandlerFunc
		wantOut       bool
	}{
		{name: "SessionStart no pane", payload: `{"session_id":"` + sid + `","cwd":"/w","hook_event_name":"SessionStart","source":"startup"}`, env: plain},
		{name: "SessionStart pane by capture", payload: `{"session_id":"` + sid + `","cwd":"/w","hook_event_name":"SessionStart"}`, env: tmuxEnv,
			fake: map[string]string{"pidpanes": "%9 99999999\n", "panes": "%1\n%2\n", "cap-1": "nothing\n", "cap-2": "codex session " + sid + "\n"}},
		{name: "UserPromptSubmit pane by pid", payload: `{"session_id":"` + sid + `","cwd":"/w","hook_event_name":"UserPromptSubmit","prompt":"hi"}`, env: tmuxEnv,
			fake: map[string]string{"pidpanes": fmt.Sprintf("%%7 %d\n", os.Getpid())}},
		{name: "Stop two panes match", payload: `{"session_id":"` + sid + `","cwd":"/w","hook_event_name":"Stop"}`, env: tmuxEnv,
			fake: map[string]string{"panes": "%1\n%2\n", "cap-1": sid + "\n", "cap-2": sid}},
		{name: "Stop under host", payload: `{"session_id":"` + sid + `","hook_event_name":"Stop"}`, env: map[string]string{"AGENT_MONITOR_URL": srv.URL, "AM_HOST_ID": "a1b2c3d4e5f60718"}},
		{name: "SessionEnd", payload: `{"session_id":"` + sid + `","hook_event_name":"SessionEnd"}`, env: plain},
		{name: "numeric session id", payload: `{"session_id":12.50,"cwd":"/w","hook_event_name":"SessionStart"}`, env: tmuxEnv,
			fake: map[string]string{"panes": "%3\n", "cap-3": "id 12.5 here\n"}},
		{name: "bad json", payload: `{"session_id":`, env: plain},
		{name: "empty", payload: "", env: plain},
		{name: "allow", payload: perm(`,"tool_input":{"command":"git   push\n origin main","description":"push"}`), env: tmuxEnv, hold: `{"holdSeconds":3}`, approval: allow, wantOut: true,
			fake: map[string]string{"panes": "%4\n", "cap-4": sid + "\n"}},
		{name: "deny with message", payload: perm(`,"tool_input":{"command":["bash","-lc","rm -rf 'x'"]}`), env: plain, approval: func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, goHookBody(approvalDecision{Behavior: "deny", Message: "no <b>&</b> café ✓ 😀"}))
		}, wantOut: true},
		{name: "description only", payload: perm(`,"tool_input":{"command":"","description":"` + strings.Repeat("ab ", 120) + `"}`), env: plain, approval: allow, wantOut: true},
		{name: "empty input", payload: perm(`,"tool_input":{}`), env: plain, approval: allow, wantOut: true},
		{name: "no input no tool", payload: `{"session_id":"` + sid + `","hook_event_name":"PermissionRequest"}`, env: plain, approval: allow, wantOut: true},
		{name: "scalar input", payload: perm(`,"tool_input":"éé x"`), env: plain, approval: allow, wantOut: true},
		{name: "html 200", payload: perm(`,"tool_input":{"command":"ls"}`), env: plain, approval: func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, "<html>agent-monitor</html>")
		}},
		{name: "ask behavior", payload: perm(`,"tool_input":{"command":"ls"}`), env: plain, approval: func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, `{"hookSpecificOutput":{"hookEventName":"PermissionRequest","decision":{"behavior":"ask"}}}`)
		}},
		{name: "500", payload: perm(`,"tool_input":{"command":"ls"}`), env: plain, approval: func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(500)
			io.WriteString(w, goHookBody(approvalDecision{Behavior: "allow"}))
		}},
	}
	for _, c := range cases {
		writeFake(c.fake)
		setHold(c.hold)
		rec.mu.Lock()
		rec.approval = c.approval
		rec.mu.Unlock()
		sh := e.run(t, rec, "codex-hook.sh", false, c.payload, c.env)
		rs := e.run(t, rec, "codex-hook.sh", true, c.payload, c.env)
		compareHookRuns(t, c.name, sh, rs, 0)
		if c.wantOut && !strings.Contains(rs.out, `"hookEventName":"PermissionRequest"`) {
			t.Errorf("%s: no decision printed: %q", c.name, rs.out)
		}
		if !c.wantOut && rs.out != "" {
			t.Errorf("%s: unexpected output %q", c.name, rs.out)
		}
		if len(rs.reqs) == 0 {
			t.Errorf("%s: no requests", c.name)
		}
		if c.name == "UserPromptSubmit pane by pid" && !strings.Contains(rs.reqs[len(rs.reqs)-1].Body, `"paneId":"%7"`) {
			t.Errorf("pane by pid: tmux-pane-id.sh did not get the agent pid: %v", rs.reqs)
		}
		if c.name == "Stop under host" && rs.reqs[0].URI != fmt.Sprintf("/events/codex?host=a1b2c3d4e5f60718&ppid=%d", os.Getpid()) {
			t.Errorf("codex ppid: %s", rs.reqs[0].URI)
		}
	}
}

// The approval waits holdSeconds+5; both sides give up then and print nothing.
func TestRustCodexHookApprovalTimeoutAndDaemonDown(t *testing.T) {
	if testing.Short() {
		t.Skip("waits 5 s per side")
	}
	helper := rustHelperBin(t)
	release := make(chan struct{})
	rec := &hookRec{approval: func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		case <-time.After(20 * time.Second):
		}
	}}
	srv := httptest.NewServer(rec)
	defer srv.Close()
	defer close(release)
	e := newHookEnv(t, helper)
	if err := os.WriteFile(filepath.Join(e.home, ".am", "approval-settings.json"), []byte(`{"holdSeconds":"0"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	p := `{"session_id":"s","hook_event_name":"PermissionRequest","tool_name":"Bash","tool_input":{"command":"ls"}}`
	env := map[string]string{"AGENT_MONITOR_URL": srv.URL}
	sh := e.run(t, rec, "codex-hook.sh", false, p, env)
	rs := e.run(t, rec, "codex-hook.sh", true, p, env)
	compareHookRuns(t, "timeout", sh, rs, 0)
	for _, r := range []hookRun{sh, rs} {
		if r.dur < 4500*time.Millisecond || r.dur > 10*time.Second {
			t.Errorf("timeout: run took %v, want about 5 s (shell %v rust %v)", r.dur, sh.dur, rs.dur)
		}
	}

	l, _ := net.Listen("tcp", "127.0.0.1:0")
	down := "http://" + l.Addr().String()
	l.Close()
	env = map[string]string{"AGENT_MONITOR_URL": down}
	sh = e.run(t, nil, "codex-hook.sh", false, p, env)
	rs = e.run(t, nil, "codex-hook.sh", true, p, env)
	compareHookRuns(t, "daemon down", sh, rs, 0)
	if rs.dur > 2*time.Second {
		t.Errorf("daemon down: rust took %v", rs.dur)
	}
}

func TestRustHookShimFallsBack(t *testing.T) {
	helper := rustHelperBin(t)
	rec := &hookRec{}
	srv := httptest.NewServer(rec)
	defer srv.Close()
	e := newHookEnv(t, helper)
	p := claudePayload("Stop", "s", "")
	r := e.run(t, rec, "claude-hook.sh", true, p, map[string]string{"AGENT_MONITOR_URL": srv.URL, "AGENT_MONITOR_RUST_HELPERS": "0"})
	if len(r.reqs) != 1 || !strings.HasPrefix(r.reqs[0].UA, "curl/") {
		t.Fatalf("RUST_HELPERS=0 did not keep the shell: %v", r.reqs)
	}
	os.Remove(filepath.Join(e.dir, rustHelperName))
	r = e.run(t, rec, "codex-hook.sh", true, `{"session_id":"s","hook_event_name":"Stop"}`, map[string]string{"AGENT_MONITOR_URL": srv.URL})
	if len(r.reqs) != 1 || !strings.HasPrefix(r.reqs[0].UA, "curl/") || r.code != 0 {
		t.Fatalf("missing am-helper did not fall back: %v", r.reqs)
	}
}
