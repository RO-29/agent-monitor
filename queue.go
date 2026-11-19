package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

// QItem is one message that waits until its thread is idle.
type QItem struct {
	ID          string   `json:"id"`
	ThreadID    string   `json:"threadId"`
	Text        string   `json:"text"`
	Attachments []string `json:"attachments,omitempty"`
	// Session is the watched session the message goes to (empty for driven threads).
	Session   string `json:"session,omitempty"`
	CreatedAt int64  `json:"createdAt"`
	State     string `json:"state"` // queued | sending | sent | failed
	Error     string `json:"error,omitempty"`
	SentAt    int64  `json:"sentAt,omitempty"`
	Mode      string `json:"mode,omitempty"` // turn | steer | pane, once sent
}

// QueueView is the GET /api/queue/{threadId} answer.
type QueueView struct {
	Items []QItem `json:"items"`
	// Paused says why automatic dispatch waits: "terminal" when a terminal owns the session.
	Paused string `json:"paused,omitempty"`
	// Terminal lists messages queued inside the agent's own TUI (read from the pane, best effort).
	Terminal []string `json:"terminal,omitempty"`
	// CLI is the agent CLI's own queue, from its transcript (Claude Code).
	CLI *CLIQueueView `json:"cli,omitempty"`
}

const (
	queueMaxItems = 50
	queueMaxText  = 64 << 10
	queueTick     = 3 * time.Second
)

// queueDriven is the orchestrator side of the queue; tests swap in a fake.
type queueDriven interface {
	exists(id string) bool
	status(id string) string
	terminal(id string) bool
	send(id, text string, atts []string) (string, error)
}

// queueWatched reads and types into a watched session's tmux pane; tests swap in a fake.
type queueWatched interface {
	phase(session string) (phase string, pane string)
	send(session, text string) error
}

// QueueManager owns the per-thread queues and dispatches them when a thread goes idle.
type QueueManager struct {
	store   *OrchStore
	driven  queueDriven
	watched queueWatched

	mu sync.Mutex
	// inflight: a dispatch for the thread runs now. attempt: the last auto dispatch (ms).
	inflight map[string]bool
	attempt  map[string]int64
	// busySeen: the last time a watched session looked busy (ms).
	busySeen map[string]int64
	// readySince: since when a watched session has read ready with no other reading in between (ms).
	readySince map[string]int64
	now        func() int64
	kicks      sync.WaitGroup // background kicks, so tests can wait for them
}

func newQueueManager(s *OrchStore, d queueDriven, w queueWatched) *QueueManager {
	q := &QueueManager{store: s, driven: d, watched: w, inflight: map[string]bool{}, attempt: map[string]int64{}, busySeen: map[string]int64{}, readySince: map[string]int64{},
		now: func() int64 { return time.Now().UnixMilli() }}
	// A send cut off by a restart may or may not have landed, so it is not repeated by itself.
	_ = s.write(func(t *orchTx) error {
		items, err := scanJSONRows[QItem](t.tx.Query(`SELECT json FROM queue WHERE json_extract(json,'$.state')='sending'`))
		if err != nil {
			return err
		}
		for i := range items {
			items[i].State, items[i].Error = "failed", "agent-monitor restarted while this message was sent. Check the thread, then send it again or remove it."
			if err := queuePutTx(t, &items[i], -1); err != nil {
				return err
			}
		}
		return nil
	})
	return q
}

// ── storage ──

func queueListTx(tx interface {
	Query(string, ...any) (*sql.Rows, error)
}, threadID string) ([]QItem, error) {
	return scanJSONRows[QItem](tx.Query(`SELECT json FROM queue WHERE thread_id=? ORDER BY ord, id`, threadID))
}

// queuePutTx writes an item; ord < 0 keeps the stored position (or appends a new item).
func queuePutTx(t *orchTx, it *QItem, ord float64) error {
	b, _ := json.Marshal(it)
	if ord < 0 {
		_, err := t.tx.Exec(`INSERT INTO queue(id, thread_id, ord, json) VALUES(?,?,(SELECT COALESCE(MAX(ord),0)+1 FROM queue WHERE thread_id=?),?)
			ON CONFLICT(id) DO UPDATE SET json=excluded.json`, it.ID, it.ThreadID, it.ThreadID, string(b))
		return err
	}
	_, err := t.tx.Exec(`INSERT INTO queue(id, thread_id, ord, json) VALUES(?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET json=excluded.json, ord=excluded.ord`, it.ID, it.ThreadID, ord, string(b))
	return err
}

