package main

import (
	"bufio"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

const claudeTrustScreen = ` Quick safety check: Is this a project you created or one you trust? (Like your
 own code, a well-known open source project, or work from your team). If not,
 take a moment to review what's in this folder first.
 Claude Code'll be able to read, edit, and execute files here.
 Security guide
 %s No, exit
 %s Yes, I trust this folder
 Enter to confirm · Esc to cancel`

const codexTrustScreen = `> You are in /Users/x/proj

  Trust this folder? Codex can read, edit, and run files here, subject to your
  permission settings. Continue only if you trust these files.

%s 1. Trust and continue
%s 2. Quit

  Press enter to continue`

// fakeHost answers the host socket protocol and redraws a menu as the cursor moves.
type fakeHost struct {
	mu       sync.Mutex
	tmpl     string
	cursor   int
	options  int
	keys     []string
	frozen   bool // keys do not move the cursor
	answered bool
	out      int64 // ping's OutBytes; 0 makes ping fail like an older host
	calls    int
}

func (f *fakeHost) screen() string {
	if f.answered {
		return " Welcome to the agent\n >"
	}
	marks := make([]any, f.options)
	for i := range marks {
		marks[i] = " "
		if i == f.cursor {
			marks[i] = "❯"
		}
	}
	return strings.ReplaceAll(sprintf(f.tmpl, marks...), "❯ 1.", "› 1.")
}

func sprintf(t string, a ...any) string {
	for _, v := range a {
		t = strings.Replace(t, "%s", v.(string), 1)
	}
	return t
}

func (f *fakeHost) handle(req hostRequest) hostResponse {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	switch req.Op {
	case "ping":
		if f.out > 0 {
			return hostResponse{OK: true, OutBytes: f.out}
		}
	case "size":
		return hostResponse{OK: true, Cols: 80, Rows: 24}
	case "screen":
		return hostResponse{OK: true, Text: f.screen()}
	case "keys":
		for _, k := range req.Keys {
			f.keys = append(f.keys, k)
			switch {
			case k == "Down" && !f.frozen && f.cursor < f.options-1:
				f.cursor++
			case k == "Up" && !f.frozen && f.cursor > 0:
				f.cursor--
			case k == "Enter" || k == "Escape":
				f.answered = true
			}
		}
		return hostResponse{OK: true}
	}
	return hostResponse{Error: "unknown op"}
}

func (f *fakeHost) Keys() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.keys...)
}

// serveFakeHost registers a host backed by f and returns its id.
func serveFakeHost(t *testing.T, f *fakeHost, agent string) string {
	t.Helper()
	id := newHostID()
	sock := shortTempDir(t) + "/" + id + ".sock"
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				line, err := bufio.NewReader(c).ReadBytes('\n')
				if err != nil {
					return
				}
				var req hostRequest
				_ = json.Unmarshal(line, &req)
				_ = json.NewEncoder(c).Encode(f.handle(req))
			}()
		}
	}()
	if _, err := hostRegistry.Register(hostRegistration{HostID: id, Pid: os.Getpid(), Agent: agent, Cwd: "/Users/x/proj", Socket: sock}); err != nil {
		t.Fatal(err)
	}
	return id
}

func resetAnswered(t *testing.T) {
	prev := hostAnswerSettle
	hostAnswerSettle = time.Millisecond
	t.Cleanup(func() {
		hostAnswerSettle = prev
		hostAnswerMu.Lock()
		hostAnsweredAt = map[string]time.Time{}
		hostAnswerMu.Unlock()
		hostViewMu.Lock()
		hostViews = map[string]hostViewEntry{}
		hostViewMu.Unlock()
	})
}

