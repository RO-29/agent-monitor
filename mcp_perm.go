package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// runMCPPermServer implements an MCP-over-stdio server that Claude Code can
// configure via:
//
//	"mcpServers": {
//	  "agent-monitor": {
//	    "command": "/Users/rohit/agent-monitor/agent-monitor",
//	    "args":    ["mcp-perm-server"]
//	  }
//	}
//
// and then invoke with:
//
//	claude --permission-prompt-tool mcp__agent-monitor__permission_prompt …
//
// Whenever Claude needs permission for a tool call, it calls our tool. We
// forward the request to the running agent-monitor daemon (HTTP), then
// long-poll for the user's UI decision and return it.
//
// CRITICAL: stdout is reserved for JSON-RPC responses. Anything else goes to
// stderr — the MCP framing breaks if we mix logging into stdout.
func runMCPPermServer() {
	logf := func(format string, args ...any) { fmt.Fprintf(os.Stderr, "[mcp-perm] "+format+"\n", args...) }

	port := "7777"
	if v := os.Getenv("AGENT_MONITOR_PORT"); v != "" {
		port = v
	}
	daemonURL := "http://127.0.0.1:" + port

	logf("starting; daemon=%s", daemonURL)

	in := bufio.NewReader(os.Stdin)
	var stdoutMu sync.Mutex
	writeRaw := func(b []byte) error {
		stdoutMu.Lock()
		defer stdoutMu.Unlock()
		_, err := os.Stdout.Write(append(b, '\n'))
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
					"tools": []map[string]any{
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
							"name": "who_am_i",
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
							"name": "read_pane",
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
							"name": "talk_to_agent",
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
					},
				},
			})

		case "tools/call":
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
	// Dispatch agent-coordination tools BEFORE the permission_prompt path —
	// they don't need the same long-poll plumbing. Each writes its own MCP
	// result and returns. The legacy permission_prompt flow falls through
	// only on an explicit "permission_prompt" name; unknown names get a
	// JSON-RPC error so a typo doesn't long-poll the daemon for 10 minutes.
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

	// Long-poll for the user's decision. Cap at 10 minutes — anything longer
	// and Claude probably gave up too. Use a long timeout on the HTTP client
	// so the connection stays open while the daemon waits for the click.
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
	// Claude's MCP permission_prompt protocol REQUIRES `updatedInput` whenever
	// behavior is "allow" — the field tells Claude what arguments to actually
	// run the tool with. The web UI doesn't edit the input, so we echo the
	// original. Without this, Claude treats the response as malformed and the
	// tool call hangs / silently denies (the bug behind "approval not working").
	if dec.Behavior == "allow" && dec.UpdatedInput == nil {
		dec.UpdatedInput = input
	}
	logf("decision for %s: %s (%s)", reg.ID, dec.Behavior, dec.Reason)
	writeMCPResult(writeJSON, id, dec)
}

// writeMCPResult sends the permission response back as the MCP tool result.
// Claude Code expects the structured permission response inside the tool
// `result`. We wrap as the standard tool-use content format and ALSO embed
// the structured response, hoping at least one is consumed correctly.
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
			"structuredContent": payload,
			"isError":           false,
		},
	})
}

func deny(reason string) PermissionResponse {
	return PermissionResponse{Behavior: "deny", Reason: reason}
}

// ─── agent-coordination MCP tools ───────────────────────────────────────────
// Each handler writes a standard MCP tool result with a single text content
// block (JSON for structured data, plain text for prose). Errors set
// isError=true so the calling agent sees a tool error.