func queueItemTx(t *orchTx, threadID, id string) (*QItem, error) {
	return getJSON[QItem](t.tx, `SELECT json FROM queue WHERE id=? AND thread_id=?`, id, threadID)
}

// emitQueue publishes the thread's queue after a change; sent carries a message that just left it.
func emitQueue(t *orchTx, threadID string, sent *QItem) error {
	items, err := queueListTx(t.tx, threadID)
	if err != nil {
		return err
	}
	data := map[string]any{"threadId": threadID, "items": items}
	if sent != nil {
		data["sent"] = sent
	}
	t.emit(threadID, "queue.updated", data)
	return nil
}

// List returns the thread's queue, oldest first.
func (q *QueueManager) List(threadID string) ([]QItem, error) {
	var out []QItem
	err := q.store.read(func(tx *sql.Tx) error {
		var err error
		out, err = queueListTx(tx, threadID)
		return err
	})
	return out, err
}

// threadsWithQueued lists the threads that hold at least one queued message.
func (q *QueueManager) threadsWithQueued() []string {
	var ids []string
	_ = q.store.read(func(tx *sql.Tx) error {
		rows, err := tx.Query(`SELECT DISTINCT thread_id FROM queue WHERE json_extract(json,'$.state')='queued'`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			if rows.Scan(&id) == nil {
				ids = append(ids, id)
			}
		}
		return rows.Err()
	})
	return ids
}

func isDrivenID(id string) bool { return strings.HasPrefix(id, "ot_") }

type queueAdd struct {
	Text        string   `json:"text"`
	Attachments []string `json:"attachments"`
	Session     string   `json:"session"`
}

// Add appends a message; a thread that is idle now gets it at once.
func (q *QueueManager) Add(threadID string, in queueAdd) (*QItem, error) {
	in.Text = strings.TrimRight(in.Text, " \t\r\n")
	if strings.TrimSpace(in.Text) == "" && len(in.Attachments) == 0 {
		return nil, orchBadRequest("text is required")
	}
	if len(in.Text) > queueMaxText {
		return nil, orchBadRequest("text is longer than 64 KB")
	}
	if len(in.Attachments) > 10 {
		return nil, orchBadRequest("at most 10 attachments")
	}
	it := &QItem{ID: orchID("q_"), ThreadID: threadID, Text: in.Text, CreatedAt: q.now(), State: "queued"}
	if isDrivenID(threadID) {
		if !q.driven.exists(threadID) {
			return nil, errOrchNotFound
		}
		it.Attachments = in.Attachments
	} else {
		if len(in.Attachments) > 0 {
			return nil, orchBadRequest("a terminal session takes text only")
		}
		if strings.TrimSpace(in.Text) == "" {
			return nil, orchBadRequest("text is required")
		}
		it.Session = in.Session
		if it.Session == "" {
			it.Session = threadID
		}
	}
	err := q.store.write(func(t *orchTx) error {
		var n int
		if err := t.tx.QueryRow(`SELECT COUNT(*) FROM queue WHERE thread_id=?`, threadID).Scan(&n); err != nil {
			return err
		}
		if n >= queueMaxItems {
			return orchConflict("the queue holds 50 messages already")
		}
		if err := queuePutTx(t, it, -1); err != nil {
			return err
		}
		return emitQueue(t, threadID, nil)
	})
	if err != nil {
		return nil, err
	}
	q.kickAsync(threadID)
	return it, nil
}

// Remove deletes one message (any state but sending).
func (q *QueueManager) Remove(threadID, id string) error {
	return q.store.write(func(t *orchTx) error {
		it, err := queueItemTx(t, threadID, id)
		if err != nil {
			return queueNotFound(err)
		}
		if it.State == "sending" {
			return orchConflict("this message is being sent")
		}
		if _, err := t.tx.Exec(`DELETE FROM queue WHERE id=?`, id); err != nil {
			return err
		}
		return emitQueue(t, threadID, nil)
	})
}

// Clear deletes every message that is not being sent.
func (q *QueueManager) Clear(threadID string) error {
	return q.store.write(func(t *orchTx) error {
		if _, err := t.tx.Exec(`DELETE FROM queue WHERE thread_id=? AND json_extract(json,'$.state')!='sending'`, threadID); err != nil {
			return err
		}
		return emitQueue(t, threadID, nil)
	})
}

