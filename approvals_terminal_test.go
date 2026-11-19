package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// Screens captured from Claude Code 2.1.326 and Codex 0.169.0 in a scratch tmux server.
const screenClaudeRm = `⏺ Bash(rm two.txt)
  ⎿  Waiting…
────────────────────────────────────────
 Bash command
 Remove two.txt file
╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌
 rm two.txt
╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌
 Do you want to proceed?
 ❯ 1. Yes
   2. Yes, and always allow access to
      /private/tmp/kt1 from this project
   3. Yes, and switch to auto mode · auto mode handles these prompts for you
   4. No
 Esc to cancel · Tab to amend
`

const screenClaudeGitInit = ` git init -q
╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌
 This command requires approval
 Do you want to proceed?
 ❯ 1. Yes
   2. Yes, and don’t ask again for: git init *
   3. Yes, and switch to auto mode · auto mode handles these prompts for you
   4. No
 Esc to cancel · Tab to amend
`

const screenClaudeWrite = ` Create file
 notes.txt
╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌
  1 hi
╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌
 Do you want to create notes.txt?
 ❯ 1. Yes
   2. Yes, and switch to accept edits (auto-approve file edits and common file commands) for this session (shift+tab)
   3. No
 Esc to cancel · Tab to amend
`

const screenCodexExec = `• Running touch a.txt
  Would you like to run the following command?
  Environment: local
  Reason: Allow touch a.txt? The read-only sandbox blocked this command.
  $ touch a.txt
› 1. Yes, proceed (y)
  2. Yes, and don't ask again for commands that start with ` + "`touch a.txt`" + ` (p)
  3. No, and tell Codex what to do differently (esc)
  Press enter to confirm or esc to cancel
`

const screenIdle = `❯
────────────────────────────────────────
  ⏸ manual mode on · ? for shortcuts
`

type termFake struct {
	screen string
	sender *fakeSender
	pane   WatchedPane
}

// newTermEnv makes every session reachable through a fake pane whose screen the test sets.
func newApprTermEnv(t *testing.T) (*approvalEnv, *termFake) {
	e := newApprovalEnv(t)
	f := &termFake{sender: &fakeSender{}, pane: WatchedPane{Registered: true, Live: true, Sendable: true, PaneID: "%9"}}
	e.s.reachable = func(string) bool { return true }
	prevPane, prevCap, prevSend, prevDelay, prevPoll := approvalPaneFn, termCaptureFn, termAnswerSender, termStepDelay, termApprovalPoll
	approvalPaneFn = func(string) WatchedPane { return f.pane }
	termCaptureFn = func(string) (string, error) { return f.screen, nil }
	termAnswerSender = f.sender
	termStepDelay = 0
	termApprovalPoll = time.Hour
	t.Cleanup(func() {
		approvalPaneFn, termCaptureFn, termAnswerSender, termStepDelay, termApprovalPoll = prevPane, prevCap, prevSend, prevDelay, prevPoll
	})
	return e, f
}

func pendingOne(t *testing.T, s *ApprovalStore) *Approval {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.pending) != 1 {
		t.Fatalf("want 1 pending, got %d", len(s.pending))
	}
	for _, p := range s.pending {
		return copyApproval(p.a)
	}
	return nil
}

func recentOf(s *ApprovalStore, id string) *Approval {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.recent {
		if a.ID == id {
			return a
		}
	}
	return nil
}

func TestTerminalShownReturnsNoOpinionAtOnce(t *testing.T) {
	e, _ := newApprTermEnv(t)
	ch := e.startHook(hookClaudePermission, claudeBash("rm two.txt"))
	r := waitResult(t, ch)
	if r.code != http.StatusNoContent || r.body != nil {
		t.Fatalf("want 204 no opinion, got %d %v", r.code, r.body)
	}
	a := pendingOne(t, e.s)
	if a.Source != approvalSourceTerminal || a.HoldUntil != 0 || a.State != "pending" {
		t.Fatalf("bad terminal-shown approval %+v", a)
	}
	// Codex gets the same treatment.
	ch = e.startHook(hookCodex, map[string]any{"session_id": "c-1", "cwd": "/r", "tool_name": "Bash", "tool_input": map[string]any{"command": "touch a.txt"}})
	if r := waitResult(t, ch); r.code != http.StatusNoContent {
		t.Fatalf("codex: want 204, got %d", r.code)
	}
}

