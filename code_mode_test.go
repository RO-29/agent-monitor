package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestTranscriptCodeMode(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	on := write("on.jsonl", `{"type":"mode","mode":"normal"}`+"\n"+`{"type":"repl-mode","replMode":true,"sessionId":"x"}`+"\n")
	off := write("off.jsonl", `{"type":"repl-mode","replMode":false}`+"\n")
	none := write("none.jsonl", `{"type":"user"}`+"\n")
	if !transcriptCodeMode(on) || transcriptCodeMode(off) || transcriptCodeMode(none) || transcriptCodeMode(filepath.Join(dir, "missing")) {
		t.Fatal("wrong code mode detection")
	}
}

func TestTerminalFlagsResume(t *testing.T) {
	id := "b0fce9f8-d36f-44d1-95fa-f8c3b5667b8f"
	f, err := terminalFlags(terminalStart{Agent: "claude", Resume: id})
	if err != nil || !slices.Equal(f, []string{"--resume", id}) {
		t.Fatalf("flags %v err %v", f, err)
	}
	if _, err := terminalFlags(terminalStart{Agent: "claude", Resume: "x; rm -rf /"}); err == nil {
		t.Fatal("bad resume id accepted")
	}
	if f, err := terminalFlags(terminalStart{Agent: "codex", Resume: id, Sandbox: "read-only"}); err != nil || !slices.Equal(f, []string{"resume", "-s", "read-only", id}) {
		t.Fatalf("codex resume flags %v err %v", f, err)
	}
}

func TestDismissPrompt(t *testing.T) {
	prev := store
	store = NewStore()
	t.Cleanup(func() { store = prev })
	store.Upsert(UpsertInput{Tool: ToolClaude, SessionID: "d1", State: StateAwaitingPermission, HasState: true})
	if code, err := answerPrompt("claude:d1", "dismiss", ""); err != nil || code != 200 {
		t.Fatalf("code %d err %v", code, err)
	}
	if st := store.Get("claude:d1").State; st != StateRunning {
		t.Fatalf("state %s, want running", st)
	}
	if code, _ := answerPrompt("claude:none", "dismiss", ""); code != 404 {
		t.Fatalf("missing session: code %d", code)
	}
}

func TestSniffUploadCodeAndMarkup(t *testing.T) {
	html := []byte("<!doctype html><html><body><script>alert(1)</script></body></html>")
	cases := []struct {
		body  []byte
		hints []string
		want  string
	}{
		{html, []string{"text/html", "page.html"}, "text/plain"}, // markup kept, served as plain text
		{html, nil, ""}, // undeclared markup still refused
		{[]byte("package main\nfunc main() {}\n"), []string{"main.go"}, "text/plain"},
		{[]byte("const a: number = 1\n"), []string{"video/mp2t", "a.ts"}, "text/plain"}, // browsers call .ts video
		{[]byte("print('x')\n"), []string{"image/png", "x.py"}, ""},                     // text claiming an image
		{[]byte("bin\x00ary"), []string{"a.go"}, ""},                                    // binary in a code name
		{[]byte("%PDF-1.4\n"), []string{"a.pdf"}, "application/pdf"},
	}
	for i, c := range cases {
		if got := sniffUpload(c.body, c.hints...); got != c.want {
			t.Errorf("case %d: got %q want %q", i, got, c.want)
		}
	}
}

func TestSessionLifecycleRefusals(t *testing.T) {
	prev := store
	store = NewStore()
	t.Cleanup(func() { store = prev })
	store.Upsert(UpsertInput{Tool: ToolCursor, SessionID: "c1", Cwd: "/tmp", HasCwd: true})
	store.Upsert(UpsertInput{Tool: ToolClaude, SessionID: "gone", Cwd: "/no/such/dir", HasCwd: true, State: StateCompleted, HasState: true})
	if _, code, _ := resumeSession("claude:missing", "", nil); code != 404 {
		t.Fatalf("missing: %d", code)
	}
	if _, code, _ := resumeSession("cursor:c1", "", nil); code != 400 {
		t.Fatalf("cursor: %d", code)
	}
	if _, code, _ := resumeSession("claude:gone", "", nil); code != 409 {
		t.Fatalf("missing folder: %d", code)
	}
	if _, code, _ := endSession("cursor:c1"); code != 400 {
		t.Fatalf("end cursor: %d", code)
	}
}

