package main

import (
	"context"
	"fmt"
	"net/http/httptest"
	"testing"
	"time"
)

func TestRuleSuggestionsFromAudit(t *testing.T) {
	e := newApprovalEnv(t)
	s := e.s
	now := time.Now()
	n := 0
	add := func(tool, summary, state string, ok bool) {
		n++
		s.writeAudit(&Approval{ID: fmt.Sprintf("ap_%d", n), Agent: "claude", Cwd: "/repo", Kind: "permission", ToolName: tool, Summary: summary,
			State: state, AlwaysOK: ok, CreatedAt: now.UnixMilli(), DecidedAt: now.Add(-time.Duration(n) * time.Minute).UnixMilli()})
	}
	for i := 0; i < 4; i++ {
		add("Bash", fmt.Sprintf("go test ./pkg%d", i), "allowed", true)
	}
	for i := 0; i < 3; i++ {
		add("Bash", "npm run build", "allowed", true)
	}
	add("Bash", "npm run build", "denied", true) // the same command once denied: vetoed
	for i := 0; i < 3; i++ {
		add("Bash", "rm -rf /tmp/x", "allowed", false) // high risk: never suggested
	}
	add("Bash", "make all", "allowed", true) // too few
	got := s.ruleSuggestions(now)
	if len(got) != 1 || got[0].Pattern != "go test *" || got[0].Count != 4 || got[0].CwdPrefix != "/repo" {
		t.Fatalf("suggestions = %+v", got)
	}
	if err := s.dismissSuggestion(got[0].Key); err != nil {
		t.Fatal(err)
	}
	if got := s.ruleSuggestions(now); len(got) != 0 {
		t.Fatalf("after dismiss = %+v", got)
	}
	if _, err := s.upsertRule(Rule{Action: "allow", Tool: "Bash", Pattern: "make*"}); err != nil {
		t.Fatal(err)
	}
	add("Bash", "make all", "allowed", true)
	add("Bash", "make all", "allowed", true)
	if got := s.ruleSuggestions(now); len(got) != 0 {
		t.Fatalf("a covered call was suggested: %+v", got)
	}
}

func TestAlwaysPatternStaysNarrow(t *testing.T) {
	for in, want := range map[string]string{
		"python3 -V":            "python3 -V",
		"go test ./pkg":         "go test *",
		"npm run build":         "npm run build",
		"bash -n x.sh":          "bash -n x.sh",
		"ls *.go":               "",
		"git diff --stat HEAD~": "git diff *",
	} {
		if got := alwaysPattern(&Approval{ToolName: "Bash", Summary: in}); got != want {
			t.Errorf("alwaysPattern(%q) = %q, want %q", in, got, want)
		}
	}
	if !globMatch("go test *", "go test ./x") || globMatch("go test *", "go testx") {
		t.Error("the word boundary does not hold")
	}
}

func TestTypedTextDropsControlBytes(t *testing.T) {
	if got := typedText("hi\r!rm x\x1b[A\x07\tok"); got != "hi\n!rm x[A\tok" {
		t.Errorf("typedText = %q", got)
	}
}

func TestBelowAdminNeedsTheAccessMark(t *testing.T) {
	r := httptest.NewRequest("POST", "/api/session/send/x", nil)
	if belowAdmin(r) {
		t.Fatal("a direct call was limited")
	}
	if !belowAdmin(r.WithContext(context.WithValue(r.Context(), callerKey{}, caller{level: LevelControl}))) {
		t.Fatal("a control caller was not limited")
	}
	if belowAdmin(r.WithContext(context.WithValue(r.Context(), callerKey{}, caller{level: LevelAdmin}))) {
		t.Fatal("an admin caller was limited")
	}
}