type queuePatch struct {
	Text  *string `json:"text"`
	Index *int    `json:"index"`
}

// Patch edits the text and/or moves the message to index (0 = first). An edited failed message is queued again.
func (q *QueueManager) Patch(threadID, id string, p queuePatch) error {
	if p.Text != nil {
		if strings.TrimSpace(*p.Text) == "" {
			return orchBadRequest("text is required")
		}
		if len(*p.Text) > queueMaxText {
			return orchBadRequest("text is longer than 64 KB")
		}
	}
	err := q.store.write(func(t *orchTx) error {
		it, err := queueItemTx(t, threadID, id)
		if err != nil {
			return queueNotFound(err)
		}
		if it.State == "sending" {
			return orchConflict("this message is being sent")
		}
		if p.Text != nil {
			it.Text = *p.Text
			if it.State == "failed" {
				it.State, it.Error = "queued", ""
			}
			if err := queuePutTx(t, it, -1); err != nil {
				return err
			}
		}
		if p.Index != nil {
			items, err := queueListTx(t.tx, threadID)
			if err != nil {
				return err
			}
			order := make([]QItem, 0, len(items))
			for _, x := range items {
				if x.ID != id {
					order = append(order, x)
				}
			}
			i := min(max(*p.Index, 0), len(order))
			order = append(order[:i], append([]QItem{*it}, order[i:]...)...)
			for n := range order {
				if _, err := t.tx.Exec(`UPDATE queue SET ord=? WHERE id=?`, n+1, order[n].ID); err != nil {
					return err
				}
			}
		}
		return emitQueue(t, threadID, nil)
	})
	if err == nil {
		q.resetAttempt(threadID)
		q.kickAsync(threadID)
	}
	return err
}

func queueNotFound(err error) error {
	if errors.Is(err, errOrchNotFound) {
		return &orchHTTPError{http.StatusNotFound, "queued message not found"}
	}
	return err
}

func (q *QueueManager) resetAttempt(threadID string) {
	q.mu.Lock()
	delete(q.attempt, threadID)
	q.mu.Unlock()
}

// ── dispatch ──

// claim marks the item sending; it fails when the thread already dispatches.
func (q *QueueManager) claim(threadID, id string) (*QItem, error) {
	q.mu.Lock()
	if q.inflight[threadID] {
		q.mu.Unlock()
		return nil, orchConflict("a message of this thread is being sent")
	}
	q.inflight[threadID] = true
	q.mu.Unlock()
	var it *QItem
	err := q.store.write(func(t *orchTx) error {
		var err error
		if id == "" {
			it, err = getJSON[QItem](t.tx, `SELECT json FROM queue WHERE thread_id=? AND json_extract(json,'$.state')='queued' ORDER BY ord, id LIMIT 1`, threadID)
		} else {
			it, err = queueItemTx(t, threadID, id)
		}
		if err != nil {
			return queueNotFound(err)
		}
		if it.State == "sending" {
			return orchConflict("this message is being sent")
		}
		it.State, it.Error = "sending", ""
		if err := queuePutTx(t, it, -1); err != nil {
			return err
		}
		return emitQueue(t, threadID, nil)
	})
	if err != nil {
		q.release(threadID)
		return nil, err
	}
	return it, nil
}

func (q *QueueManager) release(threadID string) {
	q.mu.Lock()
	delete(q.inflight, threadID)
	q.mu.Unlock()
}

// deliver sends a claimed item. keep: a refusal that leaves the message queued (cannot steer, terminal live).
func (q *QueueManager) deliver(it *QItem) (string, error) {
	defer q.release(it.ThreadID)
	var mode string
	var err error
	if isDrivenID(it.ThreadID) {
		mode, err = q.driven.send(it.ThreadID, it.Text, it.Attachments)
	} else {
		mode, err = "pane", q.watched.send(it.Session, it.Text)
	}
	keep := false
	var he *orchHTTPError
	if errors.As(err, &he) && he.code == http.StatusConflict {
		keep = true
	}
	_ = q.store.write(func(t *orchTx) error {
		if err == nil {
			it.State, it.Mode, it.SentAt = "sent", mode, t.now
			if _, e := t.tx.Exec(`DELETE FROM queue WHERE id=?`, it.ID); e != nil {
				return e
			}
			return emitQueue(t, it.ThreadID, it)
		}
		if keep {
			it.State, it.Error = "queued", err.Error()
		} else {
			it.State, it.Error = "failed", err.Error()
		}
		if e := queuePutTx(t, it, -1); e != nil {
			return e
		}
		return emitQueue(t, it.ThreadID, nil)
	})
	return mode, err
}

