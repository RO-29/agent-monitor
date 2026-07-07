package main

import (
	"bufio"
	"encoding/json"
	"os"
	"sort"
	"strings"
)

// codexAggregate is the rolled-up metadata pulled from a codex rollout JSONL.
// Mirrors claudeAggregate so the UI shows comparable info regardless of tool.
type codexAggregate struct {
	Tokens         TokenUsage
	ToolUsage      map[string]int
	Files          map[string]struct{}
	MessageCount   int
	Title          string
	FirstMessage   string
	Model          string
	Mode           string
	LastMessage    string
	LastActivityMs int64
}

// scanCodexJSONL walks a codex rollout file. Codex's event model is more
// fragmented than Claude's: each tool invocation has a function_call /
// custom_tool_call (the "use") and a separate event_msg/*_end (the "result").
// We pair them by call_id, similar to Claude's tool_use_id pairing. Reasoning
// turns are encrypted but we surface them as messages so the timeline shows
// thinking happened.
func scanCodexJSONL(path string, detailOut *SessionDetail, maxMessages int) (codexAggregate, error) {
	agg := codexAggregate{
		ToolUsage: map[string]int{},
		Files:     map[string]struct{}{},
	}

	f, err := os.Open(path)
	if err != nil {
		return agg, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)

	pending := map[string]*DetailTool{} // call_id -> tool

	for sc.Scan() {
		var j map[string]any
		if err := json.Unmarshal(sc.Bytes(), &j); err != nil {
			continue
		}
		topType, _ := j["type"].(string)
		ts := parseTimestamp(j["timestamp"])
		payload, _ := j["payload"].(map[string]any)
		if payload == nil {
			continue
		}
		innerType, _ := payload["type"].(string)

		switch topType {
		case "session_meta":
			cwd, _ := payload["cwd"].(string)
			if cwd != "" && agg.LastActivityMs == 0 {
				agg.LastActivityMs = ts
			}
			// model is on turn_context, but capture cli_version style hints if present
			continue

		case "turn_context":
			if mode, ok := payload["approval_policy"].(string); ok && agg.Mode == "" {
				agg.Mode = mode
			}
			if m, ok := payload["model"].(string); ok && m != "" {
				agg.Model = m // last turn_context wins → the model actually in use
			}
			continue
		}

		// event_msg dispatch — these are codex's lifecycle events. The *_end
		// variants carry the actual tool outputs.
		if topType == "event_msg" {
			callID, _ := payload["call_id"].(string)
			switch innerType {
			case "user_message":
				msg, _ := payload["message"].(string)
				if msg == "" {
					continue
				}
				agg.MessageCount++
				// Codex injects the workspace's AGENTS.md and various system
				// preambles as user messages — those are noise for titling.
				// Use the first message that doesn't look like a system prompt.
				if agg.FirstMessage == "" && !codexLooksLikeSystemPrompt(msg) {
					agg.FirstMessage = clipString(msg, 240)
				}
				agg.LastActivityMs = ts
				if detailOut != nil {
					detailOut.Messages = append(detailOut.Messages, DetailMsg{
						Ts: ts, Role: "user", Text: clipString(msg, 4000),
					})
				}
			case "agent_message":
				msg, _ := payload["message"].(string)
				if msg == "" {
					continue
				}
				agg.MessageCount++
				agg.LastMessage = clipString(msg, 240)
				agg.LastActivityMs = ts
				if detailOut != nil {
					detailOut.Messages = append(detailOut.Messages, DetailMsg{
						Ts: ts, Role: "assistant", Text: clipString(msg, 6000),
					})
				}
			case "task_started":
				agg.LastActivityMs = ts
			case "task_complete":
				if last, _ := payload["last_agent_message"].(string); last != "" {
					agg.LastMessage = clipString(last, 240)
				}
				agg.LastActivityMs = ts
			case "exec_command_end":
				// Pair the matching function_call (or custom_tool_call) with the
				// stdout/stderr/aggregated_output we now have.
				if t, ok := pending[callID]; ok {
					stdout, _ := payload["stdout"].(string)
					stderr, _ := payload["stderr"].(string)
					agg2, _ := payload["aggregated_output"].(string)
					out := agg2
					if out == "" {
						out = stdout
					}
					if stderr != "" {
						out = out + "\n--- STDERR ---\n" + stderr
					}
					t.Result = clipString(out, 12000)
					updateDetailToolResult(detailOut, callID, t.Result)
				}
			case "mcp_tool_call_end":
				if t, ok := pending[callID]; ok {
					result, _ := payload["result"].(map[string]any)
					txt := codexExtractMCPResult(result)
					t.Result = clipString(txt, 12000)
					updateDetailToolResult(detailOut, callID, t.Result)
				}
			case "patch_apply_end":
				if t, ok := pending[callID]; ok {
					stdout, _ := payload["stdout"].(string)
					stderr, _ := payload["stderr"].(string)
					out := stdout
					if stderr != "" {
						out = out + "\n--- STDERR ---\n" + stderr
					}
					t.Result = clipString(out, 12000)
					updateDetailToolResult(detailOut, callID, t.Result)
					if changes, _ := payload["changes"].(map[string]any); changes != nil {
						for fp := range changes {
							agg.Files[fp] = struct{}{}
						}
					}
				}
			case "web_search_end":
				if t, ok := pending[callID]; ok {
					query, _ := payload["query"].(string)
					t.Result = clipString("query: "+query, 12000)
					updateDetailToolResult(detailOut, callID, t.Result)
				}
			case "token_count":
				// Codex emits token_count events with shape:
				//   info.total_token_usage = {input_tokens, output_tokens,
				//     cached_input_tokens, reasoning_output_tokens, total_tokens}
				// `last_token_usage` is the same shape for the most-recent turn.
				// We track the running totals.
				if info, _ := payload["info"].(map[string]any); info != nil {
					if tot, _ := info["total_token_usage"].(map[string]any); tot != nil {
						if v, ok := tot["input_tokens"].(float64); ok {
							agg.Tokens.Input = int64(v)
						}
						if v, ok := tot["output_tokens"].(float64); ok {
							agg.Tokens.Output = int64(v)
						}
						if v, ok := tot["cached_input_tokens"].(float64); ok {
							agg.Tokens.CacheRead = int64(v)
						}
					}
				}
			}
			continue
		}

		// response_item dispatch — function_call / custom_tool_call etc.
		if topType == "response_item" {
			switch innerType {
			case "function_call":
				name, _ := payload["name"].(string)
				if name == "" {
					continue
				}
				callID, _ := payload["call_id"].(string)
				agg.ToolUsage[name]++
				agg.LastActivityMs = ts
				argsStr, _ := payload["arguments"].(string)
				if argsStr == "" {
					if b, err := json.Marshal(payload["arguments"]); err == nil {
						argsStr = string(b)
					}
				}
				codexFilesFromArgs(name, argsStr, agg.Files)
				if detailOut != nil {
					dt := DetailTool{ID: callID, Name: name, Ts: ts, Args: clipString(argsStr, 8000)}
					pending[callID] = &dt
					detailOut.ToolCalls = append(detailOut.ToolCalls, dt)
				}
			case "custom_tool_call":
				name, _ := payload["name"].(string)
				if name == "" {
					continue
				}
				callID, _ := payload["call_id"].(string)
				agg.ToolUsage[name]++
				agg.LastActivityMs = ts
				inputStr, _ := payload["input"].(string)
				codexFilesFromArgs(name, inputStr, agg.Files)
				if detailOut != nil {
					dt := DetailTool{ID: callID, Name: name, Ts: ts, Args: clipString(inputStr, 8000)}
					pending[callID] = &dt
					detailOut.ToolCalls = append(detailOut.ToolCalls, dt)
				}
			case "function_call_output":
				callID, _ := payload["call_id"].(string)
				out, _ := payload["output"].(string)
				if t, ok := pending[callID]; ok && t.Result == "" {
					// Some tools (like list_mcp_resources) report output here
					// rather than via event_msg/*_end. Use whichever fires first.
					t.Result = clipString(out, 12000)
					updateDetailToolResult(detailOut, callID, t.Result)
				}
			case "custom_tool_call_output":
				callID, _ := payload["call_id"].(string)
				out, _ := payload["output"].(string)
				if t, ok := pending[callID]; ok && t.Result == "" {
					t.Result = clipString(out, 12000)
					updateDetailToolResult(detailOut, callID, t.Result)
				}
			case "web_search_call":
				name := "web_search"
				callID, _ := payload["call_id"].(string)
				agg.ToolUsage[name]++
				agg.LastActivityMs = ts
				action, _ := payload["action"].(map[string]any)
				query, _ := action["query"].(string)
				if detailOut != nil {
					dt := DetailTool{ID: callID, Name: name, Ts: ts, Args: query}
					pending[callID] = &dt
					detailOut.ToolCalls = append(detailOut.ToolCalls, dt)
				}
			case "reasoning":
				// Codex's reasoning blocks are encrypted with empty `summary`
				// and `content: null`. Surfacing them as messages adds noise
				// without information. If a future codex build populates
				// `summary[].text` we can render that as the thinking content.
				summary, _ := payload["summary"].([]any)
				if len(summary) > 0 && detailOut != nil {
					var parts []string
					for _, p := range summary {
						pm, _ := p.(map[string]any)
						if t, _ := pm["text"].(string); t != "" {
							parts = append(parts, t)
						}
					}
					if text := strings.Join(parts, "\n"); text != "" {
						detailOut.Messages = append(detailOut.Messages, DetailMsg{
							Ts: ts, Role: "assistant", Text: clipString(text, 4000),
						})
					}
				}
			case "message":
				role, _ := payload["role"].(string)
				if role == "developer" || role == "system" {
					continue // skip system/developer prompts in user-facing detail
				}
				text := codexFlattenContent(payload["content"])
				if text == "" {
					continue
				}
				agg.MessageCount++
				// Skip codex's injected AGENTS.md / environment preamble (it
				// arrives as a "user" turn) so the title is the real first prompt.
				if role == "user" && agg.FirstMessage == "" && !codexLooksLikeSystemPrompt(text) {
					agg.FirstMessage = clipString(text, 240)
				}
				if role == "assistant" {
					agg.LastMessage = clipString(text, 240)
				}
				agg.LastActivityMs = ts
				if detailOut != nil {
					detailOut.Messages = append(detailOut.Messages, DetailMsg{
						Ts: ts, Role: role, Text: clipString(text, 6000),
					})
				}
			}
			continue
		}
	}

	if detailOut != nil {
		// Trim to the LATEST maxMessages (was: first N).
		if maxMessages > 0 && len(detailOut.Messages) > maxMessages {
			detailOut.Messages = detailOut.Messages[len(detailOut.Messages)-maxMessages:]
		}
		if maxMessages > 0 && len(detailOut.ToolCalls) > maxMessages {
			detailOut.ToolCalls = detailOut.ToolCalls[len(detailOut.ToolCalls)-maxMessages:]
		}
		for f := range agg.Files {
			detailOut.Files = append(detailOut.Files, f)
		}
		sort.Strings(detailOut.Files)
	}
	return agg, nil
}

