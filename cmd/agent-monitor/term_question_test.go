package main

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// Screens captured from Claude Code 2.1.326 in a scratch tmux server.
const screenTwoQuestions = `←  ☐ Color  ☐ Tools  ✔ Submit  →
Which color?
❯ 1. Red
     Select Red
  2. Green
     Select Green
  3. Blue
     Select Blue
  4. Type something.
─────────────────────────────
  5. Chat about this
Enter to select · Tab/Arrow keys to navigate · Esc to cancel
`

const screenPlan = `   Ready to code?
   Here is Claude's plan:
   1. Create hello.txt in the working directory.
   2. Write the text "hi" into it.
  ────────────────────────────
   Claude has written up a plan and is ready to execute. Would you like to proceed?
   ❯ 1. Yes, and use auto mode
     2. Yes, manually approve edits
     3. Tell Claude what to change
        shift+tab to approve with this feedback
   ctrl+g to edit in VS Code · ~/.claude/plans/x.md
`

func askInput(qs ...map[string]any) map[string]any {
	l := []any{}
	for _, q := range qs {
		l = append(l, q)
	}
	return map[string]any{"questions": l}
}

func q(text, header string, multi bool, labels ...string) map[string]any {
	opts := []any{}
	for _, l := range labels {
		opts = append(opts, map[string]any{"label": l, "description": "Select " + l})
	}
	return map[string]any{"question": text, "header": header, "multiSelect": multi, "options": opts}
}

func toolUse(id, name string, input map[string]any) map[string]any {
	return map[string]any{"type": "tool_use", "id": id, "name": name, "input": input}
}

func TestPendingTerminalTool(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.jsonl")
	ask := askInput(q("Which color?", "Color", false, "Red", "Green", "Blue"), q("Which tools?", "Tools", true, "Hammer", "Saw"))

	jsonl(t, path, cUser(1, "go"), cAsst(2, "m1", "tool_use", toolUse("tu1", "AskUserQuestion", ask)))
	got, err := pendingTerminalTool(path)
	if err != nil || got == nil || got.ToolUseID != "tu1" || len(got.Questions) != 2 || !got.Questions[1].MultiSelect || got.Questions[0].Options[2].Label != "Blue" || got.AskedAt == 0 {
		t.Fatalf("pending ask: %+v %v", got, err)
	}

	jsonl(t, path, cUser(1, "go"), cAsst(2, "m1", "tool_use", toolUse("tu1", "AskUserQuestion", ask)), cResult(3, "tu1", "answered", false))
	if got, _ := pendingTerminalTool(path); got != nil {
		t.Fatalf("answered question must not be pending: %+v", got)
	}

	jsonl(t, path, cAsst(2, "m1", "tool_use", toolUse("tu1", "AskUserQuestion", ask)), cAsst(5, "m2", "end_turn", map[string]any{"type": "text", "text": "moved on"}))
	if got, _ := pendingTerminalTool(path); got != nil {
		t.Fatalf("a later assistant message makes it stale: %+v", got)
	}

	side := cAsst(4, "m9", "tool_use", toolUse("tu9", "Bash", map[string]any{"command": "ls"}))
	side["isSidechain"] = true
	jsonl(t, path, cAsst(2, "m1", "tool_use", toolUse("tu2", "ExitPlanMode", map[string]any{"plan": "# Plan\n1. do it"})), side)
	got, _ = pendingTerminalTool(path)
	if got == nil || got.Tool != "ExitPlanMode" || !strings.HasPrefix(got.Plan, "# Plan") {
		t.Fatalf("plan: %+v", got)
	}
}

