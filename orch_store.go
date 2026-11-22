package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// Orchestrator model: Go json shapes match web/src/v3/api/orch.ts.
type OThread struct {
	ID             string `json:"id"`
	Kind           string `json:"kind"`
	Agent          string `json:"agent"`
	Title          string `json:"title"`
	Cwd            string `json:"cwd"`
	Repo           string `json:"repo"`
	Branch         string `json:"branch"`
	Model          string `json:"model"`
	Sandbox        string `json:"sandbox"`
	Status         string `json:"status"`
	NativeID       string `json:"nativeId"`
	ParentID       string `json:"parentId,omitempty"`
	CreatedAt      int64  `json:"createdAt"`
	UpdatedAt      int64  `json:"updatedAt"`
	Error          string `json:"error,omitempty"`
	ApprovalPolicy string `json:"approvalPolicy,omitempty"`
	PermissionMode string `json:"permissionMode,omitempty"`

	// Phase 3: lineage and server-derived list state.
	Children     []string   `json:"children,omitempty"`
	Depth        int        `json:"depth"`
	Workspace    string     `json:"workspace,omitempty"`
	Worktree     *OWorktree `json:"worktree,omitempty"`
	Group        string     `json:"group"`
	SettledAt    int64      `json:"settledAt,omitempty"`
	SnoozedUntil int64      `json:"snoozedUntil,omitempty"`
	Pinned       bool       `json:"pinned,omitempty"`
	NotifyParent *bool      `json:"notifyParent,omitempty"`
	ReadyAt      int64      `json:"readyAt,omitempty"`
	ReportedAt   int64      `json:"reportedAt,omitempty"`
	// SpendAllowOnce lets one more turn start past a spend limit.
	SpendAllowOnce bool `json:"spendAllowOnce,omitempty"`

	// Phase 7: review, merge, templates and schedules.
	Review     *OReview `json:"review,omitempty"`
	ReviewOf   string   `json:"reviewOf,omitempty"`
	AutoReview string   `json:"autoReview,omitempty"`
	Merge      *OMerge  `json:"merge,omitempty"`
	TemplateID string   `json:"templateId,omitempty"`
	ScheduleID string   `json:"scheduleId,omitempty"`
	CatchUp    bool     `json:"catchUp,omitempty"`
}

// OReview is the outcome of a review child, stored on the reviewed thread.
type OReview struct {
	State     string     `json:"state"` // running | done | failed
	ChildID   string     `json:"childId"`
	Agent     string     `json:"agent"`
	Run       *int       `json:"run,omitempty"`
	Focus     string     `json:"focus,omitempty"`
	Findings  []OFinding `json:"findings"`
	Summary   string     `json:"summary,omitempty"`
	Error     string     `json:"error,omitempty"`
	StartedAt int64      `json:"startedAt"`
	EndedAt   int64      `json:"endedAt,omitempty"`
}

type OFinding struct {
	Severity string `json:"severity"` // critical | high | medium | low | info
	File     string `json:"file,omitempty"`
	Line     int    `json:"line,omitempty"`
	Title    string `json:"title"`
	Body     string `json:"body,omitempty"`
}

// OMerge records the last successful merge of a worktree thread.
type OMerge struct {
	Into     string `json:"into"`
	Strategy string `json:"strategy"`
	Sha      string `json:"sha"`
	At       int64  `json:"at"`
}

type OWorktree struct {
	Path    string `json:"path"`
	Branch  string `json:"branch"`
	Removed bool   `json:"removed,omitempty"`
	// Origin is the checkout the worktree was created from; OriginBranch was its branch then.
	Origin       string `json:"origin,omitempty"`
	OriginBranch string `json:"originBranch,omitempty"`
}

// notifies reports whether this child's end is announced to its parent (nil means true).
func (th *OThread) notifies() bool { return th.NotifyParent == nil || *th.NotifyParent }

