package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// daemonURL returns the base URL for the running agent-monitor daemon.
// Honors $AGENT_MONITOR_PORT (same env var the MCP server uses) so a
// non-default-port daemon is still reachable from CLI helpers.
func daemonURL() string {
	port := os.Getenv("AGENT_MONITOR_PORT")
	if port == "" {
		port = "7777"
	}
	return "http://127.0.0.1:" + port
}

// runAgentWrapper implements `agent-monitor run <agent> [args...]`.
//
// Flow:
//  1. Read $TMUX_PANE / $TMUX. Bail with a clear error if unset — running
//     outside tmux defeats the whole purpose.
//  2. Resolve <agent> to a full executable path via $PATH lookup.
//  3. POST /api/pane/announce with {tool, paneId, cwd, ...}. The daemon
//     stores it in a 60s pending list and pairs it with the next session of
//     that tool that arrives.
//  4. exec the agent (current process becomes the agent — wrapper vanishes).
//
// We deliberately don't wait for pairing confirmation before exec'ing — the
// pairing is best-effort and asynchronous. If the daemon is down we still
// exec the agent so the user can use it; they just won't get a registered
// pane.
func runAgentWrapper(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: agent-monitor run <agent> [args...]")
		fmt.Fprintln(os.Stderr, "  agents: claude | codex | cursor-agent | opencode | <any executable>")
		os.Exit(2)
	}
	agent := args[0]
	tool := toolFromAgentName(agent)

	pane := os.Getenv("TMUX_PANE")
	if pane == "" {
		fmt.Fprintln(os.Stderr, "agent-monitor run: not inside tmux ($TMUX_PANE unset).")
		fmt.Fprintln(os.Stderr, "Start a tmux session first: `tmux new -s work` then re-run.")
		os.Exit(2)
	}
	cwd, _ := os.Getwd()
	pid := os.Getpid()
	tmuxSocket := os.Getenv("TMUX")

	body, _ := json.Marshal(map[string]any{
		"tool":       tool,
		"paneId":     pane,
		"tmuxSocket": tmuxSocket,
		"cwd":        cwd,
		"pid":        pid,
	})
	resp, err := http.Post(daemonURL()+"/api/pane/announce", "application/json", bytes.NewReader(body))
	if err != nil {
		fmt.Fprintf(os.Stderr, "agent-monitor run: daemon unreachable (%v) — exec'ing %s anyway without registration\n", err, agent)
	} else {
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}

	// Resolve absolute path (syscall.Exec needs it).
	bin, err := exec.LookPath(agent)
	if err != nil {
		fmt.Fprintf(os.Stderr, "agent-monitor run: %s not found in PATH (%v)\n", agent, err)
		os.Exit(127)
	}
	// exec replaces this process — the current PID becomes the agent's PID,
	// which means $TMUX_PANE matching can also use pid descendants if needed.
	if err := syscall.Exec(bin, args, os.Environ()); err != nil {
		fmt.Fprintf(os.Stderr, "agent-monitor run: exec failed: %v\n", err)
		os.Exit(1)
	}
}

// toolFromAgentName maps a binary name to the internal Tool enum. Falls back
// to ToolClaude for unknown names — Claude is the most common case and the
// pane registry is keyed on (tool, sessionId), so a wrong tool just means
// the pairing won't fire (the user notices and can re-launch correctly).
func toolFromAgentName(name string) Tool {
	switch strings.ToLower(name) {
	case "claude", "claude-code":
		return ToolClaude
	case "codex":
		return ToolCodex
	case "cursor-agent":
		return ToolCursorAgent
	case "opencode":
		return ToolOpencode
	default:
		return Tool(name)
	}
}