// SendNow sends one message at once: a steer for a running driven thread, a pane send for a watched session.
func (q *QueueManager) SendNow(threadID, id string) (*QItem, string, error) {
	it, err := q.claim(threadID, id)
	if err != nil {
		return nil, "", err
	}
	mode, err := q.deliver(it)
	if err != nil {
		return it, "", err
	}
	q.resetAttempt(threadID)
	return it, mode, nil
}

// ready says whether the thread may take its next queued message now, and why it waits otherwise.
func (q *QueueManager) ready(threadID string, session string) (bool, string) {
	q.mu.Lock()
	last := q.attempt[threadID]
	q.mu.Unlock()
	if isDrivenID(threadID) {
		if !q.driven.exists(threadID) || q.driven.status(threadID) != "idle" {
			return false, ""
		}
		if q.driven.terminal(threadID) {
			return false, "terminal"
		}
		// One message per idle transition: a run must have started or ended since the last attempt.
		if last > 0 && q.store.LastRunAt(threadID) < last {
			return false, ""
		}
		return true, ""
	}
	ph, _ := q.watched.phase(session)
	now := q.now()
	q.mu.Lock()
	if ph == "busy" {
		q.busySeen[session] = now
	}
	// readySince restarts on any other reading, so one wrong "ready" between busy readings cannot send.
	if ph != "ready" {
		delete(q.readySince, session)
	} else if q.readySince[session] == 0 {
		q.readySince[session] = now
	}
	seen, readyFrom := q.busySeen[session], q.readySince[session]
	q.mu.Unlock()
	if ph != "ready" {
		return false, ""
	}
	// A message that ends within one tick and fires no hook (a ! command, /clear) leaves no busy reading;
	// a session ready without a break for queueStall after the last send takes the next one anyway.
	if last > 0 && seen <= last && (now-last < queueStall.Milliseconds() || now-max(readyFrom, last) < queueStall.Milliseconds()) {
		return false, ""
	}
	return true, ""
}

const queueStall = 20 * time.Second

func (q *QueueManager) kickAsync(threadID string) {
	q.kicks.Add(1)
	go func() {
		defer q.kicks.Done()
		q.Kick(threadID)
	}()
}

// Kick sends the oldest queued message when the thread is ready for it.
func (q *QueueManager) Kick(threadID string) {
	items, err := q.List(threadID)
	if err != nil {
		return
	}
	var next *QItem
	for i := range items {
		if items[i].State == "sending" {
			return
		}
		if next == nil && items[i].State == "queued" {
			next = &items[i]
		}
	}
	if next == nil {
		return
	}
	if ok, _ := q.ready(threadID, next.Session); !ok {
		return
	}
	it, err := q.claim(threadID, next.ID)
	if err != nil {
		return
	}
	q.mu.Lock()
	q.attempt[threadID] = q.now()
	q.mu.Unlock()
	if _, err := q.deliver(it); err != nil {
		log.Printf("queue: %s: %v", threadID, err)
	}
}

// NoteBusy records that a watched session works now, from a hook or transcript event.
func (q *QueueManager) NoteBusy(session string) {
	q.mu.Lock()
	q.busySeen[session] = q.now()
	q.mu.Unlock()
}

// sweep tries every thread with queued messages; the per-thread guards keep it to one message per idle transition.
func (q *QueueManager) sweep() {
	for _, id := range q.threadsWithQueued() {
		q.Kick(id)
	}
}

// View is the GET answer: items, the pause reason and the TUI's own queue for a watched session.
func (q *QueueManager) View(threadID, session string) (*QueueView, error) {
	items, err := q.List(threadID)
	if err != nil {
		return nil, err
	}
	v := &QueueView{Items: items}
	if isDrivenID(threadID) {
		if q.driven.exists(threadID) && q.driven.terminal(threadID) {
			v.Paused = "terminal"
		}
		return v, nil
	}
	if session == "" && len(items) > 0 {
		session = items[0].Session
	}
	if session != "" {
		if _, pane := q.watched.phase(session); pane != "" {
			v.Terminal = tuiQueued(pane)
		}
	}
	return v, nil
}

