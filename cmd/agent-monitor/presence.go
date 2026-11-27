package main

import (
	"bytes"
	"context"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"sync"
	"time"
)

// The Mac is "in use" when its screen is unlocked and it had input in the last macIdleAway.
// While it is in use, phone pushes wait; they go out when the screen locks or the Mac goes idle.

const macIdleAway = 2 * time.Minute

var macPresence = struct {
	sync.Mutex
	inUse   bool
	checked time.Time
	locked  bool
	idle    time.Duration
}{}

var idleRe = regexp.MustCompile(`"HIDIdleTime" = (\d+)`)

func readMacPresence() (locked bool, idle time.Duration, ok bool) {
	if runtime.GOOS != "darwin" {
		return false, 0, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	root, err := exec.CommandContext(ctx, "ioreg", "-n", "Root", "-d1", "-a").Output()
	if err != nil {
		return false, 0, false
	}
	locked = parseScreenLocked(root)
	hid, err := exec.CommandContext(ctx, "ioreg", "-c", "IOHIDSystem", "-d4").Output()
	if err != nil {
		return locked, 0, false
	}
	m := idleRe.FindSubmatch(hid)
	if m == nil {
		return locked, 0, false
	}
	ns, _ := strconv.ParseInt(string(m[1]), 10, 64)
	return locked, time.Duration(ns), true
}

// parseScreenLocked reads CGSSessionScreenIsLocked from `ioreg -n Root -d1 -a`; macOS sets it only while locked.
func parseScreenLocked(plist []byte) bool {
	i := bytes.Index(plist, []byte("<key>CGSSessionScreenIsLocked</key>"))
	if i < 0 {
		return false
	}
	rest := bytes.TrimLeft(plist[i+len("<key>CGSSessionScreenIsLocked</key>"):], " \t\r\n")
	return bytes.HasPrefix(rest, []byte("<true/>"))
}

func macInUse(locked bool, idle time.Duration) bool { return !locked && idle < macIdleAway }

// atMac: phone pushes should wait because the person uses the Mac now.
func atMac() bool {
	if !onOr(notifySettingsNow().PhoneWhenAway) {
		return false
	}
	macPresence.Lock()
	defer macPresence.Unlock()
	return macPresence.inUse && time.Since(macPresence.checked) < 30*time.Second
}

// startMacPresence polls every 5 s and sends the held pushes when the person leaves the Mac.
func startMacPresence() {
	if runtime.GOOS != "darwin" {
		return
	}
	go func() {
		for {
			locked, idle, ok := readMacPresence()
			macPresence.Lock()
			was := macPresence.inUse
			macPresence.inUse = ok && macInUse(locked, idle)
			macPresence.locked, macPresence.idle, macPresence.checked = locked, idle, time.Now()
			left := was && !macPresence.inUse
			macPresence.Unlock()
			if left && notifier != nil {
				notifier.releaseHeld()
			}
			time.Sleep(5 * time.Second)
		}
	}()
}
