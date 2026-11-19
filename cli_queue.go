package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// The CLI's own message queue: Claude Code logs queue-operation records in the transcript.

// CLIQueueItem is one message waiting in the agent CLI's own queue.
type CLIQueueItem struct {
	ID   string `json:"id"`
	Text string `json:"text"`
	At   int64  `json:"at"`
	Kind string `json:"kind"` // user | system | bash
	// Status of a ! command agent-monitor sent: "waiting" behind a busy turn or "running"; it goes when its result is in the transcript.
	Status string `json:"status,omitempty"`
	Live   string `json:"live,omitempty"` // a running ! command's output as the terminal shows it now
}

// cliBang is a ! command sent from agent-monitor; Claude Code writes it to the transcript only when it ends.
type cliBang struct {
	ID     string
	Cmd    string
	At     int64
	Queued bool
}

var cliBangs = struct {
	mu sync.Mutex
	n  int
	m  map[string][]cliBang
}{m: map[string][]cliBang{}}

// mergeBangs puts running ! commands first; one that Claude Code queued (it drops the "!") shows once, as the queued item.
func mergeBangs(bangs, items []CLIQueueItem) []CLIQueueItem {
	var lead []CLIQueueItem
	for _, b := range bangs {
		cmd := strings.TrimPrefix(b.Text, "!")
		dup := false
		for i := range items {
			if t := strings.TrimSpace(items[i].Text); items[i].Kind == "user" && (t == cmd || t == b.Text) {
				items[i].Text, dup = b.Text, true
				break
			}
		}
		if !dup {
			lead = append(lead, b)
		}
	}
	return append(lead, items...)
}

func noteBang(session, cmd string, queued bool) {
	cliBangs.mu.Lock()
	defer cliBangs.mu.Unlock()
	now := time.Now().UnixMilli()
	cliBangs.n++
	cliBangs.m[session] = append(cliBangs.m[session], cliBang{ID: fmt.Sprintf("bang-%d-%d", now, cliBangs.n), Cmd: strings.TrimSpace(cmd), At: now, Queued: queued})
}

// draftOf is the typed text in Claude Code's input box: the box without its prompt sign, placeholder or queue hint.
func draftOf(screen string) string {
	lines := strings.Split(strings.TrimSpace(cliInputBox(screen)), "\n")
	if len(lines) == 0 {
		return ""
	}
	first := strings.TrimSpace(lines[0])
	bash := strings.HasPrefix(first, "!")
	// Only the one prompt sign goes: in bash mode a typed "!" follows the "!" prompt.
	for _, p := range []string{"❯", "›", ">", "!"} {
		if r, ok := strings.CutPrefix(first, p); ok {
			first = r
			break
		}
	}
	lines[0] = strings.TrimSpace(first)
	for i := range lines {
		lines[i] = strings.TrimSpace(lines[i])
	}
	t := strings.TrimSpace(strings.Join(lines, "\n"))
	if t == "" || strings.HasPrefix(t, `Try "`) || strings.Contains(strings.ToLower(t), cliHint) {
		return ""
	}
	if bash {
		t = "!" + t
	}
	return t
}

// cliDraft reads the terminal's input box with dim text (placeholder, suggestions) left out.
func cliDraft(target string) string {
	s, err := cliIO.typedScreen(target)
	if err != nil || s == "" {
		return ""
	}
	return draftOf(s)
}

// clearDraft deletes the text in the terminal's input box with Backspace, then checks that it is gone.
func clearDraft(id string) error {
	ref := resolveWatched(id)
	target := cliIO.target(ref.ID)
	if target == "" {
		return orchConflict("agent-monitor cannot reach this terminal")
	}
	defer cliLock(ref.ID)()
	scr, err := cliIO.screen(target)
	if err != nil {
		return &orchHTTPError{http.StatusBadGateway, err.Error()}
	}
	if panePromptOpen(scr) {
		return orchConflict("a prompt is open in the terminal; answer it first")
	}
	d := cliDraft(target)
	if d == "" {
		if cliBashEmpty(target) { // leave an empty bash mode, so the box is a plain prompt again
			return cliIO.keys(target, []string{"BSpace"})
		}
		return nil
	}
	n := min(utf8.RuneCountInString(d)+4, cliEditMaxRunes)
	for n > 0 {
		k := min(n, 200)
		if err := cliIO.keys(target, slices.Repeat([]string{"BSpace"}, k)); err != nil {
			return err
		}
		n -= k
	}
	time.Sleep(cliPause)
	if _, err := cliIO.screen(target); err != nil || cliDraft(target) != "" {
		return orchConflict("the terminal box still holds text; check the terminal")
	}
	return nil
}

