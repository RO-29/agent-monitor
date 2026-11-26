package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// notifyEvent is one orchestrator event for the notifier.
type notifyEvent struct {
	Kind     string // needs-you | finished | error | spend
	ThreadID string
	Title    string
	Body     string
	Count    int      // events this one stands for (a fan-out group); 0 = 1
	Keys     []string // approval ids; the alert resolves itself when all of them end
	URL      string   // an https link a tapped alert opens (the iPhone install page)
	reminder bool
}

// notify hands an event to the notifier when one is installed.
func (m *OrchManager) notify(ev notifyEvent) {
	if m != nil && m.notifier != nil {
		m.notifier(ev)
	}
}

// ONotification is one entry of the notification centre; Go json = web/src/v3/api/notify.ts.
type ONotification struct {
	ID       string `json:"id"`
	Kind     string `json:"kind"`
	Title    string `json:"title"`
	Body     string `json:"body,omitempty"`
	ThreadID string `json:"threadId,omitempty"`
	URL      string `json:"url,omitempty"`
	At       int64  `json:"at"`
	Read     bool   `json:"read,omitempty"`
	Count    int    `json:"count,omitempty"`
	// Quiet marks a notification made during quiet hours: listed, never pushed or alerted.
	Quiet bool `json:"quiet,omitempty"`
	// Resolved: every approval it was about ended, wherever it was answered.
	Resolved bool `json:"resolved,omitempty"`
	// Prompt: it is about a terminal prompt, so the phone offers Yes and No on it.
	Prompt bool `json:"prompt,omitempty"`
}

// NotifySettings: nil booleans mean the default (on).
type NotifySettings struct {
	NeedsYou            *bool  `json:"needsYou"`
	Finished            *bool  `json:"finished"`
	Error               *bool  `json:"error"`
	Spend               *bool  `json:"spend"`
	QuietStart          string `json:"quietStart"`
	QuietEnd            string `json:"quietEnd"`
	QuietAllowsNeedsYou *bool  `json:"quietAllowsNeedsYou"`
	BatchSec            *int   `json:"batchSec"`
	// RemindMin: minutes before an unanswered "needs you" alert comes again, once; nil = 10, 0 = off.
	RemindMin *int `json:"remindMin"`
}

func (s NotifySettings) remindAfter() time.Duration {
	if s.RemindMin == nil {
		return 10 * time.Minute
	}
	return time.Duration(max(0, *s.RemindMin)) * time.Minute
}

func onOr(b *bool) bool { return b == nil || *b }

func (s NotifySettings) enabled(kind string) bool {
	switch kind {
	case "needs-you":
		return onOr(s.NeedsYou)
	case "finished":
		return onOr(s.Finished)
	case "error":
		return onOr(s.Error)
	case "spend":
		return onOr(s.Spend)
	}
	return true
}

func (s NotifySettings) batchSecs() int {
	if s.BatchSec == nil {
		return 20
	}
	return max(0, *s.BatchSec)
}

func parseClock(s string) (int, bool) {
	h, m, ok := strings.Cut(s, ":")
	if !ok {
		return 0, false
	}
	hh, e1 := strconv.Atoi(h)
	mm, e2 := strconv.Atoi(m)
	if e1 != nil || e2 != nil || hh < 0 || hh > 23 || mm < 0 || mm > 59 || len(m) != 2 {
		return 0, false
	}
	return hh*60 + mm, true
}

// quiet reports whether t falls inside the quiet hours (local time; the window may cross midnight).
func (s NotifySettings) quiet(t time.Time) bool {
	a, ok1 := parseClock(s.QuietStart)
	b, ok2 := parseClock(s.QuietEnd)
	if !ok1 || !ok2 || a == b {
		return false
	}
	n := t.Hour()*60 + t.Minute()
	if a < b {
		return n >= a && n < b
	}
	return n >= a || n < b
}

