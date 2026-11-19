package main

import (
	"context"
	"strings"
	"sync"
	"time"

	"crypto/rand"
	"encoding/hex"
)

// PermissionRequest is a pending tool-use approval that originated from a
// Claude Code MCP `permission_prompt` call. The MCP child process registers
// the request and long-polls until the UI returns a decision.
type PermissionRequest struct {
	ID        string         `json:"id"`
	ToolName  string         `json:"toolName"`
	Input     map[string]any `json:"input"`
	ToolUseID string         `json:"toolUseId,omitempty"`
	Cwd       string         `json:"cwd,omitempty"`
	SessionID string         `json:"sessionId,omitempty"`
	CreatedAt int64          `json:"createdAt"`
	// Risk and RiskReason use the same rule as hook approvals, so every surface confirms a high-risk allow.
	Risk       string `json:"risk,omitempty"`
	RiskReason string `json:"riskReason,omitempty"`

	// response is delivered when the UI calls /api/permission/:id/respond.
	// Buffered so the responder doesn't block if the waiter has timed out.
	response chan PermissionResponse `json:"-"`
}

type PermissionResponse struct {
	Behavior     string         `json:"behavior"`               // "allow" | "deny"
	UpdatedInput map[string]any `json:"updatedInput,omitempty"` // optional override
	Reason       string         `json:"reason,omitempty"`
}

type PermStore struct {
	mu        sync.Mutex
	requests  map[string]*PermissionRequest
	listeners map[int]func(PermEvent)
	nextID    int
}

type PermEvent struct {
	Kind    string             `json:"kind"` // "perm-add" | "perm-remove"
	Request *PermissionRequest `json:"request,omitempty"`
	ID      string             `json:"id,omitempty"`
}

func NewPermStore() *PermStore {
	return &PermStore{
		requests:  map[string]*PermissionRequest{},
		listeners: map[int]func(PermEvent){},
	}
}

func (p *PermStore) Subscribe(l func(PermEvent)) func() {
	p.mu.Lock()
	id := p.nextID
	p.nextID++
	p.listeners[id] = l
	// Replay current pending requests so newly-connected clients see them.
	pending := make([]*PermissionRequest, 0, len(p.requests))
	for _, r := range p.requests {
		pending = append(pending, r)
	}
	p.mu.Unlock()
	for _, r := range pending {
		l(PermEvent{Kind: "perm-add", Request: r})
	}
	return func() {
		p.mu.Lock()
		delete(p.listeners, id)
		p.mu.Unlock()
	}
}

func (p *PermStore) broadcast(e PermEvent) {
	p.mu.Lock()
	ls := make([]func(PermEvent), 0, len(p.listeners))
	for _, l := range p.listeners {
		ls = append(ls, l)
	}
	p.mu.Unlock()
	for _, l := range ls {
		func() { defer func() { _ = recover() }(); l(e) }()
	}
}

// Register stores a new pending request and broadcasts it. The caller awaits
// Wait(id) — typically the MCP child process.
func (p *PermStore) Register(req *PermissionRequest) {
	if req.ID == "" {
		req.ID = randHex(8)
	}
	if req.CreatedAt == 0 {
		req.CreatedAt = time.Now().UnixMilli()
	}
	if req.response == nil {
		req.response = make(chan PermissionResponse, 1)
	}
	if req.Risk == "" {
		req.Risk, req.RiskReason = shownRisk("permission", req.ToolName, strings.Join(strings.Fields(approvalText("permission", req.ToolName, req.Input)), " "))
	}
	p.mu.Lock()
	p.requests[req.ID] = req
	p.mu.Unlock()
	p.broadcast(PermEvent{Kind: "perm-add", Request: req})
}

func (p *PermStore) List() []*PermissionRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]*PermissionRequest, 0, len(p.requests))
	for _, r := range p.requests {
		out = append(out, r)
	}
	return out
}

func (p *PermStore) Get(id string) *PermissionRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.requests[id]
}

// Respond delivers the user's decision and removes the request from the
// pending set. Returns false if the request has already been answered (e.g.
// Claude timed out on its end).
func (p *PermStore) Respond(id string, resp PermissionResponse) bool {
	p.mu.Lock()
	req, ok := p.requests[id]
	if !ok {
		p.mu.Unlock()
		return false
	}
	delete(p.requests, id)
	p.mu.Unlock()
	select {
	case req.response <- resp:
	default:
		// channel full or closed — request abandoned
	}
	p.broadcast(PermEvent{Kind: "perm-remove", ID: id})
	return true
}

// RegisterDecided stores a request a rule already answered: no card, and the next Wait returns the answer.
func (p *PermStore) RegisterDecided(req *PermissionRequest, resp PermissionResponse) {
	if req.ID == "" {
		req.ID = randHex(8)
	}
	req.response = make(chan PermissionResponse, 1)
	req.response <- resp
	p.mu.Lock()
	p.requests[req.ID] = req
	p.mu.Unlock()
	// A helper that ends before it waits would leave the answer here for ever.
	time.AfterFunc(time.Minute, func() {
		p.mu.Lock()
		if p.requests[req.ID] == req {
			delete(p.requests, req.ID)
		}
		p.mu.Unlock()
	})
}

// Wait blocks until a decision, the timeout, or the caller going away (ctx); the request then leaves the list.
func (p *PermStore) Wait(ctx context.Context, id string, timeout time.Duration) (PermissionResponse, bool) {
	p.mu.Lock()
	req, ok := p.requests[id]
	p.mu.Unlock()
	if !ok {
		return PermissionResponse{}, false
	}
	drop := func() {
		p.mu.Lock()
		_, still := p.requests[id]
		delete(p.requests, id)
		p.mu.Unlock()
		if still {
			p.broadcast(PermEvent{Kind: "perm-remove", ID: id})
		}
	}
	select {
	case resp := <-req.response:
		drop()
		return resp, true
	case <-ctx.Done():
		drop() // the MCP helper ended: nobody would read an answer
		return PermissionResponse{Behavior: "deny", Reason: "caller gone"}, true
	case <-time.After(timeout):
		drop() // a deny on timeout, so Claude does not hang
		return PermissionResponse{Behavior: "deny", Reason: "timeout"}, true
	}
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