type OUsage struct {
	InTokens  int64   `json:"inTokens"`
	OutTokens int64   `json:"outTokens"`
	CostUSD   float64 `json:"costUsd,omitempty"`
	// CachedIn is the part of InTokens served from the prompt cache (Codex).
	CachedIn int64 `json:"cachedInTokens,omitempty"`
	// Estimated marks a cost priced from tokens by the built-in table, not reported by the agent.
	Estimated bool `json:"estimated,omitempty"`
}

type ORun struct {
	ID        string  `json:"id"`
	ThreadID  string  `json:"threadId"`
	Index     int     `json:"index"`
	Prompt    string  `json:"prompt"`
	Status    string  `json:"status"`
	StartedAt int64   `json:"startedAt"`
	EndedAt   int64   `json:"endedAt,omitempty"`
	Usage     *OUsage `json:"usage,omitempty"`
	TurnID    string  `json:"turnId,omitempty"`
}

type OFile struct {
	Path string `json:"path"`
	Kind string `json:"kind"`
	Add  int    `json:"add"`
	Del  int    `json:"del"`
}

type OItem struct {
	ID       string  `json:"id"`
	ThreadID string  `json:"threadId"`
	RunID    string  `json:"runId"`
	Kind     string  `json:"kind"`
	Status   string  `json:"status"`
	Text     string  `json:"text"`
	Command  string  `json:"command,omitempty"`
	Cwd      string  `json:"cwd,omitempty"`
	ExitCode *int    `json:"exitCode,omitempty"`
	Output   string  `json:"output,omitempty"`
	Files    []OFile `json:"files,omitempty"`
	// Attachments are the images of a user item; ChildID links a review item to its child thread.
	Attachments []OAttachment `json:"attachments,omitempty"`
	ChildID     string        `json:"childId,omitempty"`
	// ParentID links a tool call that a Code Mode script made to the script's item.
	ParentID string `json:"parentId,omitempty"`
	// By is "user" for a shell command the user ran with ! in the agent CLI.
	By string `json:"by,omitempty"`
	// Bg marks a ! command Claude Code moved to the background; its output fills from the task file.
	Bg        bool  `json:"bg,omitempty"`
	StartedAt int64 `json:"startedAt"`
	EndedAt   int64 `json:"endedAt,omitempty"`
	Repeat    int   `json:"repeat,omitempty"` // an error seen this many times; the item keeps the first place, EndedAt the last time
}

type ORequest struct {
	ID         string `json:"id"`
	ThreadID   string `json:"threadId"`
	RunID      string `json:"runId"`
	ItemID     string `json:"itemId,omitempty"`
	Kind       string `json:"kind"`
	ApprovalID string `json:"approvalId"`
	State      string `json:"state"`
}

type OEvent struct {
	Seq      int64           `json:"seq"`
	ThreadID string          `json:"threadId"`
	At       int64           `json:"at"`
	Type     string          `json:"type"`
	Data     json.RawMessage `json:"data"`
}

const (
	orchOutputCap   = 8 << 10
	orchEventKeep   = 7 * 24 * time.Hour
	orchDeltaWindow = 250 * time.Millisecond
)

var errOrchNotFound = errors.New("not found")

// OrchStore persists the orchestrator state and its event log; writes are serialized.
type OrchStore struct {
	mu  sync.Mutex
	db  *sql.DB
	hub *OrchHub

	dmu     sync.Mutex
	deltas  map[string]*orchDelta
	order   []string
	flushAt *time.Timer
}

type orchDelta struct {
	threadID, itemID, field string
	text                    string
}

