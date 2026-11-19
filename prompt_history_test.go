package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPromptHistory(t *testing.T) {
	cfg, cx := t.TempDir(), t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	t.Setenv("CODEX_HOME", cx)
	claude := strings.Join([]string{
		`{"display":"fix the tests","timestamp":1000,"project":"/w/app","sessionId":"aaa"}`,
		`{"display":"! git status","timestamp":1500,"project":"/w/app","sessionId":"aaa"}`,
		`not json`,
		`{"display":"fix the tests","timestamp":3000,"project":"/w/app","sessionId":"bbb"}`,
		`{"display":"/exit","timestamp":4000,"sessionId":"bbb"}`,
		`{"display":"Attached files (open them with your file tools):\n- /x/uploads/new/up_0123456789abcdef.png","timestamp":5000,"sessionId":"bbb"}`,
	}, "\n")
	_ = os.WriteFile(filepath.Join(cfg, "history.jsonl"), []byte(claude+"\n"), 0o600)
	_ = os.WriteFile(filepath.Join(cx, "history.jsonl"), []byte(`{"session_id":"ccc","ts":2,"text":"add a README"}`+"\n"), 0o600)

	all := prompts("", "", "", 10)
	var got []string
	for _, p := range all {
		got = append(got, p.Text)
	}
	// Newest first, repeats once, /exit dropped; codex ts is seconds.
	if strings.Join(got, "|") != "fix the tests|add a README|! git status" || all[1].SessionID != "codex:ccc" || all[1].At != 2000 {
		t.Fatalf("all: %q %+v", got, all)
	}
	if p := prompts("claude:aaa", "", "no", 10); len(p) != 1 || p[0].Text != "fix the tests" || p[0].At != 1000 {
		t.Fatalf("session no bash: %+v", p)
	}
	if p := prompts("", "", "only", 10); len(p) != 1 || !p[0].Bash {
		t.Fatalf("bash only: %+v", p)
	}
	if p := prompts("", "readme", "", 10); len(p) != 1 || p[0].Agent != "codex" {
		t.Fatalf("query: %+v", p)
	}
}
