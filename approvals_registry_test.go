package main

import "testing"

func TestUpsertRuleTakesRegistryIDs(t *testing.T) {
	s := &ApprovalStore{dir: t.TempDir()}
	for _, id := range []string{"claude", "codex", "pi", "hermes", "opencode", "cursor-agent", ""} {
		if _, err := s.upsertRule(Rule{Action: "allow", Agent: id, Tool: "Bash", Pattern: "ls*"}); err != nil {
			t.Fatalf("%q: %v", id, err)
		}
	}
	if _, err := s.upsertRule(Rule{Action: "allow", Agent: "bogus", Tool: "Bash", Pattern: "ls*"}); err == nil {
		t.Fatal("unknown agent accepted")
	}
}