func TestQuestionKeySteps(t *testing.T) {
	one := func(i int) *int { return &i }
	single := []tqQuestion{{Question: "Which pet?", Options: []tqOption{{Label: "Cat"}, {Label: "Dog"}}}}
	multi := []tqQuestion{{Question: "Which langs?", MultiSelect: true, Options: []tqOption{{Label: "Go"}, {Label: "Rust"}, {Label: "Zig"}}}}
	two := []tqQuestion{single[0], multi[0]}
	k := func(keys ...string) keyStep { return keyStep{Keys: keys} }
	cases := []struct {
		name string
		qs   []tqQuestion
		ans  []tqAnswer
		want []keyStep
	}{
		{"single option submits at once", single, []tqAnswer{{Option: one(1)}}, []keyStep{k("2")}},
		{"single typed answer", single, []tqAnswer{{Text: "A  parrot\n"}}, []keyStep{k("3"), {Text: "A parrot"}, k("Enter")}},
		{"multi toggles, Tab, review", multi, []tqAnswer{{Options: []int{0, 2}}}, []keyStep{k("1"), k("3"), k("Tab"), k("1")}},
		{"two questions end on review", two, []tqAnswer{{Option: one(0)}, {Options: []int{1}}}, []keyStep{k("1"), k("2"), k("Tab"), k("1")}},
		{"two singles", []tqQuestion{single[0], single[0]}, []tqAnswer{{Text: "Mango"}, {Option: one(1)}}, []keyStep{k("3"), {Text: "Mango"}, k("Enter"), k("2"), k("1")}},
	}
	for _, c := range cases {
		got, err := questionKeySteps(c.qs, c.ans)
		if err != nil || !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: got %+v %v, want %+v", c.name, got, err, c.want)
		}
	}
	bad := []struct {
		name string
		qs   []tqQuestion
		ans  []tqAnswer
	}{
		{"missing answer", two, []tqAnswer{{Option: one(0)}}},
		{"option out of range", single, []tqAnswer{{Option: one(2)}}},
		{"both option and text", single, []tqAnswer{{Option: one(0), Text: "x"}}},
		{"nothing", single, []tqAnswer{{}}},
		{"multi with text", multi, []tqAnswer{{Options: []int{0}, Text: "x"}}},
		{"multi empty", multi, []tqAnswer{{}}},
		{"multi duplicate", multi, []tqAnswer{{Options: []int{0, 0}}}},
		{"no options", []tqQuestion{{Question: "free?"}}, []tqAnswer{{Text: "x"}}},
	}
	for _, c := range bad {
		if _, err := questionKeySteps(c.qs, c.ans); err == nil {
			t.Errorf("%s: want an error", c.name)
		}
	}
}

func TestPlanOptionsAndSteps(t *testing.T) {
	opts := planOptionsOnScreen(screenPlan)
	if len(opts) != 3 || opts[0].Label != "Yes, and use auto mode" || !opts[2].Feedback || opts[1].Feedback {
		t.Fatalf("options: %+v", opts)
	}
	if s, err := planKeySteps(opts, 2, ""); err != nil || !reflect.DeepEqual(s, []keyStep{{Keys: []string{"2"}}}) {
		t.Fatalf("approve: %+v %v", s, err)
	}
	if s, err := planKeySteps(opts, 3, "Name it hi.txt"); err != nil || len(s) != 3 || s[1].Text != "Name it hi.txt" || s[2].Keys[0] != "Enter" {
		t.Fatalf("feedback: %+v %v", s, err)
	}
	for _, c := range []struct {
		n    int
		text string
	}{{3, ""}, {1, "x"}, {4, ""}} {
		if _, err := planKeySteps(opts, c.n, c.text); err == nil {
			t.Errorf("plan %d %q: want an error", c.n, c.text)
		}
	}
	if planOptionsOnScreen("❯ 1. Yes\n") != nil {
		t.Fatal("options need the proceed prompt")
	}
}

