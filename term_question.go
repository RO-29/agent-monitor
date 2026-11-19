package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
)

// A terminal question is an AskUserQuestion or ExitPlanMode tool call that Claude Code shows in its pane.

type tqOption struct {
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

type tqQuestion struct {
	Question    string     `json:"question"`
	Header      string     `json:"header,omitempty"`
	MultiSelect bool       `json:"multiSelect,omitempty"`
	Options     []tqOption `json:"options"`
}

type tqPlanOption struct {
	N        int    `json:"n"`
	Label    string `json:"label"`
	Feedback bool   `json:"feedback,omitempty"`
}

// TermQuestion is the GET /api/session/{id}/question answer.
type TermQuestion struct {
	Pending     bool           `json:"pending"`
	Tool        string         `json:"tool,omitempty"`
	ToolUseID   string         `json:"toolUseId,omitempty"`
	Questions   []tqQuestion   `json:"questions,omitempty"`
	Plan        string         `json:"plan,omitempty"`
	PlanOptions []tqPlanOption `json:"planOptions,omitempty"`
	AskedAt     int64          `json:"askedAt,omitempty"`
	// Held means an agent-monitor approval holds the call, so the terminal does not show it yet.
	Held     bool   `json:"held,omitempty"`
	Sendable bool   `json:"sendable"`
	OnScreen bool   `json:"onScreen"`
	Why      string `json:"why,omitempty"`
	SentAt   int64  `json:"sentAt,omitempty"`
	// Step is the question the terminal shows now; the ones before it were answered in the terminal.
	Step int `json:"step,omitempty"`
}

type tqAnswer struct {
	Option  *int   `json:"option,omitempty"`
	Options []int  `json:"options,omitempty"`
	Text    string `json:"text,omitempty"`
}

type tqAnswerBody struct {
	ToolUseID string     `json:"toolUseId"`
	Answers   []tqAnswer `json:"answers"`
	// Shorthand for a single question.
	QuestionIndex *int   `json:"questionIndex,omitempty"`
	OptionIndex   *int   `json:"optionIndex,omitempty"`
	Text          string `json:"text,omitempty"`
	// PlanOption is the number the terminal shows next to a plan choice.
	PlanOption int `json:"planOption,omitempty"`
}

// keyStep is one send to the pane: tmux key names, or literal text.
type keyStep struct {
	Keys []string `json:"keys,omitempty"`
	Text string   `json:"text,omitempty"`
	// Pause waits this many milliseconds before the step, for a CLI that redraws first.
	Pause int `json:"pause,omitempty"`
}

type paneKeySender interface {
	Keys(pane string, keys []string) error
	Text(pane, text string) error
}

// targetKeySender sends to a session host ("host:<id>") or a tmux pane.
type targetKeySender struct{}

func (targetKeySender) Keys(target string, keys []string) error { return targetKeys(target, keys) }
func (targetKeySender) Text(target, text string) error          { return targetText(target, text, false) }

// Swapped by tests.
var (
	termAnswerSender paneKeySender = targetKeySender{}
	termCaptureFn                  = func(target string) (string, error) { return targetCapture(target, 0, true) }
	termStepDelay                  = 150 * time.Millisecond
)

// termSentAt is when an answer went in, while it still counts: a question still open 90 s later comes back.
func termSentAt(id string) int64 {
	termSent.Lock()
	defer termSent.Unlock()
	if !termSentLiveLocked(id) {
		return 0
	}
	return termSent.at[id]
}

func termSentLiveLocked(id string) bool {
	at := termSent.at[id]
	return at != 0 && time.Since(time.UnixMilli(at)) <= termSentConfirm
}

var termSent = struct {
	sync.Mutex
	at map[string]int64
}{at: map[string]int64{}}

const transcriptTailBytes = 4 << 20

// pendingTerminalTool finds the newest AskUserQuestion or ExitPlanMode call with no result yet.
func pendingTerminalTool(path string) (*TermQuestion, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if st, err := f.Stat(); err == nil && st.Size() > transcriptTailBytes {
		if _, err := f.Seek(st.Size()-transcriptTailBytes, io.SeekStart); err != nil {
			return nil, err
		}
	}
	type rec struct {
		Type        string `json:"type"`
		IsSidechain bool   `json:"isSidechain"`
		Timestamp   string `json:"timestamp"`
		Message     struct {
			ID      string          `json:"id"`
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	}
	type block struct {
		Type      string         `json:"type"`
		ID        string         `json:"id"`
		Name      string         `json:"name"`
		Input     map[string]any `json:"input"`
		ToolUseID string         `json:"tool_use_id"`
	}
	var cur *TermQuestion
	var curMsg string
	r := bufio.NewReaderSize(f, 1<<16)
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 {
			var rc rec
			if json.Unmarshal(line, &rc) == nil && !rc.IsSidechain && (rc.Type == "assistant" || rc.Type == "user") {
				var blocks []block
				_ = json.Unmarshal(rc.Message.Content, &blocks)
				if rc.Type == "assistant" && cur != nil && rc.Message.ID != "" && rc.Message.ID != curMsg {
					cur = nil // the turn moved on without a result
				}
				for _, b := range blocks {
					switch {
					case rc.Type == "assistant" && b.Type == "tool_use" && (b.Name == "AskUserQuestion" || b.Name == "ExitPlanMode"):
						cur, curMsg = toolQuestion(b.Name, b.ID, b.Input), rc.Message.ID
						if ts, err := time.Parse(time.RFC3339Nano, rc.Timestamp); err == nil {
							cur.AskedAt = ts.UnixMilli()
						}
					case rc.Type == "user" && b.Type == "tool_result" && cur != nil && b.ToolUseID == cur.ToolUseID:
						cur = nil
					}
				}
			}
		}
		if err != nil {
			break
		}
	}
	return cur, nil
}

func toolQuestion(name, id string, input map[string]any) *TermQuestion {
	q := &TermQuestion{Pending: true, Tool: name, ToolUseID: id}
	if name == "ExitPlanMode" {
		q.Plan = str(input["plan"])
		return q
	}
	raw, _ := json.Marshal(input["questions"])
	_ = json.Unmarshal(raw, &q.Questions)
	return q
}

var rePlanOption = regexp.MustCompile(`^\s*(?:❯\s*)?([1-9])\.\s+(.+?)\s*$`)

// planOptionsOnScreen reads the numbered choices under Claude Code's "Would you like to proceed?".
func planOptionsOnScreen(screen string) []tqPlanOption {
	i := strings.LastIndex(screen, "Would you like to proceed?")
	if i < 0 {
		return nil
	}
	var out []tqPlanOption
	for _, ln := range strings.Split(screen[i:], "\n")[1:] {
		m := rePlanOption.FindStringSubmatch(ln)
		if m == nil {
			continue
		}
		n, _ := strconv.Atoi(m[1])
		if n != len(out)+1 {
			break
		}
		out = append(out, tqPlanOption{N: n, Label: m[2], Feedback: strings.Contains(strings.ToLower(m[2]), "tell claude what to change")})
	}
	return out
}

func compactText(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, s)
}

