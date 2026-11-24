package main

import (
	"os/exec"
	"path/filepath"
	"regexp"
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
			EventKind: "process-tick", EventText: redactCmdSecrets(cmd),
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

// cursorAgentMatches is true only for a cursor-agent CLI run: not a shell, a wrapper script,
// the node worker it starts, Cursor desktop's background worker, or a one-shot command.
func cursorAgentMatches(cmd string) bool {
	if strings.Contains(cmd, "anysphere.cursor-agent-worker") || strings.Contains(cmd, "Cursor.app") {
		return false
	}
	f := strings.Fields(cmd)
	if len(f) == 0 || filepath.Base(f[0]) != "cursor-agent" {
		return false
	}
	sub := ""
	for _, a := range f[1:] {
		switch a {
		case "--version", "-v", "-V", "--help", "-h":
			return false
		}
		if sub == "" && !strings.HasPrefix(a, "-") && !strings.HasSuffix(a, ".js") {
			sub = a
		}
	}
	switch sub {
	case "acp", "worker-server", "status", "whoami", "login", "logout", "update", "upgrade", "ls", "mcp", "about", "models",
		"install-shell-integration", "uninstall-shell-integration", "create-chat":
		return false
	}
	return true
}

var (
	cmdSecretFlag  = regexp.MustCompile(`(?i)^--?[a-z0-9_-]*(key|token|secret|password|passwd|auth|cookie)[a-z0-9_-]*$`)
	cmdSecretValue = regexp.MustCompile(`\b(crsr|sk|ghp|gho|xox[abp])[_-][A-Za-z0-9_-]{8,}`)
)

// redactCmdSecrets hides the value of any secret-looking flag and any known token in a command line.
func redactCmdSecrets(cmd string) string {
	f := strings.Fields(cmd)
	for i := 0; i < len(f); i++ {
		if k, _, ok := strings.Cut(f[i], "="); ok && cmdSecretFlag.MatchString(k) {
			f[i] = k + "=<redacted>"
		} else if cmdSecretFlag.MatchString(f[i]) && i+1 < len(f) {
			f[i+1] = "<redacted>"
			i++
		}
	}
	return cmdSecretValue.ReplaceAllString(strings.Join(f, " "), "<redacted>")
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
