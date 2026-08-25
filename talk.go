package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Talk is one agent-to-agent message proposal. Lifecycle:
//
//	pending  → recipient's user clicks Allow → delivered (text written into pane)
//	pending  → recipient's user clicks Deny  → denied (no pane write, sender notified)
//	pending  → no decision within timeout    → timed-out (treated as deny)
//
// Each Talk gets a randhex id; sender long-polls /api/talk/{id}/await for the
// outcome so a CLI `agent-monitor send` can block until the user decides.
type Talk struct {
	ID         string `json:"id"`
	FromAgent  string `json:"fromAgent"` // sender agent id (or "" for web user)
	FromLabel  string `json:"fromLabel"` // alias-or-id used in the pane prefix
	ToAgent    string `json:"toAgent"`   // recipient agent id
	ToLabel    string `json:"toLabel,omitempty"`
	Message    string `json:"message"`
	Status     string `json:"status"`           // "pending"|"pending_reply"|"delivered"|"replied"|"denied"|"timeout"|"error"
	Reason     string `json:"reason,omitempty"` // populated on deny/error/timeout
	Reply      string `json:"reply,omitempty"`  // recipient's response (when wait_for_reply was set)
	CreatedAt  int64  `json:"createdAt"`
	ResolvedAt int64  `json:"resolvedAt,omitempty"`
	RepliedAt  int64  `json:"repliedAt,omitempty"`

	resp  chan talkResponse `json:"-"`
	reply chan talkReplyEnv `json:"-"`
}

type talkResponse struct {
	Status string
	Reason string
}
type talkReplyEnv struct {
	Message string
}

type TalkStore struct {
	mu        sync.Mutex
	pending   map[string]*Talk
	listeners map[int]func(TalkEvent)
	nextLid   int
	logPath   string // ~/.agent-monitor/talks.log (append-only, NDJSON)
}

type TalkEvent struct {
	Kind string `json:"kind"` // "talk-request" | "talk-resolved"
	Talk *Talk  `json:"talk"`
}

func NewTalkStore() *TalkStore {
	dir := filepath.Join(homeDir(), ".agent-monitor")
	_ = os.MkdirAll(dir, 0o755)
	return &TalkStore{
		pending:   map[string]*Talk{},
		listeners: map[int]func(TalkEvent){},
		logPath:   filepath.Join(dir, "talks.log"),
	}
}

func (ts *TalkStore) Subscribe(l func(TalkEvent)) func() {
	ts.mu.Lock()
	id := ts.nextLid
	ts.nextLid++
	ts.listeners[id] = l
	// Replay pending (not yet resolved) talks so a freshly-connected client
	// sees outstanding incoming requests.
	pending := make([]*Talk, 0, len(ts.pending))
	for _, t := range ts.pending {
		pending = append(pending, t)
	}
	ts.mu.Unlock()
	for _, t := range pending {
		l(TalkEvent{Kind: "talk-request", Talk: t})
	}
	return func() {
		ts.mu.Lock()
		delete(ts.listeners, id)
		ts.mu.Unlock()
	}
}

func (ts *TalkStore) broadcast(e TalkEvent) {
	ts.mu.Lock()
	ls := make([]func(TalkEvent), 0, len(ts.listeners))
	for _, l := range ts.listeners {
		ls = append(ls, l)
	}
	ts.mu.Unlock()
	for _, l := range ls {
		func() { defer func() { _ = recover() }(); l(e) }()
	}
}

// Register stores a new pending talk and broadcasts. Returns the assigned id.
// Talks with Status="pending_reply" skip the talk-request broadcast: the message
// has already been delivered directly to the recipient's pane, so showing an
// Allow/Deny banner would let the user mistakenly re-deliver and prematurely
// resolve the talk before the recipient calls reply_to_talk.
func (ts *TalkStore) Register(t *Talk) string {
	if t.ID == "" {
		t.ID = randHex(6)
	}
	if t.CreatedAt == 0 {
		t.CreatedAt = time.Now().UnixMilli()
	}
	if t.Status == "" {
		t.Status = "pending"
	}
	if t.resp == nil {
		t.resp = make(chan talkResponse, 1)
	}
	if t.reply == nil {
		t.reply = make(chan talkReplyEnv, 1)
	}
	ts.mu.Lock()
	ts.pending[t.ID] = t
	ts.mu.Unlock()
	ts.appendLog(t)
	if t.Status != "pending_reply" {
		ts.broadcast(TalkEvent{Kind: "talk-request", Talk: t})
	}
	return t.ID
}

