package main

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestPaneAttentionDecisionTable(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	iterm := `"CFBundleIdentifier"="com.googlecode.iterm2"`
	row := func(pane, flags, focusEvents string, ago int) string {
		return fmt.Sprintf("/dev/ttys001\twork\t%d\t%s\t%s\t%s\n", now.Unix()-int64(ago), flags, pane, focusEvents)
	}
	cases := []struct {
		name, clients, front string
		want                 bool
	}{
		{"all signals", row("%5", "attached,focused,UTF-8", "1", 3), iterm, true},
		{"browser in front", row("%5", "attached,focused,UTF-8", "1", 3), `"CFBundleIdentifier"="com.google.Chrome"`, false},
		{"Terminal.app counts", row("%5", "attached,focused", "1", 3), `"CFBundleIdentifier"="com.apple.Terminal"`, true},
		{"Ghostty counts", row("%5", "attached,focused", "1", 3), `"CFBundleIdentifier"="com.mitchellh.ghostty"`, true},
		{"front unknown", row("%5", "attached,focused", "1", 3), "", false},
		{"focus-events off is unknown", row("%5", "attached,focused,UTF-8", "0", 3), iterm, false},
		{"focus-events missing is unknown", row("%5", "attached,focused,UTF-8", "", 3), iterm, false},
		{"client not focused", row("%5", "attached,UTF-8", "1", 3), iterm, false},
		{"client on another pane", row("%6", "attached,focused", "1", 3), iterm, false},
		{"idle client", row("%5", "attached,focused", "1", 61), iterm, false},
		{"activity at the limit", row("%5", "attached,focused", "1", 60), iterm, true},
		{"no clients", "", iterm, false},
		{"bad activity", "/dev/ttys001\twork\tx\tattached,focused\t%5\t1\n", iterm, false},
		{"second client qualifies", row("%5", "attached", "1", 3) + row("%5", "attached,focused", "1", 3), iterm, true},
	}
	for _, c := range cases {
		got, why := paneAttention(c.clients, "%5", c.front, now)
		if got != c.want {
			t.Errorf("%s: got %v (%s), want %v", c.name, got, why, c.want)
		}
	}
}

func TestAttentionProbeErrorsAndCache(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	calls := 0
	p := newAttentionProbe()
	p.now = func() time.Time { return now }
	p.listClients = func(string) (string, error) {
		calls++
		return fmt.Sprintf("/dev/ttys001\tw\t%d\tattached,focused\t%%5\t1\n", now.Unix()), nil
	}
	p.frontBundle = func() (string, error) { return `"CFBundleIdentifier"="com.googlecode.iterm2"`, nil }
	if ok, why := p.check("%5", ""); !ok {
		t.Fatalf("want focused: %s", why)
	}
	_, _ = p.check("%5", "")
	if calls != 1 {
		t.Fatalf("cache: %d tmux calls in one second", calls)
	}
	now = now.Add(2 * time.Second)
	_, _ = p.check("%5", "")
	if calls != 2 {
		t.Fatalf("cache must expire: %d calls", calls)
	}

	p2 := newAttentionProbe()
	p2.listClients = func(string) (string, error) { return "", errors.New("no server") }
	p2.frontBundle = func() (string, error) { return `"CFBundleIdentifier"="com.googlecode.iterm2"`, nil }
	if ok, _ := p2.check("%5", ""); ok {
		t.Fatal("tmux failure must mean not focused")
	}
	p3 := newAttentionProbe()
	p3.listClients = p.listClients
	p3.now = p.now
	p3.frontBundle = func() (string, error) { return "", errors.New("timeout") }
	if ok, _ := p3.check("%5", ""); ok {
		t.Fatal("lsappinfo failure must mean not focused")
	}
}

func TestParseBundleID(t *testing.T) {
	if got := parseBundleID("\"CFBundleIdentifier\"=\"com.googlecode.iterm2\"\n"); got != "com.googlecode.iterm2" {
		t.Fatal(got)
	}
	if got := parseBundleID(""); got != "" {
		t.Fatal(got)
	}
}