func TestPromptInBoxAndDraft(t *testing.T) {
	box := "❯ Fix the failing   test in\n  host.go please"
	if !promptInBox(box, "Fix the failing test in host.go please") || promptInBox("❯ ", "Fix it") || !promptInBox("❯ [Pasted text #1 +40 lines]", "long") {
		t.Fatal("promptInBox")
	}
	rule := strings.Repeat("─", 40)
	scr := func(line string) string { return rule + "\n" + line + "\n" + rule + "\n" }
	if boxDraft(scr("❯ ")) || boxDraft(scr(`❯ Try "edit host.go to…"`)) || !boxDraft(scr("❯ delete the old migration")) {
		t.Fatal("boxDraft")
	}
}

func TestStripControl(t *testing.T) {
	if got := stripControl("a\r\nb\rc\x1b[31m\td\x00"); got != "a\nbc[31m\td" {
		t.Fatalf("got %q", got)
	}
}

func TestResumeRefusesActiveState(t *testing.T) {
	prev := store
	store = NewStore()
	t.Cleanup(func() { store = prev })
	for _, st := range []State{StateRunning, StateAwaitingInput, StateIdle} {
		store.Upsert(UpsertInput{Tool: ToolClaude, SessionID: "a", Cwd: t.TempDir(), HasCwd: true, State: st, HasState: true})
		if _, code, _ := resumeSession("claude:a", "", nil); code != 409 {
			t.Fatalf("%s: code %d", st, code)
		}
	}
}

func TestClaudeErrorItems(t *testing.T) {
	cases := map[string]string{
		`{"type":"attachment","attachment":{"type":"hook_non_blocking_error","hookName":"SessionStart:startup","stderr":"usage: graph status"}}`:              "SessionStart:startup hook failed: usage: graph status",
		`{"type":"system","subtype":"stop_hook_summary","level":"suggestion","hookErrors":["Failed with non-blocking status code: no such file"]}`:            "Stop hook failed: Failed with non-blocking status code: no such file",
		`{"type":"assistant","isApiErrorMessage":true,"message":{"role":"assistant","content":[{"type":"text","text":"Login expired · Please run /login"}]}}`: "Login expired · Please run /login",
		`{"type":"system","subtype":"informational","level":"error","content":"Could not save the session"}`:                                                  "Could not save the session",
	}
	for line, want := range cases {
		var j wClaudeLine
		if err := json.Unmarshal([]byte(line), &j); err != nil {
			t.Fatal(err)
		}
		it := claudeErrorItem(&j, 1)
		if it == nil || it.Kind != "error" || it.Text != want {
			t.Fatalf("%s: got %+v", line, it)
		}
	}
	for _, line := range []string{
		`{"type":"system","subtype":"stop_hook_summary","hookErrors":[]}`,
		`{"type":"attachment","attachment":{"type":"hook_success","hookName":"SessionStart:startup"}}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"fine"}]}}`,
	} {
		var j wClaudeLine
		_ = json.Unmarshal([]byte(line), &j)
		if it := claudeErrorItem(&j, 1); it != nil {
			t.Fatalf("%s: unexpected %+v", line, it)
		}
	}
}

func TestRepeatedErrorsGroup(t *testing.T) {
	p := &wParser{tool: ToolClaude, thread: "t"}
	p.startTurn(0, 1, "hi")
	stop := `{"type":"system","subtype":"stop_hook_summary","hookErrors":["hook.sh: No such file"],"timestamp":"2026-11-19T07:00:00Z"}`
	for i := int64(1); i <= 3; i++ {
		p.claudeLine(i*100, []byte(stop), false)
	}
	n, rep := 0, 0
	for _, wi := range p.cur.items {
		if wi.it.Kind == "error" {
			n, rep = n+1, wi.it.Repeat
		}
	}
	if n != 1 || rep != 3 {
		t.Fatalf("error items %d repeat %d, want 1 and 3", n, rep)
	}
}

func TestClaudeUserTextDesktopReminder(t *testing.T) {
	raw := json.RawMessage(`[{"type":"text","text":"<system-reminder>\nno project folder\n</system-reminder>\n\n"},{"type":"text","text":"teset desktop claude code sesison"}]`)
	if got, _ := claudeUserText(raw); claudePromptText(got) != "teset desktop claude code sesison" {
		t.Fatalf("got %q", got)
	}
	only := json.RawMessage(`[{"type":"text","text":"<system-reminder>x</system-reminder>"}]`)
	if got, _ := claudeUserText(only); claudePromptText(got) != "" {
		t.Fatalf("a reminder-only message is meta, got %q", got)
	}
}