// AwaitReply blocks until the recipient calls Reply(), or the timeout fires.
// On timeout the talk's status is moved to "timeout" so subsequent UI listing
// doesn't show it as still pending. Caller-supplied Talk must already exist
// in the pending map.
func (ts *TalkStore) AwaitReply(id string, timeout time.Duration) (talkReplyEnv, bool) {
	ts.mu.Lock()
	t, ok := ts.pending[id]
	ts.mu.Unlock()
	if !ok || t.reply == nil {
		return talkReplyEnv{}, false
	}
	select {
	case r := <-t.reply:
		return r, true
	case <-time.After(timeout):
		ts.mu.Lock()
		if cur, ok := ts.pending[id]; ok && cur.Status != "replied" {
			cur.Status = "timeout"
			cur.ResolvedAt = time.Now().UnixMilli()
			delete(ts.pending, id)
			ts.appendLog(cur)
		}
		ts.mu.Unlock()
		ts.broadcast(TalkEvent{Kind: "talk-resolved", Talk: t})
		return talkReplyEnv{}, false
	}
}

// Reply records the recipient's response to a pending-reply talk and fires
// the channel so any AwaitReply caller unblocks. Returns the talk so the
// HTTP handler can echo it back to the recipient.
func (ts *TalkStore) Reply(id, message string) (*Talk, error) {
	ts.mu.Lock()
	t, ok := ts.pending[id]
	if !ok {
		ts.mu.Unlock()
		return nil, errors.New("talk not found or already resolved")
	}
	t.Status = "replied"
	t.Reply = message
	t.RepliedAt = time.Now().UnixMilli()
	t.ResolvedAt = t.RepliedAt
	delete(ts.pending, id)
	ts.mu.Unlock()
	select {
	case t.reply <- talkReplyEnv{Message: message}:
	default:
	}
	ts.appendLog(t)
	ts.broadcast(TalkEvent{Kind: "talk-resolved", Talk: t})
	return t, nil
}

// Get returns the talk by id (or nil).
func (ts *TalkStore) Get(id string) *Talk {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	return ts.pending[id]
}

// List returns currently-pending talks (not those already resolved).
func (ts *TalkStore) List() []*Talk {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	out := make([]*Talk, 0, len(ts.pending))
	for _, t := range ts.pending {
		out = append(out, t)
	}
	return out
}

// Respond finalizes a talk. Returns the resolved Talk so the caller can
// perform the actual delivery (the talk store itself doesn't speak tmux).
// Refuses to resolve pending_reply talks — those are awaiting a reply, not an
// allow/deny verdict, so a respond call would corrupt the reply flow.
func (ts *TalkStore) Respond(id, status, reason string) (*Talk, error) {
	ts.mu.Lock()
	t, ok := ts.pending[id]
	if !ok {
		ts.mu.Unlock()
		return nil, errors.New("not found or already answered")
	}
	if t.Status == "pending_reply" {
		ts.mu.Unlock()
		return nil, errors.New("talk is awaiting a reply, not allow/deny — use /reply instead")
	}
	t.Status = status
	t.Reason = reason
	t.ResolvedAt = time.Now().UnixMilli()
	delete(ts.pending, id)
	ts.mu.Unlock()
	select {
	case t.resp <- talkResponse{Status: status, Reason: reason}:
	default:
	}
	ts.appendLog(t)
	ts.broadcast(TalkEvent{Kind: "talk-resolved", Talk: t})
	return t, nil
}

// Wait blocks until Respond fires or timeout — whichever comes first. On
// timeout the talk is auto-denied so the sender unblocks.
func (ts *TalkStore) Wait(id string, timeout time.Duration) (talkResponse, bool) {
	ts.mu.Lock()
	t, ok := ts.pending[id]
	ts.mu.Unlock()
	if !ok {
		return talkResponse{}, false
	}
	select {
	case r := <-t.resp:
		return r, true
	case <-time.After(timeout):
		_, _ = ts.Respond(id, "timeout", fmt.Sprintf("no response within %s", timeout))
		return talkResponse{Status: "timeout"}, true
	}
}

// appendLog writes a single JSON line to ~/.agent-monitor/talks.log so the
// user can audit talk history. Best-effort — failures are silently dropped.
func (ts *TalkStore) appendLog(t *Talk) {
	f, err := os.OpenFile(ts.logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	body, err := json.Marshal(t)
	if err != nil {
		return
	}
	_, _ = f.Write(append(body, '\n'))
}

// FormatPaneDelivery wraps a message in a fenced block prefixed with the
// sender's label. The recipient's CLI reads it as part of its prompt buffer;
// the fence makes the boundary visually obvious in tmux. Always followed by
// a single Enter keystroke (sent separately by the caller).
func FormatPaneDelivery(fromLabel, message string) string {
	from := fromLabel
	if from == "" {
		from = "(unknown)"
	}
	// Trim trailing whitespace/newlines so the fence sits flush.
	msg := strings.TrimRight(message, "\r\n\t ")
	return fmt.Sprintf("[from %s — agent-monitor talk]\n```\n%s\n```\n", from, msg)
}