// writeMCPText sends a text/JSON result. obj=nil emits text only; non-nil
// values are placed in `structuredContent`. MCP spec wants structuredContent
// to be an OBJECT (record), not an array — Claude rejects arrays at the
// validation layer (Codex is lenient). If obj is a slice/array, wrap it
// under "items" so the wire format always satisfies the validator.
func writeMCPText(writeJSON func(any) error, id any, text string, obj any, isErr bool) {
	content := []map[string]any{{"type": "text", "text": text}}
	result := map[string]any{"content": content, "isError": isErr}
	if obj != nil {
		result["structuredContent"] = ensureObject(obj)
	}
	_ = writeJSON(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

// ensureObject wraps non-object values so MCP's structuredContent validator
// (which expects a record) doesn't reject the response.
func ensureObject(v any) any {
	switch v.(type) {
	case map[string]any, map[string]string:
		return v
	case nil:
		return map[string]any{}
	}
	// Slice / array / scalar / anything else — wrap.
	return map[string]any{"items": v}
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

// handleWhoAmI returns the registered identity for the calling pane. We
// detect it via $TMUX_PANE first (the easy case), then fall back to walking
// our process tree — Codex strips env when spawning MCP children, so the env
// is empty there even though we're a grandchild of the tmux pane's shell.
func handleWhoAmI(daemonURL string, id any, writeJSON func(any) error, logf func(string, ...any)) {
	pane := detectMyTmuxPane()
	if pane == "" {
		writeMCPText(writeJSON, id, "Not inside tmux — no registered pane. (Tried $TMUX_PANE and process-tree descent; both came up empty.)", nil, false)
		return
	}
	var data struct{ Panes []*PaneRegistration `json:"panes"` }
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

// handleListAgents enumerates every registered agent AND every live tmux
// pane that LOOKS like an agent (running claude/codex/cursor-agent/opencode
// or node) but isn't registered yet — so the calling agent sees the full
// picture, not just the registered subset.
func handleListAgents(daemonURL string, id any, writeJSON func(any) error, logf func(string, ...any)) {
	myPane := detectMyTmuxPane()
	if myPane != "" {
		_ = ensureSelfRegistered(daemonURL, myPane)
	}
	_, _ = http.Post(daemonURL+"/api/panes/sweep", "application/json", nil)

	var regData struct{ Panes []*PaneRegistration `json:"panes"` }
	if err := httpJSON(daemonURL+"/api/panes", &regData); err != nil {
		writeMCPText(writeJSON, id, fmt.Sprintf("Couldn't reach agent-monitor daemon: %v", err), nil, true)
		return
	}
	// Also fetch every live tmux pane to detect agents that exist but
	// haven't registered. node/claude.exe/codex/cursor-agent/opencode all
	// count as "probably an agent."
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
				tool:   guessToolFromCommand(p.CurrentCommand),
				cwd:    p.CurrentPath, status: "unregistered",
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
		"agents":             rowMaps,
		"registeredCount":    len(regData.Panes),
		"unregisteredCount":  unregistered,
		"totalAgentLikePanes": len(rows),
		"myPane":             myPane,
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

// looksLikeAgent returns true if a tmux pane's current_command resembles an
// agent CLI. We pattern-match on common values: agents typically run as
// `node` (claude/codex are node apps), `claude.exe` (Claude Code's
// post-build name on macOS), `codex`, `cursor-agent`, or `opencode`.
func looksLikeAgent(cmd string) bool {
	c := strings.ToLower(cmd)
	switch c {
	case "node", "claude.exe", "claude", "codex", "cursor-agent", "opencode":
		return true
	}
	return strings.Contains(c, "claude") || strings.Contains(c, "codex") ||
		strings.Contains(c, "cursor-agent") || strings.Contains(c, "opencode")
}

// guessToolFromCommand maps a pane's current_command to a Tool enum string.
// "node" is ambiguous (claude AND codex run as node) so we return "node?"
// to flag the uncertainty.
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

// handleTalkToAgent delivers a message to another agent's pane. Default mode
// bypasses the human-confirmation banner so agents can chain calls without
// needing a click each time. Set confirm=true to use the talk-request flow
// (the calling agent will block until the recipient's user clicks Allow/Deny).
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
		var reg struct{ ID string `json:"id"` }
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
	// Reply protocol: register a Talk so we have a stable talk_id, embed
	// it in the prefix the recipient sees, then long-poll /await-reply
	// until the recipient explicitly calls reply_to_talk. This is the
	// right "ask another agent a question" mode — no polling heuristics,
	// no stability detection, the recipient says "I'm done" themselves.
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
		// Register a Talk so we get a stable id. The reply protocol bypasses
		// the user-confirmation flow — message ships immediately and the talk
		// lives until reply. replyOnly tells the daemon to mark it pending_reply
		// and skip the talk-request broadcast (no Allow/Deny banner).
		regBody, _ := json.Marshal(map[string]any{
			"fromAgent": fromAgent, "toAgent": target, "message": message,
			"replyOnly": true,
		})
		var reg struct{ ID string `json:"id"` }
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
		var sendResp struct{ Pane string `json:"pane"` }
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

// handleReplyToTalk handles the recipient end of the request/reply pattern.
// The recipient finished their work and is calling this with the talk_id
// they received in the incoming message's prefix.
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

// httpPostJSON is the POST counterpart to httpJSON.
func httpPostJSON(url string, body []byte, out any) error {
	resp, err := http.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		bb, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(bb)))
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// mcpResolveRecipient turns alias/agent-id/pane-id/sid-prefix into a canonical
// agent id by querying the registry. Mirrors wrapper.go's CLI helper.
func mcpResolveRecipient(daemonURL, recipient string) (string, error) {
	var data struct{ Panes []*PaneRegistration `json:"panes"` }
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
	var data struct{ Panes []*PaneRegistration `json:"panes"` }
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

// handleRegisterSelf is the explicit "I want my pane in the registry" tool.
// Returns the (possibly newly-created) registration so the agent can confirm
// what its agent id is.
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

// ensureSelfRegistered makes sure the calling pane has SOME registration.
// Returns nil on success, error on failure. Errors are non-fatal for the
// caller — list_agents just continues without it.
func ensureSelfRegistered(daemonURL, pane string) error {
	_, _, err := ensureSelfRegisteredVerbose(daemonURL, pane)
	return err
}

// ensureSelfRegisteredVerbose is the workhorse: look up the registry, and
// if no entry exists for this pane, synthesize one from process info and
// POST /api/pane/register. Returns the resulting registration plus a flag
// indicating whether we created it (true) or it already existed (false).
//
// Synthesis strategy: walk our parent process to find the agent CLI's
// command name, map "node|claude|codex|cursor-agent|opencode" to a Tool,
// and use "<tool>:pane-<paneId>" as both agent id and session id. That's
// stable per pane (a pane can only host one agent at a time, so this id
// remains unique) and human-readable.
func ensureSelfRegisteredVerbose(daemonURL, pane string) (*PaneRegistration, bool, error) {
	var data struct{ Panes []*PaneRegistration `json:"panes"` }
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

// detectSelfToolCwdPid walks up our process tree to find the agent CLI
// process and returns (tool, cwd, pid). Falls back to ("unknown", $PWD,
// our pid) if anything fails — better to register with imperfect metadata
// than to refuse.
func detectSelfToolCwdPid() (Tool, string, int) {
	myPid := os.Getpid()
	tree := procTree()
	parents := walkAncestors(tree, myPid)
	// Inspect each ancestor's command, return the first that matches a
	// known agent tool name. Skip our own MCP server pid + the immediate
	// daemon-launched shell wrappers.
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

// walkAncestors returns the chain of parent pids from `start` upward to init.
// We need our own walker because procTree() is parent→children; here we want
// child→parent. Builds a child→parent map on the fly.
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

// detectMyTmuxPane finds the tmux pane id this MCP server lives in. Tries
// the simple $TMUX_PANE env first; falls back to walking our process tree
// upward and matching against `tmux list-panes` pane_pids.
//
// Why the fallback: Codex (and some other MCP clients) spawn stdio MCP
// servers with a sanitized env that doesn't propagate TMUX_PANE. The
// process tree, however, is intact: codex was spawned from a shell inside
// the pane, then codex spawned us, so our pid is a descendant of pane_pid.
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
	// For each pane, check if our pid is a descendant of pane_pid. We pick
	// the FIRST match because process trees don't normally have a pid in
	// multiple panes' subtrees.
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
