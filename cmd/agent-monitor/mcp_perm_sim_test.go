// The Go mcp_perm code, kept only as the reference the Rust am-helper is tested against.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// runMCPPermServer is the stdio MCP server behind --permission-prompt-tool and the agent tools.
// stdout carries JSON-RPC only; logs go to stderr.
func runMCPPermServer() {
	logf := func(format string, args ...any) { fmt.Fprintf(os.Stderr, "[mcp-perm] "+format+"\n", args...) }

	port := "7777"
	if v := os.Getenv("AGENT_MONITOR_PORT"); v != "" {
		port = v
	}
	daemonURL := "http://127.0.0.1:" + port
	// A driven orchestrator process names its daemon and thread; only then are the delegate tools offered.
	if v := os.Getenv("AM_DAEMON_URL"); v != "" {
		daemonURL = strings.TrimRight(v, "/")
	}
	orch := mcpOrch{thread: os.Getenv("AM_ORCH_THREAD")}
	orch.depth, _ = strconv.Atoi(os.Getenv("AM_ORCH_DEPTH"))

	logf("starting; daemon=%s orch=%q", daemonURL, orch.thread)
	serveMCP(os.Stdin, os.Stdout, daemonURL, orch, logf)
}

func serveMCP(stdin io.Reader, stdout io.Writer, daemonURL string, orch mcpOrch, logf func(string, ...any)) {
	in := bufio.NewReader(stdin)
	var stdoutMu sync.Mutex
	writeRaw := func(b []byte) error {
		stdoutMu.Lock()
		defer stdoutMu.Unlock()
		_, err := stdout.Write(append(b, '\n'))
		return err
	}
	writeJSON := func(v any) error {
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		return writeRaw(b)
	}

	for {
		line, err := in.ReadBytes('\n')
		if err != nil {
			if err == io.EOF {
				logf("stdin closed; exiting")
				return
			}
			logf("read err: %v", err)
			return
		}
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}

		var req map[string]any
		if err := json.Unmarshal(line, &req); err != nil {
			logf("bad json: %v", err)
			continue
		}
		method, _ := req["method"].(string)
		id := req["id"]

		switch method {
		case "initialize":
			_ = writeJSON(map[string]any{
				"jsonrpc": "2.0",
				"id":      id,
				"result": map[string]any{
					"protocolVersion": "2025-06-18",
					"serverInfo": map[string]any{
						"name":    "agent-monitor",
						"version": "0.3.0",
					},
					"capabilities": map[string]any{
						"tools": map[string]any{},
					},
				},
			})

		case "notifications/initialized", "notifications/cancelled":
			// no response for notifications

		case "tools/list":
			_ = writeJSON(map[string]any{
				"jsonrpc": "2.0",
				"id":      id,
				"result": map[string]any{
					"tools": append([]map[string]any{
						{
							"name":        "permission_prompt",
							"description": "Forward a Claude Code tool-use permission prompt to the agent-monitor browser UI for approval.",
							"inputSchema": map[string]any{
								"type": "object",
								"properties": map[string]any{
									"tool_name":   map[string]any{"type": "string"},
									"input":       map[string]any{"type": "object"},
									"tool_use_id": map[string]any{"type": "string"},
								},
								"required": []string{"tool_name"},
							},
						},
						{
							"name":        "request_secret",
							"description": "Ask the user for a secret (a password, token, API key, PIN) on a private card in agent-monitor (phone, web, Mac app); never ask for one in chat. One value per call. The user confirms with Face ID, Touch ID or a passkey. Returns a pipe path that gives the value once: use it inside the command that needs it, never print it.",
							"inputSchema": map[string]any{
								"type": "object",
								"properties": map[string]any{
									"name":   map[string]any{"type": "string", "description": "what the secret is, for example \"staging DB password\" (at most 80 characters)"},
									"reason": map[string]any{"type": "string", "description": "why you need it and the command that will use it"},
									"url":    map[string]any{"type": "string", "description": "an https page where the user can get or make the secret (optional)"},
								},
								"required": []string{"name"},
							},
						},
						{
							"name":        "who_am_i",
							"description": "Return this agent's registered id, alias, tool, and tmux pane id. Useful when you want to identify yourself before talking to other agents.",
							"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
						},
						{
							"name":        "list_agents",
							"description": "List every agent registered with agent-monitor (yourself + others). Auto-registers your own pane if it's missing. When relaying to the user, ALWAYS show the raw markdown table from the response unchanged — do NOT summarize as 'N agents in panes %0 and %3'. The user needs to see each agent's id, tool, cwd, and pane to address them.",
							"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
						},
						{
							"name":        "register_self",
							"description": "Register the calling agent's pane with agent-monitor. Use this when you weren't launched via `agent-monitor run` and want other agents to be able to talk_to_agent you. No arguments needed — the daemon detects your pane via process-tree descent and infers your tool from the parent process command.",
							"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
						},
						{
							"name":        "read_pane",
							"description": "Read the visible terminal buffer of another agent's tmux pane (last N lines). Use this after sending them a message to see their response, or to inspect what they're currently working on.",
							"inputSchema": map[string]any{
								"type": "object",
								"properties": map[string]any{
									"recipient": map[string]any{"type": "string", "description": "alias, agent id, tmux pane id (%3), or session-id prefix"},
									"lines":     map[string]any{"type": "integer", "description": "number of buffer lines to capture (default 200)"},
								},
								"required": []string{"recipient"},
							},
						},
						{
							"name":        "talk_to_agent",
							"description": "Send a message to another agent's tmux pane. Three delivery modes:\n\n  • default:        fire-and-forget. Returns 'delivered'.\n  • wait_for_reply: synchronous request/reply. The message lands in the recipient's pane prefixed with a talk_id; when they finish their work they call reply_to_talk(talk_id, message) and your call unblocks with their reply. This is the right pattern for asking another agent a question.\n  • confirm:        recipient's user clicks Allow/Deny in the web UI before the message lands.\n\nThe message lands in the recipient's pane as a fresh user prompt (typed + Enter).",
							"inputSchema": map[string]any{
								"type": "object",
								"properties": map[string]any{
									"recipient":      map[string]any{"type": "string", "description": "alias, agent id, tmux pane id (%3), or session-id prefix"},
									"message":        map[string]any{"type": "string", "description": "the text to deliver"},
									"confirm":        map[string]any{"type": "boolean", "description": "if true, recipient's web UI shows an Allow/Deny banner before delivery (default false)"},
									"wait_for_reply": map[string]any{"type": "boolean", "description": "if true, embed a talk_id in the prefix and block until the recipient calls reply_to_talk. Their reply is returned as the result. Default false."},
									"wait_seconds":   map[string]any{"type": "integer", "description": "max seconds to wait for the recipient's reply when wait_for_reply=true (default 600 = 10 min)"},
								},
								"required": []string{"recipient", "message"},
							},
						},
						{
							"name":        "reply_to_talk",
							"description": "Reply to a talk request another agent sent you. Use this when an incoming message arrived prefixed with REPLY-EXPECTED — the prefix includes a talk_id. Pass that id here along with your reply, and the original sender's talk_to_agent call will unblock with your message as the result.",
							"inputSchema": map[string]any{
								"type": "object",
								"properties": map[string]any{
									"talk_id": map[string]any{"type": "string", "description": "the talk_id from the incoming message's REPLY-EXPECTED prefix"},
									"message": map[string]any{"type": "string", "description": "your reply"},
								},
								"required": []string{"talk_id", "message"},
							},
						},
					}, append(mcpSearchToolDefs(), orch.tools()...)...),
				},
			})

		case "tools/call":
			if orch.handles(req) {
				orch.call(req, id, daemonURL, writeJSON, logf)
				continue
			}
			handleToolCall(req, id, daemonURL, writeJSON, logf)

		case "ping":
			_ = writeJSON(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{}})

		case "":
			// missing method — could be a response. Ignore.

		default:
			_ = writeJSON(map[string]any{
				"jsonrpc": "2.0", "id": id,
				"error": map[string]any{"code": -32601, "message": "method not found"},
			})
		}
	}
}

