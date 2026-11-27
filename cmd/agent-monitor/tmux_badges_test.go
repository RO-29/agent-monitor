package main

import "testing"

func TestWindowBadgesTakeTheHighestState(t *testing.T) {
	panes := []TmuxPane{{PaneID: "%1", WindowID: "@1"}, {PaneID: "%2", WindowID: "@1"}, {PaneID: "%3", WindowID: "@2"}, {PaneID: "%4", WindowID: "@3"}}
	got := windowBadges(map[string]State{"%1": StateRunning, "%2": StateAwaitingPermission, "%3": StateRunning, "%4": StateAwaitingInput}, panes)
	if got["@1"] != badgeNeedsYou || got["@2"] != badgeWorking || got["@3"] != badgeYourTurn {
		t.Fatalf("badges = %v", got)
	}
}

func TestTmuxBadgerChangesOnlyItsOwnWindows(t *testing.T) {
	calls := map[string]string{}
	n := 0
	b := &tmuxBadger{shown: map[string]int{}, set: func(w, text string) error { calls[w] = text; n++; return nil }}
	b.apply(map[string]int{"@1": badgeWorking, "@2": badgeNone})
	if calls["@1"] != "◐ " || n != 1 {
		t.Fatalf("first apply: %v (%d calls)", calls, n)
	}
	b.apply(map[string]int{"@1": badgeWorking})
	if n != 1 {
		t.Fatalf("an unchanged badge was set again (%d calls)", n)
	}
	b.apply(map[string]int{"@1": badgeNeedsYou})
	if calls["@1"] != "● " {
		t.Fatalf("needs you: %v", calls)
	}
	b.apply(map[string]int{})
	if text, ok := calls["@1"]; !ok || text != "" || len(b.shown) != 0 {
		t.Fatalf("clear: %v shown=%v", calls, b.shown)
	}
	if _, touched := calls["@2"]; touched {
		t.Fatal("a window that never had a badge was changed")
	}
}

func TestTmuxLitKeepsATrailingSemicolon(t *testing.T) {
	for in, want := range map[string]string{"abc;": `abc\;`, ";": `\;`, `x\;`: `x\\;`, "Enter": "Enter", "a;b": "a;b"} {
		if got := tmuxLit(in); got != want {
			t.Errorf("tmuxLit(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSafeKeyNamesAllowMenuKeysOnly(t *testing.T) {
	for _, k := range []string{"Enter", "Escape", "y", "1", "Up", "BSpace", "C-c", "Tab"} {
		if !safeKeyName(k) {
			t.Errorf("%q must be allowed", k)
		}
	}
	for _, k := range []string{"!", "0x21", "U+0021", "ls -la", "y;", "Enter;", ""} {
		if safeKeyName(k) {
			t.Errorf("%q must need admin", k)
		}
	}
}