// codexLooksLikeSystemPrompt heuristically detects codex's system-injected
// preambles (AGENTS.md, INSTRUCTIONS, environment scaffolding) so we can pick
// the first real user prompt as the session title.
func codexLooksLikeSystemPrompt(msg string) bool {
	t := strings.TrimSpace(msg)
	if t == "" {
		return true
	}
	// Common opening tokens for system inject content
	for _, prefix := range []string{
		"# AGENTS.md", "# CLAUDE.md", "<INSTRUCTIONS>", "# INSTRUCTIONS",
		"You are Codex", "<context>", "Ignoring malformed agent",
		"<environment_context>", "<user_info>", "<permissions", "<codex_internal_context",
	} {
		if strings.HasPrefix(t, prefix) {
			return true
		}
	}
	// AGENTS.md instructions occasionally arrive with a leading blank/marker
	// before the heading — catch the distinctive marker anywhere near the top.
	if strings.Contains(t[:min(len(t), 120)], "AGENTS.md instructions for") {
		return true
	}
	return false
}

// codexExtractMCPResult digs the text content out of an MCP tool result. The
// shape is `{"Ok": {"content": [{"type":"text","text":"..."}], ...}}` or
// `{"Err": "..."}`.
func codexExtractMCPResult(result map[string]any) string {
	if result == nil {
		return ""
	}
	if ok, _ := result["Ok"].(map[string]any); ok != nil {
		if cs, ok2 := ok["content"].([]any); ok2 {
			var parts []string
			for _, c := range cs {
				cm, _ := c.(map[string]any)
				if t, _ := cm["text"].(string); t != "" {
					parts = append(parts, t)
				}
			}
			return strings.Join(parts, "\n")
		}
	}
	if errVal, ok := result["Err"]; ok {
		b, _ := json.Marshal(errVal)
		return "ERROR: " + string(b)
	}
	b, _ := json.Marshal(result)
	return string(b)
}