func handleToolCall(req map[string]any, id any, daemonURL string, writeJSON func(any) error, logf func(string, ...any)) {
	params, _ := req["params"].(map[string]any)
	if params == nil {
		params = map[string]any{}
	}
	calledTool, _ := params["name"].(string)
	args, _ := params["arguments"].(map[string]any)
	if args == nil {
		args = map[string]any{}
	}
	// Agent tools first; only an explicit permission_prompt long-polls, and unknown names get a JSON-RPC error.
	switch calledTool {
	case "who_am_i":
		handleWhoAmI(daemonURL, id, writeJSON, logf)
		return
	case "list_agents":
		handleListAgents(daemonURL, id, writeJSON, logf)
		return
	case "read_pane":
		handleReadPane(daemonURL, args, id, writeJSON, logf)
		return
	case "talk_to_agent":
		handleTalkToAgent(daemonURL, args, id, writeJSON, logf)
		return
	case "reply_to_talk":
		handleReplyToTalk(daemonURL, args, id, writeJSON, logf)
		return
	case "register_self":
		handleRegisterSelf(daemonURL, id, writeJSON, logf)
		return
	case "search_sessions":
		handleSearchSessions(daemonURL, args, id, writeJSON, logf)
		return
	case "session_context":
		handleSessionContext(daemonURL, args, id, writeJSON, logf)
		return
	case "permission_prompt":
		// fall through to legacy permission flow below
	default:
		logf("tools/call: unknown tool %q", calledTool)
		_ = writeJSON(map[string]any{
			"jsonrpc": "2.0", "id": id,
			"error": map[string]any{
				"code":    -32602,
				"message": fmt.Sprintf("unknown tool %q", calledTool),
			},
		})
		return
	}
	// Legacy permission_prompt arguments.
	toolName, _ := args["tool_name"].(string)
	toolUseID, _ := args["tool_use_id"].(string)
	input, _ := args["input"].(map[string]any)
	if input == nil {
		input = map[string]any{}
	}

	cwd, _ := os.Getwd()

	// Register the request with the daemon. If the daemon is down we deny so
	// Claude isn't stuck — that's safer than silent allow.
	body, _ := json.Marshal(map[string]any{
		"toolName":  toolName,
		"toolUseId": toolUseID,
		"input":     input,
		"cwd":       cwd,
	})
	resp, err := http.Post(daemonURL+"/api/permission/request", "application/json", bytes.NewReader(body))
	if err != nil {
		logf("daemon down (%v) — denying", err)
		writeMCPResult(writeJSON, id, deny("agent-monitor daemon unreachable"))
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		bb, _ := io.ReadAll(resp.Body)
		logf("daemon /request returned %d: %s", resp.StatusCode, string(bb))
		writeMCPResult(writeJSON, id, deny("daemon error"))
		return
	}
	var reg struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&reg); err != nil || reg.ID == "" {
		logf("decode /request: %v", err)
		writeMCPResult(writeJSON, id, deny("daemon parse error"))
		return
	}

	// Long-poll up to 10 minutes for the user's decision.
	client := &http.Client{Timeout: 11 * time.Minute}
	waitURL := daemonURL + "/api/permission/wait/" + reg.ID + "?timeout=" + strconv.Itoa(int(10*60))
	wr, err := client.Get(waitURL)
	if err != nil {
		logf("wait error: %v", err)
		writeMCPResult(writeJSON, id, deny("wait error"))
		return
	}
	defer wr.Body.Close()
	var dec PermissionResponse
	if err := json.NewDecoder(wr.Body).Decode(&dec); err != nil {
		writeMCPResult(writeJSON, id, deny("wait decode"))
		return
	}
	// Claude requires updatedInput on allow; echo the original input.
	if dec.Behavior == "allow" && dec.UpdatedInput == nil {
		dec.UpdatedInput = input
	}
	logf("decision for %s: %s (%s)", reg.ID, dec.Behavior, dec.Reason)
	writeMCPResult(writeJSON, id, dec)
}

// writeMCPResult sends the decision as ONE text block. Claude Code 2.1 rejects
// the result as invalid when structuredContent is also present.
func writeMCPResult(writeJSON func(any) error, id any, dec PermissionResponse) {
	if dec.Behavior == "" {
		dec.Behavior = "deny"
	}
	payload := map[string]any{
		"behavior": dec.Behavior,
	}
	if dec.UpdatedInput != nil {
		payload["updatedInput"] = dec.UpdatedInput
	}
	if dec.Reason != "" {
		payload["message"] = dec.Reason
	}
	bodyJSON, _ := json.Marshal(payload)
	_ = writeJSON(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"result": map[string]any{
			"content": []map[string]any{
				{"type": "text", "text": string(bodyJSON)},
			},
			"isError": false,
		},
	})
}

func deny(reason string) PermissionResponse {
	return PermissionResponse{Behavior: "deny", Reason: reason}
}

