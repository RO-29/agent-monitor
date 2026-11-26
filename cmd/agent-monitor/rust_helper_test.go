package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"
)

// rustHelperBin finds the built am-helper; tests that need it skip when it is not built.
func rustHelperBin(t *testing.T) string {
	t.Helper()
	p := os.Getenv("AM_HELPER_BIN")
	if p == "" {
		p, _ = filepath.Abs(filepath.Join("..", "..", "rust", "am-helper", "target", "release", rustHelperName))
	}
	if st, err := os.Stat(p); err != nil || st.Mode()&0o111 == 0 {
		if os.Getenv("AM_HELPER_REQUIRED") == "1" {
			t.Fatalf("am-helper not built (%s) and AM_HELPER_REQUIRED=1", p)
		}
		t.Skipf("am-helper not built (%s); run rust/am-helper/build.sh", p)
	}
	return p
}

func TestHelperForSwitch(t *testing.T) {
	d := t.TempDir()
	bin := filepath.Join(d, "agent-monitor")
	if err := os.WriteFile(bin, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := helperFor(bin); got != bin {
		t.Fatalf("no helper: got %s", got)
	}
	h := filepath.Join(d, rustHelperName)
	script := func(proto string) []byte {
		return []byte("#!/bin/sh\necho 'am-helper 0.3.0 (rust)'\necho 'protocol " + proto + "'\n")
	}
	if err := os.WriteFile(h, script(rustHelperProtocol), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := helperFor(bin); got != bin {
		t.Fatalf("non-executable helper used: %s", got)
	}
	_ = os.Chmod(h, 0o755)
	if got := helperFor(bin); got != h {
		t.Fatalf("helper not used: %s", got)
	}
	// A helper that speaks another protocol, or no version mode at all, is skipped.
	for _, body := range [][]byte{script("999"), []byte("#!/bin/sh\nexit 2\n")} {
		time.Sleep(10 * time.Millisecond)
		if err := os.WriteFile(h, body, 0o755); err != nil {
			t.Fatal(err)
		}
		if got := helperFor(bin); got != bin {
			t.Fatalf("mismatched helper used: %s", got)
		}
	}
	if err := os.WriteFile(h, script(rustHelperProtocol), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := helperFor(""); got != "" {
		t.Fatalf("empty bin: %q", got)
	}
}

// ── screen: the Rust renderer must give the same text as screenText ──

func TestRustScreenMatchesGo(t *testing.T) {
	bin := rustHelperBin(t)
	frags := []string{"a", "bc", "é", "✓", "日本", "\r", "\n", "\r\n", "\b", "\t", "\x07", "\x7f", "\x00", "\xff", "\xe2\x9c",
		"\x1b[2K", "\x1b[K", "\x1b[1K", "\x1b[1A", "\x1b[3B", "\x1b[5C", "\x1b[2D", "\x1b[E", "\x1b[2F", "\x1b[10G", "\x1b[3d",
		"\x1b[1;1H", "\x1b[5;7H", "\x1b[H", "\x1b[J", "\x1b[0J", "\x1b[2J", "\x1b[3J", "\x1b[?1049h", "\x1b[?1049l", "\x1b[?47h",
		"\x1b[?1047l", "\x1b[31m", "\x1b]0;t\x07", "\x1b]8;;u\x1b\\", "\x1bPq\x1b\\", "\x1b(B", "\x1b7", "\x1b8", "\x1bM", "\x1bD",
		"\x1bE", "\x1bc", "\x1b[2 q", "\x1b[>1u", "\x1b[99999999999999999999A", "\x1b[x", "\x1b", "\x1b[", "\x1b[?25l"}
	rng := rand.New(rand.NewSource(7))
	var cases []string
	for i := 0; i < 300; i++ {
		var b strings.Builder
		for j := 0; j < 5+rng.Intn(60); j++ {
			b.WriteString(frags[rng.Intn(len(frags))])
		}
		cases = append(cases, b.String())
	}
	cases = append(cases, strings.Repeat("line\r\n", 6000), strings.Repeat("x", 500))
	for i, in := range cases {
		cols, rows, n := 3+rng.Intn(100), 2+rng.Intn(40), rng.Intn(30)
		cmd := exec.Command(bin, "__screen", strconv.Itoa(cols), strconv.Itoa(rows), strconv.Itoa(n))
		cmd.Stdin = strings.NewReader(in)
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("case %d: %v", i, err)
		}
		if want := screenText([]byte(in), cols, rows, n); string(out) != want {
			t.Fatalf("case %d (%dx%d n=%d) input %q:\nrust %q\ngo   %q", i, cols, rows, n, in, out, want)
		}
	}
}

// ── MCP: the Rust server must print byte-identical JSON-RPC and send the daemon identical requests ──

type fakeDaemon struct {
	mu  sync.Mutex
	log []string
}

func (d *fakeDaemon) handler() http.Handler {
	panes := `{"panes":[
		{"agentId":"claude:abc123","tool":"claude","sessionId":"abc123def456","paneId":"%9","tmuxSocket":"/tmp/tmux,1,0","cwd":"/tmp/<w>&x","pid":42,"alias":"Bob","registeredAt":1700000000000,"lastSeenAt":1700000000001,"source":"hook"},
		{"agentId":"codex:zzz999","tool":"codex","sessionId":"zzz999aaa","paneId":"%3","cwd":"/w","registeredAt":1,"lastSeenAt":2,"source":"wrapper"},
		{"agentId":"codex:yyy888","tool":"codex","sessionId":"zzz999bbb","paneId":"%4","cwd":"/w2","registeredAt":1,"lastSeenAt":2,"source":"wrapper"}]}`
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		d.mu.Lock()
		d.log = append(d.log, r.Method+" "+r.URL.RequestURI()+" "+string(body))
		d.mu.Unlock()
		p, q := r.URL.Path, r.URL.Query()
		send := func(s string) { w.Header().Set("Content-Type", "application/json"); io.WriteString(w, s) }
		switch {
		case p == "/api/panes":
			send(panes)
		case p == "/api/panes/sweep":
			send(`{"ok":true}`)
		case p == "/api/tmux/panes":
			send(`{"ok":true,"panes":[{"PaneID":"%9","CurrentCommand":"claude"},{"PaneID":"%5","CurrentCommand":"node","CurrentPath":"/p"},{"PaneID":"%6","CurrentCommand":"zsh"},{"PaneID":"%8","CurrentCommand":"Codex-x","CurrentPath":"/q"}]}`)
		case strings.HasPrefix(p, "/api/session/pane-view/"):
			if strings.HasSuffix(p, "codex:zzz999") {
				send(`{"ok":false}`)
				return
			}
			send(`{"ok":true,"content":"line1\nline2 <b> lines=` + q.Get("lines") + `"}`)
		case strings.HasPrefix(p, "/api/session/send/"):
			send(`{"ok":true,"pane":"%3"}`)
		case p == "/api/talk/request":
			if strings.Contains(string(body), "codex:zzz999") && strings.Contains(string(body), "replyOnly") {
				send(`{"id":"t2"}`)
				return
			}
			send(`{"id":"t1"}`)
		case strings.HasSuffix(p, "/await"):
			send(`{"Status":"denied","reason":"user said no"}`)
		case strings.HasSuffix(p, "/await-reply"):
			if strings.Contains(p, "/t2/") {
				send(`{"status":"timeout"}`)
				return
			}
			send(`{"status":"replied","reply":"pong & <ok> ` + q.Get("timeout") + `"}`)
		case p == "/api/talk/t1/reply":
			send(`{"ok":true}`)
		case strings.HasPrefix(p, "/api/talk/"):
			http.Error(w, "talk not found", http.StatusNotFound)
		case p == "/api/permission/request":
			switch {
			case strings.Contains(string(body), `"toolName":"Fail"`):
				http.Error(w, "boom", 500)
			case strings.Contains(string(body), `"toolName":"Edit"`):
				send(`{"id":"p2"}`)
			case strings.Contains(string(body), `"toolName":"Upd"`):
				send(`{"id":"p3"}`)
			case strings.Contains(string(body), `"toolName":"Null"`):
				send(`{"id":"p4"}`)
			case strings.Contains(string(body), `"toolName":"NoID"`):
				send(`{"x":1}`)
			default:
				send(`{"id":"p1"}`)
			}
		case strings.HasPrefix(p, "/api/permission/wait/"):
			switch strings.TrimPrefix(p, "/api/permission/wait/") {
			case "p1":
				send(`{"behavior":"allow"}`)
			case "p2":
				send(`{"behavior":"deny","reason":"no <edits> & \u2028"}`)
			case "p3":
				send(`{"behavior":"allow","updatedInput":{"z":1,"a":[1.5,1e21,1e-7,100000000000000000000],"m":{"y":"\u0001","b":null}}}`)
			default:
				send(`null`)
			}
		case p == "/api/search":
			if strings.HasPrefix(q.Get("q"), "session:") {
				var hits []string
				for i := 0; i < 20; i++ {
					hits = append(hits, fmt.Sprintf(`{"session":"claude:broken","turn":%d,"ts":%d,"kind":"k%d","snippet":"s%d &lt;x&gt; <mark>m</mark>"}`, 1+i%4, 1700000000000+int64(i%3)*60000, i, i))
				}
				send(`{"total":20,"hits":[` + strings.Join(hits, ",") + `]}`)
				return
			}
			send(`{"total":42,"hits":[
				{"session":"claude:s1","agent":"claude","repo":"r1","cwd":"/c1","ts":1700000000000,"kind":"user","turn":2,"snippet":"&lt;b&gt; <mark>hit</mark> &amp; done &#39;q&#34;"},
				{"session":"claude:s1","agent":"claude","repo":"r1","cwd":"/c1","ts":1700000600000,"kind":"agent","turn":3,"snippet":"two"},
				{"session":"ot_9","agent":"codex","cwd":"/c2","ts":1700000700000,"kind":"command","turn":1,"span":"it_4","snippet":"three"},
				{"session":"claude:s1","agent":"claude","repo":"r1","cwd":"/c1","ts":1699000000000,"kind":"agent","turn":4,"snippet":"four"},
				{"session":"claude:s1","agent":"claude","repo":"r1","cwd":"/c1","ts":1699000000001,"kind":"agent","turn":5,"snippet":"five"},
				{"session":"codex:s3","agent":"codex","repo":"","cwd":"/c 3","ts":1700000800000,"kind":"error","turn":7,"drivenId":"ot_d","threadId":"thr2","snippet":"six é"}]}`)
		case p == "/api/v3/threads":
			send(`{"threads":[{"id":"claude:s1","title":"T1 <x>","display":""},{"id":"thr2","title":"x","display":"Disp & co"}]}`)
		case p == "/api/session/claude:s1/items":
			if q.Get("before") == "" {
				send(`{"runs":[{"id":"r5","index":5,"prompt":"p5","startedAt":1700000005000},{"id":"r6","index":6,"prompt":"p6","startedAt":1700000006000},null],
					"items":[{"runId":"r5","kind":"agent","text":"  ans5 <ok>  ","startedAt":2},{"runId":"r5","kind":"user","text":"q5","startedAt":1},null],"more":true,"earlier":100}`)
				return
			}
			send(`{"runs":[{"id":"r1","index":1,"prompt":"p1","startedAt":1700000001000},{"id":"r2","index":2,"prompt":"p2","startedAt":1700000002000},{"id":"r3","index":3,"prompt":"p3","startedAt":1700000003000},{"id":"r4","index":4,"prompt":"p4","startedAt":1700000004000}],
				"items":[{"runId":"r2","kind":"command","command":"ls -la","exitCode":1,"output":"a\nb\n","startedAt":5},{"runId":"r2","kind":"fileChange","files":[{"path":"x.go"},{"path":"y.go"}],"startedAt":6},
				{"runId":"r2","kind":"error","text":"bad","startedAt":7},{"runId":"r2","kind":"reasoning","text":"hmm","startedAt":8},{"runId":"r3","kind":"webSearch","command":"q","text":"t","startedAt":9}],"more":false}`)
		case p == "/api/session/claude:big/items":
			var items []string
			for i := 1; i <= 6; i++ {
				items = append(items, fmt.Sprintf(`{"runId":"b%d","kind":"agent","text":%q,"startedAt":1}`, i, strings.Repeat("ü", 1100)))
			}
			send(`{"runs":[{"id":"b1","index":1},{"id":"b2","index":2},{"id":"b3","index":3},{"id":"b4","index":4},{"id":"b5","index":5},{"id":"b6","index":6}],"items":[` + strings.Join(items, ",") + `]}`)
		case p == "/api/orch/threads/ot_x":
			send(`{"thread":{"id":"ot_x"},"runs":[{"id":"a","index":1,"prompt":"go","startedAt":0}],"items":[]}`)
		case p == "/api/orch/threads/ot_t/delegate":
			send(`{"threadId":"ot_c","thread":{"title":"Sub <1>","status":"running","cwd":"/w","worktree":{"path":"/w/wt","branch":"am/x","originBranch":"main"}}}`)
		case p == "/api/orch/threads/ot_t/tasks":
			send(`[{"q":"` + r.URL.RawQuery + `"}]`)
		default:
			http.Error(w, "no route "+p, 500)
		}
	})
}