func TestQuestionOnScreen(t *testing.T) {
	tq := &TermQuestion{Questions: []tqQuestion{{Question: "Which color?"}}}
	if !questionOnScreen(screenTwoQuestions, tq) {
		t.Fatal("want on screen")
	}
	if questionOnScreen(strings.Replace(screenTwoQuestions, "Which color?", "Which tools?", 1), tq) {
		t.Fatal("another step must not match")
	}
	if questionOnScreen("Which color?\n❯ ", tq) {
		t.Fatal("prompt text alone is not the form")
	}
	long := &TermQuestion{Questions: []tqQuestion{{Question: "Staging runs PR #97 for its soak. A staging deploy of PR #98 alone removes #97. How?"}}}
	wrapped := "Staging runs PR #97 for its soak. A staging\n  deploy of PR #98 alone removes #97. How?\nEnter to select"
	if !questionOnScreen(wrapped, long) {
		t.Fatal("wrapped question must match")
	}
}

type fakeSender struct{ steps []keyStep }

func (f *fakeSender) Keys(_ string, keys []string) error {
	f.steps = append(f.steps, keyStep{Keys: keys})
	return nil
}
func (f *fakeSender) Text(_ string, text string) error {
	f.steps = append(f.steps, keyStep{Text: text})
	return nil
}

func TestTermAnswerRoute(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	t.Setenv("AGENT_MONITOR_HOME", t.TempDir())
	id := "abcdef12-3456-4789-8abc-def012345679"
	dir := filepath.Join(cfg, "projects", "-w")
	_ = os.MkdirAll(dir, 0o755)
	path := filepath.Join(dir, id+".jsonl")
	ask := askInput(q("Which color?", "Color", false, "Red", "Green", "Blue"), q("Which tools?", "Tools", true, "Hammer", "Saw"))
	jsonl(t, path, cUser(1, "go"), cAsst(2, "m1", "tool_use", toolUse("tu1", "AskUserQuestion", ask)))

	prevStore, prevReg, prevList, prevSend, prevCap, prevDelay, prevAppr := store, paneRegistry, listPanesFn, termAnswerSender, termCaptureFn, termStepDelay, approvalStore
	t.Cleanup(func() {
		store, paneRegistry, listPanesFn, termAnswerSender, termCaptureFn, termStepDelay, approvalStore = prevStore, prevReg, prevList, prevSend, prevCap, prevDelay, prevAppr
	})
	store, paneRegistry, approvalStore = nil, NewPaneRegistry(), nil
	termSent.Lock()
	prevSent := termSent.at
	termSent.at = map[string]int64{} // a sent mark from an earlier run would refuse the answer
	termSent.Unlock()
	t.Cleanup(func() { termSent.Lock(); termSent.at = prevSent; termSent.Unlock() })
	termStepDelay = 0
	fs := &fakeSender{}
	termAnswerSender = fs
	screen := screenTwoQuestions
	termCaptureFn = func(string) (string, error) { return screen, nil }
	listPanesFn = func() ([]TmuxPane, error) {
		return []TmuxPane{{PaneID: "%9", SessionName: "work", WindowID: "@2", CurrentCommand: "node"}}, nil
	}
	sid := "claude:" + id

	get := func() TermQuestion {
		rec := httptest.NewRecorder()
		watchedSessionRoute(rec, httptest.NewRequest("GET", "/x", nil), sid+"/question")
		var out TermQuestion
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return out
	}
	post := func(body string) (int, string) {
		rec := httptest.NewRecorder()
		watchedSessionRoute(rec, httptest.NewRequest("POST", "/x", strings.NewReader(body)), sid+"/answer")
		return rec.Code, rec.Body.String()
	}

	if g := get(); !g.Pending || g.Sendable || g.Why == "" {
		t.Fatalf("no pane registered: %+v", g)
	}
	if code, _ := post(`{"answers":[{"option":0},{"options":[1]}]}`); code != 422 || len(fs.steps) != 0 {
		t.Fatalf("unsendable pane must refuse: %d", code)
	}

	paneRegistry.byAgentID[sid] = &PaneRegistration{AgentID: sid, Tool: ToolClaude, SessionID: id, PaneID: "%9"}
	g := get()
	if !g.Pending || !g.Sendable || !g.OnScreen || g.ToolUseID != "tu1" || len(g.Questions) != 2 {
		t.Fatalf("question: %+v", g)
	}
	if code, body := post(`{"toolUseId":"other","answers":[{"option":0},{"options":[1]}]}`); code != 409 {
		t.Fatalf("stale tool use id: %d %s", code, body)
	}
	if code, body := post(`{"answers":[{"option":0}]}`); code != 400 || len(fs.steps) != 0 {
		t.Fatalf("missing answer: %d %s", code, body)
	}
	screen = strings.NewReplacer("Which color?", "Something else?", "Which tools?", "Other thing?").Replace(screenTwoQuestions)
	if code, _ := post(`{"answers":[{"option":0},{"options":[1]}]}`); code != 422 || len(fs.steps) != 0 {
		t.Fatalf("a screen without the question must refuse: %d", code)
	}
	screen = screenTwoQuestions
	code, body := post(`{"toolUseId":"tu1","answers":[{"option":1},{"options":[0,1]}]}`)
	want := []keyStep{{Keys: []string{"2"}}, {Keys: []string{"1"}}, {Keys: []string{"2"}}, {Keys: []string{"Tab"}}, {Keys: []string{"1"}}}
	if code != 200 || !reflect.DeepEqual(fs.steps, want) {
		t.Fatalf("answer: %d %s %+v", code, body, fs.steps)
	}
	if g := get(); g.SentAt == 0 {
		t.Fatalf("sentAt: %+v", g)
	}
	if code, _ := post(`{"answers":[{"option":1},{"options":[0]}]}`); code != 409 || len(fs.steps) != len(want) {
		t.Fatalf("second send must refuse: %d", code)
	}

	appendJSONL(t, path, cResult(3, "tu1", "answered", false))
	if g := get(); g.Pending {
		t.Fatalf("answered: %+v", g)
	}
	if code, _ := post(`{"answers":[{"option":0}]}`); code != 409 {
		t.Fatalf("nothing pending: %d", code)
	}
}