// codexFilesFromArgs sniffs Edit/Write-style file paths from the tool argument
// JSON. Codex's tools have varied schemas (apply_patch parses the patch body,
// shell takes a command array) — we do a best-effort lookup of common keys.
func codexFilesFromArgs(name, argsStr string, files map[string]struct{}) {
	if argsStr == "" || files == nil {
		return
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(argsStr), &obj); err != nil {
		// Some inputs (apply_patch) are raw text, not JSON. Try to find files
		// by looking for "Update File:" / "Add File:" markers.
		if name == "apply_patch" || strings.Contains(argsStr, "*** Begin Patch") {
			for _, line := range strings.Split(argsStr, "\n") {
				for _, marker := range []string{"*** Update File: ", "*** Add File: ", "*** Delete File: "} {
					if strings.HasPrefix(line, marker) {
						files[strings.TrimSpace(line[len(marker):])] = struct{}{}
					}
				}
			}
		}
		return
	}
	for _, key := range []string{"path", "file_path", "filepath"} {
		if v, ok := obj[key].(string); ok && v != "" {
			files[v] = struct{}{}
		}
	}
	if v, ok := obj["paths"].([]any); ok {
		for _, p := range v {
			if s, ok := p.(string); ok {
				files[s] = struct{}{}
			}
		}
	}
}

// codexFlattenContent reduces codex's content array (`[{type:input_text,text:...},...]`)
// to a single string. Returns "" for non-text or empty.
func codexFlattenContent(c any) string {
	switch v := c.(type) {
	case string:
		return v
	case []any:
		var parts []string
		for _, p := range v {
			pm, _ := p.(map[string]any)
			if t, _ := pm["text"].(string); t != "" {
				parts = append(parts, t)
			} else if t, _ := pm["input_text"].(string); t != "" {
				parts = append(parts, t)
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

// updateDetailToolResult finds the matching tool entry in the SessionDetail
// (added when we saw the function_call) and patches in the result.
func updateDetailToolResult(detailOut *SessionDetail, callID, result string) {
	if detailOut == nil || callID == "" {
		return
	}
	for i := range detailOut.ToolCalls {
		if detailOut.ToolCalls[i].ID == callID {
			detailOut.ToolCalls[i].Result = result
			return
		}
	}
}
