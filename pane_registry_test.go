package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

const (
	testClaudeSID = "f7c3e243-d5fe-4cdc-806d-be083d3ff14d"
	testCodexSID  = "01a1d9c3-3ffe-72a0-a945-1b74d41ce59e"
)

func stubPaneProbe(t *testing.T, m map[string]Tool) {
	t.Helper()
	prev := paneToolProbe
	paneToolProbe = func(id string) Tool { return m[id] }
	t.Cleanup(func() { paneToolProbe = prev })
}

func TestPaneRegisterCorrectsClaimedTool(t *testing.T) {
	t.Setenv("AGENT_MONITOR_HOME", t.TempDir())
	stubPaneProbe(t, map[string]Tool{testClaudeSID: ToolClaude})
	pr := NewPaneRegistry()

	// The tab-title hook claims "codex" for a Claude session.
	pr.Register(&PaneRegistration{AgentID: sid(ToolCodex, testClaudeSID), Tool: ToolCodex, SessionID: testClaudeSID, PaneID: "%46", Source: "tab-title-hook"})
	r := pr.Get("claude:" + testClaudeSID)
	if r == nil || r.AgentID != "claude:"+testClaudeSID || r.Tool != ToolClaude {
		t.Fatalf("want claude registration, got %+v", r)
	}
	// A codex session keeps its prefix.
	pr.Register(&PaneRegistration{AgentID: sid(ToolCodex, testCodexSID), Tool: ToolCodex, SessionID: testCodexSID, PaneID: "%41", Source: "hook"})
	if r := pr.Get("codex:" + testCodexSID); r == nil || r.Tool != ToolCodex {
		t.Fatalf("codex registration lost: %+v", r)
	}
	if n := len(pr.List()); n != 2 {
		t.Fatalf("want 2 registrations, got %d", n)
	}
}

func TestPaneRegisterUUIDv4FallbackWithoutTranscript(t *testing.T) {
	t.Setenv("AGENT_MONITOR_HOME", t.TempDir())
	stubPaneProbe(t, nil)
	if got := canonicalPaneTool(ToolCodex, testClaudeSID); got != ToolClaude {
		t.Fatalf("v4 id claimed by codex: got %s", got)
	}
	if got := canonicalPaneTool(ToolCodex, testCodexSID); got != ToolCodex {
		t.Fatalf("v7 id: got %s", got)
	}
	if got := canonicalPaneTool(ToolCodex, "pane-3"); got != ToolCodex {
		t.Fatalf("register_self id: got %s", got)
	}
}

func TestPaneLookupToleratesWrongStoredPrefix(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AGENT_MONITOR_HOME", dir)
	// Unknown to the probe, so loadFromDisk cannot repair it: lookup must still work.
	stubPaneProbe(t, nil)
	other := "11111111-2222-7333-8444-555555555555"
	body, _ := json.Marshal(persistedRegistry{Registrations: []*PaneRegistration{
		{AgentID: "codex:" + other, Tool: ToolCodex, SessionID: other, PaneID: "%7", Source: "tab-title-hook", RegisteredAt: 1},
	}})
	if err := os.WriteFile(filepath.Join(dir, "panes.json"), body, 0o644); err != nil {
		t.Fatal(err)
	}
	pr := NewPaneRegistry()
	for _, q := range []string{"claude:" + other, "codex:" + other, other} {
		if r := pr.Get(q); r == nil || r.PaneID != "%7" {
			t.Fatalf("Get(%q) = %+v", q, r)
		}
	}
	if r := pr.ResolveRecipient("claude:" + other); r == nil {
		t.Fatal("ResolveRecipient missed the stored entry")
	}
	if err := pr.SetAlias("claude:"+other, "rev"); err != nil {
		t.Fatal(err)
	}
	pr.Forget("claude:" + other)
	if len(pr.List()) != 0 {
		t.Fatal("Forget by claude: prefix left the codex: entry")
	}
}

func TestPaneLoadRepairsWrongPrefix(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AGENT_MONITOR_HOME", dir)
	stubPaneProbe(t, map[string]Tool{testClaudeSID: ToolClaude})
	body, _ := json.Marshal(persistedRegistry{Registrations: []*PaneRegistration{
		{AgentID: "codex:" + testClaudeSID, Tool: ToolCodex, SessionID: testClaudeSID, PaneID: "%46", Source: "tab-title-hook"},
	}})
	_ = os.WriteFile(filepath.Join(dir, "panes.json"), body, 0o644)
	pr := NewPaneRegistry()
	l := pr.List()
	if len(l) != 1 || l[0].AgentID != "claude:"+testClaudeSID {
		t.Fatalf("not repaired: %+v", l[0])
	}
}

func TestPaneRegisterReplacesSameSessionOtherPrefix(t *testing.T) {
	t.Setenv("AGENT_MONITOR_HOME", t.TempDir())
	stubPaneProbe(t, nil)
	pr := NewPaneRegistry()
	id := "11111111-2222-7333-8444-555555555555"
	pr.Register(&PaneRegistration{AgentID: "codex:" + id, Tool: ToolCodex, SessionID: id, PaneID: "%1", Source: "hook"})
	pr.Register(&PaneRegistration{AgentID: "claude:" + id, Tool: ToolClaude, SessionID: id, PaneID: "%2", Source: "hook"})
	l := pr.List()
	if len(l) != 1 || l[0].PaneID != "%2" {
		t.Fatalf("want one entry on %%2, got %+v", l)
	}
}
