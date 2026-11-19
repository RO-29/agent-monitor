package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Approvals for a session agent-monitor can type into: the CLI shows its own prompt at once, the card
// shows on every surface, a card decision is typed into the terminal, and a terminal answer closes the card.

const (
	approvalsModeBoth      = "both"
	approvalsModeHold      = "hold"
	approvalSourceTerminal = "terminal-shown"
	stateAnsweredInTerm    = "answered-in-terminal"
)

// termWatch is the per-approval state of a terminal-shown approval.
type termWatch struct {
	transcript string
	from       int64 // transcript offset to scan from

	sending bool
	sent    *approvalDecision
	sentBy  string
	seen    bool // the prompt was on screen at least once
	gone    int  // checks in a row with the prompt off screen after it was seen
	lost    int  // checks in a row with no reachable terminal
	covered int  // checks in a row with another prompt drawn over this one
}

// Swapped by tests.
var (
	approvalPaneFn      = watchedPane
	termApprovalPoll    = time.Second
	termApprovalMaxAge  = 24 * time.Hour
	termSentConfirm     = 90 * time.Second
	termGoneAllowChecks = 3
	termCoveredChecks   = 60 // one check a second: a minute under another prompt
	termGoneOtherChecks = 10
)

var errNotOnScreen = errors.New("the terminal does not show this prompt now; answer it in the terminal")

func terminalReachable(storeID string) bool {
	if hostForSession(storeID) != nil {
		return true
	}
	p := watchedPane(storeID)
	return p.Registered && p.Live && p.Sendable
}

// transcriptSlack is how far before the request the scan starts, so the tool call itself is read.
const transcriptSlack = 1 << 20

// showInTerminal stores a terminal-shown card; false when the same prompt already has one.
func (s *ApprovalStore) showInTerminal(hook string, a *Approval, p hookPayload) bool {
	a.Source, a.ToolUseID = approvalSourceTerminal, p.ToolUseID
	tw := &termWatch{transcript: p.TranscriptPath}
	if st, err := os.Stat(p.TranscriptPath); err == nil && st.Size() > transcriptSlack {
		tw.from = st.Size() - transcriptSlack
	}
	pa := &pendingApproval{a: a, hook: hook, reply: make(chan approvalDecision, 1), term: tw}
	s.mu.Lock()
	// PreToolUse and PermissionRequest can both fire for one question or plan; the terminal shows one prompt.
	for _, o := range s.pending {
		if o.term != nil && o.a.SessionID == a.SessionID && o.a.ToolName == a.ToolName && sameInput(o.a.Input, a.Input) {
			if o.a.ToolUseID == "" {
				o.a.ToolUseID = a.ToolUseID
			}
			s.mu.Unlock()
			return false
		}
	}
	s.pending[a.ID] = pa
	snap := copyApproval(a)
	s.mu.Unlock()
	publishV3("approval", "approval", snap)
	go s.watchTerminal(a.ID, termApprovalPoll)
	return true
}

// decideInTerminal types a card decision into the session's terminal once its prompt is on screen.
func (s *ApprovalStore) decideInTerminal(id string, d approvalDecision, by string) (*Approval, int, error) {
	s.mu.Lock()
	p, ok := s.pending[id]
	if !ok || p.term == nil {
		s.mu.Unlock()
		return nil, http.StatusConflict, errors.New("approval is not pending")
	}
	if p.term.sent != nil || p.term.sending {
		s.mu.Unlock()
		return nil, http.StatusConflict, errors.New("a decision was already sent to the terminal")
	}
	if s.olderLookalikeLocked(p.a) {
		s.mu.Unlock()
		return nil, statusRefused, errors.New("an older request in this session looks the same on screen; answer that one first")
	}
	p.term.sending = true
	a := copyApproval(p.a)
	s.mu.Unlock()
	release := func() {
		s.mu.Lock()
		p.term.sending = false
		s.mu.Unlock()
	}

	pane := approvalPaneFn(a.SessionID)
	if !pane.Registered || !pane.Sendable {
		release()
		return nil, http.StatusConflict, errors.New("the session's terminal is not reachable; answer it in the terminal")
	}
	target := pane.Target()
	screen, err := termCaptureFn(target)
	if err != nil {
		release()
		return nil, http.StatusBadGateway, fmt.Errorf("could not read the terminal: %v", err)
	}
	steps, code, err := terminalKeySteps(a, d, screen)
	if err != nil {
		release()
		return nil, code, err
	}
	for i, st := range steps {
		if i > 0 && termStepDelay > 0 {
			time.Sleep(termStepDelay)
		}
		if st.Pause > 0 {
			time.Sleep(time.Duration(st.Pause) * time.Millisecond)
		}
		if st.Text != "" {
			err = termAnswerSender.Text(target, st.Text)
		} else {
			err = termAnswerSender.Keys(target, st.Keys)
		}
		if err != nil {
			if i == 0 {
				release()
			} else {
				s.markSent(id, d, by)
			}
			return nil, http.StatusBadGateway, fmt.Errorf("sending to the terminal: %v", err)
		}
	}
	snap := s.markSent(id, d, by)
	if snap == nil {
		return nil, http.StatusConflict, errors.New("approval is not pending")
	}
	if d.Always && d.Behavior == "allow" && a.Kind == "permission" && d.AlwaysScope != "claude" {
		s.addAlwaysRule(a)
	}
	return snap, http.StatusOK, nil
}

