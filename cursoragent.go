package main

import (
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// cursor-agent is a CLI without persistent session storage. Detect it via
// process scan and surface live processes as sessions; remove them when the
// process exits.

var (
	cursorAgentMu      sync.Mutex
	cursorAgentSeen    = map[int]bool{} // pid -> seen on this tick
	cursorAgentSession = map[int]string{}
)

func startCursorAgentAdapter(s *Store) {
	tick := func() { cursorAgentScan(s) }
	tick()
	go func() {
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		for range t.C {
			tick()
		}
	}()
}

func cursorAgentScan(s *Store) {
	out, err := exec.Command("ps", "-axo", "pid=,command=").Output()
	if err != nil {
		return
	}
	live := map[int]bool{}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// pid + space + command
		sp := strings.IndexByte(line, ' ')
		if sp < 0 {
			continue
		}
		pid, err := strconv.Atoi(line[:sp])
		if err != nil {
			continue
		}
		cmd := strings.TrimSpace(line[sp+1:])
		if !cursorAgentMatches(cmd) {
			continue
		}
		live[pid] = true
		cursorAgentMu.Lock()
		sessionID, ok := cursorAgentSession[pid]
		if !ok {
			sessionID = "pid-" + strconv.Itoa(pid)
			cursorAgentSession[pid] = sessionID
		}
		cursorAgentMu.Unlock()
		cwd := procCwd(pid)
		title := cursorAgentTitle(cmd)
		s.Upsert(UpsertInput{
			Tool: ToolCursorAgent, SessionID: sessionID,
			Cwd: cwd, HasCwd: true,
			Pid: pid, HasPid: true,
			State: StateRunning, HasState: true,
			Title: title, HasTitle: true,
			EventKind: "process-tick", EventText: cmd,
		})
	}
	// Mark processes that have exited as completed.
	cursorAgentMu.Lock()
	for pid, sessionID := range cursorAgentSession {
		if !live[pid] {
			s.SetState(ToolCursorAgent, sessionID, StateCompleted, "process exited")
			delete(cursorAgentSession, pid)
		}
	}
	cursorAgentMu.Unlock()
	_ = cursorAgentSeen // keep map alive for future per-tick state if needed
}

// cursorAgentMatches recognises a cursor-agent process, excluding the IDE,
// installer scripts, and the launchd-managed cursor.app.
func cursorAgentMatches(cmd string) bool {
	if !strings.Contains(cmd, "cursor-agent") {
		return false
	}
	// Skip our own ps + grep leaks, browser tabs, and installer paths.
	if strings.Contains(cmd, "grep ") {
		return false
	}
	if strings.Contains(cmd, "Cursor.app") {
		// Cursor.app's main process — that's the IDE, not the CLI agent
		return false
	}
	return true
}

func cursorAgentTitle(cmd string) string {
	// Take the prompt argument if present (last quoted string), else binary name.
	cmd = strings.TrimSpace(cmd)
	if idx := strings.LastIndexByte(cmd, '"'); idx > 0 {
		// crude: pick the trailing quoted segment if the command has one
		start := strings.LastIndexByte(cmd[:idx], '"')
		if start >= 0 && start < idx {
			return cmd[start+1 : idx]
		}
	}
	return "cursor-agent"
}

// procCwd uses lsof to read a process's working directory. Slow-ish (~30ms)
// but only invoked when a new pid is seen, and cursor-agent processes are
// rare so the cost is bounded.
func procCwd(pid int) string {
	out, err := exec.Command("lsof", "-a", "-d", "cwd", "-Fn", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "n") {
			return strings.TrimPrefix(line, "n")
		}
	}
	return ""
}
