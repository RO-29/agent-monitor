package main

import (
	"os"
	"strings"
	"testing"
)

func screenFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("testdata/screens/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func wantStatus(t *testing.T, got []string, first string, n int) {
	t.Helper()
	if len(got) != n || (n > 0 && !strings.HasPrefix(got[0], first)) {
		t.Fatalf("status = %q, want %d lines starting %q", got, n, first)
	}
}

func TestStatusLineClaudeIdle(t *testing.T) {
	got := paneStatusLine(ToolClaude, screenFixture(t, "claude-idle.txt"))
	wantStatus(t, got, "Opus 5.5", 3)
	if !strings.Contains(got[2], "auto mode") {
		t.Fatalf("mode line lost: %q", got)
	}
}

func TestStatusLineClaudePanelRuleBelow(t *testing.T) {
	scr := cliScreen("", " · ? for shortcuts") + cliRule + "\n  ◯ general-purpose  Checking things\n"
	wantStatus(t, paneStatusLine(ToolClaude, scr), "⏸ manual mode on", 1)
}

func TestStatusLineClaudeSlashSuggestions(t *testing.T) {
	// The real capture draws the list above the box, so the footer stays.
	wantStatus(t, paneStatusLine(ToolClaude, screenFixture(t, "claude-slash.txt")), "Opus 5.5", 3)
	box := "x\n" + cliRule + "\n❯ /us\n" + cliRule + "\n"
	below := box + "  /usage    Show usage\n  /statusline    Set up\n"
	if got := paneStatusLine(ToolClaude, below); got != nil {
		t.Fatalf("suggestions below the box: %q", got)
	}
	at := box + "  @src/a.go\n  @src/b.go\n"
	if got := paneStatusLine(ToolClaude, at); got != nil {
		t.Fatalf("@ suggestions below the box: %q", got)
	}
}

func TestStatusLineClaudePromptOpenAndNoBox(t *testing.T) {
	if got := paneStatusLine(ToolClaude, screenClaudeRm); got != nil {
		t.Fatalf("prompt open: %q", got)
	}
	if got := paneStatusLine(ToolClaude, "plain\ntext\n"); got != nil {
		t.Fatalf("no box: %q", got)
	}
}

func TestStatusLineCodexIdleAndDraft(t *testing.T) {
	idle := paneStatusLine(ToolCodex, screenFixture(t, "codex-idle.txt"))
	wantStatus(t, idle, "00000000", 3)
	draft := paneStatusLine(ToolCodex, screenFixture(t, "codex-draft.txt"))
	wantStatus(t, draft, "00000000", 3)
	for _, l := range draft {
		if strings.Contains(l, "line two") {
			t.Fatalf("draft line in status: %q", draft)
		}
	}
}

func TestStatusLineCodexTallPaneTrailingBlanks(t *testing.T) {
	scr := screenFixture(t, "codex-idle.txt") + "\n\n   \n\n"
	wantStatus(t, paneStatusLine(ToolCodex, scr), "00000000", 3)
}

func TestStatusLineCodexPromptOpen(t *testing.T) {
	if got := paneStatusLine(ToolCodex, screenCodexExec); got != nil {
		t.Fatalf("prompt open: %q", got)
	}
}

func TestStatusLineANSIAndClip(t *testing.T) {
	long := strings.Repeat("x", 300)
	scr := "x\n" + cliRule + "\n❯ \n" + cliRule + "\n\x1b[2m  " + long + "\x1b[0m\n  b\n  c\n  d\n  e\n"
	got := paneStatusLine(ToolClaude, scr)
	if len(got) != 4 || len([]rune(got[0])) != 200 || strings.Contains(got[0], "\x1b") {
		t.Fatalf("clip: %d lines, first %d runes", len(got), len([]rune(got[0])))
	}
	if paneStatusLine(ToolCursor, scr) != nil {
		t.Fatal("other agents get nothing")
	}
}

func TestStatusLineFooterStartingWithPath(t *testing.T) {
	scr := "x\n" + cliRule + "\n❯ \n" + cliRule + "\n  /Users/x/repo  main\n  Opus 5.5 │ repo\n"
	wantStatus(t, paneStatusLine(ToolClaude, scr), "/Users/x/repo", 2)
}

func TestClaudeBoxRulesDraftWithRuleLine(t *testing.T) {
	top := strings.Repeat("─", 60)
	draft := strings.Repeat("─", 30)
	scr := "out\n" + top + "\n❯ first\n  " + draft + "\n  more text\n" + top + "\n  Opus 5.5 │ repo\n"
	if box := cliInputBox(scr); !strings.Contains(box, "more text") {
		t.Fatalf("box cut at the draft rule: %q", box)
	}
	wantStatus(t, paneStatusLine(ToolClaude, scr), "Opus 5.5", 1)
}

func TestStatusLineSurvivesEarlyReturns(t *testing.T) {
	_, f := setupCLITest(t)
	f.tgt, f.screens = "%1", []string{screenFixture(t, "claude-idle.txt")}
	v, _, _ := cliQueueView("claude:missing-session", false)
	if v == nil || len(v.StatusLine) != 3 {
		t.Fatalf("status line lost on an early return: %+v", v)
	}
}