// httpJSON does a GET and decodes JSON. Tiny helper to keep handlers focused.
func httpJSON(url string, out any) error {
	resp, err := http.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// handleWhoAmI finds the caller's pane by $TMUX_PANE, else by the process tree (Codex strips the env).
func handleWhoAmI(daemonURL string, id any, writeJSON func(any) error, logf func(string, ...any)) {
	pane := detectMyTmuxPane()
	if pane == "" {
		writeMCPText(writeJSON, id, "Not inside tmux — no registered pane. (Tried $TMUX_PANE and process-tree descent; both came up empty.)", nil, false)
		return
	}
	var data struct {
		Panes []*PaneRegistration `json:"panes"`
	}
	if err := httpJSON(daemonURL+"/api/panes", &data); err != nil {
		writeMCPText(writeJSON, id, fmt.Sprintf("Couldn't reach agent-monitor daemon: %v", err), nil, true)
		return
	}
	for _, r := range data.Panes {
		if r.PaneID == pane {
			info := map[string]any{
				"agentId":   r.AgentID,
				"alias":     r.Alias,
				"tool":      r.Tool,
				"sessionId": r.SessionID,
				"paneId":    r.PaneID,
				"cwd":       r.Cwd,
				"source":    r.Source,
			}
			summary := fmt.Sprintf("I am %s (alias: %s, tool: %s, pane: %s, cwd: %s).",
				r.AgentID, orDash(r.Alias), r.Tool, r.PaneID, r.Cwd)
			writeMCPText(writeJSON, id, summary, info, false)
			return
		}
	}
	writeMCPText(writeJSON, id, fmt.Sprintf("My tmux pane is %s but no agent is registered to it. Re-launch via `agent-monitor run <agent>` so other agents can talk to me.", pane), nil, false)
}

// handleListAgents lists registered agents plus unregistered panes that look like agents.
func handleListAgents(daemonURL string, id any, writeJSON func(any) error, logf func(string, ...any)) {
	myPane := detectMyTmuxPane()
	if myPane != "" {
		_ = ensureSelfRegistered(daemonURL, myPane)
	}
	_, _ = http.Post(daemonURL+"/api/panes/sweep", "application/json", nil)

	var regData struct {
		Panes []*PaneRegistration `json:"panes"`
	}
	if err := httpJSON(daemonURL+"/api/panes", &regData); err != nil {
		writeMCPText(writeJSON, id, fmt.Sprintf("Couldn't reach agent-monitor daemon: %v", err), nil, true)
		return
	}
	// Live panes, to find agents that have not registered.
	var tmuxData struct {
		OK    bool       `json:"ok"`
		Panes []TmuxPane `json:"panes"`
	}
	_ = httpJSON(daemonURL+"/api/tmux/panes", &tmuxData)

	registeredByPane := map[string]*PaneRegistration{}
	for _, r := range regData.Panes {
		registeredByPane[r.PaneID] = r
	}

	type row struct {
		paneId, agentId, alias, tool, cwd, status string
		isSelf                                    bool
	}
	var rows []row
	// First add every registered agent.
	for _, r := range regData.Panes {
		rows = append(rows, row{
			paneId: r.PaneID, agentId: r.AgentID, alias: r.Alias,
			tool: string(r.Tool), cwd: r.Cwd, status: "registered",
			isSelf: myPane != "" && r.PaneID == myPane,
		})
	}
	// Then add every UNREGISTERED tmux pane that's running something
	// agent-shaped, with a hint for how to register it.
	unregistered := 0
	if tmuxData.OK {
		for _, p := range tmuxData.Panes {
			if _, ok := registeredByPane[p.PaneID]; ok {
				continue
			}
			if !looksLikeAgent(p.CurrentCommand) {
				continue
			}
			rows = append(rows, row{
				paneId: p.PaneID, agentId: "(none)", alias: "-",
				tool: guessToolFromCommand(p.CurrentCommand),
				cwd:  p.CurrentPath, status: "unregistered",
				isSelf: myPane != "" && p.PaneID == myPane,
			})
			unregistered++
		}
	}

	// Build payload (always an object — see ensureObject for why).
	rowMaps := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		rowMaps = append(rowMaps, map[string]any{
			"agentId": r.agentId, "alias": r.alias, "tool": r.tool,
			"paneId": r.paneId, "cwd": r.cwd, "status": r.status,
			"isSelf": r.isSelf,
		})
	}
	payload := map[string]any{
		"agents":              rowMaps,
		"registeredCount":     len(regData.Panes),
		"unregisteredCount":   unregistered,
		"totalAgentLikePanes": len(rows),
		"myPane":              myPane,
	}
	if len(rows) == 0 {
		writeMCPText(writeJSON, id, "No agents are registered and no tmux panes look like agents. Launch agents via `agent-monitor run <agent>` inside tmux panes first.", payload, false)
		return
	}
	// Markdown table — models tend to relay it verbatim.
	var b strings.Builder
	fmt.Fprintf(&b, "%d agent(s) found — %d registered, %d unregistered:\n\n",
		len(rows), len(regData.Panes), unregistered)
	b.WriteString("| pane | who | tool | cwd | status | agent id |\n")
	b.WriteString("|------|-----|------|-----|--------|----------|\n")
	for _, r := range rows {
		who := orDash(r.alias)
		if r.isSelf {
			if who == "-" {
				who = "**← you**"
			} else {
				who = who + " **(← you)**"
			}
		}
		statusCell := r.status
		if r.status == "unregistered" {
			statusCell = "**unregistered**"
		}
		fmt.Fprintf(&b, "| `%s` | %s | %s | `%s` | %s | `%s` |\n",
			r.paneId, who, r.tool, r.cwd, statusCell, r.agentId)
	}
	b.WriteString("\n• **registered** rows are addressable via `talk_to_agent(recipient=\"<paneId or agent id>\", message=...)`.\n")
	if unregistered > 0 {
		b.WriteString("• **unregistered** rows have an agent-like process but no registration. The agent in that pane should call `register_self()` to make itself addressable, OR the user can launch via `agent-monitor run <agent>` instead of bare `<agent>`.\n")
	}
	b.WriteString("• Skip the row marked ← you when picking a recipient.\n")
	b.WriteString("\nNote: this lists tmux PANES with agents in them. The agent-monitor session store (different concept) tracks every JSONL/chat file historically — it'll have many more entries than this list.")
	writeMCPText(writeJSON, id, b.String(), payload, false)
}

