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
	if got := iosRenew(1794983100000, now); got["started"] != true {
		t.Fatalf("first request: %v", got)
	}
	if got := iosRenew(1794983100000, now.Add(10*time.Minute)); got["started"] != false || kicks != 1 {
		t.Fatalf("a request inside the gap must not start the job again: %v, kicks %d", got, kicks)
	}
	if got := iosRenew(1794983100000, now.Add(iosRenewGap)); got["started"] != true || kicks != 2 {
		t.Fatalf("after the gap: %v, kicks %d", got, kicks)
	}
	b, _ := os.ReadFile(iosRenewFlag())
	if strings.TrimSpace(string(b)) != "1794983100000" {
		t.Fatalf("flag holds %q", b)
	}

	iosKick = func() error { return errors.New("no job") }
	iosRenewState.last = time.Time{}
	if got := iosRenew(1, now); got["ok"] != false {
		t.Fatalf("a Mac with no job must say so: %v", got)
	}
	if _, err := os.Stat(iosRenewFlag()); !os.IsNotExist(err) {
		t.Fatal("the flag must go when no job can use it")
	}
}