// runSendCmd implements `agent-monitor send <recipient> "msg" [flags]`.
//
// Two modes:
//
//	default:     POST /api/talk/request → recipient's user clicks Allow/Deny
//	             in the web UI; sender CLI long-polls for the verdict. Use
//	             this for "Claude wants to ask Codex something — let me see
//	             it first" style flows.
//
//	--no-confirm/--direct:
//	             POST /api/session/send/{id} directly. Skips the banner;
//	             the message lands in the recipient's pane immediately.
//	             Use this for fully automated agent-to-agent loops where
//	             every message doesn't need a human click. The recipient's
//	             pane simply sees a fresh user prompt — there's no [from X]
//	             prefix automatically (prepend it yourself if you want it).
//
// Sender id is auto-discovered from $TMUX_PANE; --from <id> overrides it
// (useful when the caller isn't itself a registered agent).
func runSendCmd(args []string) {
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: agent-monitor send <recipient> <message> [--from <agent-id>] [--no-confirm]")
		fmt.Fprintln(os.Stderr, "  <recipient>: alias | agent-id | tmux pane (%3) | session-id prefix")
		fmt.Fprintln(os.Stderr, "  --no-confirm (or --direct): skip the recipient-confirmation banner")
		os.Exit(2)
	}
	to := args[0]
	msg := args[1]
	from := ""
	noConfirm := false
	for i := 2; i < len(args); i++ {
		switch args[i] {
		case "--from":
			if i+1 < len(args) {
				from = args[i+1]
				i++
			}
		case "--no-confirm", "--direct", "-y":
			noConfirm = true
		}
	}
	// Direct path: bypass the talk confirmation banner. We still resolve the
	// recipient through the registry so an agent can target by alias rather
	// than knowing the raw agent id.
	if noConfirm {
		id, err := resolveRecipientToAgentID(to)
		if err != nil {
			fmt.Fprintf(os.Stderr, "agent-monitor send: %v\n", err)
			os.Exit(1)
		}
		body, _ := json.Marshal(map[string]any{"text": msg, "enter": true})
		resp, err := http.Post(daemonURL()+"/api/session/send/"+id, "application/json", bytes.NewReader(body))
		if err != nil {
			fmt.Fprintf(os.Stderr, "agent-monitor send: %v\n", err)
			os.Exit(1)
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			bb, _ := io.ReadAll(resp.Body)
			fmt.Fprintf(os.Stderr, "agent-monitor send: %s — %s\n", resp.Status, strings.TrimSpace(string(bb)))
			os.Exit(1)
		}
		fmt.Println("✓ delivered (no-confirm)")
		return
	}
	// Self-discovery via $TMUX_PANE if --from wasn't given.
	if from == "" {
		if pane := os.Getenv("TMUX_PANE"); pane != "" {
			resp, err := http.Get(daemonURL() + "/api/panes")
			if err == nil {
				var data struct {
					Panes []*PaneRegistration `json:"panes"`
				}
				_ = json.NewDecoder(resp.Body).Decode(&data)
				resp.Body.Close()
				for _, r := range data.Panes {
					if r.PaneID == pane {
						from = r.AgentID
						break
					}
				}
			}
		}
	}
	body, _ := json.Marshal(map[string]any{
		"fromAgent": from,
		"toAgent":   to,
		"message":   msg,
	})
	resp, err := http.Post(daemonURL()+"/api/talk/request", "application/json", bytes.NewReader(body))
	if err != nil {
		fmt.Fprintf(os.Stderr, "daemon unreachable: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		bb, _ := io.ReadAll(resp.Body)
		fmt.Fprintf(os.Stderr, "request failed: %s — %s\n", resp.Status, strings.TrimSpace(string(bb)))
		os.Exit(1)
	}
	var reg struct {
		ID      string `json:"id"`
		ToAgent string `json:"toAgent"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&reg)
	fmt.Printf("→ proposed talk %s to %s — waiting for recipient's decision (Ctrl-C to give up)…\n", reg.ID, reg.ToAgent)

	// Long-poll for outcome. Use a generous client timeout so the connection
	// doesn't drop while the user thinks. The server times out at 10min and
	// auto-denies, so we'll always get a response by then.
	client := &http.Client{Timeout: 11 * time.Minute}
	awaitURL := fmt.Sprintf("%s/api/talk/%s/await?timeout=600", daemonURL(), reg.ID)
	wr, err := client.Get(awaitURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "await error: %v\n", err)
		os.Exit(1)
	}
	defer wr.Body.Close()
	var dec struct {
		Status string `json:"status"`
		Reason string `json:"reason"`
	}
	if err := json.NewDecoder(wr.Body).Decode(&dec); err != nil {
		fmt.Fprintf(os.Stderr, "decode await: %v\n", err)
		os.Exit(1)
	}
	switch dec.Status {
	case "delivered":
		fmt.Println("✓ delivered")
	case "denied":
		fmt.Printf("✗ denied%s\n", reasonSuffix(dec.Reason))
		os.Exit(3)
	case "timeout":
		fmt.Printf("⏰ timed out%s\n", reasonSuffix(dec.Reason))
		os.Exit(4)
	default:
		fmt.Printf("? %s%s\n", dec.Status, reasonSuffix(dec.Reason))
		os.Exit(5)
	}
}

func reasonSuffix(reason string) string {
	if reason == "" {
		return ""
	}
	return " — " + reason
}

// ─── smux-style helpers (read / type / keys / id / resolve) ───────────────
//
// These give any agent a tiny, language-agnostic CLI for cross-pane work
// without needing to talk JSON to the daemon directly. The recipient
// argument accepts the same formats as `send`: alias | agent-id | pane id
// (%3) | session-id prefix.

// resolveRecipientToAgentID hits /api/panes and finds the agent id matching
// the supplied recipient string. Returns ("", err) if none / ambiguous.
func resolveRecipientToAgentID(recipient string) (string, error) {
	resp, err := http.Get(daemonURL() + "/api/panes")
	if err != nil {
		return "", fmt.Errorf("daemon unreachable: %w", err)
	}
	defer resp.Body.Close()
	var data struct {
		Panes []*PaneRegistration `json:"panes"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return "", err
	}
	rl := strings.ToLower(strings.TrimSpace(recipient))
	// Alias (case-insensitive)
	for _, r := range data.Panes {
		if r.Alias != "" && strings.EqualFold(r.Alias, recipient) {
			return r.AgentID, nil
		}
	}
	// Exact agent id
	for _, r := range data.Panes {
		if r.AgentID == recipient {
			return r.AgentID, nil
		}
	}
	// Pane id
	if strings.HasPrefix(recipient, "%") {
		for _, r := range data.Panes {
			if r.PaneID == recipient {
				return r.AgentID, nil
			}
		}
	}
	// Session-id prefix (>=6 chars, unambiguous)
	if len(rl) >= 6 {
		var match string
		for _, r := range data.Panes {
			if strings.HasPrefix(r.SessionID, recipient) {
				if match != "" {
					return "", fmt.Errorf("ambiguous prefix %q matches multiple agents", recipient)
				}
				match = r.AgentID
			}
		}
		if match != "" {
			return match, nil
		}
	}
	// Bare tool name (e.g. "claude", "codex") — match if exactly one agent
	// of that tool is registered. Convenient for demos/scripts where there's
	// only one of each. Multiple matches → ambiguity error so we don't pick
	// wrongly.
	toolPrefix := strings.ToLower(recipient) + ":"
	var toolMatches []string
	for _, r := range data.Panes {
		if strings.HasPrefix(strings.ToLower(r.AgentID), toolPrefix) {
			toolMatches = append(toolMatches, r.AgentID)
		}
	}
	if len(toolMatches) == 1 {
		return toolMatches[0], nil
	}
	if len(toolMatches) > 1 {
		return "", fmt.Errorf("multiple %s agents — be more specific (try alias or full id): %v", recipient, toolMatches)
	}
	return "", fmt.Errorf("no agent matches %q (try `agent-monitor list`)", recipient)
}

