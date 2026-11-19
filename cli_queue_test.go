package main

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func qop(op string, content any, extra ...string) string {
	m := map[string]any{"type": "queue-operation", "operation": op, "timestamp": "2026-11-17T10:00:00.000Z", "sessionId": "s"}
	if content != nil {
		m["content"] = content
	}
	for i := 0; i+1 < len(extra); i += 2 {
		m[extra[i]] = extra[i+1]
	}
	b, _ := json.Marshal(m)
	return string(b)
}

func quser(content any) string {
	b, _ := json.Marshal(map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": content}})
	return string(b)
}

func qtexts(items []CLIQueueItem) []string {
	out := []string{}
	for _, it := range items {
		out = append(out, it.Text)
	}
	return out
}

func parseLines(lines ...string) []CLIQueueItem {
	return parseCLIQueue(strings.NewReader(strings.Join(lines, "\n") + "\n"))
}

const tn = "<task-notification>\n<task-id>b1</task-id>\n<status>completed</status>\n</task-notification>"

func TestParseCLIQueue(t *testing.T) {
	cases := []struct {
		name  string
		lines []string
		want  []string
	}{
		{"enqueue only", []string{qop("enqueue", "one"), qop("enqueue", "two")}, []string{"one", "two"}},
		{"dequeue is fifo", []string{qop("enqueue", "one"), qop("enqueue", "two"), qop("dequeue", nil)}, []string{"two"}},
		{"dequeue on empty", []string{qop("dequeue", nil), qop("enqueue", "one")}, []string{"one"}},
		{"absorbed mid turn, not the head", []string{qop("enqueue", "one"), qop("enqueue", "two"), qop("remove", "two", "reason", "absorbed_mid_turn", "commandUuid", "u", "deliveryId", "d")}, []string{"one"}},
		{"delivered to agent", []string{qop("enqueue", "one"), qop("remove", "one", "reason", "delivered_to_agent")}, []string{}},
		{"cleared on cancel", []string{qop("enqueue", "one"), qop("enqueue", "two"), qop("remove", "one", "reason", "cleared_on_cancel"), qop("remove", "two", "reason", "cleared_on_cancel")}, []string{}},
		{"dropped by hook has no content", []string{qop("enqueue", "one"), qop("remove", nil, "reason", "dropped_by_hook")}, []string{"one"}},
		{"popAll takes each into the input", []string{qop("enqueue", "a\nb"), qop("enqueue", "c"), qop("popAll", "a\nb"), qop("popAll", "c")}, []string{}},
		{"remove of an unknown text", []string{qop("enqueue", "one"), qop("remove", "zzz", "reason", "absorbed_mid_turn")}, []string{"one"}},
		{"duplicate texts remove one at a time", []string{qop("enqueue", "go"), qop("enqueue", "go"), qop("remove", "go", "reason", "absorbed_mid_turn")}, []string{"go"}},
		{"unknown operation is ignored", []string{qop("enqueue", "one"), qop("peek", "one")}, []string{"one"}},
		{"system notifications interleave", []string{qop("enqueue", tn), qop("enqueue", "fix it"), qop("enqueue", tn+"2"), qop("dequeue", nil), qop("remove", tn+"2", "reason", "absorbed_mid_turn")}, []string{"fix it"}},
		// FIFO took the notification, but the transcript shows the user message was delivered.
		{"delivered user record corrects fifo", []string{qop("enqueue", tn), qop("enqueue", "fix it"), qop("dequeue", nil), quser("fix it")}, []string{tn}},
		{"the delivered record of a dequeued text keeps a duplicate", []string{qop("enqueue", "go"), qop("dequeue", nil), qop("enqueue", "go"), quser("go")}, []string{"go"}},
		{"text blocks match and the head goes back", []string{qop("enqueue", "one"), qop("enqueue", "two"), qop("dequeue", nil), quser([]any{map[string]any{"type": "text", "text": "two"}})}, []string{"one"}},
		{"an assistant record ends the check", []string{qop("enqueue", tn), qop("enqueue", "go"), qop("dequeue", nil), `{"type":"assistant","message":{"content":[]}}`, quser("go")}, []string{}},
		{"batch dequeue then delivered records", []string{qop("enqueue", "a"), qop("enqueue", "b"), qop("dequeue", nil), qop("dequeue", nil), quser("a"), quser("b")}, []string{}},
		{"tool results never match", []string{qop("enqueue", "one"), quser([]any{map[string]any{"type": "tool_result", "content": "one"}})}, []string{"one"}},
	}
	for _, c := range cases {
		got := qtexts(parseLines(c.lines...))
		if strings.Join(got, "|") != strings.Join(c.want, "|") {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

func TestParseCLIQueueItems(t *testing.T) {
	items := parseLines(qop("enqueue", tn), qop("enqueue", "hi"), qop("enqueue", nil), qop("enqueue", `<agent-message from="x">ping</agent-message>`), qop("enqueue", `<pasted_content id="1">text</pasted_content>`))
	kinds := []string{}
	for _, it := range items {
		kinds = append(kinds, it.Kind)
	}
	if strings.Join(kinds, ",") != "system,user,user,system,user" {
		t.Fatalf("kinds: %v", kinds)
	}
	if items[0].ID == items[1].ID || !strings.HasPrefix(items[1].ID, "cq") || items[1].At != 1794909600000 {
		t.Fatalf("ids and times: %+v", items[:2])
	}
	// Ids are the line offsets, so they stay the same across reads.
	again := parseLines(qop("enqueue", tn), qop("enqueue", "hi"))
	if again[1].ID != items[1].ID {
		t.Fatalf("unstable id %s vs %s", again[1].ID, items[1].ID)
	}
}

func TestCLIQueueOfIncremental(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	write := func(s string, appendTo bool) {
		flag := os.O_CREATE | os.O_WRONLY | os.O_TRUNC
		if appendTo {
			flag = os.O_CREATE | os.O_WRONLY | os.O_APPEND
		}
		f, err := os.OpenFile(path, flag, 0o644)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = f.WriteString(s)
		_ = f.Close()
	}
	write(qop("enqueue", "one")+"\n", false)
	if got, _ := cliQueueOf(path); strings.Join(qtexts(got), "|") != "one" {
		t.Fatalf("first read: %v", qtexts(got))
	}
	// A partial line waits for its newline.
	partial := qop("enqueue", "two")
	write(partial[:20], true)
	if got, _ := cliQueueOf(path); len(got) != 1 {
		t.Fatalf("partial line counted: %v", qtexts(got))
	}
	write(partial[20:]+"\n"+qop("dequeue", nil)+"\n", true)
	if got, _ := cliQueueOf(path); strings.Join(qtexts(got), "|") != "two" {
		t.Fatalf("after append: %v", qtexts(got))
	}
	if n := cliQueuedCount(path); n != 1 {
		t.Fatalf("count %d", n)
	}
	// A rewrite from the start is read again.
	write(qop("enqueue", "x")+"\n"+qop("enqueue", "y")+"\n", false)
	if got, _ := cliQueueOf(path); strings.Join(qtexts(got), "|") != "x|y" {
		t.Fatalf("after rewrite: %v", qtexts(got))
	}
}

// fakeCLITerm records what agent-monitor types and answers screens from a script.
type fakeCLITerm struct {
	tgt     string
	screens []string // one per screen() call; the last repeats
	n       int
	sent    [][]string
	typed   []string
	sess    *Session
}

func (f *fakeCLITerm) target(string) string { return f.tgt }
func (f *fakeCLITerm) screen(string) (string, error) {
	s := dropANSI(f.screens[min(f.n, len(f.screens)-1)])
	f.n++
	return s, nil
}
func (f *fakeCLITerm) typedScreen(string) (string, error) {
	return dropDim(f.screens[min(max(f.n-1, 0), len(f.screens)-1)]), nil
}
func (f *fakeCLITerm) keys(_ string, k []string) error { f.sent = append(f.sent, k); return nil }
func (f *fakeCLITerm) text(_ string, s string, enter bool) error {
	if enter {
		s += "⏎"
	}
	f.typed = append(f.typed, s)
	return nil
}
func (f *fakeCLITerm) session(string) *Session { return f.sess }

const cliRule = "────────────────────────────────────────"

func cliScreen(input, status string) string {
	return "  41. Forty-one is a prime number.\n" + cliRule + "\n❯ " + input + "\n" + cliRule + "\n  ⏸ manual mode on" + status + "\n"
}

var (
	scrQueued = "❯ one\n  ⎿  Queued · ctrl+enter to send now\n" + cliScreen("Press up to edit queued messages", " · esc to interrupt")
	scrPulled = cliScreen("one\n  two", "")
	scrEmpty  = cliScreen("", " · esc to interrupt")
	scrIdle   = "✻ Brewed for 29s\n" + cliScreen("", " · ? for shortcuts")
	scrBash   = "  41. Forty-one is a prime number.\n" + cliRule + "\n! \n" + cliRule + "\n  ! for shell mode\n"
	scrPrompt = "Do you want to proceed?\n❯ 1. Yes\n  2. No\n\nEsc to cancel\n"
)

func setupCLITest(t *testing.T, lines ...string) (string, *fakeCLITerm) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	t.Setenv("AGENT_MONITOR_HOME", t.TempDir())
	id := "abcdef12-3456-4789-8abc-def0123456aa"
	dir := filepath.Join(cfg, "projects", "-w")
	_ = os.MkdirAll(dir, 0o755)
	if err := os.WriteFile(filepath.Join(dir, id+".jsonl"), []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	prevStore, prevIO, prevPause, prevQ, prevSettle := store, cliIO, cliPause, queueMgr, cliBoxSettle
	t.Cleanup(func() {
		store, cliIO, cliPause, queueMgr, cliBoxSettle = prevStore, prevIO, prevPause, prevQ, prevSettle
	})
	cliBoxSettle = 400 * time.Millisecond
	prevDim := cliSeesDim
	t.Cleanup(func() { cliSeesDim = prevDim })
	cliSeesDim = func(string) bool { return true }
	store, queueMgr, cliPause = nil, nil, 0
	cliBangs.mu.Lock()
	cliBangs.m = map[string][]cliBang{}
	cliBangs.mu.Unlock()
	cliSentBusy.mu.Lock()
	cliSentBusy.m = map[string][]string{}
	cliSentBusy.mu.Unlock()
	f := &fakeCLITerm{tgt: "host:h1", sess: &Session{State: StateRunning, RecentEvents: []RecentEvent{{Kind: "UserPromptSubmit"}}}}
	cliIO = f
	return "claude:" + id, f
}

func cliPost(id, action, body string) (int, map[string]any) {
	rec := httptest.NewRecorder()
	watchedSessionRoute(rec, httptest.NewRequest("POST", "/x", strings.NewReader(body)), id+"/cli-queue/"+action)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func cliGet(id, q string) CLIQueueView {
	rec := httptest.NewRecorder()
	watchedSessionRoute(rec, httptest.NewRequest("GET", "/x"+q, nil), id+"/cli-queue")
	var out CLIQueueView
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return out
}

func TestCLIQueueViewRoute(t *testing.T) {
	id, f := setupCLITest(t, qop("enqueue", tn), qop("enqueue", "one"), qop("enqueue", "two"))
	f.screens = []string{scrQueued}
	v := cliGet(id, "")
	if !v.Supported || !v.Reachable || strings.Join(qtexts(v.Items), "|") != "one|two" || v.Hidden != 1 || v.CanRemove || v.RemoveWhy == "" {
		t.Fatalf("view: %+v", v)
	}
	if v := cliGet(id, "?all=1"); len(v.Items) != 3 || v.Items[0].Kind != "system" || v.Hidden != 0 {
		t.Fatalf("all: %+v", v)
	}
	// An idle CLI holds no queue: the records left are from an earlier process.
	f.screens, f.n = []string{scrIdle}, 0
	f.sess = &Session{State: StateAwaitingInput, RecentEvents: []RecentEvent{{Kind: "Stop"}}}
	if v := cliGet(id, ""); len(v.Items) != 0 {
		t.Fatalf("idle CLI shows a queue: %+v", v)
	}
	// Unreachable: the queue shows, the actions say why they cannot run.
	f.tgt, f.sess = "", &Session{State: StateRunning}
	if v := cliGet(id, ""); v.Reachable || len(v.Items) != 2 || v.CanRemove || !strings.Contains(v.RemoveWhy, "cannot reach") {
		t.Fatalf("unreachable: %+v", v)
	}
}

func TestCLISendTypesIntoTheCLI(t *testing.T) {
	id, f := setupCLITest(t, qop("enqueue", "one"))
	f.screens = []string{scrQueued}
	code, out := cliPost(id, "send", `{"text":"also check the tests\n"}`)
	if code != 200 || out["queued"] != true || len(f.typed) != 1 || f.typed[0] != "also check the tests⏎" {
		t.Fatalf("busy send: %d %v %q", code, out, f.typed)
	}
	f.screens, f.n = []string{scrIdle}, 0
	f.sess = &Session{State: StateAwaitingInput, RecentEvents: []RecentEvent{{Kind: "Stop"}}}
	if code, out := cliPost(id, "send", `{"text":"next"}`); code != 200 || out["queued"] != false || len(f.typed) != 2 {
		t.Fatalf("idle send: %d %v", code, out)
	}
	f.screens, f.n = []string{scrPrompt}, 0
	if code, _ := cliPost(id, "send", `{"text":"x"}`); code != 409 || len(f.typed) != 2 {
		t.Fatalf("a prompt on screen must refuse: %d %q", code, f.typed)
	}
	f.tgt = ""
	if code, _ := cliPost(id, "send", `{"text":"x"}`); code != 409 || len(f.typed) != 2 {
		t.Fatalf("unreachable must refuse so the web queues it: %d", code)
	}
	if code, _ := cliPost(id, "send", `{"text":"  "}`); code != 400 {
		t.Fatalf("empty text: %d", code)
	}
}

func TestCLISendNowKeys(t *testing.T) {
	id, f := setupCLITest(t, qop("enqueue", "one"), qop("enqueue", "two"))
	f.screens = []string{scrQueued}
	code, _ := cliPost(id, "send-now", `{}`)
	if code != 200 || len(f.sent) != 1 || strings.Join(f.sent[0], " ") != "Escape" {
		t.Fatalf("send-now: %d %v", code, f.sent)
	}
	// A just-sent message holds the box for a moment: the action waits for the hint.
	f.sent, f.screens, f.n = nil, []string{scrQueued, scrPulled, scrPulled, scrQueued}, 0
	if code, _ := cliPost(id, "send-now", `{}`); code != 200 || len(f.sent) != 1 {
		t.Fatalf("after a moment: %d %v", code, f.sent)
	}
	// No queue hint on screen (the input box holds text): no keys.
	f.sent, f.screens, f.n = nil, []string{scrPulled}, 0
	if code, _ := cliPost(id, "send-now", `{}`); code != 409 || len(f.sent) != 0 {
		t.Fatalf("without the hint: %d %v", code, f.sent)
	}
	f.screens, f.n = []string{scrQueued + scrPrompt}, 0
	if code, _ := cliPost(id, "send-now", `{}`); code != 409 || len(f.sent) != 0 {
		t.Fatalf("with a prompt: %d %v", code, f.sent)
	}
}

func TestCLIEditKeys(t *testing.T) {
	id, f := setupCLITest(t, qop("enqueue", tn), qop("enqueue", "one"), qop("enqueue", "two"))
	// view, ready check, after Up, after the backspaces, the view after.
	f.screens = []string{scrQueued, scrQueued, scrPulled, scrEmpty}
	code, out := cliPost(id, "edit", `{"id":""}`)
	if code != 200 || out["text"] != "one\ntwo" {
		t.Fatalf("edit: %d %v", code, out)
	}
	if len(f.sent) != 2 || strings.Join(f.sent[0], " ") != "Up" || len(f.sent[1]) != len("one\ntwo")+8 || f.sent[1][0] != "BSpace" {
		t.Fatalf("edit keys: %v", f.sent)
	}
	// Up that does not show the text stops before any backspace.
	f.sent, f.screens, f.n = nil, []string{scrQueued, scrQueued, scrQueued}, 0
	if code, _ := cliPost(id, "edit", `{}`); code != 409 || len(f.sent) != 1 {
		t.Fatalf("Up not seen: %d %v", code, f.sent)
	}
	// Text left in the box after the backspaces is an error.
	f.sent, f.screens, f.n = nil, []string{scrQueued, scrQueued, scrPulled, scrPulled}, 0
	if code, _ := cliPost(id, "edit", `{}`); code != 500 {
		t.Fatalf("text left: %d", code)
	}
}

func TestCLIRemoveKeys(t *testing.T) {
	id, f := setupCLITest(t, qop("enqueue", "one"), qop("enqueue", "two"))
	f.screens = []string{scrQueued}
	if code, _ := cliPost(id, "remove", `{}`); code != 409 || len(f.sent) != 0 {
		t.Fatalf("two queued: remove must refuse: %d %v", code, f.sent)
	}
	id, f = setupCLITest(t, qop("enqueue", "one"))
	f.screens = []string{scrQueued, scrQueued, scrQueued, cliScreen("one", ""), scrEmpty}
	v := cliGet(id, "")
	code, out := cliPost(id, "remove", `{"id":"`+v.Items[0].ID+`"}`)
	if code != 200 || out["text"] != nil && out["text"] != "" || len(f.sent) != 2 || len(f.sent[1]) != 3+8 {
		t.Fatalf("remove: %d %v %v", code, out, f.sent)
	}
	f.screens, f.n = []string{scrQueued}, 0
	if code, _ := cliPost(id, "remove", `{"id":"cq999"}`); code != 404 {
		t.Fatalf("unknown id: %d", code)
	}
}

func TestCLIQueueCodexAndOthers(t *testing.T) {
	_, f := setupCLITest(t)
	_ = f
	// Codex records no queue in its rollout: the view says so.
	v, _, err := cliQueueView("codex:01a1e857-8549-7d81-b014-f745ed500a21", false)
	if err == nil && v.Supported {
		t.Fatalf("codex: %+v", v)
	}
	if code, _ := cliPost("claude:missing-session", "send-now", `{}`); code != 400 {
		t.Fatalf("unknown session: %d", code)
	}
}

func TestCLIQueueBangDeliveredAndUserFirst(t *testing.T) {
	rec := func(op, content string) string {
		b, _ := json.Marshal(map[string]any{"type": "queue-operation", "operation": op, "content": content, "timestamp": "2026-11-18T20:44:14Z"})
		return string(b)
	}
	deq := `{"type":"queue-operation","operation":"dequeue","timestamp":"2026-11-18T20:44:21Z"}`
	user := func(text string) string {
		b, _ := json.Marshal(map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": text}})
		return string(b)
	}
	lines := []string{
		rec("enqueue", "<task-notification>\n<task-id>x</task-id>\n</task-notification>"),
		rec("enqueue", "bash ~/agent-monitor/ios/resign.sh --force"),
		deq,
		user("<bash-input>bash ~/agent-monitor/ios/resign.sh --force</bash-input>"),
	}
	for _, it := range parseCLIQueue(strings.NewReader(strings.Join(lines, "\n") + "\n")) {
		if it.Kind == "user" {
			t.Fatalf("a delivered ! command is still queued: %+v", it)
		}
	}
	if !cliBoxEmpty("────────────────────────\n❯ \n────────────────────────\n") || cliBoxEmpty("────────────────────────\n❯ typing\n────────────────────────\n") || cliBoxEmpty("no box here") {
		t.Fatal("cliBoxEmpty")
	}
}

func TestCLIQueueStaleWhileWorking(t *testing.T) {
	id, f := setupCLITest(t, qop("enqueue", "stale"))
	f.screens = []string{scrEmpty}
	if v := cliGet(id, ""); len(v.Items) != 0 {
		t.Fatalf("an empty input box with no queue hint must mean nothing is queued: %+v", v.Items)
	}
	f.screens, f.n = []string{scrQueued}, 0
	if v := cliGet(id, ""); len(v.Items) != 1 {
		t.Fatalf("a shown queue must keep the item: %+v", v.Items)
	}
}

// A message sent a moment before sits in the box until Claude Code queues it; the ! command waits for that.
func TestCLISendBangWaitsForJustSentMessage(t *testing.T) {
	id, f := setupCLITest(t)
	f.sess = &Session{Tool: ToolClaude, State: StateRunning, RecentEvents: []RecentEvent{{Kind: "UserPromptSubmit"}}}
	inBox := cliScreen("3 issues there is notification", " · esc to interrupt")
	f.screens = []string{inBox, inBox, scrQueued, scrBash}
	code, out := cliPost(id, "send", `{"text":"! bash x.sh"}`)
	if code != 200 || out["queued"] != true || strings.Join(f.typed, "|") != "!|bash x.sh⏎" {
		t.Fatalf("got %d %v %q", code, out, f.typed)
	}
}

func TestCLISendBangCommand(t *testing.T) {
	id, f := setupCLITest(t)
	f.screens = []string{scrIdle, scrBash}
	f.sess = &Session{Tool: ToolClaude, State: StateAwaitingInput, RecentEvents: []RecentEvent{{Kind: "Stop"}}}
	// "!" goes alone so Claude Code switches to bash mode, then the command and Enter.
	code, out := cliPost(id, "send", `{"text":"! git status --short\n"}`)
	if code != 200 || out["ok"] != true || strings.Join(f.typed, "|") != "!|git status --short⏎" {
		t.Fatalf("claude bang: %d %v %q", code, out, f.typed)
	}
	if code, _ := cliPost(id, "send", `{"text":"!  "}`); code != 400 || len(f.typed) != 2 {
		t.Fatalf("empty bang: %d %q", code, f.typed)
	}
	// Text that only contains a ! later is a normal message.
	f.typed, f.screens, f.n = nil, []string{scrIdle}, 0
	if code, _ := cliPost(id, "send", `{"text":"why did !ls fail"}`); code != 200 || strings.Join(f.typed, "|") != "why did !ls fail⏎" {
		t.Fatalf("plain: %d %q", code, f.typed)
	}

	// Half-typed text in the box would turn "!cmd" into prompt text: refuse it.
	f.typed, f.screens, f.n = nil, []string{cliScreen("refactor the", " · ? for shortcuts")}, 0
	if code, _ := cliPost(id, "send", `{"text":"!git reset --hard"}`); code != 409 || len(f.typed) != 0 {
		t.Fatalf("busy box: %d %q", code, f.typed)
	}
	// A panel below the box with its own rule is not box text.
	f.typed, f.screens, f.n = nil, []string{cliScreen("", " · ? for shortcuts") + cliRule + "\n  ◯ general-purpose  Checking things\n", scrBash}, 0
	if code, _ := cliPost(id, "send", `{"text":"!ls"}`); code != 200 || strings.Join(f.typed, "|") != "!|ls⏎" {
		t.Fatalf("panel below: %d %q", code, f.typed)
	}
	// A dim suggestion in the box is not typed text.
	f.typed, f.screens, f.n = nil, []string{cliScreen("\x1b[2mTry \"fix typecheck errors\"\x1b[0m", ""), scrBash}, 0
	if code, _ := cliPost(id, "send", `{"text":"!ls"}`); code != 200 || strings.Join(f.typed, "|") != "!|ls⏎" {
		t.Fatalf("dim suggestion: %d %q", code, f.typed)
	}
	// The queued-messages hint is a placeholder, not typed text.
	f.typed, f.screens, f.n = nil, []string{scrQueued, scrBash}, 0
	if code, _ := cliPost(id, "send", `{"text":"!ls"}`); code != 200 || strings.Join(f.typed, "|") != "!|ls⏎" {
		t.Fatalf("hint box: %d %q", code, f.typed)
	}
	f.typed, f.screens, f.n = nil, []string{scrIdle}, 0
	if code, _ := cliPost(id, "send", `{"text":"!ls","attachments":["up_0123456789"]}`); code != 400 || len(f.typed) != 0 {
		t.Fatalf("bang with files: %d %q", code, f.typed)
	}

	// A lost "!" is typed once more; a "!" taken as text is erased and nothing is sent.
	f.typed, f.sent, f.screens, f.n = nil, nil, []string{scrIdle, scrIdle, scrIdle, scrBash}, 0
	if code, _ := cliPost(id, "send", `{"text":"!ls"}`); code != 200 || strings.Join(f.typed, "|") != "!|!|ls⏎" {
		t.Fatalf("lost key: %d %q", code, f.typed)
	}
	f.typed, f.sent, f.screens, f.n = nil, nil, []string{scrIdle, cliScreen("!", "")}, 0
	if code, _ := cliPost(id, "send", `{"text":"!ls"}`); code != 409 || strings.Join(f.typed, "|") != "!" || len(f.sent) != 1 || f.sent[0][0] != "BSpace" {
		t.Fatalf("literal: %d %q %v", code, f.typed, f.sent)
	}
	f.typed, f.screens, f.n = nil, []string{scrIdle}, 0
	if code, _ := cliPost(id, "send", `{"text":"!ls"}`); code != 409 || strings.Join(f.typed, "|") != "!|!" {
		t.Fatalf("never: %d %q", code, f.typed)
	}
	// A slow first "!" plus the retry gives "! !": both are erased and nothing is sent.
	bang2 := strings.Replace(scrBash, "\n! \n", "\n! !\n", 1)
	f.typed, f.sent, f.screens, f.n = nil, nil, []string{scrIdle, scrIdle, scrIdle, bang2}, 0
	if code, _ := cliPost(id, "send", `{"text":"!ls"}`); code != 409 || strings.Join(f.typed, "|") != "!|!" || len(f.sent) != 1 || len(f.sent[0]) != 2 {
		t.Fatalf("double bang: %d %q %v", code, f.typed, f.sent)
	}

	// With messages queued, bash mode shows the dim hint after the "!": that is not a second "!".
	bashHint := strings.Replace(scrBash, "\n! \n", "\n! \x1b[2mPress up to edit queued messages\x1b[0m\n", 1)
	f.typed, f.sent, f.screens, f.n = nil, nil, []string{scrQueued, bashHint}, 0
	if code, _ := cliPost(id, "send", `{"text":"!echo four"}`); code != 200 || strings.Join(f.typed, "|") != "!|echo four⏎" || len(f.sent) != 0 {
		t.Fatalf("hint after !: %d %q %v", code, f.typed, f.sent)
	}
	f.screens, f.n = []string{scrIdle}, 0

	// Codex runs "!cmd" as a user shell command too (rollouts record source user_shell).
	f.typed = nil
	f.sess = &Session{Tool: ToolCodex, State: StateAwaitingInput}
	if code, _ := cliPost("codex:0199aaaa-bbbb-7ccc-8ddd-eeeeffff0000", "send", `{"text":"!make test"}`); code != 200 || strings.Join(f.typed, "|") != "!|make test⏎" {
		t.Fatalf("codex bang: %d %q", code, f.typed)
	}
}

// dropANSI keeps dim text, as a plain tmux capture does.
func dropANSI(s string) string { return dropDim(strings.ReplaceAll(s, "\x1b[2m", "")) }

func TestDropDim(t *testing.T) {
	in := "\x1b[39m❯ \x1b[2mTry \"x\"\x1b[0m and \x1b[1;2mdim\x1b[22mtyped"
	if got := dropDim("\x1b[38;5;2mgreen\x1b[39m \x1b[38;2;2;2;2mrgb"); got != "green rgb" {
		t.Fatalf("colour read as dim: %q", got)
	}
	if got := dropDim(in); got != "❯  and typed" {
		t.Fatalf("%q", got)
	}
}

// Screens captured from Claude Code 2.1.327, which writes enqueue records with no text.
const scr327Queued = "     (ctrl+b ctrl+b (twice) to run in background)\n❯ queued three\n  ⎿  Queued · ctrl+x ctrl+s to send now\n" +
	"──────────────────────────────────────────────────\n❯ Press up to edit queued messages\n──────────────────────────────────────────────────\n" +
	"  Haiku 5.5 │ agent-monitor-v3\n  ⏵⏵ auto mode on (shift+tab to cycle) · esc to interrupt\n"
const scr327Idle = "⏺ one\n✻ Cogitated for 1s · done 10:34 AM\n──────────────────────────────────────────────────\n❯ \n" +
	"──────────────────────────────────────────────────\n  Haiku 5.5 │ agent-monitor-v3\n  ⏵⏵ auto mode on (shift+tab to cycle)\n"

func TestCLI327QueuedTextAndPhase(t *testing.T) {
	if got := screenQueued(scr327Queued); len(got) != 1 || got[0] != "queued three" {
		t.Fatalf("screen queued: %q", got)
	}
	items := []CLIQueueItem{{ID: "a", Kind: "user"}}
	fillQueuedText(items, "s1", "", scr327Queued)
	if items[0].Text != "queued three" {
		t.Fatalf("fill from screen: %+v", items)
	}
	// No screen: the text agent-monitor typed while the agent was busy fills it.
	noteSentBusy("s2", "from the phone")
	items = []CLIQueueItem{{ID: "b", Kind: "user"}}
	fillQueuedText(items, "s2", "", "")
	if items[0].Text != "from the phone" {
		t.Fatalf("fill from sent: %+v", items)
	}
	// Events after Stop (Notification, transcript) must not make an idle screen look busy.
	s := &Session{RecentEvents: []RecentEvent{{Kind: "Stop"}, {Kind: "assistant"}, {Kind: "system"}, {Kind: "state:running"}}}
	if ph := watchedPhaseOf(s, true, scr327Idle); ph != "ready" {
		t.Fatalf("idle screen: %s", ph)
	}
	if ph := watchedPhaseOf(s, true, scr327Queued); ph != "busy" {
		t.Fatalf("busy screen: %s", ph)
	}
}

func TestCLIPendingBangShowsUntilTranscript(t *testing.T) {
	id, f := setupCLITest(t)
	f.screens = []string{scrIdle, scrBash}
	f.sess = &Session{Tool: ToolClaude, State: StateAwaitingInput, RecentEvents: []RecentEvent{{Kind: "Stop"}}}
	if code, _ := cliPost(id, "send", `{"text":"!bash resign.sh"}`); code != 200 {
		t.Fatalf("send: %d", code)
	}
	v := cliGet(id, "")
	if len(v.Items) != 1 || v.Items[0].Kind != "bash" || v.Items[0].Text != "!bash resign.sh" || v.Items[0].Status != "running" || v.CanRemove {
		t.Fatalf("pending: %+v", v)
	}
	// Claude Code writes the command when it ends; then the pending item goes.
	ref := resolveWatched(id)
	rec := `{"type":"user","message":{"role":"user","content":"<bash-input>bash resign.sh</bash-input>"},"timestamp":"` +
		time.Now().UTC().Format(time.RFC3339Nano) + `"}` + "\n" +
		`{"type":"user","message":{"role":"user","content":"<bash-stdout>done</bash-stdout><bash-stderr></bash-stderr>"},"timestamp":"` +
		time.Now().UTC().Format(time.RFC3339Nano) + `"}` + "\n"
	fh, _ := os.OpenFile(ref.Path, os.O_APPEND|os.O_WRONLY, 0o644)
	_, _ = fh.WriteString(rec)
	fh.Close()
	if v := cliGet(id, ""); len(v.Items) != 0 {
		t.Fatalf("still pending after the transcript: %+v", v)
	}
}

func TestCursorPhase(t *testing.T) {
	idle := "  ok.\n  → Add a follow-up\n  Gemini 3.8 Flash Low · 3.2%\n  ~/w · main\n"
	busy := " ⠰⠰ Working\n  → Add a follow-up                         ctrl+c\n  Gemini 3.8 Flash Low · 3.2%\n"
	if ph := watchedPhaseOf(&Session{}, true, idle); ph != "ready" {
		t.Fatalf("idle cursor: %s", ph)
	}
	if ph := watchedPhaseOf(&Session{}, true, busy); ph != "busy" {
		t.Fatalf("busy cursor: %s", ph)
	}
}

func TestMergeBangsShowsQueuedCommandOnce(t *testing.T) {
	bangs := []CLIQueueItem{{ID: "b1", Text: "!bash resign.sh --force", Kind: "bash", Status: "running"}, {ID: "b2", Text: "!ls", Kind: "bash", Status: "running"}}
	items := []CLIQueueItem{{ID: "q1", Text: "bash resign.sh --force", Kind: "user"}}
	got := mergeBangs(bangs, items)
	if len(got) != 2 || got[0].ID != "b2" || got[1].ID != "q1" || got[1].Text != "!bash resign.sh --force" {
		t.Fatalf("merged: %+v", got)
	}
}

// A host screen has no colours, so the dim queue hint after "!" arrives as plain text.
func TestCLIBangWithPlainQueueHint(t *testing.T) {
	id, f := setupCLITest(t)
	f.sess = &Session{Tool: ToolClaude, State: StateRunning, RecentEvents: []RecentEvent{{Kind: "UserPromptSubmit"}}}
	hint := "  41. Forty-one is a prime number.\n" + cliRule + "\n! Press up to edit queued messages\n" + cliRule + "\n  ! for shell mode\n"
	f.screens = []string{scrQueued, hint}
	code, out := cliPost(id, "send", `{"text":"! ls"}`)
	if code != 200 || strings.Join(f.typed, "|") != "!|ls⏎" || len(f.sent) != 0 {
		t.Fatalf("got %d %v typed=%q keys=%v", code, out, f.typed, f.sent)
	}
}

// A running ! command is listed but is not in Claude Code's queue: Edit takes only the queued messages.
func TestCLIEditSkipsRunningBang(t *testing.T) {
	id, f := setupCLITest(t, qop("enqueue", tn), qop("enqueue", "one"), qop("enqueue", "two"))
	noteBang(id, "sleep 30", false)
	f.screens = []string{scrQueued, scrQueued, scrPulled, scrEmpty}
	if code, out := cliPost(id, "edit", `{}`); code != 200 || out["text"] != "one\ntwo" {
		t.Fatalf("edit: %d %v keys=%v", code, out, f.sent)
	}
}

// A queued ! command sits in Claude Code's queue without its "!": refuse before any key.
func TestCLIEditRefusesQueuedBang(t *testing.T) {
	id, f := setupCLITest(t, qop("enqueue", tn), qop("enqueue", "ls"))
	noteBang(id, "ls", true)
	f.screens = []string{scrQueued, scrQueued}
	if code, out := cliPost(id, "remove", `{}`); code != 409 || len(f.sent) != 0 || !strings.Contains(fmt.Sprint(out), "! command") {
		t.Fatalf("remove: %d %v keys=%v", code, out, f.sent)
	}
}

// Half a sentence typed in the terminal would join a message from the phone: refuse it.
func TestCLISendRefusesOverTypedDraft(t *testing.T) {
	id, f := setupCLITest(t)
	f.sess = &Session{Tool: ToolClaude, State: StateAwaitingInput, RecentEvents: []RecentEvent{{Kind: "Stop"}}}
	f.screens = []string{cliScreen("refactor the", " · ? for shortcuts")}
	if code, _ := cliPost(id, "send", `{"text":"run the tests"}`); code != 409 || len(f.typed) != 0 {
		t.Fatalf("draft in the box: %d %q", code, f.typed)
	}
}

// A host with no tmux pane gives a plain screen: a normal message must not be refused for a placeholder.
func TestCLISendSkipsBoxCheckWithoutColours(t *testing.T) {
	id, f := setupCLITest(t)
	cliSeesDim = func(string) bool { return false }
	f.sess = &Session{Tool: ToolClaude, State: StateAwaitingInput, RecentEvents: []RecentEvent{{Kind: "Stop"}}}
	f.screens = []string{cliScreen("Try \"fix the failing test\"", " · ? for shortcuts")}
	if code, _ := cliPost(id, "send", `{"text":"run the tests"}`); code != 200 || strings.Join(f.typed, "|") != "run the tests⏎" {
		t.Fatalf("plain host screen: %d %q", code, f.typed)
	}
}

// A sent ! command whose result never reaches the transcript (Claude could not write it) can be forgotten.
func TestCLIForgetBang(t *testing.T) {
	id, _ := setupCLITest(t, qop("enqueue", tn))
	noteBang(id, "bash resign.sh --force", true)
	cliBangs.mu.Lock()
	bid := cliBangs.m[id][len(cliBangs.m[id])-1].ID
	cliBangs.mu.Unlock()
	if code, _ := cliPost(id, "forget", `{"id":"`+bid+`"}`); code != 200 {
		t.Fatalf("forget: %d", code)
	}
	if code, _ := cliPost(id, "forget", `{"id":"`+bid+`"}`); code != 404 {
		t.Fatalf("second forget: %d", code)
	}
	cliBangs.mu.Lock()
	n := len(cliBangs.m[id])
	cliBangs.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d records left", n)
	}
}

// A named session prints its name inside the top rule of the input box.
func TestCLIInputBoxNamedSession(t *testing.T) {
	top := strings.Repeat("─", 60) + " bloat-repetition-cleanup-plan ─"
	scr := "  done\n" + top + "\n! \n" + strings.Repeat("─", 90) + "\n  ! for shell mode\n"
	if got := strings.TrimSpace(cliInputBox(scr)); got != "!" {
		t.Fatalf("box %q, want !", got)
	}
	if isBoxRule("── a note") || isBoxRule("text ────────────────────────────") {
		t.Fatal("short or text-first lines are not rules")
	}
}

// A named session's input box below menu-like text means no prompt is open.
func TestPanePromptNamedSession(t *testing.T) {
	top := strings.Repeat("─", 60) + " my-session ─"
	scr := "Pick one:\n❯ 1. Yes\n  2. No\nI chose 1.\n" + top + "\n❯ \n" + strings.Repeat("─", 80) + "\n"
	if panePromptOpen(scr) {
		t.Fatal("a drawn input box under the text means the menu is not open")
	}
}
