package main

import (
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// NeedsYouItem is one thing that waits for the user; every surface draws this list and nothing else.
type NeedsYouItem struct {
	// Kind: "approval" (a hook request agent-monitor holds), "mcp" (an MCP permission prompt),
	// "question" (a question or plan form open in a terminal), "prompt" (a terminal permission prompt with no hook request),
	// "error" (a driven thread that stopped with an error; Dismiss settles it).
	Kind       string             `json:"kind"`
	Key        string             `json:"key"`
	SessionID  string             `json:"sessionId,omitempty"`
	ThreadID   string             `json:"threadId,omitempty"` // a driven thread, for "error" items
	Agent      string             `json:"agent,omitempty"`
	Cwd        string             `json:"cwd,omitempty"`
	Title      string             `json:"title,omitempty"`
	At         int64              `json:"at"`
	Answerable bool               `json:"answerable"`
	Why        string             `json:"why,omitempty"`
	Approval   *Approval          `json:"approval,omitempty"`
	MCP        *PermissionRequest `json:"mcp,omitempty"`
	Question   *TermQuestion      `json:"question,omitempty"`
	Message    string             `json:"message,omitempty"`   // a "prompt" item's text from the agent
	PromptKey  string             `json:"promptKey,omitempty"` // the open prompt's fingerprint; POST /api/session/prompt/{id} checks it
}

// needsYou builds the list: held approvals, MCP prompts, then waiting sessions with an open form or a terminal prompt.
// A session that only finished its turn is idle, not a request.
func needsYou() []NeedsYouItem {
	out := []NeedsYouItem{}
	held := map[string]bool{}
	if approvalStore != nil {
		for _, a := range approvalStore.list()["pending"].([]*Approval) {
			held[a.SessionID] = true
			out = append(out, NeedsYouItem{Kind: "approval", Key: a.ID, SessionID: a.SessionID, Agent: a.Agent, Cwd: a.Cwd,
				Title: sessionTitle(a.SessionID), At: a.CreatedAt, Answerable: true, Approval: a})
		}
	}
	if permStore != nil {
		for _, p := range permStore.List() {
			out = append(out, NeedsYouItem{Kind: "mcp", Key: "perm:" + p.ID, SessionID: p.SessionID, Cwd: p.Cwd,
				Title: sessionTitle(p.SessionID), At: p.CreatedAt, Answerable: true, MCP: p})
		}
	}
	if store != nil {
		for _, s := range waitingSessions(store.All()) {
			if held[s.ID] || held[s.SessionID] {
				continue
			}
			if it, ok := needsYouSession(s); ok {
				out = append(out, it)
			}
		}
	}
	if orchMgr != nil && orchMgr.store != nil {
		if ths, _, err := orchMgr.store.Threads(); err == nil {
			out = append(out, needsYouErrors(ths)...)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At < out[j].At })
	return out
}

func needsYouSession(s *Session) (NeedsYouItem, bool) {
	base := NeedsYouItem{SessionID: s.ID, Agent: string(s.Tool), Cwd: s.Cwd, Title: firstOf(s.Title, firstLine(s.FirstMessage)), At: s.LastActivityAt}
	if s.Tool == ToolClaude {
		if q := cachedTermQuestion(s); q != nil && q.Pending && !q.Held && q.SentAt == 0 && (len(q.Questions) > 0 || q.Tool == "ExitPlanMode") {
			it := base
			it.Kind, it.Key, it.Question = "question", "tq:"+s.ID+":"+q.ToolUseID, q
			it.Answerable, it.Why = q.OnScreen && q.Sendable, q.Why
			if q.AskedAt > 0 {
				it.At = q.AskedAt
			}
			return it, true
		}
	}
	if s.State != StateAwaitingPermission {
		return NeedsYouItem{}, false // the turn ended: idle, not a request
	}
	it := base
	it.Kind, it.Key, it.Message = "prompt", "tp:"+s.ID, firstOf(s.PermissionMessage, s.LastMessage)
	pane := watchedPane(s.ID)
	it.Answerable = pane.Sendable
	if !it.Answerable {
		it.Why = "agent-monitor cannot reach this terminal. Answer it in the terminal on the Mac, or dismiss this card if it is already answered."
	} else if scr, err := targetCapture(pane.Target(), 0, true); err == nil {
		it.PromptKey = promptKey(scr)
	}
	if it.Answerable && it.PromptKey == "" {
		it.Answerable, it.Why = false, "agent-monitor cannot read this prompt; answer it in the terminal"
	}
	return it, true
}

// needsYouErrors lists driven threads the server groups as "need" because they failed.
func needsYouErrors(ths []OThread) []NeedsYouItem {
	var out []NeedsYouItem
	for _, th := range ths {
		if th.Status != "error" || th.Group != "need" {
			continue
		}
		out = append(out, NeedsYouItem{Kind: "error", Key: "err:" + th.ID, ThreadID: th.ID, Agent: th.Agent, Cwd: th.Cwd,
			Title: th.Title, At: th.UpdatedAt, Answerable: true, Message: firstOf(strings.TrimSpace(reANSI.ReplaceAllString(th.Error, "")), "the agent stopped with an error")})
	}
	return out
}

func sessionTitle(id string) string {
	if store == nil || id == "" {
		return ""
	}
	if s := store.Get(id); s != nil {
		return firstOf(s.Title, firstLine(s.FirstMessage))
	}
	return ""
}

// The transcript read behind a terminal question is cached until the file changes; the pane is read each time.
var tqCache = struct {
	mu sync.Mutex
	m  map[string]tqCached
}{m: map[string]tqCached{}}

type tqCached struct {
	stamp  string
	absent bool
	q      *TermQuestion // an open question, kept for tqFresh so several clients share one read
	at     time.Time
}

const tqFresh = time.Second

func cachedTermQuestion(s *Session) *TermQuestion {
	path := s.TranscriptPath
	if path == "" {
		return nil
	}
	st, err := os.Stat(path)
	if err != nil {
		return nil
	}
	stamp := fmt.Sprintf("%d/%d", st.ModTime().UnixNano(), st.Size())
	tqCache.mu.Lock()
	c, ok := tqCache.m[path]
	tqCache.mu.Unlock()
	if ok && c.stamp == stamp && c.absent {
		return nil // nothing open in this transcript, and it has not changed
	}
	if ok && c.stamp == stamp && c.q != nil && time.Since(c.at) < tqFresh {
		return c.q
	}
	q, _, _, err := termQuestionFor(s.ID)
	if err != nil {
		return nil
	}
	tqCache.mu.Lock()
	if len(tqCache.m) > 256 {
		tqCache.m = map[string]tqCached{}
	}
	c = tqCached{stamp: stamp, absent: q == nil || !q.Pending}
	if !c.absent {
		c.q, c.at = q, time.Now()
	}
	tqCache.m[path] = c
	tqCache.mu.Unlock()
	return q
}

// GET /api/needs-you: {items, count}.
func registerNeedsYouRoutes(mux *http.ServeMux) {
	registerPromptAnswerRoute(mux)
	registerSessionLifecycleRoutes(mux)
	mux.HandleFunc("GET /api/needs-you", func(w http.ResponseWriter, r *http.Request) {
		items := needsYou()
		writeJSON(w, map[string]any{"items": items, "count": len(items)})
	})
}
