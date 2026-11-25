package main

import (
	"os"
	"slices"
	"strings"
	"testing"
)

// Real screens from Claude Code 2.1.332: every prompt must be seen, fingerprinted and, where the app answers it, answerable.
func TestClaudePromptScreens(t *testing.T) {
	read := func(name string) string {
		b, err := os.ReadFile("testdata/screens/claude-2.1.332-" + name + ".txt")
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	for _, name := range []string{"trust", "perm-bash", "perm-write", "perm-edit", "perm-fetch", "ask", "plan"} {
		scr := read(name)
		if !panePromptOpen(scr) || !parsedPromptOpen(scr) {
			t.Errorf("%s: prompt not seen (open=%v parsed=%v)", name, panePromptOpen(scr), parsedPromptOpen(scr))
		}
		if promptKey(scr) == "" {
			t.Errorf("%s: no fingerprint", name)
		}
	}
	for _, name := range []string{"perm-bash", "perm-write", "perm-edit", "perm-fetch"} {
		scr := read(name)
		if o := claudePermOptions(scr); len(o) < 2 || !strings.HasPrefix(o[0].Label, "Yes") {
			t.Errorf("%s: options %+v", name, o)
		}
		if k := promptAnswerKeys(scr, "yes"); !slices.Equal(k, []string{"1"}) {
			t.Errorf("%s: yes keys %v", name, k)
		}
	}
	if k := promptAnswerKeys(read("trust"), "yes"); !slices.Equal(k, []string{"Down", "Enter"}) {
		t.Errorf("trust: yes keys %v", k)
	}
	if k := promptAnswerKeys(read("ask"), "yes"); k != nil {
		t.Errorf("a question is answered by its own card, not yes: %v", k)
	}
	if o := planOptionsOnScreen(read("plan")); len(o) < 3 {
		t.Errorf("plan options %+v", o)
	}
}

func TestCursorMenuSkipsWrappedLines(t *testing.T) {
	scr := " Pick one\n ❯ No, exit and lose the\n     unsaved work\n   Yes, keep going\n Enter to confirm · Esc to cancel\n"
	if k := cursorMenuKeys(scr, "Yes"); !slices.Equal(k, []string{"Down", "Enter"}) {
		t.Fatalf("a wrapped line is not a choice: %v", k)
	}
	up := " Pick one\n   Yes, keep going\n ❯ No, exit\n Enter to confirm\n"
	if k := cursorMenuKeys(up, "Yes"); !slices.Equal(k, []string{"Up", "Enter"}) {
		t.Fatalf("choice above the cursor: %v", k)
	}
}

// Codex 0.169 shows its update and folder-trust menus at start, with the footer "enter continue · esc …".
func TestCodexStartPromptScreens(t *testing.T) {
	for _, name := range []string{"update", "trust"} {
		b, err := os.ReadFile("testdata/screens/codex-0.169-" + name + ".txt")
		if err != nil {
			t.Fatal(err)
		}
		scr := string(b)
		if !panePromptOpen(scr) || !parsedPromptOpen(scr) || promptKey(scr) == "" {
			t.Errorf("%s: open=%v parsed=%v key=%q", name, panePromptOpen(scr), parsedPromptOpen(scr), promptKey(scr))
		}
	}
}

// AM_LIVE_SCREENS names a folder of screens that bin/prompt-check.sh captured from the installed Claude Code.
func TestLiveClaudeScreens(t *testing.T) {
	dir := os.Getenv("AM_LIVE_SCREENS")
	if dir == "" {
		t.Skip("AM_LIVE_SCREENS not set")
	}
	for _, c := range []struct {
		name string
		yes  []string
	}{{"trust", nil}, {"perm-bash", []string{"1"}}} {
		b, err := os.ReadFile(dir + "/" + c.name + ".txt")
		if err != nil {
			t.Errorf("%s: not captured: %v", c.name, err)
			continue
		}
		scr := string(b)
		if !panePromptOpen(scr) || !parsedPromptOpen(scr) || promptKey(scr) == "" {
			t.Errorf("%s: not seen (open=%v parsed=%v)", c.name, panePromptOpen(scr), parsedPromptOpen(scr))
		}
		k := promptAnswerKeys(scr, "yes")
		if c.yes != nil && !slices.Equal(k, c.yes) || c.yes == nil && (len(k) == 0 || k[len(k)-1] != "Enter") {
			t.Errorf("%s: yes keys %v", c.name, k)
		}
	}
}
