package main

import "testing"

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