func TestUnreachableSessionStillHolds(t *testing.T) {
	e, _ := newApprTermEnv(t)
	e.s.reachable = func(string) bool { return false }
	ch := e.startHook(hookClaudePermission, claudeBash("rm two.txt"))
	a := e.waitPending()
	if a.Source != "" || a.HoldUntil == 0 {
		t.Fatalf("unreachable session must hold: %+v", a)
	}
	select {
	case <-ch:
		t.Fatal("hook returned before a decision")
	case <-time.After(100 * time.Millisecond):
	}
	e.decide(a.ID, map[string]any{"behavior": "allow"})
	if d := decisionOf(t, waitResult(t, ch).body); d["behavior"] != "allow" {
		t.Fatalf("hold decision %v", d)
	}
}

func TestApprovalsModeHoldHoldsReachable(t *testing.T) {
	e, f := newApprTermEnv(t)
	if code, raw := e.post("/api/approvals/settings", map[string]any{"approvalsMode": "nope"}); code != 400 {
		t.Fatalf("bad mode accepted: %d %s", code, raw)
	}
	if code, raw := e.post("/api/approvals/settings", map[string]any{"approvalsMode": "hold"}); code != 200 || !strings.Contains(string(raw), `"approvalsMode":"hold"`) {
		t.Fatalf("set hold: %d %s", code, raw)
	}
	ch := e.startHook(hookClaudePermission, claudeBash("rm two.txt"))
	a := e.waitPending()
	if a.Source != "" {
		t.Fatalf("hold mode must not show in the terminal: %+v", a)
	}
	e.decide(a.ID, map[string]any{"behavior": "deny", "message": "no"})
	if d := decisionOf(t, waitResult(t, ch).body); d["behavior"] != "deny" {
		t.Fatalf("hold deny %v", d)
	}
	if len(f.sender.steps) != 0 {
		t.Fatalf("hold mode typed keys: %v", f.sender.steps)
	}
	s := newApprovalStore(t.TempDir())
	if s.settings.ApprovalsMode != approvalsModeBoth {
		t.Fatalf("default mode %q", s.settings.ApprovalsMode)
	}
}

func TestRulesStillAnswerReachableAtOnce(t *testing.T) {
	e, _ := newApprTermEnv(t)
	e.post("/api/approvals/rules", Rule{Action: "allow", Tool: "Bash", Pattern: "npm test*"})
	r := waitResult(t, e.startHook(hookClaudePermission, claudeBash("npm test")))
	if r.code != 200 || decisionOf(t, r.body)["behavior"] != "allow" {
		t.Fatalf("rule allow: %d %v", r.code, r.body)
	}
	r = waitResult(t, e.startHook(hookClaudePermission, claudeBash("sudo rm -rf /x")))
	if r.code != 200 || decisionOf(t, r.body)["behavior"] != "deny" {
		t.Fatalf("seed deny rule: %d %v", r.code, r.body)
	}
	e.s.mu.Lock()
	n := len(e.s.pending)
	e.s.mu.Unlock()
	if n != 0 {
		t.Fatalf("rule decisions left %d cards", n)
	}
}