// bangLive returns the output under a running "! cmd" line on the screen, up to the input box; "" when not shown.
func bangLive(screen, cmd string) string {
	key := clipRunes(strings.Join(strings.Fields(cmd), " "), 40)
	if key == "" {
		return ""
	}
	lines := strings.Split(screen, "\n")
	at := -1
	for i, ln := range lines {
		t := strings.TrimSpace(ln)
		if strings.HasPrefix(t, "!") && strings.Contains(strings.Join(strings.Fields(t), " "), key) {
			at = i
		}
	}
	if at < 0 {
		return ""
	}
	var out []string
	for _, ln := range lines[at+1:] {
		t := strings.TrimSpace(ln)
		if isBoxRule(t) || strings.HasPrefix(t, "✻") || strings.HasPrefix(t, "❯") {
			break
		}
		if strings.Contains(t, "to run in background") {
			continue
		}
		t = strings.TrimSpace(strings.TrimPrefix(t, "⎿"))
		out = append(out, t)
	}
	for len(out) > 0 && out[len(out)-1] == "" {
		out = out[:len(out)-1]
	}
	if len(out) > 30 {
		out = out[len(out)-30:]
	}
	return strings.Join(out, "\n")
}

// forgetBang drops agent-monitor's record of a sent ! command (one whose result never reached the transcript).
func forgetBang(bangID string) bool {
	cliBangs.mu.Lock()
	defer cliBangs.mu.Unlock()
	for k, list := range cliBangs.m {
		for i, b := range list {
			if b.ID == bangID {
				cliBangs.m[k] = append(list[:i:i], list[i+1:]...)
				return true
			}
		}
	}
	return false
}

// pendingBangs lists sent ! commands whose result is not in the transcript yet; ones older than 30 min are dropped.
func pendingBangs(ref watchedRef, busy bool) []CLIQueueItem {
	cliBangs.mu.Lock()
	list := cliBangs.m[ref.ID]
	cliBangs.mu.Unlock()
	if len(list) == 0 || ref.Path == "" {
		return nil
	}
	done := transcriptCmds(ref.Path, ref.Tool)
	now := time.Now().UnixMilli()
	ended := map[string]bool{}
	var out []CLIQueueItem
	for _, b := range list {
		gone := now-b.At > 30*60_000
		for _, c := range done {
			if strings.TrimSpace(c.Cmd) == b.Cmd && c.At >= b.At-5_000 {
				gone = true
				break
			}
		}
		if gone {
			ended[b.ID] = true
			continue
		}
		st := "running"
		if b.Queued && busy {
			st = "waiting"
		}
		out = append(out, CLIQueueItem{ID: b.ID, Text: "!" + b.Cmd, At: b.At, Kind: "bash", Status: st})
	}
	// Drop only the ended ones from the current list: a noteBang during the read must survive.
	cliBangs.mu.Lock()
	var keep []cliBang
	for _, b := range cliBangs.m[ref.ID] {
		if !ended[b.ID] {
			keep = append(keep, b)
		}
	}
	if len(keep) == 0 {
		delete(cliBangs.m, ref.ID)
	} else {
		cliBangs.m[ref.ID] = keep
	}
	cliBangs.mu.Unlock()
	return out
}

// cliQueueState replays queue-operation records into the pending list.
type cliQueueState struct {
	items []CLIQueueItem
	// recent holds what dequeues took since the last assistant record, to check against the user records that deliver them.
	recent []CLIQueueItem
}

