package main

import (
	"bufio"
	"encoding/json"
	"os"
	"sort"
	"strings"
	"time"
)

// claudeAggregate is the rolled-up metadata extracted from a transcript: token
// totals, tool histogram, file set, subagent count, bg-task count. Used by the
// seed path to populate Session struct without holding the full message log.
type claudeAggregate struct {
	Tokens         TokenUsage
	ToolUsage      map[string]int
	Subagents      int
	Files          map[string]struct{}
	BgTasks        int
	MessageCount   int
	Title          string
	FirstMessage   string
	Model          string
	Mode           string
	LastMessage      string
	LastActivityMs   int64
	Spawns           []SpawnRef
	StartedWithClear bool // transcript began with a /clear (a continuation)
}

// scanClaudeJSONL walks an entire transcript and returns the aggregate plus,
// optionally, full per-message detail. detailOut == nil means "aggregates
// only" (cheap startup path). When non-nil, it gets populated with parsed
// messages, tool calls, subagents, etc. — bounded by maxMessages.
func scanClaudeJSONL(path string, detailOut *SessionDetail, maxMessages int) (claudeAggregate, error) {
	agg := claudeAggregate{
		ToolUsage: map[string]int{},
		Files:     map[string]struct{}{},
	}

	f, err := os.Open(path)
	if err != nil {
		return agg, err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	// JSONL lines can be large (long tool outputs). Allow up to 8 MB per line.
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)

	// tool_use_id -> *DetailTool. Parsed tool_result lines look up their
	// matching tool_use here so the detail view shows args + output paired.
	pendingTools := map[string]*DetailTool{}

	for sc.Scan() {
		line := sc.Bytes()
		var j map[string]any
		if err := json.Unmarshal(line, &j); err != nil {
			continue
		}
		kind, _ := j["type"].(string)
		ts := parseTimestamp(j["timestamp"])

		switch kind {
		case "ai-title":
			if t, _ := j["aiTitle"].(string); t != "" {
				agg.Title = t
			}
			continue
		case "permission-mode":
			if m, _ := j["permissionMode"].(string); m != "" {
				agg.Mode = m
			}
			continue
		case "queue-operation":
			// Only count + display tasks that have a real summary — the
			// "remove"/"enqueue" lifecycle ops emit empty content and would
			// otherwise pollute the bg-tasks panel.
			content, _ := j["content"].(string)
			summary := extractTagValue(content, "summary")
			if summary == "" {
				continue
			}
			agg.BgTasks++
			if detailOut != nil {
				op, _ := j["operation"].(string)
				detailOut.BgTasks = append(detailOut.BgTasks, DetailBgTask{
					Ts:      ts,
					ID:      extractTagValue(content, "task-id"),
					Status:  firstNonEmpty(extractTagValue(content, "status"), op),
					Summary: summary,
				})
			}
			continue
		}

		msg, _ := j["message"].(map[string]any)
		if msg == nil {
			continue
		}
		role, _ := msg["role"].(string)
		model, _ := msg["model"].(string)
		if model != "" {
			agg.Model = model
		}

		// Token usage on assistant turns
		if usage, _ := msg["usage"].(map[string]any); usage != nil {
			agg.Tokens.Input += int64(toFloat(usage["input_tokens"]))
			agg.Tokens.Output += int64(toFloat(usage["output_tokens"]))
			agg.Tokens.CacheRead += int64(toFloat(usage["cache_read_input_tokens"]))
			agg.Tokens.CacheCreate += int64(toFloat(usage["cache_creation_input_tokens"]))
			if st, _ := usage["server_tool_use"].(map[string]any); st != nil {
				agg.Tokens.WebSearch += int64(toFloat(st["web_search_requests"]))
				agg.Tokens.WebFetch += int64(toFloat(st["web_fetch_requests"]))
			}
		}

		text, isToolResult, isUserText, isAssistantText := extractClaudeContent(j)
		// A /clear at the top of the transcript means this session continues a
		// previous one in the same terminal — the linkage signal for chaining.
		if isUserText && agg.FirstMessage == "" && strings.HasPrefix(strings.TrimSpace(text), "<command-name>/clear") {
			agg.StartedWithClear = true
		}
		// Derive the title from the real first prompt: extract useful content
		// from slash-command wrappers (e.g. /goal's args ARE the task), and skip
		// caveats / hooks / stdout scaffolding — so the title isn't "/clear" or
		// a bare project folder name.
		if isUserText && !isToolResult && agg.FirstMessage == "" && text != "" {
			if p := claudePromptText(text); p != "" {
				agg.FirstMessage = clipString(p, 240)
			}
		}
		if isUserText || isAssistantText {
			agg.MessageCount++
			if text != "" {
				agg.LastMessage = clipString(text, 240)
			}
			agg.LastActivityMs = ts
		}

		// Walk content parts: tool_use → register, tool_result → pair.
		toolCount := 0
		if c, ok := msg["content"].([]any); ok {
			for _, p := range c {
				pm, ok := p.(map[string]any)
				if !ok {
					continue
				}
				ptype, _ := pm["type"].(string)
				switch ptype {
				case "tool_use":
					name, _ := pm["name"].(string)
					id, _ := pm["id"].(string)
					if name == "" {
						continue
					}
					agg.ToolUsage[name]++
					toolCount++
					inputMap, _ := pm["input"].(map[string]any)
					// Track files modified on Edit / Write / NotebookEdit calls.
					if name == "Edit" || name == "Write" || name == "NotebookEdit" || name == "MultiEdit" {
						if fp, _ := inputMap["file_path"].(string); fp != "" {
							agg.Files[fp] = struct{}{}
						}
					}
					// Subagent dispatch (Task tool with subagent_type)
					subType, _ := inputMap["subagent_type"].(string)
					isSub := name == "Task" && subType != ""
					if isSub {
						agg.Subagents++
					}
					// Spawn edge: Agent/Task/Workflow launching a child agent
					// (often a separate session). Capture its prompt so linkage
					// can match the child session it created.
					if name == "Agent" || name == "Task" || name == "Workflow" {
						p, _ := inputMap["prompt"].(string)
						if p == "" {
							p, _ = inputMap["description"].(string)
						}
						if line := firstNonEmptyLine(p); line != "" {
							agg.Spawns = append(agg.Spawns, SpawnRef{Name: name, Prompt: clipString(line, 160), Ts: ts})
						}
					}

					if detailOut != nil {
						argsJSON, _ := json.Marshal(inputMap)
						dt := DetailTool{
							ID: id, Name: name, Ts: ts,
							Args:       clipString(string(argsJSON), 8000),
							IsSubagent: isSub,
						}
						pendingTools[id] = &dt
						detailOut.ToolCalls = append(detailOut.ToolCalls, dt)

						if isSub {
							desc, _ := inputMap["description"].(string)
							prompt, _ := inputMap["prompt"].(string)
							detailOut.Subagents = append(detailOut.Subagents, DetailSub{
								ID:           id,
								SubagentType: subType,
								Description:  desc,
								Prompt:       clipString(prompt, 4000),
								Ts:           ts,
							})
						}
					}
				case "tool_result":
					id, _ := pm["tool_use_id"].(string)
					if detailOut != nil && id != "" {
						resultText := extractToolResultText(pm)
						if dt, ok := pendingTools[id]; ok {
							dt.Result = clipString(resultText, 12000)
							// Update the entry that's already in detailOut.ToolCalls.
							for i := range detailOut.ToolCalls {
								if detailOut.ToolCalls[i].ID == id {
									detailOut.ToolCalls[i].Result = dt.Result
									break
								}
							}
							if dt.IsSubagent {
								for i := range detailOut.Subagents {
									if detailOut.Subagents[i].ID == id {
										detailOut.Subagents[i].Result = dt.Result
										break
									}
								}
							}
						}
					}
				}
			}
		}

		if detailOut != nil && (isUserText || isAssistantText) {
			detailOut.Messages = append(detailOut.Messages, DetailMsg{
				Ts:        ts,
				Role:      role,
				Text:      clipString(text, 4000),
				ToolCount: toolCount,
				Model:     model,
			})
		}
	}

	if detailOut != nil {
		// Trim to the LATEST maxMessages (was: first N). For a 600-message
		// session you want to see what's happening now, not what kicked it
		// off. Same logic for tool calls — the user wants the trailing edge.
		if maxMessages > 0 && len(detailOut.Messages) > maxMessages {
			detailOut.Messages = detailOut.Messages[len(detailOut.Messages)-maxMessages:]
		}
		if maxMessages > 0 && len(detailOut.ToolCalls) > maxMessages {
			detailOut.ToolCalls = detailOut.ToolCalls[len(detailOut.ToolCalls)-maxMessages:]
		}
		// Files: convert set to slice, sorted for stable display
		for f := range agg.Files {
			detailOut.Files = append(detailOut.Files, f)
		}
		sort.Strings(detailOut.Files)
	}
	return agg, nil
}