func TestDecideTypesExactKeys(t *testing.T) {
	cases := []struct {
		name   string
		hook   string
		input  map[string]any
		tool   string
		screen string
		d      map[string]any
		want   []keyStep
	}{
		{"claude yes", hookClaudePermission, map[string]any{"command": "rm two.txt"}, "Bash", screenClaudeRm, map[string]any{"behavior": "allow"}, []keyStep{{Keys: []string{"1"}}}},
		{"claude no", hookClaudePermission, map[string]any{"command": "rm two.txt"}, "Bash", screenClaudeRm, map[string]any{"behavior": "deny"}, []keyStep{{Keys: []string{"4"}}}},
		{"claude no and stop", hookClaudePermission, map[string]any{"command": "rm two.txt"}, "Bash", screenClaudeRm, map[string]any{"behavior": "deny", "message": "x", "interrupt": true}, []keyStep{{Keys: []string{"4"}}}},
		{"claude no with message", hookClaudePermission, map[string]any{"command": "rm two.txt"}, "Bash", screenClaudeRm, map[string]any{"behavior": "deny", "message": "keep\n the file"},
			[]keyStep{{Keys: []string{"Down"}}, {Keys: []string{"Down"}}, {Keys: []string{"Down"}}, {Keys: []string{"Tab"}}, {Text: "keep the file"}, {Keys: []string{"Enter"}}}},
		{"claude don't ask again", hookClaudePermission, map[string]any{"command": "git init -q"}, "Bash", screenClaudeGitInit, map[string]any{"behavior": "allow", "applySuggestions": true}, []keyStep{{Keys: []string{"2"}}}},
		{"claude write no", hookClaudePermission, map[string]any{"file_path": "/w/notes.txt", "content": "hi"}, "Write", screenClaudeWrite, map[string]any{"behavior": "deny"}, []keyStep{{Keys: []string{"3"}}}},
		{"codex yes", hookCodex, map[string]any{"command": "touch a.txt"}, "Bash", screenCodexExec, map[string]any{"behavior": "allow"}, []keyStep{{Keys: []string{"y"}}}},
		{"codex no with message", hookCodex, map[string]any{"command": "touch a.txt"}, "Bash", screenCodexExec, map[string]any{"behavior": "deny", "message": "use c.txt"},
			[]keyStep{{Keys: []string{"Escape"}}, {Text: "use c.txt"}, {Keys: []string{"Enter"}}}},
		{"plan approve", hookClaudePretool, map[string]any{"plan": "# Plan\nstep"}, "ExitPlanMode", screenPlan, map[string]any{"behavior": "allow"}, []keyStep{{Keys: []string{"2"}}}},
		{"plan feedback", hookClaudePretool, map[string]any{"plan": "# Plan\nstep"}, "ExitPlanMode", screenPlan, map[string]any{"behavior": "deny", "message": "split it"}, []keyStep{{Keys: []string{"3"}}, {Text: "split it"}, {Keys: []string{"Enter"}}}},
		{"question", hookClaudePretool, askInput(q("Which color?", "Color", false, "Red", "Green", "Blue")), "AskUserQuestion", screenTwoQuestions, map[string]any{"behavior": "allow", "answers": map[string]any{"Which color?": "Green"}}, []keyStep{{Keys: []string{"2"}}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e, f := newApprTermEnv(t)
			f.screen = c.screen
			p := map[string]any{"session_id": "s-1", "cwd": "/w", "tool_name": c.tool, "tool_input": c.input}
			if c.hook == hookClaudePermission {
				p["permission_suggestions"] = []any{map[string]any{"type": "addRules"}}
			}
			waitResult(t, e.startHook(c.hook, p))
			a := pendingOne(t, e.s)
			code, raw := e.decide(a.ID, c.d)
			if code != 200 {
				t.Fatalf("decide %d %s", code, raw)
			}
			got := f.sender.steps
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("keys %+v, want %+v", got, c.want)
			}
			var snap Approval
			_ = json.Unmarshal(raw, &snap)
			if snap.State != "pending" || snap.SentAt == 0 {
				t.Fatalf("card must stay as sent until confirmed: %+v", snap)
			}
			if code, _ := e.decide(a.ID, c.d); code != 409 {
				t.Fatalf("second decision must be refused, got %d", code)
			}
			// The prompt leaves the screen: the sent decision is confirmed.
			f.screen = screenIdle
			e.s.checkTerminal(a.ID)
			e.s.checkTerminal(a.ID)
			done := recentOf(e.s, a.ID)
			want := "allowed"
			if c.d["behavior"] == "deny" {
				want = "denied"
			}
			if done == nil || done.State != want || done.DecidedBy != "web" {
				t.Fatalf("not confirmed: %+v", done)
			}
		})
	}
}

