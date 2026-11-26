package main

import (
	"strings"
	"testing"
)

func TestPromptKeyAndAnswerKeys(t *testing.T) {
	status := "\n  Opus 5.5 │ repo ███░░ 35%\n  ↻ 3h 33m"
	k1 := promptKey(screenClaudeRm + status)
	if k1 == "" || k1 != promptKey(screenClaudeRm+strings.Replace(status, "33m", "32m", 1)) {
		t.Fatalf("the status bar must not change the key: %q", k1)
	}
	if other := promptKey(strings.ReplaceAll(screenClaudeRm, "rm two.txt", "rm one.txt")); other == k1 {
		t.Fatal("a different prompt needs a different key")
	}
	if promptKey(screenIdle) != "" {
		t.Fatal("no prompt, no key")
	}
	if got := promptAnswerKeys(screenClaudeRm, "yes"); strings.Join(got, ",") != "1" {
		t.Fatalf("claude yes: %v", got)
	}
	if got := promptAnswerKeys(screenCodexExec, "yes"); len(got) != 1 || got[0] == "Enter" {
		t.Fatalf("codex yes: %v", got)
	}
	if got := promptAnswerKeys(screenClaudeRm, "no"); strings.Join(got, ",") != "Escape" {
		t.Fatalf("no: %v", got)
	}
	if got := promptAnswerKeys("Pick one?\n> a\n  b\n", "yes"); got != nil {
		t.Fatalf("unknown prompt: %v", got)
	}
}