func (s *ApprovalStore) markSent(id string, d approvalDecision, by string) *Approval {
	s.mu.Lock()
	p, ok := s.pending[id]
	if !ok || p.term == nil {
		s.mu.Unlock()
		return nil
	}
	p.term.sending, p.term.sent, p.term.sentBy, p.term.gone = false, &d, by, 0
	p.a.SentAt = time.Now().UnixMilli()
	snap := copyApproval(p.a)
	s.mu.Unlock()
	publishV3("approval", "approval", snap)
	return snap
}

func (s *ApprovalStore) watchTerminal(id string, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for range t.C {
		if !s.checkTerminal(id) {
			return
		}
	}
}

// checkTerminal looks once for the answer; false when the approval left pending.
func (s *ApprovalStore) checkTerminal(id string) bool {
	s.mu.Lock()
	p, ok := s.pending[id]
	if !ok || p.term == nil {
		s.mu.Unlock()
		return false
	}
	a := copyApproval(p.a)
	tw := *p.term
	s.mu.Unlock()

	if tw.transcript == "" {
		if ref := resolveWatched(a.SessionID); ref.Path != "" {
			tw.transcript = ref.Path
			s.updateWatch(id, func(w *termWatch) { w.transcript = ref.Path })
		}
	}
	if out, ok := terminalOutcome(a, tw.transcript, tw.from); ok {
		s.closeFromTerminal(id, out)
		return false
	}
	if time.Since(time.UnixMilli(a.CreatedAt)) > termApprovalMaxAge {
		s.finish(id, "expired", "", "", nil)
		return false
	}
	pane := approvalPaneFn(a.SessionID)
	if !pane.Registered || !pane.Live {
		lost := 0
		s.updateWatch(id, func(w *termWatch) { w.lost++; lost = w.lost })
		if lost >= 5 {
			s.finish(id, "expired", "", "the session's terminal is gone", nil)
			return false
		}
		return true
	}
	screen, err := termCaptureFn(pane.Target())
	if err != nil {
		return true
	}
	on := promptOnScreen(a, screen)
	// Another prompt drawn over this one hides it; that is not an answer.
	covered := !on && panePromptOpen(screen)
	var gone, coveredFor int
	var seen bool
	s.updateWatch(id, func(w *termWatch) {
		w.lost = 0
		if covered && w.seen {
			w.covered++
		} else {
			w.covered = 0
		}
		if on {
			w.seen, w.gone = true, 0
		} else if !covered && (w.seen || w.sent != nil) {
			w.gone++
		}
		gone, seen, coveredFor = w.gone, w.seen, w.covered
	})
	switch {
	case tw.sent != nil && gone >= 2:
		s.closeFromTerminal(id, termOutcome{})
		return false
	case tw.sent != nil && on && time.Since(time.UnixMilli(a.SentAt)) > termSentConfirm:
		s.clearSent(id)
	case tw.sent == nil && seen && coveredFor >= termCoveredChecks:
		// Covered for a minute: it was most likely answered under the cover; record no choice.
		s.closeFromTerminal(id, termOutcome{})
		return false
	case tw.sent == nil && seen && a.Kind == "permission" && gone >= termGoneAllowChecks:
		// A rejection writes its result at once, so a prompt that left with no result means the tool runs.
		s.closeFromTerminal(id, termOutcome{state: "allowed", choice: "Yes"})
		return false
	case tw.sent == nil && seen && gone >= termGoneOtherChecks:
		s.closeFromTerminal(id, termOutcome{})
		return false
	}
	return true
}