// runStopCmd kills ONLY the agent-monitor daemon — the process holding the
// listen port. We deliberately avoid `pkill -f` because that pattern matches
// every `agent-monitor mcp-perm-server` child too, which closes codex's /
// claude's MCP transport unrecoverably (they don't auto-restart it).
//
// Identifies the daemon by `lsof -ti :PORT -sTCP:LISTEN`. Sends SIGTERM
// first, falls back to SIGKILL after 2s if it's still alive.
func runStopCmd() {
	port := os.Getenv("AGENT_MONITOR_PORT")
	if port == "" {
		port = "7777"
	}
	out, err := exec.Command("lsof", "-ti", ":"+port, "-sTCP:LISTEN").Output()
	if err != nil || len(strings.TrimSpace(string(out))) == 0 {
		fmt.Println("agent-monitor stop: no daemon listening on port " + port)
		return
	}
	pids := strings.Fields(strings.TrimSpace(string(out)))
	for _, p := range pids {
		pidI, err := strconv.Atoi(p)
		if err != nil {
			continue
		}
		fmt.Printf("agent-monitor stop: killing daemon pid %d\n", pidI)
		_ = syscall.Kill(pidI, syscall.SIGTERM)
	}
	// Give it ~2s to exit gracefully; then SIGKILL anything still hanging.
	for i := 0; i < 20; i++ {
		time.Sleep(100 * time.Millisecond)
		out, _ := exec.Command("lsof", "-ti", ":"+port, "-sTCP:LISTEN").Output()
		if len(strings.TrimSpace(string(out))) == 0 {
			fmt.Println("✓ stopped")
			return
		}
	}
	for _, p := range pids {
		pidI, _ := strconv.Atoi(p)
		_ = syscall.Kill(pidI, syscall.SIGKILL)
	}
	fmt.Println("✓ stopped (after SIGKILL)")
}