// questionStep is the index of the question the pane's form shows now, or -1; earlier ones were answered there.
func questionStep(screen string, q *TermQuestion) int {
	// The review tab lists every question, so a text match there would point digits at Submit / Cancel.
	if len(q.Questions) == 0 || questionReviewTab(screen) || !strings.Contains(screen, "Enter to select") {
		return -1
	}
	shown := compactText(screen)
	for i, qq := range q.Questions {
		want := []rune(compactText(qq.Question))
		if len(want) > 60 {
			want = want[:60]
		}
		if len(want) > 0 && strings.Contains(shown, string(want)) {
			return i
		}
	}
	return -1
}

// questionReviewTab is true on the last tab of a Claude Code 2.1.327 form: "Review your answers", then Submit or Cancel.
func questionReviewTab(screen string) bool {
	return strings.Contains(screen, "Review your answers") && strings.Contains(screen, "Submit answers")
}

// questionOnScreen is true when the pane shows the question form at any of its questions.
func questionOnScreen(screen string, q *TermQuestion) bool { return questionStep(screen, q) >= 0 }

// questionKeyStepsFrom types the answers from the step the terminal shows: earlier questions were answered there.
func questionKeyStepsFrom(qs []tqQuestion, ans []tqAnswer, step int) ([]keyStep, error) {
	if step <= 0 || step >= len(qs) {
		return questionKeySteps(qs, ans)
	}
	if len(ans) == len(qs) {
		ans = ans[step:]
	}
	rest := qs[step:]
	steps, err := questionKeySteps(rest, ans)
	// One single-select question left of a longer form: the sub-call adds no review step, but the form has one.
	if err == nil && len(rest) == 1 && !rest[0].MultiSelect {
		steps = append(steps, keyStep{Keys: []string{"1"}})
	}
	return steps, err
}

