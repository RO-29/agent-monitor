package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

// The older Codex dialog: its "No" option continues the session with approvals on.
const codexApprovalScreen = `  Allow Codex to work in this folder without asking for approval?

%s 1. Yes, allow Codex to work in this folder without asking for approval
%s 2. No, ask me to approve edits and commands

  Press enter to continue`

func (f *fakeHost) Calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func TestTrustClassifyNoThatContinues(t *testing.T) {
	cases := map[string]string{
		"No, exit":                 "deny",
		"No, quit":                 "deny",
		"Quit":                     "deny",
		"Cancel":                   "deny",
		"No, don't trust":          "deny",
		"Yes, I trust this folder": "trust",
		"No, ask me to approve edits and commands": "",
		"Nothing here": "",
	}
	for label, want := range cases {
		if got := classifyTrustOption(label); got != want {
			t.Errorf("%q: got %q want %q", label, got, want)
		}
	}
	p := detectTrustPrompt(sprintf(codexApprovalScreen, "›", " "), "codex")
	if p == nil || p.Options[1].ID != "option2" || p.Options[1].Label != "No, ask me to approve edits and commands" {
		t.Fatalf("prompt %+v", p)
	}
}

func postAnswerJSON(t *testing.T, url, option string) (int, map[string]any) {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(`{"option":"`+option+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// Cancel must not pick a "No" option that starts the session; it sends Escape and says so.
func TestTrustCancelSendsEscapeWhenNoDeny(t *testing.T) {
	srv := withHostDaemon(t)
	resetAnswered(t)
	f := &fakeHost{tmpl: codexApprovalScreen, options: 2}
	id := serveFakeHost(t, f, "codex")
	h := getHostJSON(t, srv.URL+"/api/hosts/"+id)
	p, _ := h["prompt"].(map[string]any)
	opts, _ := p["options"].([]any)
	if len(opts) != 2 || opts[1].(map[string]any)["label"] != "No, ask me to approve edits and commands" {
		t.Fatalf("options %v", h)
	}
	code, out := postAnswerJSON(t, srv.URL+"/api/hosts/"+id+"/answer", "cancel")
	if code != 200 || out["key"] != "Escape" || out["option"] != nil {
		t.Fatalf("cancel %d %v", code, out)
	}
	if k := strings.Join(f.Keys(), ","); k != "Escape" {
		t.Fatalf("keys %s", k)
	}
	g := &fakeHost{tmpl: claudeTrustScreen, options: 2}
	gid := serveFakeHost(t, g, "claude")
	if code, out := postAnswerJSON(t, srv.URL+"/api/hosts/"+gid+"/answer", "cancel"); code != 200 || out["key"] != "Enter" || out["option"] != "deny" {
		t.Fatalf("cancel with deny %d %v", code, out)
	}
}

func TestTrustDetectWindow(t *testing.T) {
	filler := strings.Repeat(" some earlier output line\n", 30)
	// Header scrolled far above the bottom of the screen.
	old := " Do you trust the files in this folder?\n" + filler + "› 1. Yes\n  2. No, exit\n"
	if p := detectTrustPrompt(old, "claude"); p != nil {
		t.Fatalf("header outside the last screenful: %+v", p)
	}
	// Options too far after the header.
	far := " Do you trust the files in this folder?\n" + strings.Repeat(" text\n", 15) + "› 1. Yes\n  2. No, exit\n"
	if p := detectTrustPrompt(far, "claude"); p != nil {
		t.Fatalf("options far from the header: %+v", p)
	}
	near := filler + " Do you trust the files in this folder?\n\n› 1. Yes\n  2. No, exit\n"
	if p := detectTrustPrompt(near, "claude"); p == nil {
		t.Fatal("dialog at the bottom not found")
	}
}

// A long-running host that shows a trust-like dialog is not at its first-run question.
func TestTrustRefusedAfterMuchOutput(t *testing.T) {
	srv := withHostDaemon(t)
	resetAnswered(t)
	f := &fakeHost{tmpl: claudeTrustScreen, options: 2, out: hostPromptMaxOutBytes + 1}
	id := serveFakeHost(t, f, "claude")
	if h := getHostJSON(t, srv.URL+"/api/hosts/"+id); h["prompt"] != nil {
		t.Fatalf("prompt after much output: %v", h["prompt"])
	}
	if code, _ := postAnswer(t, srv.URL+"/api/hosts/"+id+"/answer", "trust"); code != http.StatusConflict {
		t.Fatalf("answer %d", code)
	}
	if len(f.Keys()) != 0 {
		t.Fatalf("keys sent: %v", f.Keys())
	}
	// Without a byte count, a long history refuses the dialog too.
	g := &fakeHost{tmpl: strings.Repeat(" build output\n", hostPromptMaxLines) + claudeTrustScreen, options: 2}
	gid := serveFakeHost(t, g, "claude")
	if h := getHostJSON(t, srv.URL+"/api/hosts/"+gid); h["prompt"] != nil {
		t.Fatalf("prompt after a long history: %v", h["prompt"])
	}
	k := &fakeHost{tmpl: claudeTrustScreen, options: 2, out: 4 << 10}
	kid := serveFakeHost(t, k, "claude")
	if h := getHostJSON(t, srv.URL+"/api/hosts/"+kid); h["prompt"] == nil {
		t.Fatalf("fresh host lost its prompt: %v", h)
	}
}

func TestHostViewCachedAndPruned(t *testing.T) {
	srv := withHostDaemon(t)
	resetAnswered(t)
	f := &fakeHost{answered: true}
	id := serveFakeHost(t, f, "claude")
	getHostJSON(t, srv.URL+"/api/hosts")
	getHostJSON(t, srv.URL+"/api/hosts/"+id)
	if n := f.Calls(); n != 1 {
		t.Fatalf("socket calls for two reads inside the TTL: %d", n)
	}
	hostAnswerMu.Lock()
	hostAnsweredAt["gone"] = time.Now()
	hostAnswerMu.Unlock()
	hostViewMu.Lock()
	hostViews["gone"] = hostViewEntry{at: time.Now()}
	hostViewMu.Unlock()
	getHostJSON(t, srv.URL+"/api/hosts")
	hostAnswerMu.Lock()
	_, a := hostAnsweredAt["gone"]
	hostAnswerMu.Unlock()
	hostViewMu.Lock()
	_, v := hostViews["gone"]
	_, live := hostViews[id]
	hostViewMu.Unlock()
	if a || v || !live {
		t.Fatalf("prune: answered %v view %v live %v", a, v, live)
	}
}