// ── watched phase ──

var (
	reTUIBusy   = regexp.MustCompile(`(?im)esc to interrupt|^\s*[\x{2800}-\x{28FF}]+\s+(working|thinking|generating|running)\b`)
	reTUIQueued = regexp.MustCompile(`(?i)(press up to (edit|select) (queued|a queued)|queued messages?)`)
)

// idleEvents end a turn: the agent waits for the next prompt.
var idleEvents = map[string]bool{"Stop": true, "task_complete": true, "SessionStart": true, "session_meta": true, "Notification": true}

// watchedPhaseOf: "ready" (idle at its prompt), "busy", "prompt" (a question or permission is open) or "off".
func watchedPhaseOf(s *Session, sendable bool, pane string) string {
	if !sendable {
		return "off"
	}
	if reTUIBusy.MatchString(lastLines(pane, 12)) {
		return "busy"
	}
	if panePromptOpen(pane) || (s != nil && s.State == StateAwaitingPermission) {
		return "prompt"
	}
	// The box with no "esc to interrupt" above it is the agent waiting for input; Cursor's box is a "→ " line.
	if strings.TrimSpace(cliInputBox(pane)) != "" || cursorInputLine(pane) {
		return "ready"
	}
	if s == nil {
		return "busy"
	}
	// Notification, transcript and state events come after Stop too: only start and stop events decide.
	for i := len(s.RecentEvents) - 1; i >= 0; i-- {
		k := s.RecentEvents[i].Kind
		if idleEvents[k] {
			return "ready"
		}
		if busyEvents[k] {
			return "busy"
		}
	}
	return "busy"
}

// tuiQueued reads the messages Claude Code holds in its own queue: the lines between the
// spinner and the input box, shown only while its "press up to edit queued messages" hint is on screen.
func tuiQueued(pane string) []string {
	lines := strings.Split(lastLines(pane, 40), "\n")
	if !reTUIQueued.MatchString(strings.Join(lines, "\n")) {
		return nil
	}
	start := -1
	for i := len(lines) - 1; i >= 0; i-- {
		if reTUIBusy.MatchString(lines[i]) {
			start = i + 1
			break
		}
	}
	if start < 0 {
		return nil
	}
	var out []string
	for _, ln := range lines[start:] {
		t := strings.TrimSpace(ln)
		if strings.HasPrefix(t, "╭") || (t != "" && strings.Trim(t, "─━═-") == "") {
			break
		}
		t = strings.TrimSpace(strings.TrimLeft(t, "❯>│ "))
		if t == "" || reTUIQueued.MatchString(t) {
			continue
		}
		out = append(out, clipString(t, 300))
	}
	return out
}

// ── adapters ──

type orchQueueDriven struct{ m *OrchManager }

func (d orchQueueDriven) exists(id string) bool {
	_, err := d.m.store.Thread(id)
	return err == nil
}
func (d orchQueueDriven) status(id string) string {
	th, err := d.m.store.Thread(id)
	if err != nil {
		return ""
	}
	return th.Status
}
func (d orchQueueDriven) terminal(id string) bool { return d.m.handover(id).Terminal }
func (d orchQueueDriven) send(id, text string, atts []string) (string, error) {
	return d.m.SendWith(id, text, atts)
}

type paneQueueWatched struct{}

func (paneQueueWatched) phase(session string) (string, string) {
	p := watchedPane(session)
	if !p.Sendable || p.Target() == "" {
		return "off", ""
	}
	content, err := targetCapture(p.Target(), 40, true)
	if err != nil {
		return "off", ""
	}
	var s *Session
	if store != nil {
		s = store.Get(p.AgentID)
		if s == nil {
			s = store.Get(resolveWatched(session).ID)
		}
	}
	return watchedPhaseOf(s, true, content), content
}

// send types through cliSend, the one path every client uses: lock, prompt and box checks, the Cursor pause, ! tracking.
func (paneQueueWatched) send(session, text string) error {
	_, err := cliSend(session, text)
	return err
}

// ── wiring ──

var queueMgr *QueueManager

