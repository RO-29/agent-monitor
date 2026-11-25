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