// runIDCmd prints the agent id registered to the current pane (or "" + exit
// 1 if nothing is registered). Used by agents to discover their own identity
// before calling other helpers.
func runIDCmd() {
	pane := os.Getenv("TMUX_PANE")
	if pane == "" {
		fmt.Fprintln(os.Stderr, "agent-monitor id: not inside tmux ($TMUX_PANE unset)")
		os.Exit(2)
	}
	id, err := resolveRecipientToAgentID(pane)
	if err != nil {
		fmt.Fprintf(os.Stderr, "agent-monitor id: %v\n", err)
		os.Exit(1)
	}
	fmt.Println(id)
}

// runResolveCmd echoes back the canonical agent id for a recipient string.
// Useful for scripts that want to validate a target before taking action.
func runResolveCmd(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: agent-monitor resolve <alias|agent-id|pane-id|sid-prefix>")
		os.Exit(2)
	}
	id, err := resolveRecipientToAgentID(args[0])
	if err != nil {
		fmt.Fprintf(os.Stderr, "agent-monitor resolve: %v\n", err)
		os.Exit(1)
	}
	fmt.Println(id)
}

// runReadCmd prints the visible buffer of the recipient's pane. Mirrors
// `tmux-bridge read`. Lines defaults to 200; pass 0 for "viewport only".
func runReadCmd(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: agent-monitor read <recipient> [lines]")
		os.Exit(2)
	}
	id, err := resolveRecipientToAgentID(args[0])
	if err != nil {
		fmt.Fprintf(os.Stderr, "agent-monitor read: %v\n", err)
		os.Exit(1)
	}
	lines := 200
	if len(args) >= 2 {
		if n, err := strconv.Atoi(args[1]); err == nil {
			lines = n
		}
	}
	url := fmt.Sprintf("%s/api/session/pane-view/%s?lines=%d", daemonURL(), id, lines)
	resp, err := http.Get(url)
	if err != nil {
		fmt.Fprintf(os.Stderr, "agent-monitor read: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	var data struct {
		OK      bool   `json:"ok"`
		Content string `json:"content"`
		Error   string `json:"error"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&data)
	if !data.OK {
		fmt.Fprintf(os.Stderr, "agent-monitor read: %s\n", data.Error)
		os.Exit(1)
	}
	fmt.Print(data.Content)
}

// runTypeCmd types text into the recipient's pane WITHOUT pressing Enter.
// Use `agent-monitor send` for type+Enter (and the talk confirmation flow).
func runTypeCmd(args []string) {
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: agent-monitor type <recipient> <text>")
		os.Exit(2)
	}
	id, err := resolveRecipientToAgentID(args[0])
	if err != nil {
		fmt.Fprintf(os.Stderr, "agent-monitor type: %v\n", err)
		os.Exit(1)
	}
	body, _ := json.Marshal(map[string]any{"text": args[1], "enter": false})
	resp, err := http.Post(daemonURL()+"/api/session/send/"+id, "application/json", bytes.NewReader(body))
	if err != nil {
		fmt.Fprintf(os.Stderr, "agent-monitor type: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		bb, _ := io.ReadAll(resp.Body)
		fmt.Fprintf(os.Stderr, "agent-monitor type: %s — %s\n", resp.Status, strings.TrimSpace(string(bb)))
		os.Exit(1)
	}
}

// runKeysCmd sends one or more tmux key names to the recipient pane. Each
// argument is a tmux key spec ("Enter", "Escape", "C-c", "1", "y").
func runKeysCmd(args []string) {
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: agent-monitor keys <recipient> <key> [key...]")
		fmt.Fprintln(os.Stderr, "  examples: Enter, Escape, C-c, Up, Down, Tab, 1, y, n")
		os.Exit(2)
	}
	id, err := resolveRecipientToAgentID(args[0])
	if err != nil {
		fmt.Fprintf(os.Stderr, "agent-monitor keys: %v\n", err)
		os.Exit(1)
	}
	body, _ := json.Marshal(map[string]any{"keys": args[1:]})
	resp, err := http.Post(daemonURL()+"/api/session/keys/"+id, "application/json", bytes.NewReader(body))
	if err != nil {
		fmt.Fprintf(os.Stderr, "agent-monitor keys: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		bb, _ := io.ReadAll(resp.Body)
		fmt.Fprintf(os.Stderr, "agent-monitor keys: %s — %s\n", resp.Status, strings.TrimSpace(string(bb)))
		os.Exit(1)
	}
}

func runListAgentsCmd() {
	resp, err := http.Get(daemonURL() + "/api/panes")
	if err != nil {
		fmt.Fprintf(os.Stderr, "agent-monitor list-agents: daemon unreachable (%v)\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	var data struct {
		Panes []*PaneRegistration `json:"panes"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		fmt.Fprintf(os.Stderr, "agent-monitor list-agents: parse error (%v)\n", err)
		os.Exit(1)
	}
	if len(data.Panes) == 0 {
		fmt.Println("(no registered agents)")
		return
	}
	fmt.Printf("%-30s %-12s %-6s %-8s %s\n", "AGENT-ID", "TOOL", "PANE", "ALIAS", "CWD")
	for _, r := range data.Panes {
		alias := r.Alias
		if alias == "" {
			alias = "-"
		}
		fmt.Printf("%-30s %-12s %-6s %-8s %s\n",
			truncate(r.AgentID, 30), r.Tool, r.PaneID, truncate(alias, 8), r.Cwd)
	}
}