func TestDecideRefusedWhenPromptNotOnScreen(t *testing.T) {
	e, f := newApprTermEnv(t)
	waitResult(t, e.startHook(hookClaudePermission, claudeBash("rm two.txt")))
	a := pendingOne(t, e.s)
	f.screen = screenIdle
	code, raw := e.decide(a.ID, map[string]any{"behavior": "allow"})
	if code != 409 || !strings.Contains(string(raw), "does not show this prompt") {
		t.Fatalf("want 409 not on screen, got %d %s", code, raw)
	}
	// A different command on screen is not this prompt either.
	f.screen = strings.ReplaceAll(screenClaudeRm, "rm two.txt", "rm other.txt")
	if code, _ := e.decide(a.ID, map[string]any{"behavior": "allow"}); code != 409 {
		t.Fatalf("other command: want 409, got %d", code)
	}
	if len(f.sender.steps) != 0 {
		t.Fatalf("keys sent: %v", f.sender.steps)
	}
	if b := pendingOne(t, e.s); b.SentAt != 0 || b.State != "pending" {
		t.Fatalf("card must stay: %+v", b)
	}
	// An edited command cannot be typed.
	f.screen = screenClaudeRm
	if code, _ := e.decide(a.ID, map[string]any{"behavior": "allow", "updatedInput": map[string]any{"command": "rm x"}}); code != 400 {
		t.Fatalf("edited input: want 400, got %d", code)
	}
	// A send failure before any key leaves the card answerable.
	termAnswerSender = failSender{}
	if code, _ := e.decide(a.ID, map[string]any{"behavior": "allow"}); code != http.StatusBadGateway {
		t.Fatalf("send failure: want 502, got %d", code)
	}
	termAnswerSender = f.sender
	if code, raw := e.decide(a.ID, map[string]any{"behavior": "allow"}); code != 200 {
		t.Fatalf("retry: %d %s", code, raw)
	}
}

type failSender struct{}

func (failSender) Keys(string, []string) error { return errors.New("down") }
func (failSender) Text(string, string) error   { return errors.New("down") }

func tsNow(d time.Duration) string { return time.Now().Add(d).UTC().Format(time.RFC3339Nano) }

func TestTerminalAnswerFromClaudeTranscript(t *testing.T) {
	e, f := newApprTermEnv(t)
	path := filepath.Join(t.TempDir(), "s.jsonl")
	in := map[string]any{"command": "rm two.txt", "description": "run it"}
	use := map[string]any{"type": "assistant", "timestamp": tsNow(-time.Second), "message": map[string]any{"id": "m1", "content": []any{
		map[string]any{"type": "tool_use", "id": "tu1", "name": "Bash", "input": in}}}}
	jsonl(t, path, use)
	p := claudeBash("rm two.txt")
	p["transcript_path"] = path
	waitResult(t, e.startHook(hookClaudePermission, p))
	a := pendingOne(t, e.s)
	f.screen = screenClaudeRm
	if !e.s.checkTerminal(a.ID) {
		t.Fatal("no answer yet: must keep watching")
	}
	reject := map[string]any{"type": "user", "timestamp": tsNow(time.Second), "message": map[string]any{"content": []any{
		map[string]any{"type": "tool_result", "tool_use_id": "tu1", "is_error": true,
			"content": "The user doesn't want to proceed with this tool use. The tool use was rejected (eg. if it was a file edit, the new_string was NOT written to the file). To tell you how to proceed, the user said:\nkeep the file"}}}}
	jsonl(t, path, use, reject)
	if e.s.checkTerminal(a.ID) {
		t.Fatal("answer in transcript: must stop watching")
	}
	done := recentOf(e.s, a.ID)
	if done == nil || done.State != stateAnsweredInTerm || done.Choice != "No: keep the file" || done.DecidedBy != "terminal" {
		t.Fatalf("answered in terminal: %+v", done)
	}

	// An old result of the same command does not close a new request.
	waitResult(t, e.startHook(hookClaudePermission, p))
	b := pendingOne(t, e.s)
	old := map[string]any{"type": "user", "timestamp": tsNow(-time.Minute), "message": map[string]any{"content": []any{
		map[string]any{"type": "tool_result", "tool_use_id": "tu1", "content": "ok"}}}}
	jsonl(t, path, use, old)
	if !e.s.checkTerminal(b.ID) {
		t.Fatal("an old result closed a new request")
	}
	// The prompt leaves the screen with no rejection: the tool runs, so the answer was Yes.
	f.screen = screenIdle
	for i := 0; i < termGoneAllowChecks; i++ {
		e.s.checkTerminal(b.ID)
	}
	if done := recentOf(e.s, b.ID); done == nil || done.State != stateAnsweredInTerm || done.Choice != "Yes" {
		t.Fatalf("screen-gone answer: %+v", done)
	}
}