func (s *ApprovalStore) updateWatch(id string, fn func(w *termWatch)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p, ok := s.pending[id]; ok && p.term != nil {
		fn(p.term)
	}
}

// clearSent puts the card back when the terminal still shows the prompt long after the keys went in.
func (s *ApprovalStore) clearSent(id string) {
	s.mu.Lock()
	p, ok := s.pending[id]
	if !ok || p.term == nil {
		s.mu.Unlock()
		return
	}
	p.term.sent, p.term.sentBy = nil, ""
	p.a.SentAt = 0
	snap := copyApproval(p.a)
	s.mu.Unlock()
	publishV3("approval", "approval", snap)
}

type termOutcome struct {
	state  string // allowed or denied; empty when unknown
	choice string
}

// closeFromTerminal finishes a terminal-shown approval: a sent decision is confirmed, otherwise the terminal answered.
func (s *ApprovalStore) closeFromTerminal(id string, out termOutcome) {
	s.mu.Lock()
	p, ok := s.pending[id]
	if !ok || p.term == nil {
		s.mu.Unlock()
		return
	}
	sent, by, kind := p.term.sent, p.term.sentBy, p.a.Kind
	s.mu.Unlock()
	if sent != nil {
		state := "allowed"
		if sent.Behavior == "deny" {
			state = "denied"
		}
		s.finishChoice(id, state, by, sent.Message, firstOf(out.choice, decisionChoice(kind, *sent)), nil)
		return
	}
	s.finishChoice(id, stateAnsweredInTerm, "terminal", "", out.choice, nil)
}

// terminalEvent closes the session's terminal-shown approvals on a hook event that only follows an answer.
func (s *ApprovalStore) terminalEvent(tool Tool, sessionID, event, toolName string) {
	switch event {
	case "Stop", "UserPromptSubmit", "SessionEnd", "PostToolUse", "PostToolUseFailure":
	default:
		return
	}
	storeID := sid(tool, sessionID)
	now := time.Now().UnixMilli()
	type item struct {
		id         string
		a          *Approval
		transcript string
		from       int64
	}
	var list []item
	s.mu.Lock()
	for id, p := range s.pending {
		// A PostToolUse of another tool says nothing about this prompt.
		if strings.HasPrefix(event, "PostToolUse") && toolName != p.a.ToolName {
			continue
		}
		if p.term != nil && p.a.SessionID == storeID && p.a.CreatedAt < now {
			list = append(list, item{id, copyApproval(p.a), p.term.transcript, p.term.from})
		}
	}
	s.mu.Unlock()
	for _, it := range list {
		if event == "SessionEnd" {
			s.finish(it.id, "expired", "", "the session ended", nil)
			continue
		}
		out, _ := terminalOutcome(it.a, it.transcript, it.from)
		s.closeFromTerminal(it.id, out)
	}
}

// decisionChoice is the short text of a decision, as the terminal would show it.
func decisionChoice(kind string, d approvalDecision) string {
	if d.Behavior == "deny" {
		if m := strings.TrimSpace(d.Message); m != "" && !d.Interrupt {
			return "No: " + clipString(m, 200)
		}
		return "No"
	}
	switch {
	case kind == "question" && len(d.Answers) > 0:
		parts := make([]string, 0, len(d.Answers))
		for _, v := range d.Answers {
			parts = append(parts, v)
		}
		return clipString(strings.Join(parts, "; "), 200)
	case d.Always:
		return "Yes, don't ask again"
	}
	return "Yes"
}

// ── terminal answers from the transcript ──

func terminalOutcome(a *Approval, path string, from int64) (termOutcome, bool) {
	if path == "" {
		return termOutcome{}, false
	}
	if a.Agent == "codex" {
		return codexToolOutcome(path, a.CreatedAt, from)
	}
	return claudeToolOutcome(path, a, from)
}

// openTail opens a transcript at from, or at its last transcriptTailBytes when that is later.
func openTail(path string, from int64) (*os.File, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	if st, err := f.Stat(); err == nil {
		if t := st.Size() - transcriptTailBytes; t > from {
			from = t
		}
	}
	if from > 0 {
		if _, err := f.Seek(from, io.SeekStart); err != nil {
			f.Close()
			return nil, err
		}
	}
	return f, nil
}

