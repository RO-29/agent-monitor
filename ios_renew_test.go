package main

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

func TestIOSRenewStartsTheJobAtMostOncePerGap(t *testing.T) {
	t.Setenv("AGENT_MONITOR_HOME", t.TempDir())
	kicks := 0
	old := iosKick
	iosKick = func() error { kicks++; return nil }
	t.Cleanup(func() { iosKick = old; iosRenewState.last = time.Time{} })
	iosRenewState.last = time.Time{}

	now := time.Unix(1_800_000_000, 0)
	if got := iosRenew(1794983100000, now, false); got["started"] != true {
		t.Fatalf("first request: %v", got)
	}
	if got := iosRenew(1794983100000, now.Add(10*time.Minute), false); got["started"] != false || kicks != 1 {
		t.Fatalf("a request inside the gap must not start the job again: %v, kicks %d", got, kicks)
	}
	if got := iosRenew(1794983100000, now.Add(iosRenewGap), false); got["started"] != true || kicks != 2 {
		t.Fatalf("after the gap: %v, kicks %d", got, kicks)
	}
	b, _ := os.ReadFile(iosRenewFlag())
	if strings.TrimSpace(string(b)) != "1794983100000" {
		t.Fatalf("flag holds %q", b)
	}

	iosKick = func() error { return errors.New("no job") }
	iosRenewState.last = time.Time{}
	oldPeers := iosRenewPeers
	t.Cleanup(func() { iosRenewPeers = oldPeers })
	asked := 0
	iosRenewPeers = func(int64) (string, bool, bool) { asked++; return "", false, false }
	if got := iosRenew(1, now, false); got["ok"] != false || asked != 1 {
		t.Fatalf("a Mac with no job and no peer job must say so: %v", got)
	}
	if got := iosRenew(1, now, true); got["ok"] != false || asked != 1 {
		t.Fatalf("a forwarded ask must not go on to peers: %v asked=%d", got, asked)
	}
	iosRenewPeers = func(int64) (string, bool, bool) { return "build-mac", true, true }
	if got := iosRenew(1, now, false); got["ok"] != true || got["peer"] != "build-mac" {
		t.Fatalf("a peer with the job takes the ask: %v", got)
	}
	if got := iosRenew(1, now.Add(time.Minute), false); got["started"] != false {
		t.Fatalf("a forwarded ask also waits the gap: %v", got)
	}
	iosRenewState.last = time.Time{}
	if got := iosRenew(now.Add(9*24*time.Hour).UnixMilli(), now, false); got["ok"] != false {
		t.Fatalf("an end far ahead is refused: %v", got)
	}
	if _, err := os.Stat(iosRenewFlag()); !os.IsNotExist(err) {
		t.Fatal("the flag must go when no job can use it")
	}
}