// Notifier stores notifications, batches them and fans them out to SSE subscribers and Web Push.
type Notifier struct {
	db   *sql.DB
	cfg  func() NotifySettings
	now  func() time.Time
	push *webPusher
	// bark sends to the Bark app too; only the daemon's own notifier sets it, so tests send nothing.
	bark bool
	// unit is one batchSec (tests shorten it).
	unit time.Duration

	mu      sync.Mutex
	subs    map[int]chan ONotification
	next    int
	batches map[string]*notifyBatch
	keyOf   map[string]int64    // approval id -> notification id
	open    map[int64]int       // notification id -> approvals still open
	gone    map[string]bool     // approvals that ended before their alert was stored
	openAt  map[int64]time.Time // open "needs you" alerts not reminded yet
}

type notifyBatch struct {
	held []notifyEvent
}

const (
	notifyKeep     = 30 * 24 * time.Hour
	notifyKeepRows = 2000
)

func openNotifier(dir string, cfg func() NotifySettings) (*Notifier, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, "notify.db")+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	for _, q := range []string{
		`CREATE TABLE IF NOT EXISTS notifications(id INTEGER PRIMARY KEY AUTOINCREMENT, at INTEGER, read INTEGER DEFAULT 0, json TEXT)`,
		`CREATE TABLE IF NOT EXISTS push_subs(endpoint TEXT PRIMARY KEY, json TEXT, created_at INTEGER)`,
	} {
		if _, err := db.Exec(q); err != nil {
			db.Close()
			return nil, err
		}
	}
	// Older databases lack the device column; the error on a second ALTER is expected.
	_, _ = db.Exec(`ALTER TABLE push_subs ADD COLUMN device TEXT NOT NULL DEFAULT ''`)
	n := &Notifier{db: db, cfg: cfg, now: time.Now, unit: time.Second, subs: map[int]chan ONotification{}, batches: map[string]*notifyBatch{}, keyOf: map[string]int64{}, open: map[int64]int{}, gone: map[string]bool{}, openAt: map[int64]time.Time{}}
	if keys, err := loadVAPID(dir); err != nil {
		log.Printf("notify: web push off: %v", err)
	} else {
		n.push = &webPusher{keys: keys, sub: vapidSubject(), client: newPushClient()}
	}
	return n, nil
}

// Emit takes an event: the first of a kind goes out at once, later ones in the batch window fold into one.
func (n *Notifier) Emit(ev notifyEvent) {
	cfg := n.cfg()
	if !cfg.enabled(ev.Kind) {
		return
	}
	win := time.Duration(cfg.batchSecs()) * n.unit
	n.mu.Lock()
	if b := n.batches[ev.Kind]; b != nil {
		b.held = append(b.held, ev)
		n.mu.Unlock()
		return
	}
	if win > 0 {
		n.batches[ev.Kind] = &notifyBatch{}
		time.AfterFunc(win, func() { n.flush(ev.Kind, win) })
	}
	n.mu.Unlock()
	n.deliver(ev)
}

// flush sends what the window held as one notification and keeps the window open while events keep coming.
func (n *Notifier) flush(kind string, win time.Duration) {
	n.mu.Lock()
	b := n.batches[kind]
	if b == nil || len(b.held) == 0 {
		delete(n.batches, kind)
		n.mu.Unlock()
		return
	}
	held := b.held
	b.held = nil
	time.AfterFunc(win, func() { n.flush(kind, win) })
	n.mu.Unlock()
	n.deliver(foldEvents(kind, held))
}

func foldEvents(kind string, held []notifyEvent) notifyEvent {
	if len(held) == 1 {
		return held[0]
	}
	total := 0
	thread := held[0].ThreadID
	var titles []string
	for _, e := range held {
		total += max(1, e.Count)
		if e.ThreadID != thread {
			thread = ""
		}
		titles = append(titles, e.Title)
	}
	head := map[string]string{"needs-you": "%d requests need you", "finished": "%d threads finished", "error": "%d threads stopped with an error",
		"spend": "%d spend limit notices"}[kind]
	if head == "" {
		head = "%d notifications"
	}
	var keys []string
	for _, e := range held {
		keys = append(keys, e.Keys...)
	}
	return notifyEvent{Kind: kind, ThreadID: thread, Title: fmt.Sprintf(head, total), Body: clipString(strings.Join(titles, "; "), 400), Count: total, Keys: keys}
}