const rejectMarker = "doesn't want to proceed with this tool use"

// claudeToolOutcome finds the result of the approval's tool call; it skips results older than the approval.
func claudeToolOutcome(path string, a *Approval, from int64) (termOutcome, bool) {
	f, err := openTail(path, from)
	if err != nil {
		return termOutcome{}, false
	}
	defer f.Close()
	type rec struct {
		Type        string `json:"type"`
		IsSidechain bool   `json:"isSidechain"`
		Timestamp   string `json:"timestamp"`
		Message     struct {
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	}
	type block struct {
		Type      string          `json:"type"`
		ID        string          `json:"id"`
		Name      string          `json:"name"`
		Input     map[string]any  `json:"input"`
		ToolUseID string          `json:"tool_use_id"`
		Content   json.RawMessage `json:"content"`
		IsError   bool            `json:"is_error"`
	}
	ids := map[string]bool{}
	var out termOutcome
	found := false
	r := bufio.NewReaderSize(f, 1<<16)
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 {
			var rc rec
			if json.Unmarshal(line, &rc) == nil && (rc.Type == "assistant" || rc.Type == "user") {
				var blocks []block
				_ = json.Unmarshal(rc.Message.Content, &blocks)
				for _, b := range blocks {
					switch {
					case rc.Type == "assistant" && b.Type == "tool_use":
						if (a.ToolUseID != "" && b.ID == a.ToolUseID) || (a.ToolUseID == "" && b.Name == a.ToolName && sameInput(b.Input, a.Input)) {
							ids[b.ID] = true
						}
					case rc.Type == "user" && b.Type == "tool_result" && ids[b.ToolUseID]:
						if ts, err := time.Parse(time.RFC3339Nano, rc.Timestamp); err == nil && ts.UnixMilli() < a.CreatedAt-2000 {
							continue
						}
						out, found = classifyClaudeResult(a.Kind, resultText(b.Content), b.IsError), true
					}
				}
			}
		}
		if err != nil {
			break
		}
	}
	return out, found
}

func sameInput(x, y map[string]any) bool {
	if reflect.DeepEqual(x, y) {
		return true
	}
	bx, _ := json.Marshal(x)
	by, _ := json.Marshal(y)
	return string(bx) == string(by)
}

func resultText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	_ = json.Unmarshal(raw, &parts)
	var b strings.Builder
	for _, p := range parts {
		b.WriteString(p.Text)
	}
	return b.String()
}

func classifyClaudeResult(kind, text string, isError bool) termOutcome {
	if strings.Contains(text, rejectMarker) {
		choice := "No"
		if _, msg, ok := strings.Cut(text, "the user said:\n"); ok && strings.TrimSpace(msg) != "" {
			choice = "No: " + clipString(strings.TrimSpace(msg), 200)
		}
		return termOutcome{state: "denied", choice: choice}
	}
	if kind == "question" {
		if _, ans, ok := strings.Cut(text, "User has answered your questions:"); ok {
			ans = strings.TrimSpace(ans)
			if i := strings.Index(ans, ". You can now continue"); i >= 0 {
				ans = ans[:i]
			}
			return termOutcome{state: "allowed", choice: clipString(ans, 200)}
		}
	}
	if kind == "plan" && isError {
		return termOutcome{state: "denied", choice: "No"}
	}
	return termOutcome{state: "allowed", choice: "Yes"}
}