// extractToolResultText pulls a useful string out of a tool_result content.
// Content may be a string, an array of {type:text,text:...} parts, or absent.
func extractToolResultText(pm map[string]any) string {
	c := pm["content"]
	switch v := c.(type) {
	case string:
		return v
	case []any:
		var parts []string
		for _, p := range v {
			ppm, ok := p.(map[string]any)
			if !ok {
				continue
			}
			if t, _ := ppm["text"].(string); t != "" {
				parts = append(parts, t)
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

func parseTimestamp(v any) int64 {
	if s, ok := v.(string); ok {
		if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
			return t.UnixMilli()
		}
	}
	return 0
}

func toFloat(v any) float64 {
	if f, ok := v.(float64); ok {
		return f
	}
	return 0
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// firstNonEmptyLine returns the first non-blank, trimmed line of s.
func firstNonEmptyLine(s string) string {
	for _, ln := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(ln); t != "" {
			return t
		}
	}
	return ""
}

// claudePromptText turns a raw "user" turn into the text worth titling by —
// or "" if it's pure scaffolding to skip. Slash-command wrappers contribute
// their <command-args> (so `/goal <task>` titles as the task; `/clear` with no
// args is skipped); the /goal Stop-hook echo contributes its quoted condition.
func claudePromptText(text string) string {
	t := strings.TrimSpace(text)
	if t == "" {
		return ""
	}
	if strings.HasPrefix(t, "<command-name>") {
		args := strings.TrimSpace(extractTagValue(t, "command-args")) // "" for /clear etc → skip
		// Strip a FleetView prompt marker and a leading slash-command echo
		// (e.g. "▎ /goal Read…" → "Read…") that can slip into the args.
		args = strings.TrimSpace(strings.TrimLeft(args, "▎▏│┃| \t"))
		if strings.HasPrefix(args, "/") {
			if sp := strings.IndexAny(args, " \t\n"); sp > 0 {
				args = strings.TrimSpace(args[sp:])
			}
		}
		return args
	}
	if strings.HasPrefix(t, "A session-scoped Stop hook is now active") || strings.HasPrefix(t, "Goal set:") {
		return firstQuoted(t) // the goal condition text
	}
	if claudeLooksLikeMeta(t) {
		return ""
	}
	return t
}

// claudeLooksLikeMeta detects caveat / hook / stdout scaffolding Claude records
// as "user" turns, so we can skip it when picking the title.
func claudeLooksLikeMeta(text string) bool {
	lower := strings.ToLower(strings.TrimSpace(text))
	if lower == "" {
		return true
	}
	for _, p := range []string{
		"<command-name>", "<command-message>", "<command-args>",
		"<local-command-stdout>", "<local-command-caveat>", "<user-prompt-submit-hook>",
		"<system-reminder>", "caveat:", "the messages below were generated",
		"a session-scoped stop hook", "goal set:",
	} {
		if strings.HasPrefix(lower, p) {
			return true
		}
	}
	return false
}

// firstQuoted returns the text inside the first pair of double quotes, or "".
func firstQuoted(s string) string {
	a := strings.Index(s, "\"")
	if a < 0 {
		return ""
	}
	rest := s[a+1:]
	if b := strings.Index(rest, "\""); b >= 0 {
		return strings.TrimSpace(rest[:b])
	}
	return strings.TrimSpace(rest)
}

// extractTagValue pulls "value" out of "<key>value</key>" — used to read
// task-id/status/summary from the queue-operation content blob, which embeds
// XML-ish tags in a string.
func extractTagValue(s, key string) string {
	open := "<" + key + ">"
	close := "</" + key + ">"
	i := strings.Index(s, open)
	if i < 0 {
		return ""
	}
	rest := s[i+len(open):]
	j := strings.Index(rest, close)
	if j < 0 {
		return ""
	}
	return rest[:j]
}