func getHostJSON(t *testing.T, url string) map[string]any {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

func postAnswer(t *testing.T, url, option string) (int, string) {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(`{"option":"`+option+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	msg, _ := out["error"].(string)
	return resp.StatusCode, msg
}

func TestTrustDetectClaude(t *testing.T) {
	p := detectTrustPrompt(sprintf(claudeTrustScreen, "❯", " "), "claude")
	if p == nil {
		t.Fatal("no prompt")
	}
	if p.Kind != "trust" || p.Agent != "claude" || p.cursor != 0 || len(p.Options) != 2 {
		t.Fatalf("prompt %+v", p)
	}
	if p.Options[0] != (hostPromptOption{"deny", "No, exit"}) || p.Options[1] != (hostPromptOption{"trust", "Yes, I trust this folder"}) {
		t.Fatalf("options %+v", p.Options)
	}
	if !strings.HasPrefix(p.Text, "Quick safety check:") || !strings.HasSuffix(p.Text, "Security guide") {
		t.Fatalf("text %q", p.Text)
	}
}

func TestTrustDetectCodex(t *testing.T) {
	p := detectTrustPrompt(sprintf(codexTrustScreen, "›", " "), "")
	if p == nil {
		t.Fatal("no prompt")
	}
	if p.Agent != "codex" || p.cursor != 0 || p.digits[0] != 1 || p.digits[1] != 2 {
		t.Fatalf("prompt %+v", p)
	}
	if p.Options[0] != (hostPromptOption{"trust", "Trust and continue"}) || p.Options[1] != (hostPromptOption{"deny", "Quit"}) {
		t.Fatalf("options %+v", p.Options)
	}
	old := "  Do you trust the contents of this directory?\n\n› 1. Yes, continue\n  2. No, quit\n\n  Press enter to continue"
	if q := detectTrustPrompt(old, "codex"); q == nil || q.Options[0].ID != "trust" || q.Options[1].ID != "deny" {
		t.Fatalf("older codex prompt %+v", q)
	}
}

func TestTrustDetectIgnoresOtherScreens(t *testing.T) {
	for _, s := range []string{
		"",
		" > hello\n ? for shortcuts",
		" Do you want to proceed?\n ❯ 1. Yes\n   2. No",
		" I trust this folder layout, so the plan is:\n ❯ do it",
	} {
		if p := detectTrustPrompt(s, "claude"); p != nil {
			t.Fatalf("false prompt for %q: %+v", s, p)
		}
	}
}

func TestTrustHostReportsAndAnswersClaude(t *testing.T) {
	srv := withHostDaemon(t)
	resetAnswered(t)
	f := &fakeHost{tmpl: claudeTrustScreen, options: 2}
	id := serveFakeHost(t, f, "claude")

	h := getHostJSON(t, srv.URL+"/api/hosts/"+id)
	p, _ := h["prompt"].(map[string]any)
	if p == nil || p["kind"] != "trust" || p["agent"] != "claude" || p["folder"] != "/Users/x/proj" {
		t.Fatalf("host %v", h)
	}
	opts, _ := p["options"].([]any)
	if len(opts) != 2 || opts[1].(map[string]any)["id"] != "trust" {
		t.Fatalf("options %v", opts)
	}
	if tail, _ := h["screenTail"].([]any); len(tail) == 0 || len(tail) > hostScreenTailLines || tail[len(tail)-1] != " Enter to confirm · Esc to cancel" {
		t.Fatalf("screenTail %v", h["screenTail"])
	}
	list := getHostJSON(t, srv.URL+"/api/hosts")
	if hs, _ := list["hosts"].([]any); len(hs) != 1 || hs[0].(map[string]any)["prompt"] == nil {
		t.Fatalf("list %v", list)
	}

	if code, msg := postAnswer(t, srv.URL+"/api/hosts/"+id+"/answer", "trust"); code != 200 {
		t.Fatalf("answer %d %s", code, msg)
	}
	if k := strings.Join(f.Keys(), ","); k != "Down,Enter" {
		t.Fatalf("keys %s", k)
	}
	if h := getHostJSON(t, srv.URL+"/api/hosts/"+id); h["prompt"] != nil {
		t.Fatalf("prompt still reported: %v", h["prompt"])
	}
	if code, _ := postAnswer(t, srv.URL+"/api/hosts/"+id+"/answer", "trust"); code != http.StatusConflict {
		t.Fatalf("second answer %d", code)
	}

	// A linked host reports neither the prompt nor the tail.
	hostRegistry.mu.Lock()
	hostRegistry.hosts[id].AgentID = "claude:x"
	hostRegistry.mu.Unlock()
	if h := getHostJSON(t, srv.URL+"/api/hosts/"+id); h["prompt"] != nil || h["screenTail"] != nil {
		t.Fatalf("linked host %v", h)
	}
}

func TestTrustAnswerDenyAndCancel(t *testing.T) {
	srv := withHostDaemon(t)
	resetAnswered(t)
	f := &fakeHost{tmpl: claudeTrustScreen, options: 2, cursor: 1}
	id := serveFakeHost(t, f, "claude")
	if code, msg := postAnswer(t, srv.URL+"/api/hosts/"+id+"/answer", "cancel"); code != 200 {
		t.Fatalf("cancel %d %s", code, msg)
	}
	if k := strings.Join(f.Keys(), ","); k != "Up,Enter" {
		t.Fatalf("keys %s", k)
	}
	g := &fakeHost{tmpl: claudeTrustScreen, options: 2}
	gid := serveFakeHost(t, g, "claude")
	if code, _ := postAnswer(t, srv.URL+"/api/hosts/"+gid+"/answer", "bogus"); code != http.StatusBadRequest {
		t.Fatalf("bogus option %d", code)
	}
	if code, _ := postAnswer(t, srv.URL+"/api/hosts/"+gid+"/answer", "deny"); code != 200 || strings.Join(g.Keys(), ",") != "Enter" {
		t.Fatalf("deny %d %v", code, g.Keys())
	}
}

func TestTrustAnswerRefusesWhenCursorStuck(t *testing.T) {
	srv := withHostDaemon(t)
	resetAnswered(t)
	f := &fakeHost{tmpl: claudeTrustScreen, options: 2, frozen: true}
	id := serveFakeHost(t, f, "claude")
	if code, _ := postAnswer(t, srv.URL+"/api/hosts/"+id+"/answer", "trust"); code != http.StatusBadGateway {
		t.Fatalf("code %d", code)
	}
	if k := strings.Join(f.Keys(), ","); k != "Down" {
		t.Fatalf("Enter must not be sent: %s", k)
	}
}

func TestTrustAnswerCodex(t *testing.T) {
	srv := withHostDaemon(t)
	resetAnswered(t)
	f := &fakeHost{tmpl: codexTrustScreen, options: 2}
	id := serveFakeHost(t, f, "codex")
	h := getHostJSON(t, srv.URL+"/api/hosts/"+id)
	if p, _ := h["prompt"].(map[string]any); p == nil || p["agent"] != "codex" {
		t.Fatalf("host %v", h)
	}
	if code, msg := postAnswer(t, srv.URL+"/api/hosts/"+id+"/answer", "trust"); code != 200 {
		t.Fatalf("answer %d %s", code, msg)
	}
	if k := strings.Join(f.Keys(), ","); k != "Enter" {
		t.Fatalf("keys %s", k)
	}
}

func TestTrustAnswerNoPrompt(t *testing.T) {
	srv := withHostDaemon(t)
	resetAnswered(t)
	f := &fakeHost{answered: true}
	id := serveFakeHost(t, f, "claude")
	h := getHostJSON(t, srv.URL+"/api/hosts/"+id)
	if h["prompt"] != nil || h["screenTail"] == nil {
		t.Fatalf("host %v", h)
	}
	if code, _ := postAnswer(t, srv.URL+"/api/hosts/"+id+"/answer", "trust"); code != http.StatusConflict {
		t.Fatalf("code %d", code)
	}
	if code, _ := postAnswer(t, srv.URL+"/api/hosts/nope/answer", "trust"); code != http.StatusNotFound {
		t.Fatalf("missing host %d", code)
	}
}