func (n *Notifier) deliver(ev notifyEvent) {
	cfg := n.cfg()
	now := n.now()
	no := ONotification{Kind: ev.Kind, Title: ev.Title, Body: ev.Body, ThreadID: ev.ThreadID, At: now.UnixMilli()}
	if ev.Count > 1 {
		no.Count = ev.Count
	}
	if strings.HasPrefix(ev.URL, "https://") {
		no.URL = ev.URL
	}
	if ev.ThreadID == "" && ev.Kind == "spend" {
		no.URL = "/settings/limits"
	}
	for _, k := range ev.Keys {
		no.Prompt = no.Prompt || strings.HasPrefix(k, "tp:")
	}
	no.Quiet = cfg.quiet(now) && !(ev.Kind == "needs-you" && onOr(cfg.QuietAllowsNeedsYou))
	b, _ := json.Marshal(no)
	res, err := n.db.Exec(`INSERT INTO notifications(at, json) VALUES(?, ?)`, no.At, string(b))
	if err != nil {
		log.Printf("notify: store: %v", err)
		return
	}
	id, _ := res.LastInsertId()
	no.ID = strconv.FormatInt(id, 10)
	if len(ev.Keys) > 0 {
		n.mu.Lock()
		left := 0
		for _, k := range ev.Keys {
			if n.gone[k] {
				delete(n.gone, k)
				continue
			}
			n.keyOf[k] = id
			left++
		}
		if left > 0 {
			n.open[id] = left
			if ev.Kind == "needs-you" && !ev.reminder {
				n.openAt[id] = now
			}
		}
		n.mu.Unlock()
		if left == 0 {
			no.Read, no.Resolved = true, true
			b, _ := json.Marshal(no)
			_, _ = n.db.Exec(`UPDATE notifications SET read=1, json=? WHERE id=?`, string(b), id)
			n.publish(no)
			return
		}
	}
	if id%100 == 0 {
		n.prune(now)
	}
	n.publish(no)
	if !no.Quiet && n.push != nil {
		go n.pushAll(no)
	}
	if !no.Quiet && n.bark {
		barkQueue(no, n.get)
	}
}

// remindTick sends an alert again, once, when its request is still open and nobody read it.
func (n *Notifier) remindTick() {
	cfg := n.cfg()
	after := cfg.remindAfter()
	now := n.now()
	if after <= 0 || !cfg.enabled("needs-you") || cfg.quiet(now) && !onOr(cfg.QuietAllowsNeedsYou) {
		return
	}
	n.mu.Lock()
	var due []int64
	for id, at := range n.openAt {
		if now.Sub(at) >= after {
			due = append(due, id)
			delete(n.openAt, id)
		}
	}
	n.mu.Unlock()
	for _, id := range due {
		n.remind(id, now)
	}
}

// remind stores a "Still waiting" copy, moves the open requests to it and retires the first alert.
func (n *Notifier) remind(old int64, now time.Time) {
	first, ok := n.get(strconv.FormatInt(old, 10))
	if !ok || first.Read || first.Resolved {
		return
	}
	no := first
	no.ID, no.At, no.Quiet = "", now.UnixMilli(), false
	no.Title = fmt.Sprintf("Still waiting (%d min): %s", max(1, int(now.Sub(time.UnixMilli(first.At)).Minutes())), first.Title)
	b, _ := json.Marshal(no)
	res, err := n.db.Exec(`INSERT INTO notifications(at, json) VALUES(?, ?)`, no.At, string(b))
	if err != nil {
		return
	}
	id, _ := res.LastInsertId()
	n.mu.Lock()
	moved := false
	for k, v := range n.keyOf {
		if v == old {
			n.keyOf[k], moved = id, true
		}
	}
	if moved {
		n.open[id] = n.open[old]
		delete(n.open, old)
	}
	n.mu.Unlock()
	if !moved { // the request ended while the copy was written
		_, _ = n.db.Exec(`DELETE FROM notifications WHERE id=?`, id)
		return
	}
	first.Read, first.Resolved = true, true // superseded: every device clears its banner
	fb, _ := json.Marshal(first)
	_, _ = n.db.Exec(`UPDATE notifications SET read=1, json=? WHERE id=?`, string(fb), old)
	n.publish(first)
	no.ID = strconv.FormatInt(id, 10)
	n.publish(no)
	if n.push != nil {
		go n.pushAll(no)
	}
	if n.bark {
		barkDelete(first.ID)
		barkQueue(no, n.get)
	}
}