func mcpScript() string {
	call := func(id int, name string, args string) string {
		return fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":%q,"arguments":%s}}`, id, name, args)
	}
	lines := []string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":"two","method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3.5,"method":"ping"}`,
		`{"jsonrpc":"2.0","id":4,"method":"nope"}`,
		`not json`, `[]`, ``, `null`,
		`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"bogus"}}`,
		`{"jsonrpc":"2.0","id":6,"method":"tools/call"}`,
		call(10, "who_am_i", `{}`),
		call(11, "list_agents", `{}`),
		call(12, "register_self", `{}`),
		call(13, "read_pane", `{"recipient":"bob","lines":7.9}`),
		call(14, "read_pane", `{}`),
		call(15, "read_pane", `{"recipient":"%3"}`),
		call(16, "read_pane", `{"recipient":"codex"}`),
		call(17, "read_pane", `{"recipient":"zzz999"}`),
		call(18, "read_pane", `{"recipient":"nobody \"q\""}`),
		call(19, "talk_to_agent", `{"recipient":"%3","message":"hi <there>"}`),
		call(20, "talk_to_agent", `{"recipient":"bob","message":"hi","confirm":true}`),
		call(21, "talk_to_agent", `{"recipient":"bob","message":"q?","wait_for_reply":true,"wait_seconds":30}`),
		call(22, "talk_to_agent", `{"recipient":"%3","message":"q?","wait_for_reply":true}`),
		call(23, "talk_to_agent", `{"recipient":"bob"}`),
		call(24, "reply_to_talk", `{"talk_id":"t1","message":"done & dusted"}`),
		call(25, "reply_to_talk", `{"talk_id":"bad","message":"x"}`),
		call(26, "reply_to_talk", `{"talk_id":"t1"}`),
		call(27, "permission_prompt", `{"tool_name":"Bash","tool_use_id":"tu1","input":{"command":"ls <x>","z":2,"a":{"q":1}}}`),
		call(28, "permission_prompt", `{"tool_name":"Edit","input":"notobj"}`),
		call(29, "permission_prompt", `{"tool_name":"Upd"}`),
		call(30, "permission_prompt", `{"tool_name":"Null"}`),
		call(31, "permission_prompt", `{"tool_name":"Fail"}`),
		call(32, "permission_prompt", `{"tool_name":"NoID"}`),
		call(40, "search_sessions", `{"query":"  hit  ","agent":"claude","repo":"my repo","since":"7d","limit":"3"}`),
		call(41, "search_sessions", `{"query":"x","limit":0}`),
		call(42, "search_sessions", `{}`),
		call(43, "session_context", `{"sessionId":"claude:s1","turn":2,"around":1}`),
		call(44, "session_context", `{"sessionId":"claude:s1"}`),
		call(45, "session_context", `{"sessionId":"ot_x","turn":9}`),
		call(46, "session_context", `{"sessionId":"claude:broken","turn":"2","around":9}`),
		call(47, "session_context", `{"sessionId":"claude:big","turn":3,"around":5}`),
		call(48, "session_context", `{"sessionId":"claude:s1","turn":30,"around":0}`),
		call(49, "session_context", `{}`),
		call(50, "delegate_task", `{"title":"t","prompt":"p <x>","agent":"claude","z":{"b":1,"a":2}}`),
		call(51, "wait_tasks", `{"threadIds":["ot_a","","ot_b"],"timeoutSec":5000}`),
		call(52, "task_status", `{}`),
		call(53, "wait_tasks", `{"threadIds":[]}`),
	}
	return strings.Join(lines, "\n") + "\n"
}

func runGoMCP(t *testing.T, url, script string) string {
	var out bytes.Buffer
	serveMCP(strings.NewReader(script), &out, url, mcpOrch{thread: "ot_t", depth: 1}, func(string, ...any) {})
	return out.String()
}

func runRustMCP(t *testing.T, bin, url, script string) (string, string) {
	cmd := exec.Command(bin, "mcp-perm-server")
	cmd.Env = append(os.Environ(), "AM_DAEMON_URL="+url+"/", "AM_ORCH_THREAD=ot_t", "AM_ORCH_DEPTH=1")
	cmd.Stdin = strings.NewReader(script)
	var errb bytes.Buffer
	cmd.Stderr = &errb
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("am-helper mcp-perm-server: %v\n%s", err, errb.String())
	}
	return string(out), errb.String()
}

func diffLines(t *testing.T, what, rust, goOut string) {
	t.Helper()
	r, g := strings.Split(rust, "\n"), strings.Split(goOut, "\n")
	for i := 0; i < len(r) || i < len(g); i++ {
		var a, b string
		if i < len(r) {
			a = r[i]
		}
		if i < len(g) {
			b = g[i]
		}
		if a != b {
			t.Fatalf("%s differ at line %d:\nrust %s\ngo   %s", what, i, a, b)
		}
	}
}

func TestRustMCPMatchesGo(t *testing.T) {
	bin := rustHelperBin(t)
	t.Setenv("TMUX_PANE", "%9")
	script := mcpScript()
	goD, rustD := &fakeDaemon{}, &fakeDaemon{}
	goSrv, rustSrv := httptest.NewServer(goD.handler()), httptest.NewServer(rustD.handler())
	defer goSrv.Close()
	defer rustSrv.Close()
	goOut := runGoMCP(t, goSrv.URL, script)
	rustOut, logs := runRustMCP(t, bin, rustSrv.URL, script)
	// Each side talks to its own daemon; make the URLs equal before comparing.
	rustOut = strings.ReplaceAll(rustOut, rustSrv.URL, goSrv.URL)
	if n := strings.Count(goOut, "\n"); n < 40 {
		t.Fatalf("go side answered only %d lines", n)
	}
	diffLines(t, "JSON-RPC output", rustOut, goOut)
	var rustReqs []string
	for _, l := range rustD.log {
		if !strings.HasPrefix(l, "GET /api/agents/registry") { // only the Rust helper reads the agent list at start
			rustReqs = append(rustReqs, l)
		}
	}
	diffLines(t, "daemon requests", strings.Join(rustReqs, "\n"), strings.Join(goD.log, "\n"))
	if !strings.Contains(logs, "[mcp-perm] starting; daemon="+rustSrv.URL+` orch="ot_t"`) || !strings.Contains(logs, "stdin closed; exiting") {
		t.Fatalf("stderr log: %s", logs)
	}
}

func TestRustMCPDaemonDownMatchesGo(t *testing.T) {
	bin := rustHelperBin(t)
	t.Setenv("TMUX_PANE", "%9")
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	url := "http://" + ln.Addr().String()
	ln.Close()
	call := func(id int, name, args string) string {
		return fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":%q,"arguments":%s}}`, id, name, args)
	}
	script := strings.Join([]string{
		call(1, "who_am_i", `{}`), call(2, "list_agents", `{}`), call(3, "permission_prompt", `{"tool_name":"Bash"}`),
		call(4, "search_sessions", `{"query":"x"}`), call(5, "session_context", `{"sessionId":"claude:x"}`),
		call(6, "reply_to_talk", `{"talk_id":"a","message":"b"}`), call(7, "delegate_task", `{"title":"t"}`),
		call(8, "wait_tasks", `{}`), call(9, "talk_to_agent", `{"recipient":"a","message":"b"}`), call(10, "register_self", `{}`),
	}, "\n") + "\n"
	goOut := runGoMCP(t, url, script)
	rustOut, _ := runRustMCP(t, bin, url, script)
	diffLines(t, "daemon-down output", rustOut, goOut)
}

