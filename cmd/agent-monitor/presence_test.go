package main

import (
	"testing"
	"time"
)

func TestScreenLockAndIdleMeanAway(t *testing.T) {
	locked := []byte("<dict>\n\t<key>CGSSessionScreenIsLocked</key>\n\t<true/>\n</dict>")
	unlocked := []byte("<dict>\n\t<key>CGSSessionOnConsoleKey</key>\n\t<true/>\n</dict>")
	if !parseScreenLocked(locked) || parseScreenLocked(unlocked) {
		t.Fatal("lock flag read wrong")
	}
	if !macInUse(false, 10*time.Second) || macInUse(true, 0) || macInUse(false, 5*time.Minute) {
		t.Fatal("in-use rule wrong")
	}
}

func TestHeldPushWaitsForAway(t *testing.T) {
	macPresence.Lock()
	macPresence.inUse, macPresence.checked = true, time.Now()
	macPresence.Unlock()
	t.Cleanup(func() { macPresence.Lock(); macPresence.inUse = false; macPresence.Unlock() })
	if !atMac() {
		t.Fatal("atMac is false while the Mac is in use")
	}
	macPresence.Lock()
	macPresence.checked = time.Now().Add(-time.Minute)
	macPresence.Unlock()
	if atMac() {
		t.Fatal("a stale reading still holds pushes")
	}
}