func (n *Notifier) prune(now time.Time) {
	_, _ = n.db.Exec(`DELETE FROM notifications WHERE at < ? OR id <= (SELECT MAX(id) FROM notifications) - ?`, now.Add(-notifyKeep).UnixMilli(), notifyKeepRows)
}

func (n *Notifier) subscribe() (<-chan ONotification, func()) {
	n.mu.Lock()
	defer n.mu.Unlock()
	id := n.next
	n.next++
	ch := make(chan ONotification, 64)
	n.subs[id] = ch
	return ch, func() {
		n.mu.Lock()
		defer n.mu.Unlock()
		if c, ok := n.subs[id]; ok {
			delete(n.subs, id)
			close(c)
		}
	}
}

func (n *Notifier) publish(no ONotification) {
	n.mu.Lock()
	defer n.mu.Unlock()
	for id, ch := range n.subs {
		select {
		case ch <- no:
		default:
			delete(n.subs, id)
			close(ch)
		}
	}
}

func scanNotifications(rows *sql.Rows, err error) ([]ONotification, error) {
	out := []ONotification{}
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var read int
		var js string
		if rows.Scan(&id, &read, &js) != nil {
			continue
		}
		var no ONotification
		if json.Unmarshal([]byte(js), &no) == nil {
			no.ID, no.Read = strconv.FormatInt(id, 10), read != 0
			out = append(out, no)
		}
	}
	return out, rows.Err()
}

// List returns notifications with id > after, newest first.
func (n *Notifier) List(after int64, limit int) ([]ONotification, int, error) {
	items, err := scanNotifications(n.db.Query(`SELECT id, read, json FROM notifications WHERE id > ? ORDER BY id DESC LIMIT ?`, after, limit))
	if err != nil {
		return nil, 0, err
	}
	return items, n.unread(), nil
}

// since returns notifications with id > after, oldest first (stream replay).
func (n *Notifier) since(after int64) []ONotification {
	items, _ := scanNotifications(n.db.Query(`SELECT id, read, json FROM notifications WHERE id > ? ORDER BY id LIMIT 500`, after))
	return items
}

func (n *Notifier) unread() int {
	var c int
	_ = n.db.QueryRow(`SELECT COUNT(*) FROM notifications WHERE read=0`).Scan(&c)
	return c
}

// Resolve marks the alert for an ended approval read and resolved, and drops it if still held in a batch.
func (n *Notifier) Resolve(key string) {
	n.mu.Lock()
	dropped := false
	for _, b := range n.batches {
		kept := b.held[:0]
		for _, e := range b.held {
			if !(len(e.Keys) == 1 && e.Keys[0] == key) {
				kept = append(kept, e)
			} else {
				dropped = true
			}
		}
		b.held = kept
	}
	id, ok := n.keyOf[key]
	delete(n.keyOf, key)
	// "gone" catches an alert stored after its approval ended; a session key repeats for each prompt, so it never applies.
	if !ok && !dropped && !strings.HasPrefix(key, "tp:") {
		if len(n.gone) > 1000 {
			n.gone = map[string]bool{}
		}
		n.gone[key] = true
	}
	if ok {
		n.open[id]--
		ok = n.open[id] <= 0
	}
	if ok {
		delete(n.open, id)
		delete(n.openAt, id)
	}
	n.mu.Unlock()
	if !ok {
		return
	}
	var raw string
	if err := n.db.QueryRow(`SELECT json FROM notifications WHERE id=?`, id).Scan(&raw); err != nil {
		return
	}
	var no ONotification
	if json.Unmarshal([]byte(raw), &no) != nil {
		return
	}
	no.ID, no.Read, no.Resolved = strconv.FormatInt(id, 10), true, true
	b, _ := json.Marshal(no)
	if _, err := n.db.Exec(`UPDATE notifications SET read=1, json=? WHERE id=?`, string(b), id); err != nil {
		log.Printf("notify: resolve: %v", err)
		return
	}
	n.publish(no)
	if n.bark {
		barkDelete(no.ID)
	}
}