// looksLikeAgent matches node, claude.exe, codex, cursor-agent and opencode.
func looksLikeAgent(cmd string) bool {
	c := strings.ToLower(cmd)
	switch c {
	case "node", "claude.exe", "claude", "codex", "cursor-agent", "opencode":
		return true
	}
	return strings.Contains(c, "claude") || strings.Contains(c, "codex") ||
		strings.Contains(c, "cursor-agent") || strings.Contains(c, "opencode")
}

// guessToolFromCommand maps a pane command to a Tool; "node" gives "node?".
func guessToolFromCommand(cmd string) string {
	c := strings.ToLower(cmd)
	switch {
	case c == "claude" || c == "claude.exe" || strings.Contains(c, "claude"):
		return "claude?"
	case c == "codex" || strings.Contains(c, "codex"):
		return "codex?"
	case strings.Contains(c, "cursor-agent"):
		return "cursor-agent?"
	case strings.Contains(c, "opencode"):
		return "opencode?"
	case c == "node":
		return "node?"
	}
	return c
}

// handleReadPane returns the recipient's captured tmux buffer as plain text.
// The agent gets exactly what they'd see if they ran `tmux capture-pane`.
func handleReadPane(daemonURL string, args map[string]any, id any, writeJSON func(any) error, logf func(string, ...any)) {
	recipient, _ := args["recipient"].(string)
	if recipient == "" {
		writeMCPText(writeJSON, id, "read_pane: 'recipient' argument is required", nil, true)
		return
	}
	lines := 200
	if v, ok := args["lines"].(float64); ok && v > 0 {
		lines = int(v)
	}
	target, err := mcpResolveRecipient(daemonURL, recipient)
	if err != nil {
		writeMCPText(writeJSON, id, fmt.Sprintf("read_pane: %v", err), nil, true)
		return
	}
	url := fmt.Sprintf("%s/api/session/pane-view/%s?lines=%d", daemonURL, target, lines)
	var data struct {
		OK      bool   `json:"ok"`
		Content string `json:"content"`
		Error   string `json:"error"`
	}
	if err := httpJSON(url, &data); err != nil {
		writeMCPText(writeJSON, id, fmt.Sprintf("read_pane: %v", err), nil, true)
		return
	}
	if !data.OK {
		writeMCPText(writeJSON, id, fmt.Sprintf("read_pane: %s", orDash(data.Error)), nil, true)
		return
	}
	writeMCPText(writeJSON, id, data.Content, nil, false)
}

// handleTalkToAgent delivers to another agent's pane; confirm=true waits for the user's Allow/Deny.
func handleTalkToAgent(daemonURL string, args map[string]any, id any, writeJSON func(any) error, logf func(string, ...any)) {
	recipient, _ := args["recipient"].(string)
	message, _ := args["message"].(string)
	confirm, _ := args["confirm"].(bool)
	if recipient == "" || message == "" {
		writeMCPText(writeJSON, id, "talk_to_agent: 'recipient' and 'message' are required", nil, true)
		return
	}
	target, err := mcpResolveRecipient(daemonURL, recipient)
	if err != nil {
		writeMCPText(writeJSON, id, fmt.Sprintf("talk_to_agent: %v", err), nil, true)
		return
	}
	if confirm {
		// Talk-request flow: register, long-poll for verdict.
		fromAgent := mcpSelfAgentID(daemonURL) // empty if unknown
		body, _ := json.Marshal(map[string]any{
			"fromAgent": fromAgent, "toAgent": target, "message": message,
		})
		var reg struct {
			ID string `json:"id"`
		}
		if err := httpPostJSON(daemonURL+"/api/talk/request", body, &reg); err != nil {
			writeMCPText(writeJSON, id, fmt.Sprintf("talk_to_agent: %v", err), nil, true)
			return
		}
		// Long-poll up to 10 minutes.
		client := &http.Client{Timeout: 11 * time.Minute}
		wr, err := client.Get(fmt.Sprintf("%s/api/talk/%s/await?timeout=600", daemonURL, reg.ID))
		if err != nil {
			writeMCPText(writeJSON, id, fmt.Sprintf("talk_to_agent: await failed: %v", err), nil, true)
			return
		}
		defer wr.Body.Close()
		var dec struct {
			Status, Reason string
		}
		_ = json.NewDecoder(wr.Body).Decode(&dec)
		writeMCPText(writeJSON, id, fmt.Sprintf("talk_to_agent: %s%s", dec.Status, reasonSuf(dec.Reason)),
			map[string]any{"status": dec.Status, "reason": dec.Reason}, dec.Status != "delivered")
		return
	}
	// wait_for_reply registers a talk and long-polls until the recipient calls reply_to_talk.
	waitForReply, _ := args["wait_for_reply"].(bool)
	waitSec := 600
	if v, ok := args["wait_seconds"].(float64); ok && v > 0 {
		waitSec = int(v)
	}
	if waitForReply {
		fromAgent := mcpSelfAgentID(daemonURL)
		fromLabel := fromAgent
		if fromLabel == "" {
			fromLabel = "unknown-sender"
		}
		// replyOnly sends at once and skips the Allow/Deny banner.
		regBody, _ := json.Marshal(map[string]any{
			"fromAgent": fromAgent, "toAgent": target, "message": message,
			"replyOnly": true,
		})
		var reg struct {
			ID string `json:"id"`
		}
		if err := httpPostJSON(daemonURL+"/api/talk/request", regBody, &reg); err != nil {
			writeMCPText(writeJSON, id, fmt.Sprintf("talk_to_agent: register failed: %v", err), nil, true)
			return
		}
		// Type the message with the reply-expected prefix.
		prefixed := fmt.Sprintf(
			"[talk from %s · REPLY-EXPECTED · talk_id=%s · use reply_to_talk(talk_id=%q, message=...) when done]\n\n%s",
			fromLabel, reg.ID, reg.ID, message,
		)
		sendBody, _ := json.Marshal(map[string]any{"text": prefixed, "enter": true})
		var sendResp struct {
			Pane string `json:"pane"`
		}
		if err := httpPostJSON(daemonURL+"/api/session/send/"+target, sendBody, &sendResp); err != nil {
			writeMCPText(writeJSON, id, fmt.Sprintf("talk_to_agent: send failed: %v", err), nil, true)
			return
		}
		// Long-poll for the recipient's reply_to_talk call. Server times out
		// at waitSec; client gives a small buffer on top.
		client := &http.Client{Timeout: time.Duration(waitSec+10) * time.Second}
		awaitURL := fmt.Sprintf("%s/api/talk/%s/await-reply?timeout=%d", daemonURL, reg.ID, waitSec)
		wr, err := client.Get(awaitURL)
		if err != nil {
			writeMCPText(writeJSON, id, fmt.Sprintf("talk_to_agent: await-reply failed: %v", err), nil, true)
			return
		}
		defer wr.Body.Close()
		var dec struct {
			Status string `json:"status"`
			Reply  string `json:"reply"`
		}
		_ = json.NewDecoder(wr.Body).Decode(&dec)
		if dec.Status != "replied" {
			writeMCPText(writeJSON, id,
				fmt.Sprintf("delivered to %s with talk_id=%s, but recipient did not reply within %ds. The talk has been closed; a later reply_to_talk(%q, ...) call will fail with \"talk not found\". Re-send the message with a longer wait_seconds if you need more time.",
					sendResp.Pane, reg.ID, waitSec, reg.ID),
				map[string]any{"talk_id": reg.ID, "status": dec.Status, "timed_out": true}, false)
			return
		}
		writeMCPText(writeJSON, id,
			fmt.Sprintf("Reply from %s (talk_id=%s):\n\n%s", target, reg.ID, dec.Reply),
			map[string]any{"talk_id": reg.ID, "status": "replied", "reply": dec.Reply, "from": target}, false)
		return
	}
	// Plain direct delivery: type + Enter, no banner, no reply expectation.
	body, _ := json.Marshal(map[string]any{"text": message, "enter": true})
	var resp struct {
		OK   bool   `json:"ok"`
		Pane string `json:"pane"`
	}
	if err := httpPostJSON(daemonURL+"/api/session/send/"+target, body, &resp); err != nil {
		writeMCPText(writeJSON, id, fmt.Sprintf("talk_to_agent: %v", err), nil, true)
		return
	}
	writeMCPText(writeJSON, id, fmt.Sprintf("delivered to %s", resp.Pane),
		map[string]any{"delivered": true, "pane": resp.Pane}, false)
}