// questionKeySteps maps answers to Claude Code 2.1.326 form keys: a digit picks or toggles an option,
// Tab leaves a multi-select, and a multi-question or multi-select form ends on a review tab where 1 submits.
func questionKeySteps(qs []tqQuestion, ans []tqAnswer) ([]keyStep, error) {
	if len(qs) == 0 {
		return nil, errors.New("no questions")
	}
	if len(ans) != len(qs) {
		return nil, fmt.Errorf("need %d answers, got %d", len(qs), len(ans))
	}
	var steps []keyStep
	review := len(qs) > 1
	for i, q := range qs {
		a := ans[i]
		n := len(q.Options)
		if n == 0 || n > 8 {
			return nil, fmt.Errorf("question %d has %d options; answer it in the terminal", i+1, n)
		}
		text := strings.Join(strings.Fields(a.Text), " ")
		if q.MultiSelect {
			review = true
			if text != "" {
				return nil, fmt.Errorf("question %d: typed answers to a multi-select question need the terminal", i+1)
			}
			if len(a.Options) == 0 {
				return nil, fmt.Errorf("question %d: pick at least one option", i+1)
			}
			seen := map[int]bool{}
			for _, o := range a.Options {
				if o < 0 || o >= n || seen[o] {
					return nil, fmt.Errorf("question %d: bad option %d", i+1, o)
				}
				seen[o] = true
				steps = append(steps, keyStep{Keys: []string{strconv.Itoa(o + 1)}})
			}
			steps = append(steps, keyStep{Keys: []string{"Tab"}})
			continue
		}
		switch {
		case text != "" && a.Option == nil:
			if len([]rune(text)) > 2000 {
				return nil, fmt.Errorf("question %d: answer is too long", i+1)
			}
			steps = append(steps, keyStep{Keys: []string{strconv.Itoa(n + 1)}}, keyStep{Text: text}, keyStep{Keys: []string{"Enter"}})
		case a.Option != nil && text == "":
			if *a.Option < 0 || *a.Option >= n {
				return nil, fmt.Errorf("question %d: bad option %d", i+1, *a.Option)
			}
			steps = append(steps, keyStep{Keys: []string{strconv.Itoa(*a.Option + 1)}})
		default:
			return nil, fmt.Errorf("question %d: give one option or a typed answer", i+1)
		}
	}
	if review {
		steps = append(steps, keyStep{Keys: []string{"1"}})
	}
	return steps, nil
}

// planKeySteps picks a plan choice by its number; the feedback choice also takes text and Enter.
func planKeySteps(opts []tqPlanOption, n int, text string) ([]keyStep, error) {
	var o *tqPlanOption
	for i := range opts {
		if opts[i].N == n {
			o = &opts[i]
		}
	}
	if o == nil {
		return nil, fmt.Errorf("the terminal shows no choice %d", n)
	}
	text = strings.Join(strings.Fields(text), " ")
	steps := []keyStep{{Keys: []string{strconv.Itoa(n)}}}
	if o.Feedback {
		if text == "" {
			return nil, errors.New("type what Claude should change")
		}
		steps = append(steps, keyStep{Text: text}, keyStep{Keys: []string{"Enter"}})
	} else if text != "" {
		return nil, errors.New("only the feedback choice takes text")
	}
	return steps, nil
}

// heldByApproval reports a pending question or plan approval of the session, and whether the terminal shows it too.
func heldByApproval(sessionID string) (held, shown bool) {
	if approvalStore == nil {
		return false, false
	}
	approvalStore.mu.Lock()
	defer approvalStore.mu.Unlock()
	for _, p := range approvalStore.pending {
		if p.a.SessionID == sessionID && (p.a.Kind == "question" || p.a.Kind == "plan") {
			return true, p.term != nil
		}
	}
	return false, false
}

