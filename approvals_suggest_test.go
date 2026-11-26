package main

import (
	"fmt"
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
	add("Bash", "npm run build --watch", "denied", true) // same "npm run build*" shape: vetoed
	for i := 0; i < 3; i++ {
		add("Bash", "rm -rf /tmp/x", "allowed", false) // high risk: never suggested
	}
	add("Bash", "make all", "allowed", true) // too few
	got := s.ruleSuggestions(now)
	if len(got) != 1 || got[0].Pattern != "go test*" || got[0].Count != 4 || got[0].CwdPrefix != "/repo" {
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