func TestTerminalAnswerFromCodexRollout(t *testing.T) {
	e, _ := newApprTermEnv(t)
	path := filepath.Join(t.TempDir(), "r.jsonl")
	call := map[string]any{"timestamp": tsNow(-time.Second), "type": "response_item", "payload": map[string]any{"type": "custom_tool_call", "call_id": "c1", "name": "exec"}}
	jsonl(t, path, call)
	waitResult(t, e.startHook(hookCodex, map[string]any{"session_id": "c-1", "transcript_path": path, "cwd": "/r", "tool_name": "Bash", "tool_input": map[string]any{"command": "touch b.txt"}}))
	a := pendingOne(t, e.s)
	jsonl(t, path, call, map[string]any{"timestamp": tsNow(time.Second), "type": "response_item", "payload": map[string]any{"type": "custom_tool_call_output", "call_id": "c1", "output": "aborted by user after 13.0s"}})
	e.s.checkTerminal(a.ID)
	if done := recentOf(e.s, a.ID); done == nil || done.State != stateAnsweredInTerm || done.Choice != "No" {
		t.Fatalf("codex answered: %+v", done)
	}
}

func TestTerminalEventClosesCard(t *testing.T) {
	e, _ := newApprTermEnv(t)
	waitResult(t, e.startHook(hookClaudePermission, claudeBash("rm two.txt")))
	a := pendingOne(t, e.s)
	time.Sleep(2 * time.Millisecond)
	e.s.terminalEvent(ToolClaude, "11111111-2222", "Notification", "")
	e.s.terminalEvent(ToolClaude, "11111111-2222", "PostToolUse", "Read")
	if pendingOne(t, e.s).ID != a.ID {
		t.Fatal("a notification must not close the card")
	}
	e.s.terminalEvent(ToolClaude, "11111111-2222", "Stop", "")
	if done := recentOf(e.s, a.ID); done == nil || done.State != stateAnsweredInTerm {
		t.Fatalf("stop event: %+v", done)
	}
}

func TestPromptOptionsParse(t *testing.T) {
	o := claudePermOptions(screenClaudeRm)
	if len(o) != 4 || !o[0].Cursor || o[3].Label != "No" || !strings.HasPrefix(o[1].Label, "Yes, and always allow") {
		t.Fatalf("claude options %+v", o)
	}
	c := codexPermOptions(screenCodexExec)
	if len(c) != 3 || c[0].Key != "y" || c[1].Key != "p" || c[2].Key != "esc" || !c[0].Cursor {
		t.Fatalf("codex options %+v", c)
	}
	if claudePermOptions(screenIdle) != nil || codexPermOptions(screenIdle) != nil {
		t.Fatal("idle screen has no prompt")
	}
	// The cursor moved to Yes, and …: a message reaches No with two Downs.
	moved := strings.Replace(strings.Replace(screenClaudeRm, " ❯ 1. Yes", "   1. Yes", 1), "   2. Yes", " ❯ 2. Yes", 1)
	steps, _, err := claudePermSteps(claudePermOptions(moved), approvalDecision{Behavior: "deny", Message: "m"}, "m")
	if err != nil || len(steps) != 5 || steps[0].Keys[0] != "Down" || steps[2].Keys[0] != "Tab" {
		t.Fatalf("moved cursor steps %+v %v", steps, err)
	}
	if _, code, err := claudePermSteps(claudePermOptions(screenClaudeWrite), approvalDecision{Behavior: "allow", Always: true, AlwaysScope: "claude"}, ""); err != nil || code != 0 {
		t.Fatalf("accept edits counts as don't ask again: %v", err)
	}
	noAlways := strings.Replace(screenClaudeGitInit, "   2. Yes, and don’t ask again for: git init *\n   3. Yes, and switch to auto mode · auto mode handles these prompts for you\n   4. No", "   2. No", 1)
	if _, code, err := claudePermSteps(claudePermOptions(noAlways), approvalDecision{Behavior: "allow", Always: true, AlwaysScope: "claude"}, ""); err == nil || code != 409 {
		t.Fatalf("no don't-ask-again choice must be 409, got %d %v", code, err)
	}
}

func TestOnePromptOneCard(t *testing.T) {
	e, _ := newApprTermEnv(t)
	in := map[string]any{"plan": "# Plan\nstep"}
	waitResult(t, e.startHook(hookClaudePretool, map[string]any{"session_id": "s-1", "tool_use_id": "tu9", "tool_name": "ExitPlanMode", "tool_input": in}))
	waitResult(t, e.startHook(hookClaudePermission, map[string]any{"session_id": "s-1", "tool_name": "ExitPlanMode", "tool_input": in}))
	if a := pendingOne(t, e.s); a.ToolUseID != "tu9" || a.Kind != "plan" {
		t.Fatalf("one card per prompt: %+v", a)
	}
}