// codexToolOutcome finds the first tool output written after the approval for a call started before it.
func codexToolOutcome(path string, createdAt, from int64) (termOutcome, bool) {
	f, err := openTail(path, from)
	if err != nil {
		return termOutcome{}, false
	}
	defer f.Close()
	type rec struct {
		Timestamp string `json:"timestamp"`
		Type      string `json:"type"`
		Payload   struct {
			Type   string          `json:"type"`
			CallID string          `json:"call_id"`
			Output json.RawMessage `json:"output"`
		} `json:"payload"`
	}
	calls := map[string]bool{}
	r := bufio.NewReaderSize(f, 1<<16)
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 {
			var rc rec
			if json.Unmarshal(line, &rc) == nil && rc.Type == "response_item" {
				ts, terr := time.Parse(time.RFC3339Nano, rc.Timestamp)
				switch rc.Payload.Type {
				case "function_call", "custom_tool_call", "local_shell_call":
					if terr == nil && ts.UnixMilli() <= createdAt+1000 {
						calls[rc.Payload.CallID] = true
					}
				case "function_call_output", "custom_tool_call_output":
					if terr == nil && ts.UnixMilli() >= createdAt && calls[rc.Payload.CallID] {
						text := resultText(rc.Payload.Output)
						if json.Unmarshal(rc.Payload.Output, &struct{}{}) == nil {
							var o struct {
								Output string `json:"output"`
							}
							_ = json.Unmarshal(rc.Payload.Output, &o)
							text += o.Output
						}
						low := strings.ToLower(text)
						if strings.Contains(low, "aborted by user") || strings.Contains(low, "rejected by user") || strings.Contains(low, "declined by user") {
							return termOutcome{state: "denied", choice: "No"}, true
						}
						return termOutcome{state: "allowed", choice: "Yes"}, true
					}
				}
			}
		}
		if err != nil {
			break
		}
	}
	return termOutcome{}, false
}

// ── prompts on screen and their key maps ──

// promptOption is one numbered choice of a CLI prompt; Key is the letter the label names, as "(y)".
type promptOption struct {
	N      int
	Label  string
	Cursor bool
	Key    string
}

var (
	rePromptOption = regexp.MustCompile(`^\s*([❯›>])?\s*([1-9])\.\s+(.+?)\s*$`)
	rePromptKey    = regexp.MustCompile(`\(([a-z]|esc)\)$`)
)

// promptOptions reads the numbered choices after the last header line, up to the footer when one is shown.
// Some dialogs (Claude Code WebFetch) end on the last choice with no footer line.
func promptOptions(screen, header, footer string) []promptOption {
	i := strings.LastIndex(screen, header)
	if i < 0 {
		return nil
	}
	var out []promptOption
	for _, ln := range strings.Split(screen[i:], "\n")[1:] {
		if strings.Contains(ln, footer) {
			break
		}
		m := rePromptOption.FindStringSubmatch(ln)
		if m == nil {
			continue
		}
		n, _ := strconv.Atoi(m[2])
		if n != len(out)+1 {
			if len(out) > 0 {
				break
			}
			continue
		}
		o := promptOption{N: n, Label: m[3], Cursor: m[1] != ""}
		if k := rePromptKey.FindStringSubmatch(o.Label); k != nil {
			o.Key = k[1]
		}
		out = append(out, o)
	}
	return out
}

// Claude Code 2.1.326 permission dialog: "Do you want to …?", numbered choices, "Esc to cancel".
func claudePermOptions(screen string) []promptOption {
	opts := promptOptions(screen, "Do you want to", "Esc to cancel")
	if len(opts) < 2 || !strings.HasPrefix(opts[0].Label, "Yes") {
		return nil
	}
	return opts
}

// Codex 0.169 approval: "Would you like to run the following command?" or "… make the following edits?".
func codexPermOptions(screen string) []promptOption {
	opts := promptOptions(screen, "Would you like to", "Press enter to confirm")
	if len(opts) < 2 || !strings.HasPrefix(opts[0].Label, "Yes") {
		return nil
	}
	return opts
}

// promptNeedle is text the prompt must show for this approval; empty means no content check.
func promptNeedle(a *Approval) string {
	in := a.Input
	var s string
	switch {
	case str(in["file_path"]) != "":
		s = filepath.Base(str(in["file_path"]))
	case str(in["notebook_path"]) != "":
		s = filepath.Base(str(in["notebook_path"]))
	case str(in["url"]) != "":
		s = str(in["url"])
	case isPatchTool(a.ToolName):
		if files := patchFiles(str(in["command"]) + str(in["patch"]) + str(in["input"])); len(files) > 0 {
			s = filepath.Base(files[0])
		}
	default:
		switch v := in["command"].(type) {
		case string:
			s = v
		case []any:
			parts := make([]string, 0, len(v))
			for _, p := range v {
				parts = append(parts, str(p))
			}
			s = strings.Join(parts, " ")
		}
	}
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	r := []rune(compactText(s))
	if len(r) > 30 {
		r = r[:30]
	}
	return string(r)
}