// termQuestionFor builds the GET answer; it captures the pane only when a question is pending.
func termQuestionFor(id string) (*TermQuestion, string, int, error) {
	ref := resolveWatched(id)
	if !ref.Resolved || ref.Path == "" {
		return nil, "", http.StatusNotFound, errors.New("session not found")
	}
	if ref.Tool != ToolClaude {
		return &TermQuestion{Why: "only Claude Code questions are supported"}, "", 0, nil
	}
	q, err := pendingTerminalTool(ref.Path)
	if err != nil {
		return nil, "", http.StatusInternalServerError, err
	}
	if q == nil {
		return &TermQuestion{}, "", 0, nil
	}
	held, shown := heldByApproval(ref.ID)
	q.Held = held
	q.SentAt = termSentAt(q.ToolUseID)
	pane := watchedPane(ref.ID)
	q.Sendable = pane.Sendable
	switch {
	case q.Held && shown:
		q.Why = "an approval card shows this request; answer it there or in the terminal"
	case q.Held:
		q.Why = "agent-monitor holds this request; answer its approval card"
	case !pane.Registered:
		q.Why = "agent-monitor cannot reach this terminal; answer it there"
	case !pane.Live:
		q.Why = "the terminal of this session is closed"
	case !pane.Sendable:
		q.Why = "the agent no longer runs in its terminal"
	default:
		screen, err := termCaptureFn(pane.Target())
		if err != nil {
			q.Why = "agent-monitor could not read the terminal"
			break
		}
		if q.Tool == "ExitPlanMode" {
			q.PlanOptions = planOptionsOnScreen(screen)
			q.OnScreen = len(q.PlanOptions) > 0
		} else {
			q.Step = questionStep(screen, q)
			q.OnScreen = q.Step >= 0
			q.Step = max(q.Step, 0)
		}
		switch {
		case q.Tool != "ExitPlanMode" && questionReviewTab(screen):
			q.Why = "the form shows its review tab; submit or cancel it in the terminal"
		case !q.OnScreen:
			q.Why = "the terminal does not show this question now"
		}
	}
	return q, pane.Target(), 0, nil
}

func serveTermQuestion(w http.ResponseWriter, id string) {
	q, _, code, err := termQuestionFor(id)
	if err != nil {
		writeJSONStatus(w, code, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, q)
}

func serveTermAnswer(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body tqAnswerBody
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
		writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	q, paneID, code, err := termQuestionFor(id)
	if err != nil {
		writeJSONStatus(w, code, map[string]any{"error": err.Error()})
		return
	}
	if !q.Pending {
		writeJSONStatus(w, http.StatusConflict, map[string]any{"error": "no question waits in the terminal"})
		return
	}
	if body.ToolUseID != "" && body.ToolUseID != q.ToolUseID {
		writeJSONStatus(w, http.StatusConflict, map[string]any{"error": "the terminal now shows a different question"})
		return
	}
	if !q.OnScreen {
		// Not answerable from here (not "answered elsewhere"): clients keep the card and show why.
		writeJSONStatus(w, http.StatusUnprocessableEntity, map[string]any{"error": q.Why, "code": "not-answerable"})
		return
	}
	var steps []keyStep
	if q.Tool == "ExitPlanMode" {
		steps, err = planKeySteps(q.PlanOptions, body.PlanOption, body.Text)
	} else {
		ans := body.Answers
		if len(ans) == 0 && len(q.Questions) == 1 && (body.QuestionIndex == nil || *body.QuestionIndex == 0) {
			ans = []tqAnswer{{Option: body.OptionIndex, Text: body.Text}}
		}
		steps, err = questionKeyStepsFrom(q.Questions, ans, q.Step)
	}
	if err != nil {
		writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	termSent.Lock()
	if termSentLiveLocked(q.ToolUseID) {
		termSent.Unlock()
		writeJSONStatus(w, http.StatusConflict, map[string]any{"error": "an answer was already sent to the terminal"})
		return
	}
	now := time.Now().UnixMilli()
	for k, at := range termSent.at {
		if now-at > int64(time.Hour/time.Millisecond) {
			delete(termSent.at, k)
		}
	}
	termSent.at[q.ToolUseID] = now
	termSent.Unlock()
	for i, s := range steps {
		if i > 0 && termStepDelay > 0 {
			time.Sleep(termStepDelay)
		}
		if s.Text != "" {
			err = termAnswerSender.Text(paneID, s.Text)
		} else {
			err = termAnswerSender.Keys(paneID, s.Keys)
		}
		if err != nil {
			// Nothing reached the pane yet, so the answer may be retried.
			if i == 0 {
				termSent.Lock()
				delete(termSent.at, q.ToolUseID)
				termSent.Unlock()
			}
			writeJSONStatus(w, http.StatusInternalServerError, map[string]any{"error": err.Error(), "sentSteps": i})
			return
		}
	}
	if paneRegistry != nil {
		paneRegistry.Touch(id)
	}
	writeJSON(w, map[string]any{"ok": true, "toolUseId": q.ToolUseID, "steps": steps, "sentAt": now})
}