// handleReplyToTalk is the recipient end of the request/reply pattern.
func handleReplyToTalk(daemonURL string, args map[string]any, id any, writeJSON func(any) error, logf func(string, ...any)) {
	talkID, _ := args["talk_id"].(string)
	message, _ := args["message"].(string)
	if talkID == "" || message == "" {
		writeMCPText(writeJSON, id, "reply_to_talk: 'talk_id' and 'message' are required", nil, true)
		return
	}
	body, _ := json.Marshal(map[string]any{"message": message})
	resp, err := http.Post(daemonURL+"/api/talk/"+talkID+"/reply", "application/json", bytes.NewReader(body))
	if err != nil {
		writeMCPText(writeJSON, id, fmt.Sprintf("reply_to_talk: %v", err), nil, true)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		bb, _ := io.ReadAll(resp.Body)
		writeMCPText(writeJSON, id, fmt.Sprintf("reply_to_talk: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(bb))), nil, true)
		return
	}
	writeMCPText(writeJSON, id, fmt.Sprintf("✓ replied to talk_id=%s", talkID),
		map[string]any{"replied": true, "talk_id": talkID}, false)
}

// mcpResolveRecipient turns alias/agent-id/pane-id/sid-prefix into a canonical
// agent id by querying the registry. Mirrors wrapper.go's CLI helper.
func mcpResolveRecipient(daemonURL, recipient string) (string, error) {
	var data struct {
		Panes []*PaneRegistration `json:"panes"`
	}
	if err := httpJSON(daemonURL+"/api/panes", &data); err != nil {
		return "", err
	}
	for _, r := range data.Panes {
		if r.Alias != "" && strings.EqualFold(r.Alias, recipient) {
			return r.AgentID, nil
		}
	}
	for _, r := range data.Panes {
		if r.AgentID == recipient {
			return r.AgentID, nil
		}
	}
	if strings.HasPrefix(recipient, "%") {
		for _, r := range data.Panes {
			if r.PaneID == recipient {
				return r.AgentID, nil
			}
		}
	}
	if len(recipient) >= 6 {
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
	// Bare tool name — match if exactly one agent of that tool is registered.
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
		return "", fmt.Errorf("multiple %s agents — be more specific: %v", recipient, toolMatches)
	}
	return "", fmt.Errorf("no agent matches %q (try the list_agents tool first)", recipient)
}

// mcpSelfAgentID looks up the registered agent for our pane. Returns ""
// if not in tmux or not registered — that's fine for talk-request fromAgent.
func mcpSelfAgentID(daemonURL string) string {
	pane := detectMyTmuxPane()
	if pane == "" {
		return ""
	}
	var data struct {
		Panes []*PaneRegistration `json:"panes"`
	}
	if httpJSON(daemonURL+"/api/panes", &data) != nil {
		return ""
	}
	for _, r := range data.Panes {
		if r.PaneID == pane {
			return r.AgentID
		}
	}
	return ""
}

// handleRegisterSelf registers the calling pane and returns its registration.
func handleRegisterSelf(daemonURL string, id any, writeJSON func(any) error, logf func(string, ...any)) {
	pane := detectMyTmuxPane()
	if pane == "" {
		writeMCPText(writeJSON, id, "Not running inside tmux — can't register a pane I don't have.", nil, true)
		return
	}
	reg, created, err := ensureSelfRegisteredVerbose(daemonURL, pane)
	if err != nil {
		writeMCPText(writeJSON, id, fmt.Sprintf("register_self: %v", err), nil, true)
		return
	}
	verb := "already registered"
	if created {
		verb = "registered"
	}
	writeMCPText(writeJSON, id,
		fmt.Sprintf("✓ %s as %s (alias: %s, tool: %s, pane: %s, cwd: %s)",
			verb, reg.AgentID, orDash(reg.Alias), reg.Tool, reg.PaneID, reg.Cwd),
		map[string]any{"registration": reg, "created": created}, false)
}

// ensureSelfRegistered registers the pane if needed; callers may ignore the error.
func ensureSelfRegistered(daemonURL, pane string) error {
	_, _, err := ensureSelfRegisteredVerbose(daemonURL, pane)
	return err
}

// ensureSelfRegisteredVerbose registers the pane as "<tool>:pane-<paneId>" when no entry exists.
// created is false when an entry already existed.
func ensureSelfRegisteredVerbose(daemonURL, pane string) (*PaneRegistration, bool, error) {
	var data struct {
		Panes []*PaneRegistration `json:"panes"`
	}
	if err := httpJSON(daemonURL+"/api/panes", &data); err != nil {
		return nil, false, err
	}
	for _, r := range data.Panes {
		if r.PaneID == pane {
			return r, false, nil
		}
	}
	// Need to create one. Detect tool + cwd from parent process.
	tool, cwd, pid := detectSelfToolCwdPid()
	sessionID := fmt.Sprintf("pane-%s", strings.TrimPrefix(pane, "%"))
	body, _ := json.Marshal(map[string]any{
		"tool":      string(tool),
		"sessionId": sessionID,
		"paneId":    pane,
		"cwd":       cwd,
		"pid":       pid,
		"source":    "register_self",
	})
	resp, err := http.Post(daemonURL+"/api/pane/register", "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		bb, _ := io.ReadAll(resp.Body)
		return nil, false, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(bb)))
	}
	// Re-fetch to get the canonical registration.
	if err := httpJSON(daemonURL+"/api/panes", &data); err != nil {
		return nil, true, err
	}
	for _, r := range data.Panes {
		if r.PaneID == pane {
			return r, true, nil
		}
	}
	return nil, true, fmt.Errorf("registration succeeded but couldn't read it back")
}