func runNameCmd(args []string) {
	// `agent-monitor name <agentId|alias|pane|.> <new-alias>`
	// `.` resolves to "the agent in $TMUX_PANE" so an agent can name itself.
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: agent-monitor name <agentId|alias|pane|.> <new-alias>")
		os.Exit(2)
	}
	target := args[0]
	alias := args[1]
	if target == "." {
		pane := os.Getenv("TMUX_PANE")
		if pane == "" {
			fmt.Fprintln(os.Stderr, "agent-monitor name .: not inside tmux ($TMUX_PANE unset)")
			os.Exit(2)
		}
		// Resolve "." → agent id by listing panes and finding the match.
		resp, err := http.Get(daemonURL() + "/api/panes")
		if err != nil {
			fmt.Fprintf(os.Stderr, "daemon unreachable: %v\n", err)
			os.Exit(1)
		}
		var data struct {
			Panes []*PaneRegistration `json:"panes"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&data)
		resp.Body.Close()
		for _, r := range data.Panes {
			if r.PaneID == pane {
				target = r.AgentID
				break
			}
		}
		if target == "." {
			fmt.Fprintln(os.Stderr, "no registered agent in this pane — launch via `agent-monitor run` first")
			os.Exit(1)
		}
	}
	body, _ := json.Marshal(map[string]any{"alias": alias})
	r, err := http.Post(daemonURL()+"/api/pane/name/"+target, "application/json", bytes.NewReader(body))
	if err != nil {
		fmt.Fprintf(os.Stderr, "daemon unreachable: %v\n", err)
		os.Exit(1)
	}
	defer r.Body.Close()
	if r.StatusCode != 200 {
		bb, _ := io.ReadAll(r.Body)
		fmt.Fprintf(os.Stderr, "name failed: %s — %s\n", r.Status, strings.TrimSpace(string(bb)))
		os.Exit(1)
	}
	fmt.Printf("✓ %s named %q\n", target, alias)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

// drainHTTP is a tiny helper to fully consume + close a response body without
// caring about its content. Used by fire-and-forget POSTs.
func drainHTTP(resp *http.Response, _ time.Duration) {
	if resp == nil {
		return
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
}
