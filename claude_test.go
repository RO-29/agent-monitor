package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestHandleHookCodexPermissionNotification(t *testing.T) {
	store := NewStore()
	payload := ClaudeHookPayload{
		SessionID:        "01a-test",
		TranscriptPath:   "/tmp/codex-rollout.jsonl",
		Cwd:              "/Users/rohit",
		HookEventName:    "Notification",
		NotificationType: "permission_prompt",
		Message:          "Run: rm /tmp/example",
	}

	handleHook(store, ToolCodex, payload)

	sess := store.Get("codex:01a-test")
	if sess == nil {
		t.Fatal("codex session was not created")
	}
	if sess.State != StateAwaitingPermission {
		t.Fatalf("state = %q, want %q", sess.State, StateAwaitingPermission)
	}
	if sess.PermissionMessage != payload.Message {
		t.Fatalf("permission message = %q, want %q", sess.PermissionMessage, payload.Message)
	}
	if store.Get("claude:01a-test") != nil {
		t.Fatal("Claude session was created for a Codex hook")
	}
}

func TestHandleHookClaudeNotificationUnchanged(t *testing.T) {
	store := NewStore()
	payload := ClaudeHookPayload{
		SessionID:     "claude-test",
		Cwd:           "/Users/rohit",
		HookEventName: "Notification",
		Message:       "Permission required for Bash",
	}

	handleHook(store, ToolClaude, payload)

	sess := store.Get("claude:claude-test")
	if sess == nil {
		t.Fatal("Claude session was not created")
	}
	if sess.State != StateAwaitingPermission {
		t.Fatalf("state = %q, want %q", sess.State, StateAwaitingPermission)
	}
	if store.Get("codex:claude-test") != nil {
		t.Fatal("Codex session was created for a Claude hook")
	}
}

func TestCodexInternalSessionsIgnored(t *testing.T) {
	t.Setenv("CODEX_HOME", "/h/.codex")
	if !codexInternalCwd(ToolCodex, "/h/.codex/memories") || codexInternalCwd(ToolCodex, "/h/work") || codexInternalCwd(ToolClaude, "/h/.codex/memories") {
		t.Fatal("codexInternalCwd")
	}
	store := NewStore()
	handleHook(store, ToolCodex, ClaudeHookPayload{SessionID: "mem", Cwd: "/h/.codex/memories", HookEventName: "SessionStart"})
	if store.Get("codex:mem") != nil {
		t.Fatal("a Codex memories session must not become a session")
	}
}

func TestCodexRolloutAfterStopStaysIdle(t *testing.T) {
	store := NewStore()
	handleHook(store, ToolCodex, ClaudeHookPayload{SessionID: "c1", Cwd: "/w", HookEventName: "SessionStart"})
	handleHook(store, ToolCodex, ClaudeHookPayload{SessionID: "c1", Cwd: "/w", HookEventName: "Stop"})
	before := store.Get("codex:c1").State
	fp := filepath.Join(t.TempDir(), "rollout-c1.jsonl")
	_ = os.WriteFile(fp, []byte("{}\n"), 0o644)
	codexHandleLine(store, fp, `{"type":"session_meta","timestamp":"2026-11-18T05:38:19Z","payload":{"id":"c1","cwd":"/w"}}`)
	if got := store.Get("codex:c1").State; got != before || got == StateRunning {
		t.Fatalf("state after a late session_meta: %s (was %s)", got, before)
	}
}

func TestClaudeCustomTitleWins(t *testing.T) {
	var agg claudeAggregate
	for _, l := range []string{`{"type":"custom-title","customTitle":"Restore sentinel items"}`, `{"type":"ai-title","aiTitle":"generated"}`} {
		agg.addLine([]byte(l), nil, map[string]*DetailTool{})
	}
	if agg.Title != "Restore sentinel items" {
		t.Fatalf("title %q", agg.Title)
	}
}

func TestCursorHooksAndProcessTwin(t *testing.T) {
	tool, p := cursorHookAsCursor(ToolClaude, ClaudeHookPayload{SessionID: "chat1", HookEventName: "stop"})
	if tool != ToolCursor || p.HookEventName != "Stop" {
		t.Fatalf("cursor hook: %s %s", tool, p.HookEventName)
	}
	if tool, _ := cursorHookAsCursor(ToolClaude, ClaudeHookPayload{SessionID: "c", HookEventName: "Stop"}); tool != ToolClaude {
		t.Fatal("a Claude Code hook must stay Claude")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	_ = os.MkdirAll(filepath.Join(home, ".cursor", "chats", "abc", "chat1"), 0o755)
	if !cursorChatExists("chat1") || cursorChatExists("nope") || cursorChatExists("../x") {
		t.Fatal("cursorChatExists")
	}
	ss := []*Session{
		{Tool: ToolCursorAgent, SessionID: "pid-1", Cwd: "/w", StartedAt: 100},
		{Tool: ToolCursor, SessionID: "chat1", Cwd: "/w", LastActivityAt: 200},
		{Tool: ToolCursorAgent, SessionID: "pid-2", Cwd: "/other", StartedAt: 100},
	}
	if got := dropCursorProcessTwins(ss); len(got) != 2 || got[0].SessionID != "chat1" || got[1].SessionID != "pid-2" {
		t.Fatalf("twins: %+v", got)
	}
}

func TestAdoptCursorPane(t *testing.T) {
	t.Setenv("AGENT_MONITOR_HOME", t.TempDir())
	prev := paneRegistry
	t.Cleanup(func() { paneRegistry = prev })
	paneRegistry = NewPaneRegistry()
	paneRegistry.Register(&PaneRegistration{AgentID: "claude:chat9", Tool: ToolClaude, SessionID: "chat9", PaneID: "%9"})
	adoptCursorPane("chat9")
	if r := paneRegistry.Get("cursor:chat9"); r == nil || r.PaneID != "%9" || r.Tool != ToolCursor {
		t.Fatalf("cursor pane: %+v", r)
	}
	if r := paneRegistry.Get("claude:chat9"); r != nil && r.Tool == ToolClaude {
		t.Fatalf("claude twin left: %+v", r)
	}
}

func TestCursorEchoOfClaudeID(t *testing.T) {
	prev := store
	t.Cleanup(func() { store = prev })
	t.Setenv("HOME", t.TempDir())
	store = NewStore()
	store.Upsert(UpsertInput{Tool: ToolClaude, SessionID: "abc", TranscriptPath: "/t/abc.jsonl", HasTranscript: true})
	echo := store.Upsert(UpsertInput{Tool: ToolCursor, SessionID: "abc", State: StateAwaitingInput, HasState: true})
	if !claudeOwnsID("abc") || !cursorEcho(echo) {
		t.Fatal("an inherited Claude id must read as an echo")
	}
	if got := waitingSessions(store.All()); len(got) != 0 {
		t.Fatalf("echo in the waiting list: %+v", got)
	}
}
