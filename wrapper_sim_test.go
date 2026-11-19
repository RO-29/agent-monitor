// The Go wrapper code, kept only as the reference the Rust am-helper is tested against.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// runAgentWrapper hosts the agent on a pty when stdin and stdout are a terminal, else execs it plainly.
// Inside tmux it also announces the pane, as before; both work with the daemon down.
func runAgentWrapper(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: agent-monitor run <agent> [args...]")
		fmt.Fprintln(os.Stderr, "  agents: claude | codex | cursor-agent | opencode | <any executable>")
		os.Exit(2)
	}
	agent := args[0]
	pane := os.Getenv("TMUX_PANE")
	if pane != "" {
		announcePane(toolFromAgentName(agent), pane)
	}
	if isTerminal(os.Stdin) && isTerminal(os.Stdout) {
		os.Exit(runHostMain(args, pane))
	}
	bin, err := exec.LookPath(agent)
	if err != nil {
		fmt.Fprintf(os.Stderr, "agent-monitor run: %s not found in PATH (%v)\n", agent, err)
		os.Exit(127)
	}
	// A plain agent must not claim an outer host's id.
	if err := syscall.Exec(bin, args, hostChildEnv(os.Environ())); err != nil {
		fmt.Fprintf(os.Stderr, "agent-monitor run: exec failed: %v\n", err)
		os.Exit(1)
	}
}

// announcePane tells the daemon this tmux pane runs a new agent; the pid lives as long as the agent.
func announcePane(tool Tool, pane string) {
	cwd, _ := os.Getwd()
	body, _ := json.Marshal(map[string]any{
		"tool":       tool,
		"paneId":     pane,
		"tmuxSocket": os.Getenv("TMUX"),
		"cwd":        cwd,
		"pid":        os.Getpid(),
	})
	c := &http.Client{Timeout: time.Second}
	resp, err := c.Post(daemonURL()+"/api/pane/announce", "application/json", bytes.NewReader(body))
	if err != nil {
		return
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
}
