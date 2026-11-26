package main

import (
	"encoding/hex"
	"os"
	"strings"
	"testing"
)

func TestCursorBuild(t *testing.T) {
	ids := []string{strings.Repeat("aa", 32), strings.Repeat("bb", 32), strings.Repeat("cc", 32), strings.Repeat("dd", 32), strings.Repeat("ee", 32)}
	blobs := map[string][]byte{
		ids[0]: []byte(`{"role":"system","content":"You are..."}`),
		ids[1]: []byte(`{"role":"user","content":[{"type":"text","text":"<timestamp>Wednesday, Nov 18, 2026, 11:55 AM (UTC+5:30)</timestamp>\n<user_query>\nList files\n</user_query>"}]}`),
		ids[2]: []byte(`{"role":"assistant","content":[{"type":"text","text":"<think>look first</think>Listing."},{"type":"tool-call","toolCallId":"t1","toolName":"Shell","args":{"command":"ls"}}]}`),
		ids[3]: []byte(`{"role":"tool","content":[{"type":"tool-result","toolCallId":"t1","toolName":"Shell","result":"a.go\nb.go"}]}`),
		ids[4]: []byte(`{"role":"assistant","content":[{"type":"text","text":"Two files."}]}`),
	}
	var root []byte
	for _, id := range ids {
		b, _ := hex.DecodeString(id)
		root = append(append(root, 0x0a, 0x20), b...)
	}
	order := cursorOrder(root)
	if len(order) != 5 || order[2] != ids[2] {
		t.Fatalf("order %v", order)
	}
	items, runs := cursorBuild("cursor:x", order, blobs, 0)
	var got []string
	for _, it := range items {
		got = append(got, it.Kind+":"+firstOf(it.Command, it.Text))
	}
	if strings.Join(got, "|") != "user:List files|reasoning:look first|agent:Listing.|command:ls|agent:Two files." {
		t.Fatalf("items %q", got)
	}
	if items[3].Output != "a.go\nb.go" || items[3].Status != "completed" {
		t.Fatalf("tool result: %+v", items[3])
	}
	if len(runs) != 1 || runs[0].Prompt != "List files" || runs[0].Status != "completed" || runs[0].StartedAt != parseCursorTime("Wednesday, Nov 18, 2026, 11:55 AM (UTC+5:30)") || runs[0].StartedAt == 0 {
		t.Fatalf("runs %+v", runs[0])
	}
}

// CURSOR_CHAT=<chat dir> go test -run TestCursorRealChat -v prints a real chat.
func TestCursorRealChat(t *testing.T) {
	dir := os.Getenv("CURSOR_CHAT")
	if dir == "" {
		t.Skip("set CURSOR_CHAT")
	}
	items, runs, err := cursorItemsFor("cursor:real", dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range items {
		t.Logf("%-9s %-9s %.70q", it.Kind, it.Status, firstOf(it.Command, it.Text))
	}
	t.Logf("runs %d", len(runs))
}

func TestCursorWorkspaceFromContext(t *testing.T) {
	blobs := map[string][]byte{"a": []byte(`{"role":"user","content":"<user_info>\nOS Version: darwin\n\nWorkspace Path: /Users/me/proj\n\nIs directory a git repo: Yes"}`)}
	if got := cursorWorkspace([]string{"a"}, blobs); got != "/Users/me/proj" {
		t.Fatalf("workspace %q", got)
	}
}

func TestCursorTitleLine(t *testing.T) {
	for in, want := range map[string]string{
		"SYSTEM:\nYou are a ClickHouse engineer.\nMore": "You are a ClickHouse engineer.",
		"  \nFix the login page":                        "Fix the login page",
		"Note: keep tests green":                        "Note: keep tests green",
		"SYSTEM:\n\n":                                   "",
	} {
		if got := cursorTitleLine(in); got != want {
			t.Errorf("cursorTitleLine(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCursorPromptTitle(t *testing.T) {
	for in, want := range map[string]string{
		"Fix the login page\nplease": "Fix the login page",
		"You are the agent inside DataStoria.\nConversation:\nUSER:\nfirst ask\nASSISTANT:\nok\nUSER:\nwhy is memory lower?":                                           "why is memory lower?",
		"SYSTEM:\nYou are an engineer.\nOriginal goal: speed up the query\nSQL query:\nSELECT 1\n\nFOLLOW-UP QUESTION:\nwhat are those tables?\n\nThink step by step.": "what are those tables?",
		"SYSTEM:\nYou are an engineer.\nSQL query:\nSELECT count() FROM t\n":                                                                                           "SELECT count() FROM t",
		"SYSTEM:\nYou are an engineer.\nOriginal goal: speed up the query\nSQL query:\nSELECT 1":                                                                       "speed up the query",
		"You are a bot.\nUSER:\nwhat is X?\nUSER:\n?":                                                                                                                  "what is X?",
		"You are a bot.\nOriginal goal:\nGoal: help me understand":                                                                                                     "help me understand",
		"You are a helper with no labels.":                                                                                                                             "You are a helper with no labels.",
	} {
		if got := cursorPromptTitle(in); got != want {
			t.Errorf("cursorPromptTitle(%q) = %q, want %q", in, got, want)
		}
	}
}