// get reads one stored notification by id.
func (n *Notifier) get(id string) (ONotification, bool) {
	var raw string
	var read int
	if err := n.db.QueryRow(`SELECT json, read FROM notifications WHERE id=?`, id).Scan(&raw, &read); err != nil {
		return ONotification{}, false
	}
	var no ONotification
	if json.Unmarshal([]byte(raw), &no) != nil {
		return ONotification{}, false
	}
	no.ID, no.Read = id, no.Read || read == 1
	return no, true
}

// MarkRead also tells the other devices, and takes the alert off the lock screen.
func (n *Notifier) MarkRead(ids []string, all bool) error {
	if all {
		ids = nil
		rows, err := n.db.Query(`SELECT id FROM notifications WHERE read=0 ORDER BY id DESC LIMIT 500`)
		if err != nil {
			return err
		}
		for rows.Next() {
			var id int64
			if rows.Scan(&id) == nil {
				ids = append(ids, strconv.FormatInt(id, 10))
			}
		}
		rows.Close()
		if _, err := n.db.Exec(`UPDATE notifications SET read=1 WHERE read=0`); err != nil {
			return err
		}
	}
	for _, s := range ids {
		id, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			continue
		}
		if !all {
			if _, err := n.db.Exec(`UPDATE notifications SET read=1 WHERE id=?`, id); err != nil {
				return err
			}
		}
		if no, ok := n.get(s); ok {
			n.publish(no)
		}
		if n.bark {
			barkDelete(s)
		}
	}
	return nil
}

// ── Web Push subscriptions ──

const pushSubsMax = 50

func (n *Notifier) addSub(s pushSub) error { return n.addSubFor(s, "") }