// detectSelfToolCwdPid finds the agent CLI among our ancestors; it falls back to ("unknown", $PWD, our pid).
func detectSelfToolCwdPid() (Tool, string, int) {
	myPid := os.Getpid()
	tree := procTree()
	parents := walkAncestors(tree, myPid)
	// First ancestor whose command is a known agent.
	for _, pid := range parents {
		cmd := procCommandLine(pid)
		switch {
		case strings.Contains(cmd, "claude") && !strings.Contains(cmd, "agent-monitor"):
			return ToolClaude, procCwd(pid), pid
		case strings.Contains(cmd, "codex") && !strings.Contains(cmd, "agent-monitor"):
			return ToolCodex, procCwd(pid), pid
		case strings.Contains(cmd, "cursor-agent"):
			return ToolCursorAgent, procCwd(pid), pid
		case strings.Contains(cmd, "opencode"):
			return ToolOpencode, procCwd(pid), pid
		}
	}
	cwd, _ := os.Getwd()
	return "unknown", cwd, myPid
}

// walkAncestors returns the parent chain from start upward.
func walkAncestors(tree map[int][]int, start int) []int {
	if tree == nil {
		return nil
	}
	parentOf := map[int]int{}
	for parent, kids := range tree {
		for _, k := range kids {
			parentOf[k] = parent
		}
	}
	var out []int
	pid := start
	for i := 0; i < 32; i++ { // safety bound
		p, ok := parentOf[pid]
		if !ok || p <= 1 {
			break
		}
		out = append(out, p)
		pid = p
	}
	return out
}

// procCommandLine returns the full command line of a pid (or "" if it can't
// read it). macOS-friendly: uses `ps -o command=` which is portable enough.
func procCommandLine(pid int) string {
	if pid <= 0 {
		return ""
	}
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "command=").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// detectMyTmuxPane uses $TMUX_PANE, else the pane whose pid is our ancestor (Codex strips the env).
func detectMyTmuxPane() string {
	if pane := os.Getenv("TMUX_PANE"); pane != "" {
		return pane
	}
	panes, err := listTmuxPanes()
	if err != nil || len(panes) == 0 {
		return ""
	}
	tree := procTree()
	if tree == nil {
		return ""
	}
	myPid := os.Getpid()
	// First pane that holds our pid in its subtree.
	for _, p := range panes {
		if p.PanePID == 0 {
			continue
		}
		if isDescendant(tree, p.PanePID, myPid) {
			return p.PaneID
		}
	}
	return ""
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func reasonSuf(s string) string {
	if s == "" {
		return ""
	}
	return " — " + s
}

func (o mcpOrch) handles(req map[string]any) bool {
	if o.thread == "" {
		return false
	}
	params, _ := req["params"].(map[string]any)
	switch name, _ := params["name"].(string); name {
	case "delegate_task", "wait_tasks", "task_status":
		return true
	}
	return false
}

func mcpSearchToolDefs() []map[string]any {
	return []map[string]any{
		{
			"name":        "search_sessions",
			"description": "Search the user's local agent history: every Claude Code, Codex and other coding-agent session that agent-monitor indexed on this machine (prompts, replies, commands, tool output, errors). Returns matching sessions, best match first, with id, agent, title, repo, cwd, last activity, hit count, the top 3 matching snippets (plain text, with the turn number) and a Workspace URL. Use it to list the user's previous sessions about a topic. Query syntax: plain words, \"quoted phrases\", and chips such as tool:Bash, kind:error, is:error, file:path, since:7d.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query": map[string]any{"type": "string", "description": "keywords to find in the transcripts"},
					"agent": map[string]any{"type": "string", "description": "only this agent, e.g. claude or codex"},
					"repo":  map[string]any{"type": "string", "description": "only this repository (folder name)"},
					"since": map[string]any{"type": "string", "description": "only activity since: 24h, 7d, 2w, today, yesterday, monday or YYYY-MM-DD"},
					"limit": map[string]any{"type": "integer", "description": "sessions to return (default 10, max 50)"},
				},
				"required": []string{"query"},
			},
		},
		{
			"name":        "session_context",
			"description": "Read part of a session from the user's local agent history as plain text: the turns around a search hit (user prompt, agent replies, commands with output, edits). Pass the sessionId and the turn from search_sessions. Without turn it returns the last turns. Output is capped at 8 KB.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"sessionId": map[string]any{"type": "string", "description": "session id from search_sessions, e.g. claude:<uuid>, codex:<uuid> or ot_<id>"},
					"turn":      map[string]any{"type": "integer", "description": "turn number of the hit (default: the last turn)"},
					"around":    map[string]any{"type": "integer", "description": "turns to include on each side (default 1, max 5)"},
				},
				"required": []string{"sessionId"},
			},
		},
	}
}

func argStr(args map[string]any, k string) string {
	s, _ := args[k].(string)
	return strings.TrimSpace(s)
}

func argInt(args map[string]any, k string, d int) int {
	if v, ok := args[k].(float64); ok {
		return int(v)
	}
	if s, ok := args[k].(string); ok {
		if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil {
			return n
		}
	}
	return d
}

func chipArg(k, v string) string {
	if strings.ContainsAny(v, " \t\"") {
		return " " + k + `:"` + strings.ReplaceAll(v, `"`, "") + `"`
	}
	return " " + k + ":" + v
}

// plainSnippet drops the <mark> tags and HTML escapes of a search snippet.
func plainSnippet(s string) string { return html.UnescapeString(reMarkTag.ReplaceAllString(s, "")) }

