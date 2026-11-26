package main

import (
	"errors"
	"testing"
)

// A pane whose agent ended shows a shell: typing is refused, reading still works.
func TestDeadPaneRefusesWritesOnly(t *testing.T) {
	t.Setenv("AGENT_MONITOR_HOME", t.TempDir())
	prevReg, prevHost, prevList := paneRegistry, hostRegistry, listPanesFn
	t.Cleanup(func() { paneRegistry, hostRegistry, listPanesFn = prevReg, prevHost, prevList })
	paneRegistry, hostRegistry = NewPaneRegistry(), nil
	paneRegistry.Register(&PaneRegistration{AgentID: "claude:dead1", Tool: ToolClaude, SessionID: "dead1", PaneID: "%77", Source: "hook"})
	listPanesFn = func() ([]TmuxPane, error) {
		return []TmuxPane{{PaneID: "%77", SessionName: "s", CurrentCommand: "zsh"}}, nil
	}
	if _, code, err := withTerminal("claude:dead1", func(string) error { return nil }); !errors.Is(err, errDeadPane) || code != statusRefused {
		t.Fatalf("write: %d %v", code, err)
	}
	if _, _, err := readTerminal("claude:dead1", func(string) error { return nil }); err != nil {
		t.Fatalf("read: %v", err)
	}
}
