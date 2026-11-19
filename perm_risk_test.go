package main

import "testing"

func TestPermRequestGetsRisk(t *testing.T) {
	p := NewPermStore()
	hi := &PermissionRequest{ToolName: "Bash", Input: map[string]any{"command": "git push --force origin main"}}
	p.Register(hi)
	lo := &PermissionRequest{ToolName: "Read", Input: map[string]any{"file_path": "/tmp/x.txt"}}
	p.Register(lo)
	if hi.Risk != "high" || hi.RiskReason == "" {
		t.Fatalf("force push: %q %q", hi.Risk, hi.RiskReason)
	}
	if lo.Risk == "high" {
		t.Fatalf("a read is not high risk: %q", lo.Risk)
	}
}