func TestRustMCPRegisterSelfCreates(t *testing.T) {
	bin := rustHelperBin(t)
	var mu sync.Mutex
	var regBody map[string]any
	registered := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.URL.Path {
		case "/api/panes":
			if registered {
				io.WriteString(w, `{"panes":[{"agentId":"x:pane-77","tool":"x","sessionId":"pane-77","paneId":"%77","cwd":"/c","source":"register_self"}]}`)
				return
			}
			io.WriteString(w, `{"panes":[]}`)
		case "/api/pane/register":
			_ = json.NewDecoder(r.Body).Decode(&regBody)
			registered = true
			io.WriteString(w, `{"ok":true}`)
		}
	}))
	defer srv.Close()
	cmd := exec.Command(bin, "mcp-perm-server")
	cmd.Env = append(os.Environ(), "AM_DAEMON_URL="+srv.URL, "TMUX_PANE=%77")
	cmd.Stdin = strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"register_self"}}` + "\n")
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `✓ registered as x:pane-77`) || !strings.Contains(string(out), `"created":true`) {
		t.Fatalf("output %s", out)
	}
	if regBody["paneId"] != "%77" || regBody["sessionId"] != "pane-77" || regBody["source"] != "register_self" || regBody["pid"].(float64) <= 0 || regBody["tool"] == "" {
		t.Fatalf("register body %v", regBody)
	}
}

// ── session host and pty leader ──

type hostDaemon struct {
	mu       sync.Mutex
	regs     []hostRegistration
	deleted  []string
	announce []map[string]any
}

func (d *hostDaemon) server(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		defer d.mu.Unlock()
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/hosts":
			var reg hostRegistration
			_ = json.NewDecoder(r.Body).Decode(&reg)
			d.regs = append(d.regs, reg)
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/api/hosts/"):
			d.deleted = append(d.deleted, strings.TrimPrefix(r.URL.Path, "/api/hosts/"))
		case r.URL.Path == "/api/pane/announce":
			var m map[string]any
			_ = json.NewDecoder(r.Body).Decode(&m)
			d.announce = append(d.announce, m)
		}
		io.WriteString(w, `{"ok":true}`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (d *hostDaemon) firstReg(t *testing.T) hostRegistration {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		d.mu.Lock()
		if len(d.regs) > 0 {
			r := d.regs[0]
			d.mu.Unlock()
			return r
		}
		d.mu.Unlock()
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("no registration")
	return hostRegistration{}
}

// startRustHost runs `am-helper run <fake agent>` on the user terminal u.
func startRustHost(t *testing.T, bin string, u *userTerm, port, id string, attr *syscall.SysProcAttr) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(bin, append([]string{"run"}, fakeAgentArgv()...)...)
	cmd.Env = append(fakeAgentEnv(), "AGENT_MONITOR_HOME="+shortTempDir(t), "AGENT_MONITOR_PORT="+port, "AM_HOST_ID_ASSIGN="+id, "TMUX_PANE=%77", "AM_HOST_ID=outer")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = u.slave, u.slave, u.slave
	cmd.SysProcAttr = attr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	return cmd
}

func waitCode(t *testing.T, cmd *exec.Cmd) int {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("am-helper did not exit")
	}
	return cmd.ProcessState.ExitCode()
}

func rawSocketLine(t *testing.T, sock, line string) string {
	t.Helper()
	c, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_, _ = c.Write([]byte(line + "\n"))
	r, err := bufio.NewReader(c).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// hostScript drives a host through its socket and returns the responses that must match between Go and Rust.
func hostScript(t *testing.T, u *userTerm, sock, id string) []string {
	u.waitFor(t, "READY host="+id)
	_, _ = u.master.Write([]byte("hello\r"))
	u.waitFor(t, "GOT:hello")
	sockCall(t, sock, hostRequest{Op: "input", Data: "via-socket\r"})
	u.waitFor(t, "GOT:via-socket")
	sockCall(t, sock, hostRequest{Op: "keys", Keys: []string{"x", "y", "Enter"}})
	u.waitFor(t, "GOT:xy")
	sockCall(t, sock, hostRequest{Op: "input", Data: "color\r"})
	u.waitFor(t, "plain")
	time.Sleep(100 * time.Millisecond)
	var out []string
	for _, l := range []string{`{"op":"screen","lines":20}`, `{"op":"screen"}`, `{"op":"size"}`, `{"op":"keys","keys":["Enter","Nope"]}`,
		`{"op":"keys"}`, `{"op":"input"}`, `{"op":"what"}`, `not json`, `{"op":1}`, `{"OP":"size","Lines":"x"}`, `null`, `{"op":"size"}` + "\r"} {
		out = append(out, rawSocketLine(t, sock, l))
	}
	return out
}

func TestRustHostMatchesGoHost(t *testing.T) {
	bin := rustHelperBin(t)
	d := &hostDaemon{}
	srv := d.server(t)
	port := srv.URL[strings.LastIndex(srv.URL, ":")+1:]

	// Go host, in process.
	gu := newUserTerm(t)
	_ = pty.Setsize(gu.master, &pty.Winsize{Cols: 120, Rows: 40})
	gregs := make(chan hostRegistration, 16)
	_, gdone := startHost(t, hostOptions{Argv: fakeAgentArgv(), Env: fakeAgentEnv(), HostID: "gohost1234", SockDir: shortTempDir(t),
		In: gu.slave, Out: gu.slave, TTY: gu.slave, Register: func(r hostRegistration) error { gregs <- r; return nil }})
	var greg hostRegistration
	select {
	case greg = <-gregs:
	case <-time.After(5 * time.Second):
		t.Fatal("go host did not register")
	}
	goResp := hostScript(t, gu, greg.Socket, "gohost1234")
	_, _ = gu.master.Write([]byte("exit 7\r"))
	if r := waitExit(t, gdone); r.code != 7 {
		t.Fatalf("go host exit %+v", r)
	}

	// Rust host, as the session leader of the user terminal, like a tmux pane.
	u := newUserTerm(t)
	_ = pty.Setsize(u.master, &pty.Winsize{Cols: 120, Rows: 40})
	cmd := startRustHost(t, bin, u, port, "rshost1234", &syscall.SysProcAttr{Setsid: true, Setctty: true})
	reg := d.firstReg(t)
	rustResp := hostScript(t, u, reg.Socket, "rshost1234")
	for i := range goResp {
		g := strings.ReplaceAll(goResp[i], "gohost1234", "rshost1234")
		if rustResp[i] != g {
			t.Fatalf("socket response %d:\nrust %q\ngo   %q", i, rustResp[i], g)
		}
	}
	tio, err := unix.IoctlGetTermios(int(u.slave.Fd()), ioctlGetTermios)
	if err != nil || tio.Lflag&(unix.ICANON|unix.ECHO) != 0 {
		t.Fatalf("terminal not raw: %v %x", err, tio.Lflag)
	}
	// The kernel sends SIGWINCH to the host as the terminal's foreground group.
	_ = pty.Setsize(u.master, &pty.Winsize{Cols: 90, Rows: 33})
	time.Sleep(200 * time.Millisecond)
	_, _ = u.master.Write([]byte("size\r"))
	u.waitFor(t, "SIZE 90x33")
	if sz := sockCall(t, reg.Socket, hostRequest{Op: "size"}); sz.Cols != 90 || sz.Rows != 33 {
		t.Fatalf("size %+v", sz)
	}
	p := sockCall(t, reg.Socket, hostRequest{Op: "ping"})
	if p.HostID != "rshost1234" || p.OutBytes == 0 || p.AgentPid != reg.AgentPid || p.Agent != filepath.Base(os.Args[0]) {
		t.Fatalf("ping %+v reg %+v", p, reg)
	}
	ttyName, _ := os.Readlink("/dev/fd/" + strconv.Itoa(int(u.slave.Fd())))
	if reg.HostID != "rshost1234" || reg.Pid != cmd.Process.Pid || reg.AgentPid <= 0 || reg.AgentPid == reg.Pid || reg.Agent != filepath.Base(os.Args[0]) ||
		reg.TmuxPane != "%77" || !strings.HasSuffix(reg.Socket, "/hosts/rshost1234.sock") || reg.Cwd == "" || (ttyName != "" && reg.Tty != ttyName) {
		t.Fatalf("registration %+v (tty %q)", reg, ttyName)
	}
	if reg.Tty == "" || !strings.HasPrefix(reg.Tty, "/dev/") {
		t.Fatalf("tty %q", reg.Tty)
	}
	st, err := os.Stat(reg.Socket)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("socket mode: %v %v", err, st)
	}
	_, _ = u.master.Write([]byte("exit 7\r"))
	if code := waitCode(t, cmd); code != 7 {
		t.Fatalf("exit code %d", code)
	}
	if _, err := os.Stat(reg.Socket); !os.IsNotExist(err) {
		t.Fatalf("socket not removed: %v", err)
	}
	d.mu.Lock()
	deleted, ann := append([]string{}, d.deleted...), append([]map[string]any{}, d.announce...)
	d.mu.Unlock()
	if len(deleted) != 1 || deleted[0] != "rshost1234" {
		t.Fatalf("deleted %v", deleted)
	}
	if len(ann) != 1 || ann[0]["paneId"] != "%77" || ann[0]["tool"] != os.Args[0] || ann[0]["pid"].(float64) != float64(cmd.Process.Pid) {
		t.Fatalf("announce %v", ann)
	}
	// The session leader's exit revokes u.slave, so terminal restore is checked in the job-control test.
}

func TestRustHostCtrlZSuspendsAndResumes(t *testing.T) {
	bin := rustHelperBin(t)
	d := &hostDaemon{}
	srv := d.server(t)
	port := srv.URL[strings.LastIndex(srv.URL, ":")+1:]
	u := newUserTerm(t)
	// Its own process group in our session, as a shell job: the host can stop itself.
	cmd := startRustHost(t, bin, u, port, "jobhost1234", &syscall.SysProcAttr{Setpgid: true})
	u.waitFor(t, "READY host=jobhost1234")
	reg := d.firstReg(t)
	if reg.AgentPid == reg.Pid || reg.AgentPid <= 0 {
		t.Fatalf("agent pid %d, host %d", reg.AgentPid, reg.Pid)
	}
	if !termIsRaw(t, u.slave) {
		t.Fatal("terminal not raw while the agent runs")
	}
	_, _ = u.master.Write([]byte("tstp\r"))
	stopped := false
	for i := 0; i < 500 && !stopped; i++ {
		var ws syscall.WaitStatus
		pid, err := syscall.Wait4(cmd.Process.Pid, &ws, syscall.WUNTRACED|syscall.WNOHANG, nil)
		if err != nil {
			t.Fatal(err)
		}
		stopped = pid == cmd.Process.Pid && ws&0x7f == 0x7f
		time.Sleep(20 * time.Millisecond)
	}
	if !stopped {
		t.Fatalf("host did not stop; terminal %q", u.String())
	}
	if termIsRaw(t, u.slave) {
		t.Fatal("terminal still raw while the host is suspended")
	}
	if strings.Contains(u.String(), "RESUMED") {
		t.Fatal("agent ran on while suspended")
	}
	_ = pty.Setsize(u.master, &pty.Winsize{Cols: 120, Rows: 40})
	_ = syscall.Kill(cmd.Process.Pid, syscall.SIGCONT)
	u.waitFor(t, "RESUMED")
	if !termIsRaw(t, u.slave) {
		t.Fatal("raw mode not restored after resume")
	}
	_, _ = u.master.Write([]byte("size\r"))
	u.waitFor(t, "SIZE 120x40")
	_, _ = u.master.Write([]byte("exit 7\r"))
	if code := waitCode(t, cmd); code != 7 {
		t.Fatalf("exit code %d", code)
	}
	if termIsRaw(t, u.slave) {
		t.Fatal("terminal not restored after exit")
	}
}

func TestRustHostSignalsAndMissingAgent(t *testing.T) {
	bin := rustHelperBin(t)
	u := newUserTerm(t)
	cmd := exec.Command(bin, "run", "am-no-such-agent-xyz")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = u.slave, u.slave, u.slave
	cmd.Env = append(os.Environ(), "AGENT_MONITOR_HOME="+shortTempDir(t))
	if err := cmd.Run(); cmd.ProcessState.ExitCode() != 127 {
		t.Fatalf("missing agent: %v", err)
	}
	u.waitFor(t, `agent-monitor run: am-no-such-agent-xyz not found in PATH (exec: "am-no-such-agent-xyz": executable file not found in $PATH)`)

	// SIGTERM to the host reaches the agent's group; the host exits with the agent's status.
	d := &hostDaemon{}
	srv := d.server(t)
	u2 := newUserTerm(t)
	h := startRustHost(t, bin, u2, srv.URL[strings.LastIndex(srv.URL, ":")+1:], "sighost1234", &syscall.SysProcAttr{Setsid: true, Setctty: true})
	u2.waitFor(t, "READY host=sighost1234")
	_ = h.Process.Signal(syscall.SIGTERM)
	if code := waitCode(t, h); code != 128+int(syscall.SIGTERM) {
		t.Fatalf("exit code after SIGTERM %d", code)
	}
}

func TestRustRunWithoutTerminalExecs(t *testing.T) {
	bin := rustHelperBin(t)
	cmd := exec.Command(bin, "run", "sh", "-c", `echo "[$AM_HOST_ID][$AM_HOST_ID_ASSIGN][$0]"; exit 5`)
	cmd.Env = append(os.Environ(), "AM_HOST_ID=outer", "AM_HOST_ID_ASSIGN=x")
	out, _ := cmd.Output()
	if cmd.ProcessState.ExitCode() != 5 || strings.TrimSpace(string(out)) != "[][][sh]" {
		t.Fatalf("exit %d out %q", cmd.ProcessState.ExitCode(), out)
	}
	cmd = exec.Command(bin, "run")
	if out, _ := cmd.CombinedOutput(); cmd.ProcessState.ExitCode() != 2 || !strings.Contains(string(out), "usage: agent-monitor run <agent>") {
		t.Fatalf("usage: %d %q", cmd.ProcessState.ExitCode(), out)
	}
}

func TestRustLeader(t *testing.T) {
	bin := rustHelperBin(t)
	cmd := exec.Command(bin, "__pty-leader", "--")
	out, _ := cmd.CombinedOutput()
	if cmd.ProcessState.ExitCode() != 2 || strings.TrimSpace(string(out)) != "agent-monitor __pty-leader: agent required" {
		t.Fatalf("no agent: %d %q", cmd.ProcessState.ExitCode(), out)
	}
	// On a pty: the agent runs in its own foreground group, its pid goes to fd 3, and its status is the leader's.
	m, s, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	defer s.Close()
	go io.Copy(io.Discard, m)
	pr, pw, _ := os.Pipe()
	cmd = exec.Command(bin, "__pty-leader", "--", "/bin/sh", "-c", `ps -o pid=,pgid=,tpgid= -p $$; exit 9`)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = s, s, s
	cmd.ExtraFiles = []*os.File{pw}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pw.Close()
	b, _ := io.ReadAll(pr)
	agent, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	if code := waitCode(t, cmd); code != 9 {
		t.Fatalf("leader exit %d", code)
	}
	if agent <= 0 || agent == cmd.Process.Pid {
		t.Fatalf("agent pid %q leader %d", b, cmd.Process.Pid)
	}
	m2, s2, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer m2.Close()
	defer s2.Close()
	var errOut bytes.Buffer
	go io.Copy(&errOut, m2)
	cmd = exec.Command(bin, "__pty-leader", "--", "/nonexistent/agent")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = s2, s2, s2
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true}
	_ = cmd.Run()
	if cmd.ProcessState.ExitCode() != 126 {
		t.Fatalf("bad agent exit %d", cmd.ProcessState.ExitCode())
	}
}

// TestHelperExecRustHelper checks the switch in main: a helper mode execs am-helper beside the binary.
func TestHelperExecRustHelper(t *testing.T) {
	if os.Getenv("AM_EXEC_HELPER") == "1" {
		execRustHelper([]string{"version"}) // execs am-helper, or exits 1: there is no Go fallback
		fmt.Println("returned")
		os.Exit(0)
	}
	bin := rustHelperBin(t)
	d := t.TempDir()
	self := filepath.Join(d, "agent-monitor.test")
	b, err := os.ReadFile(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(self, b, 0o755); err != nil {
		t.Fatal(err)
	}
	run := func() (string, string, error) {
		cmd := exec.Command(self, "-test.run=^TestHelperExecRustHelper$")
		cmd.Env = append(os.Environ(), "AM_EXEC_HELPER=1")
		var stderr strings.Builder
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		return strings.TrimSpace(string(out)), stderr.String(), err
	}
	if out, stderr, err := run(); err == nil || out != "" || !strings.Contains(stderr, "is not next to") {
		t.Fatalf("no helper must fail clearly: out %q, stderr %q, err %v", out, stderr, err)
	}
	if err := os.Symlink(bin, filepath.Join(d, rustHelperName)); err != nil {
		t.Fatal(err)
	}
	if out, _, err := run(); err != nil || out != "am-helper 0.3.0 (rust)\nprotocol "+rustHelperProtocol {
		t.Fatalf("helper present: %q %v", out, err)
	}
}

// A panic in the host must not leave the user's terminal raw (panic = "abort" runs no destructors).
func TestRustPanicRestoresTerminal(t *testing.T) {
	bin := rustHelperBin(t)
	m, s, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	defer s.Close()
	go io.Copy(io.Discard, m)
	before, err := unix.IoctlGetTermios(int(s.Fd()), ioctlGetTermios)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "__panic-test")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = s, s, s
	_ = cmd.Run()
	if cmd.ProcessState.Success() {
		t.Fatal("self-test did not panic")
	}
	after, err := unix.IoctlGetTermios(int(s.Fd()), ioctlGetTermios)
	if err != nil {
		t.Fatal(err)
	}
	if after.Lflag != before.Lflag || after.Iflag != before.Iflag || after.Oflag != before.Oflag {
		t.Fatalf("terminal left changed: lflag %x -> %x", before.Lflag, after.Lflag)
	}
}