// Claude Code 2.1.326 WebFetch dialog, captured from a hosted session: no "Esc to cancel" footer.
const webFetchScreen = `⏺ Fetch(https://example.org)

 Fetch
 Claude wants to fetch content from example.org
 url: https://example.org/
 │ prompt: What is the exact text of the page's <title>?
 Do you want to allow Claude to fetch this content?
 ❯ 1. Yes
   2. Yes, and don't ask again for example.org
   3. No, and tell Claude what to do differently (esc)`

func TestWebFetchPromptOnScreen(t *testing.T) {
	a := &Approval{Agent: "claude", Kind: "permission", ToolName: "WebFetch", Input: map[string]any{"url": "https://example.org", "prompt": "title"}}
	if opts := claudePermOptions(webFetchScreen); len(opts) != 3 || opts[2].Key != "esc" {
		t.Fatalf("options %+v", opts)
	}
	if !promptOnScreen(a, webFetchScreen) {
		t.Fatal("WebFetch prompt not seen on screen")
	}
	other := &Approval{Agent: "claude", Kind: "permission", ToolName: "WebFetch", Input: map[string]any{"url": "https://other.example.com"}}
	if promptOnScreen(other, webFetchScreen) {
		t.Fatal("a different URL matched this prompt")
	}
}

func TestNumberedAnswerIsNotAPrompt(t *testing.T) {
	answer := "Suggestions\n\n1. Apply in this order: topic first.\n2. Do not wait for the hourly tick to test.\n3. No action needed.\n\n✻ Cogitated for 5m 24s\n\n" +
		strings.Repeat("─", 40) + "\n❯ \n" + strings.Repeat("─", 40) + "\n  Opus 5.5 │ rohit\n  ⏵⏵ auto mode on (shift+tab to cycle)\n"
	if panePromptOpen(answer) {
		t.Fatal("a numbered list in the answer above an empty input box was read as a prompt")
	}
	for name, s := range map[string]string{"rm": screenClaudeRm, "git init": screenClaudeGitInit, "write": screenClaudeWrite} {
		if !panePromptOpen("old output\n" + strings.Repeat("─", 40) + "\n" + s) {
			t.Fatalf("%s prompt not detected", name)
		}
	}
}

// Two requests of one session whose prompts look the same: only the older one can be decided.
func TestDecideRefusesNewerLookalike(t *testing.T) {
	e, f := newApprTermEnv(t)
	push := "cd /Users/rohit/agent-monitor-v3 && git push --force"
	status := "cd /Users/rohit/agent-monitor-v3 && git status"
	waitResult(t, e.startHook(hookClaudePermission, claudeBash(push)))
	time.Sleep(2 * time.Millisecond)
	waitResult(t, e.startHook(hookClaudePermission, claudeBash(status)))
	var older, newer *Approval
	e.s.mu.Lock()
	for _, p := range e.s.pending {
		if str(p.a.Input["command"]) == push {
			older = copyApproval(p.a)
		} else {
			newer = copyApproval(p.a)
		}
	}
	e.s.mu.Unlock()
	if older == nil || newer == nil {
		t.Fatal("want two pending cards")
	}
	f.screen = strings.ReplaceAll(screenClaudeRm, "rm two.txt", push)
	if code, raw := e.decide(newer.ID, map[string]any{"behavior": "allow"}); code != statusRefused || !strings.Contains(string(raw), "older request") {
		t.Fatalf("newer lookalike: want 423, got %d %s", code, raw)
	}
	if len(f.sender.steps) != 0 {
		t.Fatalf("keys sent for the newer card: %v", f.sender.steps)
	}
	if code, raw := e.decide(older.ID, map[string]any{"behavior": "deny"}); code != 200 {
		t.Fatalf("older card: want 200, got %d %s", code, raw)
	}
}