// workspacePath is the Workspace route that opens a hit: driven docs by item, watched docs by turn.
func workspacePath(h searchHit) string {
	q := url.Values{}
	target := firstOf(h.ThreadID, h.Session)
	switch {
	case strings.HasPrefix(h.Session, "ot_"):
		target = h.Session
		q.Set("hit", h.Span)
	case h.DrivenID != "":
		target = h.DrivenID
	default:
		q.Set("hit", strconv.Itoa(h.Turn))
		q.Set("session", h.Session)
	}
	q.Set("ts", strconv.FormatInt(h.Ts, 10))
	return "/w/" + url.PathEscape(target) + "?" + q.Encode()
}

func fmtMs(ms int64) string {
	if ms <= 0 {
		return ""
	}
	return time.UnixMilli(ms).Format("2006-01-02 15:04")
}

func handleSearchSessions(daemonURL string, args map[string]any, id any, writeJSON func(any) error, logf func(string, ...any)) {
	query := argStr(args, "query")
	if query == "" {
		writeMCPText(writeJSON, id, "search_sessions: query is required", nil, true)
		return
	}
	for _, k := range []string{"agent", "repo", "since"} {
		if v := argStr(args, k); v != "" {
			query += chipArg(k, v)
		}
	}
	limit := clampInt(argInt(args, "limit", 10), 1, 50)
	var res searchResult
	if err := httpJSON(daemonURL+"/api/search?"+url.Values{"q": {query}, "limit": {"200"}}.Encode(), &res); err != nil {
		writeMCPText(writeJSON, id, "search_sessions failed: "+err.Error(), nil, true)
		return
	}
	var th struct {
		Threads []struct {
			ID      string   `json:"id"`
			Title   string   `json:"title"`
			Display string   `json:"display"`
			LastAt  int64    `json:"lastAt"`
			Sess    []string `json:"sessions"`
		} `json:"threads"`
	}
	if err := httpJSON(daemonURL+"/api/v3/threads?all=1&driven=1", &th); err != nil {
		logf("search_sessions: threads: %v", err)
	}
	titles := map[string]string{}
	for _, t := range th.Threads {
		titles[t.ID] = firstOf(t.Display, t.Title)
	}
	var order []*mcpSessionResult
	by := map[string]*mcpSessionResult{}
	for _, h := range res.Hits {
		s := by[h.Session]
		if s == nil {
			s = &mcpSessionResult{ID: h.Session, Agent: h.Agent, Repo: h.Repo, Cwd: h.Cwd, URL: daemonURL + workspacePath(h),
				Title: firstOf(titles[h.Session], titles[h.ThreadID], titles[h.DrivenID]), Snippets: []mcpSnippet{}}
			by[h.Session] = s
			order = append(order, s)
		}
		s.Hits++
		s.lastMs = max(s.lastMs, h.Ts)
		if len(s.Snippets) < 3 {
			s.Snippets = append(s.Snippets, mcpSnippet{Text: plainSnippet(h.Snippet), Kind: h.Kind, Turn: h.Turn, Ts: fmtMs(h.Ts)})
		}
	}
	order = order[:min(len(order), limit)]
	var b strings.Builder
	if len(order) == 0 {
		fmt.Fprintf(&b, "No sessions in the local agent history match %q.", query)
	} else {
		fmt.Fprintf(&b, "%d sessions match %q (%d matching lines in all).\n", len(order), query, res.Total)
	}
	out := make([]mcpSessionResult, 0, len(order))
	for i, s := range order {
		s.LastActivity = fmtMs(s.lastMs)
		out = append(out, *s)
		fmt.Fprintf(&b, "\n%d. %s\n   %s · %s · %s · last %s · %d hits\n   %s\n", i+1, firstOf(s.Title, "(untitled)"), s.ID, s.Agent, firstOf(s.Repo, s.Cwd), s.LastActivity, s.Hits, s.URL)
		for _, sn := range s.Snippets {
			fmt.Fprintf(&b, "   - [turn %d, %s] %s\n", sn.Turn, sn.Kind, sn.Text)
		}
	}
	writeMCPText(writeJSON, id, b.String(), map[string]any{"query": query, "total": res.Total, "sessions": out}, false)
}

func handleSessionContext(daemonURL string, args map[string]any, id any, writeJSON func(any) error, logf func(string, ...any)) {
	sid := argStr(args, "sessionId")
	if sid == "" {
		writeMCPText(writeJSON, id, "session_context: sessionId is required", nil, true)
		return
	}
	turn := argInt(args, "turn", 0)
	around := clampInt(argInt(args, "around", 1), 0, 5)
	runs, items, err := sessionTurns(daemonURL, sid, turn)
	if err != nil {
		logf("session_context: items: %v; using the search index", err)
		text, ferr := contextFromIndex(daemonURL, sid, turn, around)
		if ferr != nil {
			writeMCPText(writeJSON, id, "session_context failed: "+err.Error(), nil, true)
			return
		}
		writeMCPText(writeJSON, id, text, nil, false)
		return
	}
	writeMCPText(writeJSON, id, renderTurns(sid, runs, items, turn, around), nil, false)
}

// sessionTurns loads runs and items: a driven thread at once, a watched session paged back until turn is loaded.
func sessionTurns(daemonURL, sid string, turn int) ([]ORun, []OItem, error) {
	if strings.HasPrefix(sid, "ot_") {
		var d OThreadDetail
		if err := httpJSON(daemonURL+"/api/orch/threads/"+url.PathEscape(sid), &d); err != nil {
			return nil, nil, err
		}
		return d.Runs, d.Items, nil
	}
	base := daemonURL + "/api/session/" + url.PathEscape(sid) + "/items"
	var page WatchedItems
	if err := httpJSON(base, &page); err != nil {
		return nil, nil, err
	}
	runs, items := derefRuns(page.Runs), derefItems(page.Items)
	for i := 0; i < 200 && turn > 0 && page.More && page.Earlier != nil && (len(runs) == 0 || minRunIndex(runs) > max(1, turn-5)); i++ {
		var prev WatchedItems
		if err := httpJSON(fmt.Sprintf("%s?before=%d", base, *page.Earlier), &prev); err != nil {
			return nil, nil, err
		}
		runs, items = append(derefRuns(prev.Runs), runs...), append(derefItems(prev.Items), items...)
		page.More, page.Earlier = prev.More, prev.Earlier
	}
	return runs, items, nil
}

func derefRuns(in []*ORun) []ORun {
	out := make([]ORun, 0, len(in))
	for _, r := range in {
		if r != nil {
			out = append(out, *r)
		}
	}
	return out
}

func derefItems(in []*OItem) []OItem {
	out := make([]OItem, 0, len(in))
	for _, it := range in {
		if it != nil {
			out = append(out, *it)
		}
	}
	return out
}