// addSubFor stores a subscription owned by a device ("" for the Mac or a password session).
func (n *Notifier) addSubFor(s pushSub, device string) error {
	if err := s.validate(); err != nil {
		return err
	}
	var n0, exists int
	_ = n.db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(endpoint=?),0) FROM push_subs`, s.Endpoint).Scan(&n0, &exists)
	if exists == 0 && n0 >= pushSubsMax {
		return fmt.Errorf("at most %d push subscriptions", pushSubsMax)
	}
	b, _ := json.Marshal(s)
	_, err := n.db.Exec(`INSERT INTO push_subs(endpoint, json, created_at, device) VALUES(?,?,?,?) ON CONFLICT(endpoint) DO UPDATE SET json=excluded.json, device=excluded.device`,
		s.Endpoint, string(b), n.now().UnixMilli(), device)
	return err
}

func (n *Notifier) removeDeviceSubs(device string) error {
	if device == "" {
		return nil
	}
	_, err := n.db.Exec(`DELETE FROM push_subs WHERE device=?`, device)
	return err
}

func (n *Notifier) removeSub(endpoint string) error {
	_, err := n.db.Exec(`DELETE FROM push_subs WHERE endpoint=?`, endpoint)
	return err
}

func (n *Notifier) pushSubs() []pushSub {
	rows, err := n.db.Query(`SELECT json FROM push_subs`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []pushSub
	for rows.Next() {
		var js string
		var s pushSub
		if rows.Scan(&js) == nil && json.Unmarshal([]byte(js), &s) == nil {
			out = append(out, s)
		}
	}
	return out
}

// pushAll sends one notification to every subscription and drops the ones the push service says are gone.
func (n *Notifier) pushAll(no ONotification) {
	payload, _ := json.Marshal(map[string]any{"id": no.ID, "kind": no.Kind, "title": no.Title, "body": clipString(no.Body, 600),
		"threadId": no.ThreadID, "url": no.URL})
	urgency := "normal"
	if no.Kind == "needs-you" || no.Kind == "error" {
		urgency = "high"
	}
	for _, s := range n.pushSubs() {
		code, err := n.push.send(s, payload, urgency)
		if code == http.StatusNotFound || code == http.StatusGone {
			_ = n.removeSub(s.Endpoint)
			continue
		}
		if err != nil {
			u := s.Endpoint
			if i := strings.Index(u, "://"); i >= 0 {
				if j := strings.Index(u[i+3:], "/"); j >= 0 {
					u = u[:i+3+j]
				}
			}
			log.Printf("notify: push to %s: %v", u, err)
		}
	}
}

// ── HTTP ──

var notifier *Notifier

func notifySettingsNow() NotifySettings { return getSettings().Notify }

// registerNotifyRoutes wires /api/notify/*; it runs after registerOrchRoutes so driven events reach it.
func registerNotifyRoutes(mux *http.ServeMux) {
	n, err := openNotifier(stateDir(), notifySettingsNow)
	if err != nil {
		log.Printf("notify: disabled: %v", err)
		return
	}
	n.bark = true
	notifier = n
	if orchMgr != nil {
		orchMgr.notifier = n.Emit
	}
	n.routes(mux)
}

var notifyHeartbeat = 15 * time.Second

func (n *Notifier) routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/notify", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		after, _ := strconv.ParseInt(q.Get("after"), 10, 64)
		limit := clampInt(qInt(q.Get("limit"), 100), 1, 500)
		items, unread, err := n.List(after, limit)
		if err != nil {
			writeJSONStatus(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, map[string]any{"items": items, "unread": unread})
	})
	mux.HandleFunc("POST /api/notify/read", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			IDs []string `json:"ids"`
			All bool     `json:"all"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&in); err != nil {
			writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": "bad json"})
			return
		}
		if err := n.MarkRead(in.IDs, in.All); err != nil {
			writeJSONStatus(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, map[string]any{"ok": true, "unread": n.unread()})
	})
	mux.HandleFunc("GET /api/notify/stream", n.serveStream)
	mux.HandleFunc("GET /api/notify/vapid", func(w http.ResponseWriter, r *http.Request) {
		if n.push == nil {
			writeJSONStatus(w, http.StatusServiceUnavailable, map[string]any{"error": "web push is not available"})
			return
		}
		writeJSON(w, map[string]any{"publicKey": n.push.keys.public})
	})
	mux.HandleFunc("POST /api/notify/subscribe", func(w http.ResponseWriter, r *http.Request) {
		var s pushSub
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&s); err != nil {
			writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": "bad json"})
			return
		}
		owner := ""
		if d := requestDevice(r); d != nil {
			owner = d.ID
		}
		if err := n.addSubFor(s, owner); err != nil {
			writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, map[string]any{"ok": true})
	})
	mux.HandleFunc("POST /api/notify/unsubscribe", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Endpoint string `json:"endpoint"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&in); err != nil || in.Endpoint == "" {
			writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": "endpoint is required"})
			return
		}
		_ = n.removeSub(in.Endpoint)
		writeJSON(w, map[string]any{"ok": true})
	})
	// ios/resign.sh posts here when a new iPhone build waits on the install page; only this Mac may call it.
	mux.HandleFunc("POST /api/notify/install", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Build   string `json:"build"`
			URL     string `json:"url"`
			Waiting bool   `json:"waiting"`
			Renew   bool   `json:"renew"`
			Prompts string `json:"prompts"` // bin/prompt-check.sh: the Claude Code version it checked
			Failed  string `json:"failed"`
			NoLogin bool   `json:"nologin"`
		}
		if !localCaller(r) || json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&in) != nil || (!in.Waiting && in.Prompts == "" && !strings.HasPrefix(in.URL, "https://")) {
			writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": "local callers only, with an https url or waiting"})
			return
		}
		if in.Prompts != "" {
			ev := notifyEvent{Kind: "finished", Title: "Claude Code " + clipString(in.Prompts, 24) + ": prompts still work",
				Body: "The check on the build Mac read its prompt screens, and the app can see and answer them."}
			if in.NoLogin {
				ev.Kind, ev.Title = "needs-you", "Log in to Claude Code on the build Mac"
				ev.Body = "The nightly prompt check cannot run while Claude Code there is logged out. Run claude, then /login."
			} else if in.Failed != "" {
				ev.Kind, ev.Title = "error", "Claude Code "+clipString(in.Prompts, 24)+" changed its prompt screens"
				ev.Body = clipString(in.Failed, 240) + ". The app may not see or answer its prompts until agent-monitor is updated."
			}
			if ev.Kind == "finished" {
				n.Emit(ev) // "still works" follows the Finished setting; a failure always shows
			} else {
				n.deliver(ev)
			}
			writeJSON(w, map[string]any{"ok": true})
			return
		}
		if in.Waiting { // the build Mac cannot reach the phone: it installs on the next run with the phone near
			ev := notifyEvent{Kind: "finished", Title: "Agent Monitor " + clipString(in.Build, 12) + " waits for your iPhone",
				Body: "Connect the iPhone to the build Mac's Wi-Fi or USB. The build Mac installs the update within 3 hours."}
			if in.Renew {
				ev.Kind, ev.Title = "needs-you", "Agent Monitor needs a new build to keep opening"
			}
			n.deliver(ev)
			writeJSON(w, map[string]any{"ok": true})
			return
		}
		n.deliver(notifyEvent{Kind: "finished", Title: "Agent Monitor " + clipString(in.Build, 12) + " is ready", Body: "Tap to open the install page, then tap Install.", URL: in.URL})
		writeJSON(w, map[string]any{"ok": true})
	})
	mux.HandleFunc("POST /api/notify/test", func(w http.ResponseWriter, r *http.Request) {
		n.deliver(notifyEvent{Kind: "finished", Title: "Test notification", Body: "Notifications from agent-monitor reach this device."})
		writeJSON(w, map[string]any{"ok": true})
	})
}

// serveStream sends one `data: ONotification` per notification (id: its id), replaying ids > after / Last-Event-ID.
func (n *Notifier) serveStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	if v := r.Header.Get("Last-Event-ID"); v != "" {
		if id, err := strconv.ParseInt(v, 10, 64); err == nil && id > after {
			after = id
		}
	}
	live, cancel := n.subscribe()
	defer cancel()
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, ": notify stream\n\n")
	send := func(no ONotification) error {
		b, _ := json.Marshal(no)
		if no.Resolved {
			// No id line, so a resolution never rewinds the client's Last-Event-ID.
			_, err := fmt.Fprintf(w, "data: %s\n\n", b)
			return err
		}
		_, err := fmt.Fprintf(w, "id: %s\ndata: %s\n\n", no.ID, b)
		return err
	}
	var last int64
	if after > 0 {
		for _, no := range n.since(after) {
			if send(no) != nil {
				return
			}
			last, _ = strconv.ParseInt(no.ID, 10, 64)
		}
	}
	flusher.Flush()
	tick := time.NewTicker(notifyHeartbeat)
	defer tick.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case no, ok := <-live:
			if !ok {
				return
			}
			if id, _ := strconv.ParseInt(no.ID, 10, 64); id <= last && !no.Resolved {
				continue
			}
			if send(no) != nil {
				return
			}
			flusher.Flush()
		case <-tick.C:
			if _, err := fmt.Fprint(w, ": hb\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

var errNotifyClock = errors.New("quietStart and quietEnd must be HH:MM (24 h) or empty")

// validNotifySettings checks the fields a POST /api/settings may set.
func validNotifySettings(s NotifySettings) error {
	for _, v := range []string{s.QuietStart, s.QuietEnd} {
		if _, ok := parseClock(v); v != "" && !ok {
			return errNotifyClock
		}
	}
	if s.BatchSec != nil && (*s.BatchSec < 0 || *s.BatchSec > 600) {
		return errors.New("batchSec must be 0 to 600")
	}
	if s.RemindMin != nil && (*s.RemindMin < 0 || *s.RemindMin > 240) {
		return errors.New("remindMin must be 0 to 240")
	}
	return nil
}

// ── orchestrator events ──

func (m *OrchManager) notifyNeedsYou(id string, a *Approval) {
	if m.notifier == nil {
		return
	}
	th, err := m.store.Thread(id)
	if err != nil {
		return
	}
	var title string
	switch a.Kind {
	case "question":
		title = th.Title + ": a question for you"
	case "plan":
		title = th.Title + ": a plan to review"
	default:
		title = th.Title + ": " + firstOf(a.ToolName, "a tool") + " needs approval"
	}
	m.notify(notifyEvent{Kind: "needs-you", ThreadID: id, Title: title, Body: clipString(a.Summary, 300), Keys: []string{a.ID}})
}

// notifyRunEnd reports an error, a finished root run, or a fan-out group whose last child just ended (once).
func (m *OrchManager) notifyRunEnd(id string) {
	if m.notifier == nil {
		return
	}
	th, err := m.store.Thread(id)
	if err != nil {
		return
	}
	var res orchTaskResult
	var last *ORun
	_ = m.store.read(func(tx *sql.Tx) error {
		res = taskResultTx(tx, th)
		last, _ = getJSON[ORun](tx, `SELECT json FROM runs WHERE thread_id=? ORDER BY idx DESC LIMIT 1`, id)
		return nil
	})
	failed := th.Status == "error" || (last != nil && last.Status == "failed")
	switch {
	case failed:
		m.notify(notifyEvent{Kind: "error", ThreadID: id, Title: th.Title + " stopped with an error", Body: clipString(firstOf(th.Error, res.Result), 300)})
	case th.ParentID == "" && last != nil && last.Status == "completed" && !m.anyActive(id):
		m.notify(notifyEvent{Kind: "finished", ThreadID: id, Title: th.Title + " finished", Body: clipString(firstLine(strings.TrimSpace(res.Result)), 300)})
	}
	if th.ParentID != "" && th.ReviewOf == "" {
		m.notifyGroup(th.ParentID)
	}
}

func (m *OrchManager) anyActive(id string) bool {
	desc, _ := m.store.Subtree(id)
	for _, d := range desc {
		if activeStatus(d.Status) {
			return true
		}
	}
	return false
}

// notifyGroup sends one "finished" notification when no child of parentID runs any more.
func (m *OrchManager) notifyGroup(parentID string) {
	kids, err := m.store.Children(parentID)
	if err != nil || len(kids) == 0 {
		return
	}
	var newest int64
	counts := map[string]int{}
	for _, k := range kids {
		if activeStatus(k.Status) {
			return
		}
		newest = max(newest, k.UpdatedAt)
		st := k.Status
		if r := m.taskResults([]OThread{k}); len(r) == 1 {
			st = r[0].Status
		}
		counts[st]++
	}
	m.nmu.Lock()
	if m.groupNotified == nil {
		m.groupNotified = map[string]int64{}
	}
	if m.groupNotified[parentID] >= newest {
		m.nmu.Unlock()
		return
	}
	m.groupNotified[parentID] = newest
	m.nmu.Unlock()
	parent, err := m.store.Thread(parentID)
	if err != nil {
		return
	}
	var parts []string
	for _, st := range []string{"completed", "failed", "interrupted", "error", "closed", "idle"} {
		if counts[st] > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", counts[st], st))
		}
	}
	noun := "subagents"
	if len(kids) == 1 {
		noun = "subagent"
	}
	m.notify(notifyEvent{Kind: "finished", ThreadID: parentID, Count: len(kids),
		Title: fmt.Sprintf("%d %s finished", len(kids), noun), Body: parent.Title + ": " + strings.Join(parts, ", ")})
}

// notifyWatchedApproval puts a terminal session's approval or question into Alerts.
func notifyWatchedApproval(a *Approval) {
	if notifier == nil || a == nil || a.SessionID == "" {
		return
	}
	id := a.SessionID
	name := filepath.Base(a.Cwd)
	if store != nil {
		if s := store.Get(id); s != nil && s.Title != "" {
			name = s.Title
		}
	}
	var title string
	switch a.Kind {
	case "question":
		title = name + ": a question for you"
	case "plan":
		title = name + ": a plan to review"
	default:
		title = name + ": " + firstOf(a.ToolName, "a tool") + " needs approval"
	}
	notifier.Emit(notifyEvent{Kind: "needs-you", ThreadID: id, Title: title, Body: clipString(a.Summary, 300), Keys: []string{a.ID}})
}