func TestTermQuestionFromLaterStep(t *testing.T) {
	q := &TermQuestion{Questions: []tqQuestion{
		{Question: "Is this a test of the question dialog?", Options: []tqOption{{Label: "Yes"}, {Label: "No"}}},
		{Question: "Do you want more yes/no questions after this one?", Options: []tqOption{{Label: "Yes"}, {Label: "No"}}},
	}}
	screen := "←  ☒ Test  ☐ More  ✔ Submit  →\nDo you want more yes/no questions after this one?\n❯ 1. Yes\n  2. No\nEnter to select · Tab/Arrow keys to navigate"
	if got := questionStep(screen, q); got != 1 {
		t.Fatalf("step %d", got)
	}
	if got := questionStep("Enter to select\nnothing here", q); got != -1 {
		t.Fatalf("no question: %d", got)
	}
}

func TestTermSentCountsOnlyWhileFresh(t *testing.T) {
	termSent.Lock()
	termSent.at["tu-fresh"] = time.Now().UnixMilli()
	termSent.at["tu-old"] = time.Now().Add(-2 * termSentConfirm).UnixMilli()
	termSent.Unlock()
	t.Cleanup(func() {
		termSent.Lock()
		delete(termSent.at, "tu-fresh")
		delete(termSent.at, "tu-old")
		termSent.Unlock()
	})
	if termSentAt("tu-fresh") == 0 || termSentAt("tu-old") != 0 {
		t.Fatalf("fresh %d, old %d", termSentAt("tu-fresh"), termSentAt("tu-old"))
	}
}

func TestQuestionStepOnReviewTab(t *testing.T) {
	q := &TermQuestion{Questions: []tqQuestion{{Question: "Pick a color?"}, {Question: "Pick a size?"}}}
	review := "←  ☒ Color  ☒ Size  ✔ Submit  →\nReview your answers\n ● Pick a color?\n   → Red\n ● Pick a size?\n   → Large\n❯ 1. Submit answers\n  2. Cancel\nEnter to select · Esc to cancel"
	if got := questionStep(review, q); got != -1 {
		t.Fatalf("review tab step %d, want -1", got)
	}
}