// The user answered question 1 in the terminal; the card's answers start at the step on screen.
func TestDecideQuestionFromTerminalStep(t *testing.T) {
	e, f := newApprTermEnv(t)
	q1, q2 := "Is this a test of the question dialog?", "Do you want more yes/no questions after this one?"
	f.screen = "←  ☒ Test  ☐ More  ✔ Submit  →\n" + q2 + "\n❯ 1. Yes\n  2. No\nEnter to select · Tab/Arrow keys to navigate"
	p := map[string]any{"session_id": "s-1", "cwd": "/w", "tool_name": "AskUserQuestion",
		"tool_input": askInput(q(q1, "Test", false, "Yes", "No"), q(q2, "More", false, "Yes", "No"))}
	waitResult(t, e.startHook(hookClaudePretool, p))
	a := pendingOne(t, e.s)
	code, raw := e.decide(a.ID, map[string]any{"behavior": "allow", "answers": map[string]any{q1: "Yes", q2: "No"}})
	if code != 200 {
		t.Fatalf("decide %d %s", code, raw)
	}
	want := []keyStep{{Keys: []string{"2"}}, {Keys: []string{"1"}}} // No for question 2, then submit on review
	if !reflect.DeepEqual(f.sender.steps, want) {
		t.Fatalf("keys %+v, want %+v", f.sender.steps, want)
	}
}

// Claude Code 2.1.327 review tab, captured from a scratch session.
const screenReviewTab = "←  ☒ Color  ☒ Size  ✔ Submit  →\nReview your answers\n ● Pick a color?\n   → Red\n ● Pick a size?\n   → Large\n❯ 1. Submit answers\n  2. Cancel"

func TestQuestionCardOnReviewTab(t *testing.T) {
	e, f := newApprTermEnv(t)
	f.screen = screenReviewTab
	p := map[string]any{"session_id": "s-1", "cwd": "/w", "tool_name": "AskUserQuestion",
		"tool_input": askInput(q("Pick a color?", "Color", false, "Red", "Blue"), q("Pick a size?", "Size", false, "Small", "Large"))}
	waitResult(t, e.startHook(hookClaudePretool, p))
	a := pendingOne(t, e.s)
	code, raw := e.decide(a.ID, map[string]any{"behavior": "allow", "answers": map[string]any{"Pick a color?": "Blue", "Pick a size?": "Small"}})
	if code != statusRefused || !strings.Contains(string(raw), "review tab") || len(f.sender.steps) != 0 {
		t.Fatalf("review tab: %d %s keys=%v", code, raw, f.sender.steps)
	}
	if !promptOnScreen(a, screenReviewTab) {
		t.Fatal("the review tab is still the open form")
	}
}

// A second prompt drawn over the card's prompt hides it; the card must not close as "allowed".
func TestCoveredPromptIsNotAllowed(t *testing.T) {
	e, f := newApprTermEnv(t)
	f.screen = screenClaudeRm
	waitResult(t, e.startHook(hookClaudePermission, claudeBash("rm two.txt")))
	a := pendingOne(t, e.s)
	e.s.checkTerminal(a.ID) // seen
	f.screen = strings.ReplaceAll(screenClaudeRm, "rm two.txt", "rm other.txt")
	for i := 0; i < termGoneAllowChecks+2; i++ {
		if !e.s.checkTerminal(a.ID) {
			t.Fatalf("covered prompt closed the card after %d checks", i+1)
		}
	}
	f.screen = screenIdle
	for i := 0; i < termGoneAllowChecks; i++ {
		e.s.checkTerminal(a.ID)
	}
	e.s.mu.Lock()
	_, still := e.s.pending[a.ID]
	e.s.mu.Unlock()
	if still {
		t.Fatal("a prompt that really left must still close the card")
	}
}

// Covered for a minute: the card closes, and it claims no choice.
func TestLongCoveredPromptClosesWithoutAChoice(t *testing.T) {
	e, f := newApprTermEnv(t)
	f.screen = screenClaudeRm
	waitResult(t, e.startHook(hookClaudePermission, claudeBash("rm two.txt")))
	a := pendingOne(t, e.s)
	e.s.checkTerminal(a.ID)
	f.screen = strings.ReplaceAll(screenClaudeRm, "rm two.txt", "rm other.txt")
	for i := 0; i < termCoveredChecks-1; i++ {
		if !e.s.checkTerminal(a.ID) {
			t.Fatalf("closed after %d covered checks", i+1)
		}
	}
	if e.s.checkTerminal(a.ID) {
		t.Fatal("still open after a minute under another prompt")
	}
	e.s.mu.Lock()
	last := e.s.recent[len(e.s.recent)-1]
	e.s.mu.Unlock()
	if last.ID != a.ID || last.State == "allowed" || last.Choice != "" {
		t.Fatalf("closed as %q choice %q", last.State, last.Choice)
	}
}