func minRunIndex(runs []ORun) int {
	m := runs[0].Index
	for _, r := range runs {
		m = min(m, r.Index)
	}
	return m
}

func renderTurns(sid string, runs []ORun, items []OItem, turn, around int) string {
	if len(runs) == 0 {
		return fmt.Sprintf("Session %s has no turns.", sid)
	}
	last := 0
	for _, r := range runs {
		last = max(last, r.Index)
	}
	if turn <= 0 || turn > last {
		turn = last
	}
	lo, hi := turn-around, turn+around
	sel := map[string]ORun{}
	for _, r := range runs {
		if r.Index >= lo && r.Index <= hi {
			sel[r.ID] = r
		}
	}
	sort.Slice(runs, func(i, j int) bool { return runs[i].Index < runs[j].Index })
	byRun := map[string][]OItem{}
	for _, it := range items {
		if _, ok := sel[it.RunID]; ok {
			byRun[it.RunID] = append(byRun[it.RunID], it)
		}
	}
	type seg struct {
		idx  int
		text string
	}
	var segs []seg
	size := 0
	for _, r := range runs {
		if _, ok := sel[r.ID]; !ok {
			continue
		}
		var b strings.Builder
		mark := ""
		if r.Index == turn {
			mark = " (hit)"
		}
		fmt.Fprintf(&b, "\n## Turn %d%s · %s\n", r.Index, mark, fmtMs(r.StartedAt))
		its := byRun[r.ID]
		sort.SliceStable(its, func(i, j int) bool { return its[i].StartedAt < its[j].StartedAt })
		if len(its) == 0 && r.Prompt != "" {
			fmt.Fprintf(&b, "User: %s\n", r.Prompt)
		}
		for _, it := range its {
			b.WriteString(itemText(it))
		}
		segs = append(segs, seg{r.Index, b.String()})
		size += b.Len()
	}
	if len(segs) == 0 {
		return fmt.Sprintf("Session %s has no turns %d to %d (last turn %d).", sid, lo, hi, last)
	}
	// Over the cap, the turns farthest from the hit go first.
	dropped := 0
	for size > sessionContextMax-200 && len(segs) > 1 {
		far := 0
		for i, sg := range segs {
			if abs(sg.idx-turn) > abs(segs[far].idx-turn) {
				far = i
			}
		}
		size -= len(segs[far].text)
		segs = append(segs[:far], segs[far+1:]...)
		dropped++
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Session %s, turns %d to %d of %d\n", sid, segs[0].idx, segs[len(segs)-1].idx, last)
	if dropped > 0 {
		fmt.Fprintf(&b, "[%d turns left out to stay under 8 KB]\n", dropped)
	}
	for _, sg := range segs {
		b.WriteString(sg.text)
	}
	return clipContext(b.String())
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

func itemText(it OItem) string {
	oneLine := func(s string, n int) string { return clipString(strings.TrimSpace(s), n) }
	switch it.Kind {
	case "user":
		return "User: " + oneLine(it.Text, 2000) + "\n"
	case "agent":
		return "Agent: " + oneLine(it.Text, 2000) + "\n"
	case "command":
		exit := ""
		if it.ExitCode != nil && *it.ExitCode != 0 {
			exit = fmt.Sprintf(" (exit %d)", *it.ExitCode)
		}
		s := "$ " + oneLine(firstOf(it.Command, it.Text), 400) + exit + "\n"
		if o := strings.TrimSpace(it.Output); o != "" {
			s += "  " + strings.ReplaceAll(oneLine(o, 600), "\n", "\n  ") + "\n"
		}
		return s
	case "fileChange":
		paths := make([]string, 0, len(it.Files))
		for _, f := range it.Files {
			paths = append(paths, f.Path)
		}
		return "Edited: " + strings.Join(paths, ", ") + "\n"
	case "error":
		return "Error: " + oneLine(it.Text, 600) + "\n"
	case "reasoning", "plan":
		return ""
	}
	t := strings.TrimSpace(it.Command + " " + it.Text)
	if t == "" {
		return ""
	}
	return "Tool: " + oneLine(t, 300) + "\n"
}

func clipContext(s string) string {
	if len(s) <= sessionContextMax {
		return s
	}
	const tail = "\n[clipped at 8 KB]"
	cut := sessionContextMax - len(tail)
	for cut > 0 && s[cut]&0xC0 == 0x80 {
		cut--
	}
	return s[:cut] + tail
}

// contextFromIndex answers from the search index when the transcript cannot be read as items.
func contextFromIndex(daemonURL, sid string, turn, around int) (string, error) {
	var res searchResult
	if err := httpJSON(daemonURL+"/api/search?"+url.Values{"q": {"session:" + sid}, "limit": {"200"}}.Encode(), &res); err != nil {
		return "", err
	}
	if len(res.Hits) == 0 {
		return "", fmt.Errorf("no indexed lines for %s", sid)
	}
	last := 0
	for _, h := range res.Hits {
		last = max(last, h.Turn)
	}
	if turn <= 0 || turn > last {
		turn = last
	}
	hits := res.Hits
	sort.Slice(hits, func(i, j int) bool { return hits[i].Ts < hits[j].Ts })
	var b strings.Builder
	fmt.Fprintf(&b, "Session %s, turns %d to %d (from the search index, lines clipped)\n", sid, max(turn-around, 1), turn+around)
	cur := -1
	for _, h := range hits {
		if h.Turn < turn-around || h.Turn > turn+around {
			continue
		}
		if h.Turn != cur {
			cur = h.Turn
			fmt.Fprintf(&b, "\n## Turn %d · %s\n", h.Turn, fmtMs(h.Ts))
		}
		fmt.Fprintf(&b, "%s: %s\n", h.Kind, plainSnippet(h.Snippet))
	}
	return clipContext(b.String()), nil
}

var reMarkTag = regexp.MustCompile(`</?mark>`)

type mcpSnippet struct {
	Text string `json:"text"`
	Kind string `json:"kind"`
	Turn int    `json:"turn"`
	Ts   string `json:"ts"`
}

type mcpSessionResult struct {
	ID           string       `json:"id"`
	Agent        string       `json:"agent"`
	Title        string       `json:"title"`
	Repo         string       `json:"repo"`
	Cwd          string       `json:"cwd"`
	LastActivity string       `json:"lastActivity"`
	Hits         int          `json:"hits"`
	Snippets     []mcpSnippet `json:"snippets"`
	URL          string       `json:"url"`
	lastMs       int64
}

const sessionContextMax = 8 << 10