func openOrchStore(dir string, hub *OrchHub) (*OrchStore, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, "orch.db")+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	for _, q := range []string{
		`CREATE TABLE IF NOT EXISTS threads(id TEXT PRIMARY KEY, created_at INTEGER, json TEXT)`,
		`CREATE TABLE IF NOT EXISTS runs(id TEXT PRIMARY KEY, thread_id TEXT, idx INTEGER, json TEXT)`,
		`CREATE INDEX IF NOT EXISTS runs_thread ON runs(thread_id, idx)`,
		`CREATE TABLE IF NOT EXISTS items(id TEXT PRIMARY KEY, thread_id TEXT, run_id TEXT, ord INTEGER, json TEXT)`,
		`CREATE INDEX IF NOT EXISTS items_thread ON items(thread_id, ord)`,
		`CREATE TABLE IF NOT EXISTS requests(id TEXT PRIMARY KEY, thread_id TEXT, approval_id TEXT, json TEXT)`,
		`CREATE INDEX IF NOT EXISTS requests_thread ON requests(thread_id)`,
		`CREATE TABLE IF NOT EXISTS events(seq INTEGER PRIMARY KEY AUTOINCREMENT, thread_id TEXT, at INTEGER, type TEXT, data TEXT)`,
		`CREATE INDEX IF NOT EXISTS events_at ON events(at)`,
		`CREATE TABLE IF NOT EXISTS checkpoints(ref TEXT PRIMARY KEY, thread_id TEXT, root TEXT, at INTEGER, json TEXT)`,
		`CREATE INDEX IF NOT EXISTS checkpoints_thread ON checkpoints(thread_id)`,
		`CREATE TABLE IF NOT EXISTS queue(id TEXT PRIMARY KEY, thread_id TEXT, ord REAL, json TEXT)`,
		`CREATE INDEX IF NOT EXISTS queue_thread ON queue(thread_id, ord)`,
	} {
		if _, err := db.Exec(q); err != nil {
			db.Close()
			return nil, fmt.Errorf("orch db: %w", err)
		}
	}
	if err := migrateOrchDB(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("orch db migrate: %w", err)
	}
	if hub == nil {
		hub = newOrchHub()
	}
	return &OrchStore{db: db, hub: hub, deltas: map[string]*orchDelta{}}, nil
}

func (s *OrchStore) Close() error {
	s.flushDeltas("")
	return s.db.Close()
}

