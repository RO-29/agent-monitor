package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestNeedsYouList(t *testing.T) {
	prevStore, prevAppr, prevPerm := store, approvalStore, permStore
	t.Cleanup(func() { store, approvalStore, permStore = prevStore, prevAppr, prevPerm })
	store, permStore = NewStore(), NewPermStore()
	approvalStore = newApprovalStore(t.TempDir())
	t.Cleanup(func() {
		if approvalStore.db != nil {
			approvalStore.db.Close()
		}
	})
	store.Upsert(UpsertInput{Tool: ToolClaude, SessionID: "idle", State: StateAwaitingInput, HasState: true})
	store.Upsert(UpsertInput{Tool: ToolCodex, SessionID: "ask", State: StateAwaitingPermission, HasState: true, PermissionMessage: "Allow rm?", HasPerm: true})
	store.Upsert(UpsertInput{Tool: ToolClaude, SessionID: "held", State: StateAwaitingPermission, HasState: true})
	a := &Approval{ID: "ap1", SessionID: "claude:held", Agent: "claude", State: "pending", CreatedAt: 1}
	approvalStore.mu.Lock()
	approvalStore.pending[a.ID] = &pendingApproval{a: a, reply: make(chan approvalDecision, 1)}
	approvalStore.mu.Unlock()

	items := needsYou()
	kinds := map[string]string{}
	for _, it := range items {
		kinds[it.Key] = it.Kind
	}
	if kinds["ap1"] != "approval" || kinds["tp:codex:ask"] != "prompt" || len(items) != 2 {
		t.Fatalf("items %+v", kinds)
	}
	for _, it := range items {
		if it.Kind == "prompt" && it.Message != "Allow rm?" {
			t.Fatalf("prompt message %q", it.Message)
		}
	}
}

func TestNeedsYouErrorsOnlyFailedNeedThreads(t *testing.T) {
	got := needsYouErrors([]OThread{
		{ID: "a", Status: "error", Group: "need", Title: "fix login", Error: "codex exited 1", UpdatedAt: 5},
		{ID: "b", Status: "error", Group: "settled"},
		{ID: "c", Status: "idle", Group: "need"},
		{ID: "d", Status: "error", Group: "need"},
	})
	if len(got) != 2 || got[0].Key != "err:a" || got[0].ThreadID != "a" || got[0].Message != "codex exited 1" || got[0].Kind != "error" {
		t.Fatalf("got %+v", got)
	}
	if got[1].Message == "" {
		t.Fatal("an error item needs a message")
	}
}

func TestThreadJSONDropsColorCodes(t *testing.T) {
	b, _ := json.Marshal(OThread{ID: "a", Error: "codex exited\n\x1b[2mtime\x1b[0m \x1b[31mERROR\x1b[0m"})
	if strings.Contains(string(b), `\u001b`) || !strings.Contains(string(b), "time ERROR") {
		t.Fatalf("got %s", b)
	}
}
