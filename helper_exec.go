package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// rustHelperName is the native helper that serves mcp-perm-server, run, __pty-leader and the hook modes when it sits next to the Go binary.
const rustHelperName = "am-helper"

// rustHelperProtocol must match the "protocol N" line of `am-helper version`; bump both when a mode's contract changes.
const rustHelperProtocol = "1"

var helperOK = struct {
	sync.Mutex
	m map[string]bool
}{m: map[string]bool{}}

// helperProtocolOK runs `<p> version` once per path, size and mtime, and is true when its protocol matches.
// A probe that times out (a first run after a build can be slow) counts as a match and is not cached.
func helperProtocolOK(p string, st os.FileInfo) bool {
	key := fmt.Sprintf("%s|%d|%d", p, st.Size(), st.ModTime().UnixNano())
	helperOK.Lock()
	ok, seen := helperOK.m[key]
	helperOK.Unlock()
	if seen {
		return ok
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, p, "version").Output()
	if ctx.Err() != nil { // a slow first run after a build passes; a hung helper is probed again after 30 s
		helperOK.Lock()
		helperOK.m[key] = true
		helperOK.Unlock()
		time.AfterFunc(30*time.Second, func() { helperOK.Lock(); delete(helperOK.m, key); helperOK.Unlock() })
		return true
	}
	ok = err == nil && helperProtocolLine(out) == rustHelperProtocol
	helperOK.Lock()
	helperOK.m[key] = ok
	helperOK.Unlock()
	return ok
}

func helperProtocolLine(out []byte) string {
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		if v, found := strings.CutPrefix(strings.TrimSpace(sc.Text()), "protocol "); found {
			return v
		}
	}
	return ""
}

// helperFor returns am-helper beside bin when it is executable and speaks rustHelperProtocol; else bin.
func helperFor(bin string) string {
	if bin == "" {
		return bin
	}
	p := filepath.Join(filepath.Dir(bin), rustHelperName)
	if st, err := os.Stat(p); err == nil && st.Mode().IsRegular() && st.Mode()&0o111 != 0 && helperProtocolOK(p, st) {
		return p
	}
	return bin
}

// execRustHelper replaces this process with am-helper; it returns only to exit with an error.
func execRustHelper(args []string) {
	exe, err := os.Executable()
	if err == nil {
		if p := helperFor(exe); p != exe {
			err = syscall.Exec(p, append([]string{p}, args...), os.Environ())
		} else {
			err = fmt.Errorf("%s with protocol %s is not next to %s; build it with rust/am-helper/build.sh", rustHelperName, rustHelperProtocol, exe)
		}
	}
	fmt.Fprintf(os.Stderr, "agent-monitor %s: %v\n", args[0], err)
	os.Exit(1)
}