var busyEvents = map[string]bool{"UserPromptSubmit": true, "PreToolUse": true, "PostToolUse": true, "task_started": true, "user_message": true}

// startQueue runs the dispatcher: a tick, run ends of driven threads, and session events of watched ones.
func startQueue(m *OrchManager) {
	queueMgr = newQueueManager(m.store, orchQueueDriven{m}, paneQueueWatched{})
	m.queue = queueMgr
	if store != nil {
		store.Subscribe(func(e ServerEvent) {
			s := e.Session
			if e.Kind != "upsert" || s == nil || len(s.RecentEvents) == 0 {
				return
			}
			k := s.RecentEvents[len(s.RecentEvents)-1].Kind
			if busyEvents[k] {
				queueMgr.NoteBusy(s.ID)
			} else if idleEvents[k] {
				go queueMgr.sweep()
			}
		})
	}
	go func() {
		for range time.Tick(queueTick) {
			queueMgr.sweep()
		}
	}()
}

func (m *OrchManager) queueRoutes(mux *http.ServeMux) {
	on := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if m.queue == nil {
				orchWriteErr(w, &orchHTTPError{http.StatusServiceUnavailable, "queue is off"})
				return
			}
			h(w, r)
		}
	}
	mux.HandleFunc("GET /api/queue/{thread}", on(func(w http.ResponseWriter, r *http.Request) {
		thread, session := r.PathValue("thread"), r.URL.Query().Get("session")
		v, err := m.queue.View(thread, session)
		if err != nil {
			orchWriteErr(w, err)
			return
		}
		if !isDrivenID(thread) {
			if session == "" {
				session = thread
			}
			if cv, _, err := cliQueueView(session, r.URL.Query().Get("cliAll") == "1"); err == nil {
				v.CLI = cv
				if cv.Supported {
					v.Terminal = nil
				}
			}
		}
		writeJSON(w, v)
	}))
	mux.HandleFunc("POST /api/queue/{thread}", on(func(w http.ResponseWriter, r *http.Request) {
		var in queueAdd
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&in); err != nil {
			orchWriteErr(w, orchBadRequest("bad json"))
			return
		}
		it, err := m.queue.Add(r.PathValue("thread"), in)
		if err != nil {
			orchWriteErr(w, err)
			return
		}
		items, _ := m.queue.List(it.ThreadID)
		writeJSONStatus(w, http.StatusCreated, map[string]any{"item": it, "items": items})
	}))
	mux.HandleFunc("DELETE /api/queue/{thread}", on(func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("thread")
		if err := m.queue.Clear(id); err != nil {
			orchWriteErr(w, err)
			return
		}
		items, _ := m.queue.List(id)
		writeJSON(w, map[string]any{"ok": true, "items": items})
	}))
	mux.HandleFunc("DELETE /api/queue/{thread}/{id}", on(func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("thread")
		if err := m.queue.Remove(id, r.PathValue("id")); err != nil {
			orchWriteErr(w, err)
			return
		}
		items, _ := m.queue.List(id)
		writeJSON(w, map[string]any{"ok": true, "items": items})
	}))
	mux.HandleFunc("PATCH /api/queue/{thread}/{id}", on(func(w http.ResponseWriter, r *http.Request) {
		var in queuePatch
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&in); err != nil {
			orchWriteErr(w, orchBadRequest("bad json"))
			return
		}
		id := r.PathValue("thread")
		if err := m.queue.Patch(id, r.PathValue("id"), in); err != nil {
			orchWriteErr(w, err)
			return
		}
		items, _ := m.queue.List(id)
		writeJSON(w, map[string]any{"ok": true, "items": items})
	}))
	mux.HandleFunc("POST /api/queue/{thread}/{id}/send-now", on(func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("thread")
		it, mode, err := m.queue.SendNow(id, r.PathValue("id"))
		if err != nil {
			orchWriteErr(w, err)
			return
		}
		items, _ := m.queue.List(id)
		writeJSON(w, map[string]any{"ok": true, "item": it, "mode": mode, "items": items})
	}))
}

// cursorInputLine is true when Cursor CLI's "→ " input line is near the bottom with no stop hint on it.
func cursorInputLine(pane string) bool {
	for _, l := range strings.Split(lastLines(pane, 8), "\n") {
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, "→ ") && !strings.Contains(t, "ctrl+c") {
			return true
		}
	}
	return false
}