type cliQueueRec struct {
	Type      string  `json:"type"`
	Operation string  `json:"operation"`
	Timestamp string  `json:"timestamp"`
	Content   *string `json:"content"`
	IsMeta    bool    `json:"isMeta"`
	Message   *struct {
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

var reCLISystemTag = regexp.MustCompile(`^\s*<([a-z][a-z0-9_-]*)[\s>]`)

// cliQueueKind: notifications the CLI queues by itself are "system"; typed text and pastes are "user".
func cliQueueKind(text string) string {
	m := reCLISystemTag.FindStringSubmatch(text)
	if m == nil {
		return "user"
	}
	switch tag := m[1]; {
	case tag == "task-notification", tag == "agent-message", tag == "system-reminder", tag == "teammate-message",
		strings.HasSuffix(tag, "-notification"), strings.HasSuffix(tag, "-message"), strings.HasSuffix(tag, "-event"):
		return "system"
	}
	return "user"
}

func (q *cliQueueState) takeText(text string) bool {
	for i, it := range q.items {
		if it.Text == text {
			q.items = append(q.items[:i], q.items[i+1:]...)
			return true
		}
	}
	return false
}

// line applies one transcript line at byte offset off.
func (q *cliQueueState) line(off int64, b []byte) {
	isQ := bytes.Contains(b, []byte(`"queue-operation"`))
	if !isQ && len(q.recent) > 0 && bytes.Contains(b, []byte(`"type":"assistant"`)) {
		q.recent = nil
		return
	}
	if !isQ && (len(q.items) == 0 || !bytes.Contains(b, []byte(`"type":"user"`))) {
		return
	}
	var r cliQueueRec
	if json.Unmarshal(b, &r) != nil {
		return
	}
	if r.Type == "user" {
		q.delivered(&r)
		return
	}
	if r.Type != "queue-operation" {
		return
	}
	text := ""
	if r.Content != nil {
		text = *r.Content
	}
	switch r.Operation {
	case "enqueue":
		var at int64
		if t, err := time.Parse(time.RFC3339Nano, r.Timestamp); err == nil {
			at = t.UnixMilli()
		}
		q.items = append(q.items, CLIQueueItem{ID: "cq" + strconv.FormatInt(off, 10), Text: text, At: at, Kind: cliQueueKind(text)})
	case "dequeue":
		// The CLI takes the highest priority first; priority is not logged, so the oldest goes (FIFO).
		if len(q.items) > 0 {
			q.recent = append(q.recent, q.items[0])
			q.items = q.items[1:]
		}
	case "remove", "popAll":
		// remove: absorbed_mid_turn, delivered_to_agent, cleared_on_cancel and others; popAll: Up took it into the input.
		if r.Content != nil {
			q.takeText(text)
		}
	}
}

// delivered matches a user prompt record against the messages dequeues took.
func (q *cliQueueState) delivered(r *cliQueueRec) {
	if r.IsMeta || r.Message == nil {
		return
	}
	text, isResult := claudeUserText(r.Message.Content)
	if isResult || text == "" {
		return
	}
	// A queued ! command arrives as <bash-input>cmd</bash-input>; the queue holds cmd or !cmd.
	if inner, ok := strings.CutPrefix(text, "<bash-input>"); ok {
		if inner, ok = strings.CutSuffix(inner, "</bash-input>"); ok {
			if !q.takeText("!" + inner) {
				q.takeText(inner)
			}
			return
		}
	}
	for i, d := range q.recent {
		if d.Text == text {
			q.recent = append(q.recent[:i], q.recent[i+1:]...)
			return
		}
	}
	// FIFO took another message: this one was delivered, so the last one taken goes back to the front.
	if q.takeText(text) && len(q.recent) > 0 {
		back := q.recent[len(q.recent)-1]
		q.recent = q.recent[:len(q.recent)-1]
		q.items = append([]CLIQueueItem{back}, q.items...)
	}
}

// parseCLIQueue replays a whole transcript; tests and one-shot readers use it.
func parseCLIQueue(r io.Reader) []CLIQueueItem {
	q := &cliQueueState{}
	br := bufio.NewReaderSize(r, 64<<10)
	var off int64
	for {
		b, err := br.ReadBytes('\n')
		if len(b) > 0 && (err == nil || len(bytes.TrimSpace(b)) > 0) {
			q.line(off, b)
			off += int64(len(b))
		}
		if err != nil {
			break
		}
	}
	return q.items
}

// ── incremental cache per transcript ──

type cliQueueFile struct {
	fi  os.FileInfo
	off int64
	q   cliQueueState
	use time.Time
}

var cliQueueCache = struct {
	sync.Mutex
	m map[string]*cliQueueFile
}{m: map[string]*cliQueueFile{}}

const cliQueueCacheMax = 64

// cliQueueOf returns the pending CLI queue of a Claude transcript, reading only bytes added since the last call.
func cliQueueOf(path string) ([]CLIQueueItem, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	cliQueueCache.Lock()
	defer cliQueueCache.Unlock()
	c := cliQueueCache.m[path]
	if c == nil || !os.SameFile(c.fi, st) || st.Size() < c.off {
		c = &cliQueueFile{}
		cliQueueCache.m[path] = c
	}
	c.fi, c.use = st, time.Now()
	if st.Size() > c.off {
		end, err := forEachLine(f, c.off, st.Size(), func(off int64, line []byte) {
			if len(line) > 0 {
				c.q.line(off, line)
			}
		})
		if err != nil {
			return nil, err
		}
		c.off = end
	}
	if len(cliQueueCache.m) > cliQueueCacheMax {
		var oldK string
		var old time.Time
		for k, v := range cliQueueCache.m {
			if v != c && (oldK == "" || v.use.Before(old)) {
				oldK, old = k, v.use
			}
		}
		delete(cliQueueCache.m, oldK)
	}
	return append([]CLIQueueItem(nil), c.q.items...), nil
}

func cliUserItems(items []CLIQueueItem) []CLIQueueItem {
	out := []CLIQueueItem{}
	for _, it := range items {
		if it.Kind == "user" {
			out = append(out, it)
		}
	}
	return out
}

// cliQueuedCount is the number of user messages queued in a Claude transcript (0 on any error).
func cliQueuedCount(path string) int {
	items, err := cliQueueOf(path)
	if err != nil {
		return 0
	}
	return len(cliUserItems(items))
}

// ── view and actions ──

// CLIQueueView is the GET /api/session/{id}/cli-queue answer.
type CLIQueueView struct {
	Agent     Tool           `json:"agent"`
	Supported bool           `json:"supported"`
	Items     []CLIQueueItem `json:"items"`
	// Hidden counts system notifications left out (ask with ?all=1).
	Hidden    int    `json:"hidden,omitempty"`
	Reachable bool   `json:"reachable"`
	Note      string `json:"note,omitempty"`
	CanRemove bool   `json:"canRemove"`
	RemoveWhy string `json:"removeWhy,omitempty"`
	// Draft is text typed in the terminal's input box and not sent yet (not the placeholder or hint).
	Draft string `json:"draft,omitempty"`
}

// cliTerm reads and types into a session's terminal, host first; tests swap it.
type cliTerm interface {
	target(session string) string
	screen(target string) (string, error)
	// typedScreen reads the screen without dim text, which is Claude Code's placeholder and suggested prompt.
	typedScreen(target string) (string, error)
	keys(target string, keys []string) error
	text(target, text string, enter bool) error
	session(id string) *Session
}

type realCLITerm struct{}

func (realCLITerm) target(session string) string {
	p := watchedPane(session)
	if !p.Sendable {
		return ""
	}
	return p.Target()
}
func (realCLITerm) screen(t string) (string, error) { return targetCapture(t, 40, true) }
func (realCLITerm) typedScreen(t string) (string, error) {
	// A host screen has no colours; its tmux pane, when it has one, shows which text is dim.
	if h, isHost, _ := targetHost(t); isHost && h != nil && h.TmuxPane != "" {
		if s, err := tmuxCaptureFn(h.TmuxPane, 40, false); err == nil {
			return dropDim(s), nil
		}
	}
	s, err := targetCapture(t, 40, false)
	return dropDim(s), err
}
func (realCLITerm) keys(t string, keys []string) error    { return targetKeys(t, keys) }
func (realCLITerm) text(t, text string, enter bool) error { return targetText(t, text, enter) }
func (realCLITerm) session(id string) *Session {
	if store == nil {
		return nil
	}
	if s := store.Get(id); s != nil {
		return s
	}
	return store.Get(resolveWatched(id).ID)
}

var (
	cliIO        cliTerm  = realCLITerm{}
	cliPause              = 400 * time.Millisecond
	cliBangPause          = 150 * time.Millisecond
	cliLocks     sync.Map // session id -> *sync.Mutex
)

const (
	cliEditMaxRunes = 4000
	cliHint         = "press up to edit queued messages"
)

var errCLINotSupported = errors.New("only Claude Code records its queue in the transcript")

const cliRemoveWhy = "Claude Code gives queued messages back only all at once. Edit takes them all into the message box; send back the ones you want."

// cliQueueView builds the view; all keeps system notifications.
func cliQueueView(id string, all bool) (*CLIQueueView, string, error) {
	ref := resolveWatched(id)
	v := &CLIQueueView{Agent: ref.Tool, Items: []CLIQueueItem{}}
	sessBusy := func() bool {
		st := cliIO.session(ref.ID)
		return st != nil && st.State == StateRunning
	}
	if ref.Tool == ToolCodex {
		v.Note = "Codex does not record its queued messages, so they show only in its terminal."
		v.Reachable = cliIO.target(ref.ID) != ""
		v.Items = append(v.Items, pendingBangs(ref, sessBusy())...)
		return v, "", nil
	}
	if ref.Tool != ToolClaude || !ref.Resolved || ref.Path == "" {
		v.Reachable = cliIO.target(firstOf(ref.ID, id)) != "" // Cursor and others take sends with no queue view
		return v, "", errCLINotSupported
	}
	v.Supported = true
	items, err := cliQueueOf(ref.Path)
	if err != nil {
		return v, "", err
	}
	target := cliIO.target(ref.ID)
	v.Reachable = target != ""
	screen := ""
	if len(items) > 0 {
		// Claude Code never keeps a queue while it waits at its prompt: what is left is from an earlier process.
		s := cliIO.session(ref.ID)
		idle := s != nil && (s.State == StateCompleted || s.State == StateAbandoned || s.State == StateIdle || s.State == StateAwaitingInput)
		if target != "" {
			screen, _ = cliIO.screen(target)
			if screen != "" {
				noHint := !strings.Contains(strings.ToLower(screen), cliHint)
				idle = (watchedPhaseOf(s, true, screen) == "ready" && noHint) || (noHint && cliBoxEmpty(screen))
			}
		}
		if idle {
			items = nil
		}
	}
	fillQueuedText(items, ref.ID, target, screen)
	for _, it := range items {
		if it.Kind == "user" || all {
			v.Items = append(v.Items, it)
		} else {
			v.Hidden++
		}
	}
	v.Items = mergeBangs(pendingBangs(ref, sessBusy()), v.Items)
	if v.Items == nil {
		v.Items = []CLIQueueItem{} // clients read items as a list
	}
	for i := range v.Items {
		if it := &v.Items[i]; it.Kind == "bash" && it.Status == "running" && target != "" {
			if screen == "" {
				screen, _ = cliIO.screen(target)
			}
			it.Live = bangLive(screen, strings.TrimPrefix(it.Text, "!"))
		}
	}
	if target != "" {
		v.Draft = cliDraft(target)
	}
	users := len(cliUserItems(items))
	switch {
	case !v.Reachable:
		v.RemoveWhy = "agent-monitor cannot reach this terminal."
	case users > 1:
		v.RemoveWhy = cliRemoveWhy
	default:
		v.CanRemove = users == 1
	}
	return v, target, nil
}

func cliLock(id string) func() {
	m, _ := cliLocks.LoadOrStore(id, &sync.Mutex{})
	mu := m.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// isBoxRule is a horizontal rule of Claude Code's input box; a named session prints its name inside the top one.
func isBoxRule(t string) bool {
	if !strings.HasPrefix(t, "─") && !strings.HasPrefix(t, "━") {
		return false
	}
	return strings.Count(t, "─")+strings.Count(t, "━") >= 20
}

// cliInputBox returns the lines between the last two horizontal rules: Claude Code's input box.
func cliInputBox(screen string) string {
	lines := strings.Split(strings.TrimRight(screen, "\n"), "\n")
	var rules []int
	for i, ln := range lines {
		t := strings.TrimSpace(ln)
		if isBoxRule(t) {
			rules = append(rules, i)
		}
	}
	// Panels below the box (subagents, todos) can draw rules too: use the pair that holds the ❯ prompt.
	for i := len(rules) - 2; i >= 0; i-- {
		a, b := rules[i], rules[i+1]
		if l := strings.TrimSpace(lines[a+1]); b > a+1 && (strings.HasPrefix(l, "❯") || strings.HasPrefix(l, "!")) {
			return strings.Join(lines[a+1:b], "\n")
		}
	}
	if len(rules) < 2 {
		return ""
	}
	a, b := rules[len(rules)-2], rules[len(rules)-1]
	return strings.Join(lines[a+1:b], "\n")
}

func firstLineKey(s string) string {
	ln, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return clipString(strings.TrimSpace(ln), 24)
}

// cliReady checks the screen before keys: Claude works and shows its queue hint (its input box is empty).
func cliReady(target string) (string, error) {
	screen, err := cliIO.screen(target)
	if err != nil {
		return "", &orchHTTPError{http.StatusBadGateway, err.Error()}
	}
	if panePromptOpen(screen) {
		return "", orchConflict("a question or permission prompt is open in the terminal; answer it first")
	}
	// A message sent a moment ago can hold the box for a few seconds before the hint returns.
	for end := time.Now().Add(cliBoxSettle); !strings.Contains(strings.ToLower(cliInputBox(screen)), cliHint); {
		if time.Now().After(end) {
			return "", orchConflict("the terminal does not show queued messages with an empty input box now; open the pane")
		}
		time.Sleep(cliBangPause)
		if screen, err = cliIO.screen(target); err != nil {
			return "", &orchHTTPError{http.StatusBadGateway, err.Error()}
		}
		if panePromptOpen(screen) {
			return "", orchConflict("a question or permission prompt is open in the terminal; answer it first")
		}
	}
	return screen, nil
}

type cliActionResult struct {
	Text  string         `json:"text,omitempty"`
	Items []CLIQueueItem `json:"items"`
}

// cliAction runs send-now, edit or remove through the CLI's own keys.
func cliAction(id, action, itemID string) (*cliActionResult, error) {
	v, target, err := cliQueueView(id, false)
	if err != nil {
		return nil, orchBadRequest(err.Error())
	}
	if !v.Supported {
		return nil, orchBadRequest(v.Note)
	}
	if target == "" {
		return nil, orchConflict("agent-monitor cannot reach this terminal")
	}
	// Running ! commands are listed but are not in Claude Code's queue.
	users := cliUserItems(v.Items)
	if len(users) == 0 {
		return nil, orchConflict("nothing is queued in Claude Code")
	}
	if itemID != "" {
		found := false
		for _, it := range users {
			found = found || it.ID == itemID
		}
		if !found {
			return nil, &orchHTTPError{http.StatusNotFound, "that message is no longer queued"}
		}
	}
	lockID := id
	if ref := resolveWatched(id); ref.Resolved {
		lockID = ref.ID // the same lock as cliSend, also for an alias id
	}
	defer cliLock(lockID)()
	if _, err := cliReady(target); err != nil {
		return nil, err
	}
	res := &cliActionResult{}
	switch action {
	case "send-now":
		// Esc interrupts the turn; Claude Code then sends every queued message as the next turn.
		if err := cliIO.keys(target, []string{"Escape"}); err != nil {
			return nil, err
		}
	case "edit", "remove":
		if action == "remove" && !v.CanRemove {
			return nil, orchConflict(v.RemoveWhy)
		}
		for _, it := range users {
			if strings.HasPrefix(it.Text, "!") { // Claude Code holds it without the "!", so Up would not match
				return nil, orchConflict("a ! command waits in the queue; edit or remove it in the terminal")
			}
		}
		texts := make([]string, len(users))
		for i, it := range users {
			if strings.TrimSpace(it.Text) == "" {
				return nil, orchConflict("a queued message has no text (an image); edit it in the terminal")
			}
			texts[i] = it.Text
		}
		joined := strings.Join(texts, "\n")
		n := utf8.RuneCountInString(joined)
		if n > cliEditMaxRunes {
			return nil, orchConflict("the queued text is too long to take back by keys; edit it in the terminal")
		}
		// Up moves every queued message into the input box with the cursor at the end.
		if err := cliIO.keys(target, []string{"Up"}); err != nil {
			return nil, err
		}
		time.Sleep(cliPause)
		scr, err := cliIO.screen(target)
		box := strings.ToLower(cliInputBox(scr))
		if err != nil || !strings.Contains(box, strings.ToLower(firstLineKey(texts[0]))) {
			return nil, &orchHTTPError{http.StatusConflict, "Up did not bring the queued messages into the input box; open the pane"}
		}
		bs := make([]string, n+8)
		for i := range bs {
			bs[i] = "BSpace"
		}
		if err := cliIO.keys(target, bs); err != nil {
			return nil, err
		}
		time.Sleep(cliPause)
		scr, err = cliIO.screen(target)
		box = strings.ToLower(cliInputBox(scr))
		for _, t := range texts {
			if k := strings.ToLower(firstLineKey(t)); err != nil || (k != "" && strings.Contains(box, k)) {
				return nil, &orchHTTPError{http.StatusInternalServerError, "some text may still be in the terminal's input box; open the pane to check"}
			}
		}
		if action == "edit" {
			res.Text = joined
		}
	default:
		return nil, &orchHTTPError{http.StatusNotFound, "unknown action"}
	}
	if nv, _, err := cliQueueView(id, false); err == nil {
		res.Items = nv.Items
	}
	return res, nil
}

// cliSend types a message into the CLI as if typed: a busy CLI queues it itself.
func cliSend(id, text string, attachments ...string) (bool, error) {
	if strings.TrimSpace(text) == "" && len(attachments) == 0 {
		return false, orchBadRequest("text is required")
	}
	if len(text) > queueMaxText {
		return false, orchBadRequest("text is longer than 64 KB")
	}
	ref := resolveWatched(id)
	sessID := id
	if ref.Resolved {
		sessID = ref.ID
	}
	tool := ref.Tool
	if s := cliIO.session(sessID); tool == "" && s != nil {
		tool = s.Tool
	}
	bang := strings.HasPrefix(text, "!") && (tool == ToolClaude || tool == ToolCodex)
	if bang && strings.TrimSpace(text[1:]) == "" {
		return false, orchBadRequest("a ! command needs a command after the !")
	}
	if bang && len(attachments) > 0 {
		return false, orchBadRequest("a ! command cannot carry attachments")
	}
	if len(attachments) > 0 {
		if orchMgr == nil {
			return false, orchBadRequest("this daemon keeps no uploads")
		}
		ups, err := orchMgr.resolveUploads(attachments)
		if err != nil {
			return false, err
		}
		// Claude Code reads an @path as an attached file; Codex gets the bare path.
		text = withFilePaths(text, ups)
	}
	target := cliIO.target(sessID)
	if target == "" {
		return false, orchConflict("agent-monitor cannot reach this terminal")
	}
	defer cliLock(sessID)()
	screen, err := cliIO.screen(target)
	if err != nil {
		return false, &orchHTTPError{http.StatusBadGateway, err.Error()}
	}
	ph := watchedPhaseOf(cliIO.session(sessID), true, screen)
	if ph == "prompt" {
		return false, orchConflict("a question or permission prompt is open in the terminal")
	}
	text = strings.TrimRight(text, " \t\r\n")
	// An empty bash prompt ("!" alone) is a mode, not text: a ! command uses it, a message leaves it first.
	inBash := false
	if tool == ToolClaude && cliBashEmpty(target) {
		if bang {
			inBash = true
		} else {
			if err := cliIO.keys(target, []string{"BSpace"}); err != nil {
				return false, err
			}
			time.Sleep(cliBangPause)
			if screen, err = cliIO.screen(target); err != nil {
				return false, &orchHTTPError{http.StatusBadGateway, err.Error()}
			}
		}
	}
	// Text typed into the box by hand would join this message (or turn a ! command into prompt text).
	if !inBash && (bang || tool == ToolClaude && cliSeesDim(target)) && cliBoxHasText(screen, target) {
		// A message sent a moment ago sits in the box until Claude Code moves it to its queue.
		for end := time.Now().Add(cliBoxSettle); ; {
			time.Sleep(cliBangPause)
			if screen, err = cliIO.screen(target); err != nil || !cliBoxHasText(screen, target) {
				break
			}
			if time.Now().After(end) {
				return false, orchConflict("the terminal input box has text in it; send it or clear it there first")
			}
		}
		if err != nil {
			return false, &orchHTTPError{http.StatusBadGateway, err.Error()}
		}
	}
	if bang && tool == ToolClaude {
		if !inBash {
			if err := cliEnterBash(target); err != nil {
				return false, err
			}
		}
		if err := cliIO.text(target, strings.TrimLeft(text[1:], " "), true); err != nil {
			return false, err
		}
	} else if tool == ToolCursor || tool == ToolCursorAgent {
		// Cursor's box takes text and an Enter that arrive together as one paste: pause before Enter.
		if err := cliIO.text(target, text, false); err != nil {
			return false, err
		}
		time.Sleep(cliBangPause * 2)
		if err := cliIO.keys(target, []string{"Enter"}); err != nil {
			return false, err
		}
	} else if err := typeBang(func(t string, enter bool) error { return cliIO.text(target, t, enter) }, text, bang); err != nil {
		return false, err
	}
	if bang {
		noteBang(sessID, strings.TrimLeft(text[1:], " "), ph == "busy")
	}
	if !bang && ph == "busy" {
		noteSentBusy(sessID, text)
	}
	if queueMgr != nil && ph == "busy" {
		queueMgr.NoteBusy(sessID)
	}
	return ph == "busy", nil
}

// cliEnterBash types "!" and checks that Claude Code shows exactly its "!" prompt; a lost key is typed once more.
// Any other result is erased, so nothing is left in the box, and the command is not sent.
func cliEnterBash(target string) error {
	for try := 0; try < 2; try++ {
		if err := cliIO.text(target, "!", false); err != nil {
			return err
		}
		for _, wait := range []time.Duration{cliBangPause, 2 * cliBangPause} {
			time.Sleep(wait)
			scr, err := cliIO.screen(target)
			if err != nil {
				return err
			}
			box := strings.TrimSpace(cliInputBox(scr))
			if strings.HasPrefix(box, "!") && box != "!" {
				// Claude Code shows its dim hint after the ! when messages are queued.
				if t, err := cliIO.typedScreen(target); err == nil {
					box = strings.TrimSpace(cliInputBox(t))
				}
			}
			if box == "!" || strings.EqualFold(strings.TrimSpace(box[min(1, len(box)):]), cliHint) && strings.HasPrefix(box, "!") {
				return nil
			}
			if strings.HasPrefix(box, "!") { // a second "!" landed as text in bash mode
				n := utf8.RuneCountInString(strings.TrimSpace(box[1:])) + 1
				_ = cliIO.keys(target, slices.Repeat([]string{"BSpace"}, n))
				return orchConflict("Claude Code got the ! twice; the command was not sent, try again")
			}
			if strings.HasPrefix(box, "❯") && strings.Contains(box, "!") {
				_ = cliIO.keys(target, []string{"BSpace"})
				return orchConflict("Claude Code took the ! as text, not bash mode; the command was not sent")
			}
		}
	}
	// Erase each "!" typed above, so nothing is left in the box.
	_ = cliIO.keys(target, []string{"BSpace", "BSpace"})
	return orchConflict("Claude Code did not switch to bash mode; the command was not sent")
}

// Claude Code 2.1.327 and later write enqueue records with no text: take it from the screen, then from what agent-monitor typed.
var cliSentBusy = struct {
	mu sync.Mutex
	m  map[string][]string
}{m: map[string][]string{}}

func noteSentBusy(session, text string) {
	cliSentBusy.mu.Lock()
	defer cliSentBusy.mu.Unlock()
	l := append(cliSentBusy.m[session], text)
	if len(l) > 20 {
		l = l[len(l)-20:]
	}
	cliSentBusy.m[session] = l
}

func fillQueuedText(items []CLIQueueItem, session, target, screen string) {
	missing := 0
	for _, it := range items {
		if it.Kind == "user" && strings.TrimSpace(it.Text) == "" {
			missing++
		}
	}
	if missing == 0 {
		return
	}
	if screen == "" && target != "" {
		screen, _ = cliIO.screen(target)
	}
	texts := screenQueued(screen)
	if len(texts) < missing {
		cliSentBusy.mu.Lock()
		sent := cliSentBusy.m[session]
		cliSentBusy.mu.Unlock()
		if len(sent) >= missing {
			texts = sent[len(sent)-missing:]
		}
	}
	k := max(len(texts)-missing, 0)
	for i := range items {
		if items[i].Kind == "user" && strings.TrimSpace(items[i].Text) == "" && k < len(texts) {
			items[i].Text = texts[k]
			k++
		}
	}
}

// screenQueued reads the messages Claude Code shows above its box as "❯ text" ... "⎿  Queued".
func screenQueued(screen string) []string {
	lines := strings.Split(screen, "\n")
	var out []string
	for i, ln := range lines {
		if !strings.Contains(ln, "⎿") || !strings.Contains(ln, "Queued") {
			continue
		}
		j := i - 1
		for j >= 0 && !strings.HasPrefix(strings.TrimSpace(lines[j]), "❯") {
			j--
		}
		if j < 0 {
			continue
		}
		var b []string
		for _, l := range lines[j:i] {
			b = append(b, strings.TrimSpace(l))
		}
		out = append(out, strings.TrimSpace(strings.TrimPrefix(strings.Join(b, "\n"), "❯")))
	}
	return out
}

// cliTypedEmpty is true when the box holds only dim text (a placeholder or a suggested prompt).
// cliBashEmpty: Claude Code is in bash mode with nothing typed after its "!" prompt.
func cliBashEmpty(target string) bool {
	scr, err := cliIO.typedScreen(target)
	return err == nil && strings.TrimSpace(cliInputBox(scr)) == "!"
}

func cliTypedEmpty(target string) bool {
	scr, err := cliIO.typedScreen(target)
	return err == nil && cliBoxEmpty(scr)
}

// dropDim removes text drawn dim (SGR 2) and then every escape sequence.
func dropDim(s string) string {
	var b strings.Builder
	dim := false
	for i := 0; i < len(s); i++ {
		if s[i] != 0x1b {
			if !dim {
				b.WriteByte(s[i])
			}
			continue
		}
		if i+1 < len(s) && s[i+1] == '[' {
			j := i + 2
			for j < len(s) && (s[j] < 0x40 || s[j] > 0x7e) {
				j++
			}
			if j < len(s) && s[j] == 'm' {
				ps := strings.Split(s[i+2:j], ";")
				for k := 0; k < len(ps); k++ {
					switch ps[k] {
					case "38", "48", "58": // colour: skip "5;n" or "2;r;g;b"
						if k+1 < len(ps) && ps[k+1] == "5" {
							k += 2
						} else if k+1 < len(ps) && ps[k+1] == "2" {
							k += 4
						}
					case "2":
						dim = true
					case "", "0", "22":
						dim = false
					}
				}
			}
			i = j
			continue
		}
		i++ // a two-byte escape
	}
	return b.String()
}

// typeBang types "!" alone first, since only "!" in an empty box switches Claude Code to bash mode.
func typeBang(send func(string, bool) error, text string, bang bool) error {
	if bang {
		if err := send("!", false); err != nil {
			return err
		}
		time.Sleep(cliBangPause)
		text = strings.TrimLeft(text[1:], " ")
	}
	return send(text, true)
}

// serveCLIQueue handles /api/session/{id}/cli-queue[/send|/send-now|/edit|/remove|/forget|/clear-draft]; false for other paths.
func serveCLIQueue(w http.ResponseWriter, r *http.Request, path string) bool {
	id, action, ok := strings.Cut(path, "/cli-queue")
	if !ok || id == "" || (action != "" && !strings.HasPrefix(action, "/")) {
		return false
	}
	action = strings.TrimPrefix(action, "/")
	if action == "" {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return true
		}
		v, _, err := cliQueueView(id, r.URL.Query().Get("all") == "1")
		if err != nil && !errors.Is(err, errCLINotSupported) {
			writeJSONStatus(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return true
		}
		writeJSON(w, v)
		return true
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return true
	}
	var body struct {
		Text        string   `json:"text"`
		ID          string   `json:"id"`
		Attachments []string `json:"attachments"`
	}
	if r.ContentLength != 0 {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
			writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": "bad json"})
			return true
		}
	}
	if action == "send" {
		queued, err := cliSend(id, body.Text, body.Attachments...)
		if err != nil {
			orchWriteErr(w, err)
			return true
		}
		writeJSON(w, map[string]any{"ok": true, "queued": queued})
		return true
	}
	if action == "clear-draft" {
		if err := clearDraft(id); err != nil {
			orchWriteErr(w, err)
			return true
		}
		writeJSON(w, map[string]any{"ok": true})
		return true
	}
	if action == "forget" { // only agent-monitor's record; nothing is typed into the terminal
		if !strings.HasPrefix(body.ID, "bang-") || !forgetBang(body.ID) {
			writeJSONStatus(w, http.StatusNotFound, map[string]any{"error": "no such sent command"})
			return true
		}
		writeJSON(w, map[string]any{"ok": true})
		return true
	}
	res, err := cliAction(id, action, body.ID)
	if err != nil {
		orchWriteErr(w, err)
		return true
	}
	writeJSON(w, map[string]any{"ok": true, "text": res.Text, "items": res.Items})
	return true
}

// cliSeesDim is false for a host with no tmux pane: its plain screen cannot tell a dim placeholder from typed text,
// so a normal message there skips the box check rather than being refused for a suggestion.
var cliSeesDim = func(target string) bool {
	h, isHost, _ := targetHost(target)
	return !isHost || (h != nil && h.TmuxPane != "")
}

// cliBoxSettle is how long a just-sent message may sit in the box before a ! command is refused.
var cliBoxSettle = 4 * time.Second

// cliBoxHasText is true when the box holds typed text: not empty, not the queue hint, not a dim placeholder.
func cliBoxHasText(screen, target string) bool {
	box := strings.TrimSpace(cliInputBox(screen))
	return box != "" && !cliBoxEmpty(screen) && !strings.Contains(strings.ToLower(box), cliHint) && !cliTypedEmpty(target)
}

// cliBoxEmpty reports an input box with no typed text; Claude Code then shows its queue hint whenever it holds messages.
func cliBoxEmpty(screen string) bool {
	box := strings.TrimSpace(cliInputBox(screen))
	return box != "" && strings.TrimSpace(strings.TrimLeft(box, "❯>")) == ""
}