// olderLookalikeLocked is true when an older unanswered terminal request of the same session
// cannot be told apart from a on screen: the keys for a would answer that older prompt.
func (s *ApprovalStore) olderLookalikeLocked(a *Approval) bool {
	n := promptNeedle(a)
	for _, q := range s.pending {
		o := q.a
		if q.term == nil || q.term.sent != nil || o.ID == a.ID || o.SessionID != a.SessionID {
			continue
		}
		older := o.CreatedAt < a.CreatedAt || (o.CreatedAt == a.CreatedAt && o.ID < a.ID)
		if on := promptNeedle(o); older && (n == "" || on == "" || on == n) {
			return true
		}
	}
	return false
}

func needleOnScreen(a *Approval, screen string) bool {
	n := promptNeedle(a)
	return n == "" || strings.Contains(compactText(screen), n)
}

// promptOnScreen is true when the terminal shows this approval's prompt.
func promptOnScreen(a *Approval, screen string) bool {
	if a.Agent == "codex" {
		return codexPermOptions(screen) != nil && needleOnScreen(a, screen)
	}
	switch a.Kind {
	case "question":
		// The review tab is still the open form: the user has not submitted it.
		return questionReviewTab(screen) || questionOnScreen(screen, toolQuestion("AskUserQuestion", a.ToolUseID, a.Input))
	case "plan":
		return len(planOptionsOnScreen(screen)) > 0
	}
	return claudePermOptions(screen) != nil && needleOnScreen(a, screen)
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// terminalKeySteps maps a card decision to keys for the prompt on screen; the int is the HTTP status of an error.
func terminalKeySteps(a *Approval, d approvalDecision, screen string) ([]keyStep, int, error) {
	if !promptOnScreen(a, screen) {
		return nil, http.StatusConflict, errNotOnScreen
	}
	msg := oneLine(d.Message)
	if len([]rune(msg)) > 2000 {
		return nil, http.StatusBadRequest, errors.New("the message is too long")
	}
	switch {
	case a.Agent == "codex":
		return codexKeySteps(codexPermOptions(screen), d, msg)
	case a.Agent != "claude":
		return nil, http.StatusBadRequest, fmt.Errorf("%s prompts cannot be answered from a card", a.Agent)
	case a.Kind == "question" && questionReviewTab(screen) && d.Behavior != "deny":
		return nil, statusRefused, errors.New("the form shows its review tab; submit or cancel it in the terminal")
	case a.Kind == "question":
		if d.Behavior == "deny" {
			return claudeEscSteps(d, msg), 0, nil
		}
		q := toolQuestion("AskUserQuestion", a.ToolUseID, a.Input)
		ans, err := answersToTerm(q.Questions, d.Answers)
		if err != nil {
			return nil, http.StatusBadRequest, err
		}
		steps, err := questionKeyStepsFrom(q.Questions, ans, questionStep(screen, q))
		if err != nil {
			return nil, http.StatusBadRequest, err
		}
		return steps, 0, nil
	case a.Kind == "plan":
		return claudePlanSteps(planOptionsOnScreen(screen), d, msg)
	}
	if d.UpdatedInput != nil && !sameInput(d.UpdatedInput, a.Input) {
		return nil, http.StatusBadRequest, errors.New("an edited request cannot be typed into the terminal; allow or deny it as it is")
	}
	return claudePermSteps(claudePermOptions(screen), d, msg)
}

// claudeEscSteps cancels a question or plan; a message follows as the next prompt.
func claudeEscSteps(d approvalDecision, msg string) []keyStep {
	steps := []keyStep{{Keys: []string{"Escape"}}}
	if msg != "" && !d.Interrupt {
		steps = append(steps, keyStep{Text: msg, Pause: 400}, keyStep{Keys: []string{"Enter"}})
	}
	return steps
}

// claudePermSteps: a digit picks a choice at once; "No" with a message moves the cursor there, Tab opens the amend field.
func claudePermSteps(opts []promptOption, d approvalDecision, msg string) ([]keyStep, int, error) {
	digit := func(o promptOption) []keyStep { return []keyStep{{Keys: []string{strconv.Itoa(o.N)}}} }
	if d.Behavior == "allow" {
		if d.Always && d.AlwaysScope == "claude" {
			for _, o := range opts {
				l := strings.ToLower(strings.ReplaceAll(o.Label, "’", "'"))
				if strings.HasPrefix(l, "yes, and") && (strings.Contains(l, "don't ask again") || strings.Contains(l, "always allow") || strings.Contains(l, "accept edits")) {
					return digit(o), 0, nil
				}
			}
			return nil, http.StatusConflict, errors.New("the terminal offers no \"don't ask again\" choice for this request")
		}
		return digit(opts[0]), 0, nil
	}
	var no *promptOption
	for i := range opts {
		if strings.HasPrefix(opts[i].Label, "No") {
			no = &opts[i]
		}
	}
	if no == nil {
		return nil, http.StatusConflict, errors.New("the terminal shows no \"No\" choice")
	}
	if msg == "" || d.Interrupt {
		return digit(*no), 0, nil
	}
	cur := 1
	for _, o := range opts {
		if o.Cursor {
			cur = o.N
		}
	}
	var steps []keyStep
	for i := cur; i < no.N; i++ {
		steps = append(steps, keyStep{Keys: []string{"Down"}})
	}
	for i := cur; i > no.N; i-- {
		steps = append(steps, keyStep{Keys: []string{"Up"}})
	}
	return append(steps, keyStep{Keys: []string{"Tab"}}, keyStep{Text: msg}, keyStep{Keys: []string{"Enter"}}), 0, nil
}

// claudePlanSteps: allow picks "manually approve edits" when offered, else the first Yes; deny with text uses the feedback choice.
func claudePlanSteps(opts []tqPlanOption, d approvalDecision, msg string) ([]keyStep, int, error) {
	if d.Behavior == "allow" {
		pick := 0
		for _, o := range opts {
			l := strings.ToLower(o.Label)
			if strings.HasPrefix(l, "yes") && pick == 0 {
				pick = o.N
			}
			if strings.HasPrefix(l, "yes") && strings.Contains(l, "manually approve") {
				pick = o.N
				break
			}
		}
		if pick == 0 {
			return nil, http.StatusConflict, errors.New("the terminal shows no choice that approves the plan")
		}
		steps, err := planKeySteps(opts, pick, "")
		if err != nil {
			return nil, http.StatusBadRequest, err
		}
		return steps, 0, nil
	}
	if msg != "" && !d.Interrupt {
		for _, o := range opts {
			if o.Feedback {
				steps, err := planKeySteps(opts, o.N, msg)
				if err != nil {
					return nil, http.StatusBadRequest, err
				}
				return steps, 0, nil
			}
		}
	}
	return []keyStep{{Keys: []string{"Escape"}}}, 0, nil
}

// codexKeySteps uses the letter each choice names; "No" is Esc, and a message follows as the next prompt.
func codexKeySteps(opts []promptOption, d approvalDecision, msg string) ([]keyStep, int, error) {
	if d.Behavior == "allow" {
		k := opts[0].Key
		if k == "" || k == "esc" {
			k = "y"
		}
		return []keyStep{{Keys: []string{k}}}, 0, nil
	}
	steps := []keyStep{{Keys: []string{"Escape"}}}
	if msg != "" && !d.Interrupt {
		steps = append(steps, keyStep{Text: msg, Pause: 500}, keyStep{Keys: []string{"Enter"}})
	}
	return steps, 0, nil
}

// answersToTerm turns card answers (question text to labels joined by ", ") into terminal form answers.
func answersToTerm(qs []tqQuestion, m map[string]string) ([]tqAnswer, error) {
	out := make([]tqAnswer, len(qs))
	for i, q := range qs {
		v, ok := m[q.Question]
		v = strings.TrimSpace(v)
		if !ok || v == "" {
			return nil, fmt.Errorf("answer every question: %q has no answer", clipString(q.Question, 80))
		}
		idx := func(label string) int {
			for j, o := range q.Options {
				if o.Label == label {
					return j
				}
			}
			return -1
		}
		if q.MultiSelect {
			labels := []string{v}
			if idx(v) < 0 {
				labels = strings.Split(v, ", ")
			}
			for _, l := range labels {
				j := idx(strings.TrimSpace(l))
				if j < 0 {
					return nil, fmt.Errorf("question %d: typed answers to a multi-select question need the terminal", i+1)
				}
				out[i].Options = append(out[i].Options, j)
			}
			continue
		}
		if j := idx(v); j >= 0 {
			out[i].Option = &j
		} else {
			out[i].Text = v
		}
	}
	return out, nil
}