// migrateOrchDB adds the phase 3 columns to a phase 1-2 database.
func migrateOrchDB(db *sql.DB) error {
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('threads') WHERE name='parent_id'`).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		if _, err := db.Exec(`ALTER TABLE threads ADD COLUMN parent_id TEXT`); err != nil {
			return err
		}
		if _, err := db.Exec(`UPDATE threads SET parent_id=NULLIF(json_extract(json,'$.parentId'),'')`); err != nil {
			return err
		}
	}
	_, err := db.Exec(`CREATE INDEX IF NOT EXISTS threads_parent ON threads(parent_id)`)
	return err
}

// orchTx is one write transaction; emitted events get their seq at commit.
type orchTx struct {
	tx     *sql.Tx
	now    int64
	events []OEvent
	// touched threads get their group recomputed (with their ancestors) before commit.
	touched map[string]bool
}

func (t *orchTx) touch(id string) {
	if id == "" {
		return
	}
	if t.touched == nil {
		t.touched = map[string]bool{}
	}
	t.touched[id] = true
}

func (t *orchTx) putThread(th *OThread) error {
	t.touch(th.ID)
	return t.storeThread(th)
}

func (t *orchTx) storeThread(th *OThread) error {
	b, _ := json.Marshal(th)
	_, err := t.tx.Exec(`INSERT INTO threads(id, created_at, json, parent_id) VALUES(?,?,?,NULLIF(?,''))
		ON CONFLICT(id) DO UPDATE SET json=excluded.json, parent_id=excluded.parent_id`, th.ID, th.CreatedAt, string(b), th.ParentID)
	return err
}

func (t *orchTx) putRun(r *ORun) error {
	t.touch(r.ThreadID)
	b, _ := json.Marshal(r)
	_, err := t.tx.Exec(`INSERT INTO runs(id, thread_id, idx, json) VALUES(?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET json=excluded.json`, r.ID, r.ThreadID, r.Index, string(b))
	return err
}

func (t *orchTx) putItem(it *OItem) error {
	if len(it.Output) > orchOutputCap {
		it.Output = it.Output[len(it.Output)-orchOutputCap:]
	}
	b, _ := json.Marshal(it)
	_, err := t.tx.Exec(`INSERT INTO items(id, thread_id, run_id, ord, json) VALUES(?,?,?,
		(SELECT COALESCE(MAX(ord),0)+1 FROM items WHERE thread_id=?),?)
		ON CONFLICT(id) DO UPDATE SET json=excluded.json`, it.ID, it.ThreadID, it.RunID, it.ThreadID, string(b))
	return err
}

func (t *orchTx) putRequest(r *ORequest) error {
	t.touch(r.ThreadID)
	b, _ := json.Marshal(r)
	_, err := t.tx.Exec(`INSERT INTO requests(id, thread_id, approval_id, json) VALUES(?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET json=excluded.json`, r.ID, r.ThreadID, r.ApprovalID, string(b))
	return err
}

func (t *orchTx) emit(threadID, typ string, data any) {
	b, _ := json.Marshal(data)
	t.events = append(t.events, OEvent{ThreadID: threadID, At: t.now, Type: typ, Data: b})
}

func getJSON[T any](q interface {
	QueryRow(string, ...any) *sql.Row
}, query string, args ...any) (*T, error) {
	var js string
	if err := q.QueryRow(query, args...).Scan(&js); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errOrchNotFound
		}
		return nil, err
	}
	v := new(T)
	if err := json.Unmarshal([]byte(js), v); err != nil {
		return nil, err
	}
	return v, nil
}

func (t *orchTx) thread(id string) (*OThread, error) {
	return getJSON[OThread](t.tx, `SELECT json FROM threads WHERE id=?`, id)
}

func (t *orchTx) run(id string) (*ORun, error) {
	return getJSON[ORun](t.tx, `SELECT json FROM runs WHERE id=?`, id)
}

func (t *orchTx) item(id string) (*OItem, error) {
	return getJSON[OItem](t.tx, `SELECT json FROM items WHERE id=?`, id)
}

// write runs fn in a transaction, assigns seqs to its events and publishes them in order.
func (s *OrchStore) write(fn func(t *orchTx) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	t := &orchTx{tx: tx, now: time.Now().UnixMilli()}
	if err := fn(t); err != nil {
		tx.Rollback()
		return err
	}
	if err := t.refreshGroups(); err != nil {
		tx.Rollback()
		return err
	}
	for i := range t.events {
		ev := &t.events[i]
		res, err := tx.Exec(`INSERT INTO events(thread_id, at, type, data) VALUES(?,?,?,?)`, ev.ThreadID, ev.At, ev.Type, string(ev.Data))
		if err != nil {
			tx.Rollback()
			return err
		}
		ev.Seq, _ = res.LastInsertId()
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	for _, ev := range t.events {
		s.hub.publish(ev)
	}
	return nil
}

// read runs fn in one read transaction so the snapshot and its seq agree.
func (s *OrchStore) read(fn func(tx *sql.Tx) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	return fn(tx)
}

func currentSeq(tx *sql.Tx) int64 {
	var seq int64
	_ = tx.QueryRow(`SELECT seq FROM sqlite_sequence WHERE name='events'`).Scan(&seq)
	return seq
}

func scanJSONRows[T any](rows *sql.Rows, err error) ([]T, error) {
	out := []T{}
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var js string
		if err := rows.Scan(&js); err != nil {
			return out, err
		}
		var v T
		if json.Unmarshal([]byte(js), &v) == nil {
			out = append(out, v)
		}
	}
	return out, rows.Err()
}

func (s *OrchStore) Threads() ([]OThread, int64, error) {
	var out []OThread
	var seq int64
	err := s.read(func(tx *sql.Tx) error {
		var err error
		rows, qerr := tx.Query(`SELECT json FROM threads`)
		if out, err = scanJSONRows[OThread](rows, qerr); err != nil {
			return err
		}
		seq = currentSeq(tx)
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].UpdatedAt > out[j].UpdatedAt })
	return out, seq, err
}

type OThreadDetail struct {
	Thread   *OThread   `json:"thread"`
	Children []OThread  `json:"children"`
	Runs     []ORun     `json:"runs"`
	Items    []OItem    `json:"items"`
	Requests []ORequest `json:"requests"`
	Seq      int64      `json:"seq"`
}

func (s *OrchStore) ThreadDetail(id string) (*OThreadDetail, error) {
	d := &OThreadDetail{}
	err := s.read(func(tx *sql.Tx) error {
		th, err := getJSON[OThread](tx, `SELECT json FROM threads WHERE id=?`, id)
		if err != nil {
			return err
		}
		d.Thread = th
		rows, qerr := tx.Query(`SELECT json FROM threads WHERE parent_id=? ORDER BY created_at`, id)
		if d.Children, err = scanJSONRows[OThread](rows, qerr); err != nil {
			return err
		}
		rows, qerr = tx.Query(`SELECT json FROM runs WHERE thread_id=? ORDER BY idx`, id)
		if d.Runs, err = scanJSONRows[ORun](rows, qerr); err != nil {
			return err
		}
		rows, qerr = tx.Query(`SELECT json FROM items WHERE thread_id=? ORDER BY ord`, id)
		if d.Items, err = scanJSONRows[OItem](rows, qerr); err != nil {
			return err
		}
		for i := range d.Items {
			if d.Items[i].Kind == "error" { // errors saved before color codes were dropped
				d.Items[i].Text = reANSI.ReplaceAllString(d.Items[i].Text, "")
			}
		}
		rows, qerr = tx.Query(`SELECT json FROM requests WHERE thread_id=? ORDER BY rowid`, id)
		if d.Requests, err = scanJSONRows[ORequest](rows, qerr); err != nil {
			return err
		}
		d.Seq = currentSeq(tx)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return d, nil
}

func (s *OrchStore) Thread(id string) (*OThread, error) {
	var th *OThread
	err := s.read(func(tx *sql.Tx) error {
		var err error
		th, err = getJSON[OThread](tx, `SELECT json FROM threads WHERE id=?`, id)
		return err
	})
	return th, err
}

// LastRunAt is the newest start or end time of any run of the thread, 0 when it has none.
func (s *OrchStore) LastRunAt(threadID string) int64 {
	var at sql.NullInt64
	_ = s.read(func(tx *sql.Tx) error {
		return tx.QueryRow(`SELECT MAX(MAX(COALESCE(json_extract(json,'$.startedAt'),0), COALESCE(json_extract(json,'$.endedAt'),0))) FROM runs WHERE thread_id=?`, threadID).Scan(&at)
	})
	return at.Int64
}

// EventsAfter returns up to limit events with seq > after, oldest first.
func (s *OrchStore) EventsAfter(after int64, limit int) ([]OEvent, error) {
	out := []OEvent{}
	err := s.read(func(tx *sql.Tx) error {
		rows, err := tx.Query(`SELECT seq, thread_id, at, type, data FROM events WHERE seq > ? ORDER BY seq LIMIT ?`, after, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var ev OEvent
			var data string
			if err := rows.Scan(&ev.Seq, &ev.ThreadID, &ev.At, &ev.Type, &data); err != nil {
				return err
			}
			ev.Data = json.RawMessage(data)
			out = append(out, ev)
		}
		return rows.Err()
	})
	return out, err
}

// Prune drops events older than the keep window.
func (s *OrchStore) Prune(now time.Time) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.Exec(`DELETE FROM events WHERE at < ?`, now.Add(-orchEventKeep).UnixMilli())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// AppendDelta buffers streamed text; a flush appends it to the item and emits one item.delta.
func (s *OrchStore) AppendDelta(threadID, itemID, field, text string) {
	if text == "" {
		return
	}
	s.dmu.Lock()
	defer s.dmu.Unlock()
	key := itemID + "\x00" + field
	if d, ok := s.deltas[key]; ok {
		d.text += text
		return
	}
	s.deltas[key] = &orchDelta{threadID: threadID, itemID: itemID, field: field, text: text}
	s.order = append(s.order, key)
	if s.flushAt == nil {
		s.flushAt = time.AfterFunc(orchDeltaWindow, func() { s.flushDeltas("") })
	}
}

// flushDeltas writes buffered deltas: all of them, or only those of one item.
func (s *OrchStore) flushDeltas(itemID string) {
	s.dmu.Lock()
	var batch []*orchDelta
	keep := s.order[:0:0]
	for _, k := range s.order {
		d := s.deltas[k]
		if itemID == "" || d.itemID == itemID {
			batch = append(batch, d)
			delete(s.deltas, k)
		} else {
			keep = append(keep, k)
		}
	}
	s.order = keep
	if len(s.order) == 0 && s.flushAt != nil {
		s.flushAt.Stop()
		s.flushAt = nil
	}
	s.dmu.Unlock()
	if len(batch) == 0 {
		return
	}
	err := s.write(func(t *orchTx) error {
		for _, d := range batch {
			it, err := t.item(d.itemID)
			if err != nil || it.Status != "started" {
				continue // a completed item already holds its final text
			}
			ev := map[string]any{"itemId": d.itemID, "text": d.text}
			if d.field == "output" {
				it.Output += d.text
				ev["field"] = "output"
			} else {
				it.Text += d.text
			}
			if err := t.putItem(it); err != nil {
				return err
			}
			t.emit(d.threadID, "item.delta", ev)
		}
		return nil
	})
	if err != nil {
		log.Printf("orch: flush deltas: %v", err)
	}
}

// RecoverAfterRestart marks every live thread idle: its process died with the old daemon.
func (s *OrchStore) RecoverAfterRestart() error {
	return s.write(func(t *orchTx) error {
		threads, err := scanJSONRows[OThread](t.tx.Query(`SELECT json FROM threads`))
		if err != nil {
			return err
		}
		for i := range threads {
			th := &threads[i]
			if th.Status == "closed" || th.Status == "idle" {
				continue
			}
			if th.Status != "error" {
				th.Status = "idle"
			}
			th.UpdatedAt = t.now
			if err := t.putThread(th); err != nil {
				return err
			}
			t.emit(th.ID, "thread.updated", map[string]any{"thread": th})
		}
		runs, err := scanJSONRows[ORun](t.tx.Query(`SELECT json FROM runs WHERE json_extract(json,'$.status')='running'`))
		if err != nil {
			return err
		}
		for i := range runs {
			r := &runs[i]
			r.Status, r.EndedAt = "interrupted", t.now
			if err := t.putRun(r); err != nil {
				return err
			}
			t.emit(r.ThreadID, "run.ended", map[string]any{"run": r})
		}
		items, err := scanJSONRows[OItem](t.tx.Query(`SELECT json FROM items WHERE json_extract(json,'$.status')='started'`))
		if err != nil {
			return err
		}
		for i := range items {
			it := &items[i]
			it.Status, it.EndedAt = "failed", t.now
			if err := t.putItem(it); err != nil {
				return err
			}
			t.emit(it.ThreadID, "item.completed", map[string]any{"item": it})
		}
		reqs, err := scanJSONRows[ORequest](t.tx.Query(`SELECT json FROM requests WHERE json_extract(json,'$.state')='open'`))
		if err != nil {
			return err
		}
		for i := range reqs {
			r := &reqs[i]
			r.State = "closed"
			if err := t.putRequest(r); err != nil {
				return err
			}
			t.emit(r.ThreadID, "request.closed", map[string]any{"request": r})
		}
		return nil
	})
}

// MarshalJSON drops terminal color codes from Error; errors saved before the fix still carry them.
func (t OThread) MarshalJSON() ([]byte, error) {
	type plain OThread
	if strings.Contains(t.Error, "\x1b") {
		t.Error = reANSI.ReplaceAllString(t.Error, "")
	}
	return json.Marshal(plain(t))
}
