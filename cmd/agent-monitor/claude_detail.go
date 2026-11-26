package main

import (
	"bufio"
	"encoding/json"
	"os"
	"sort"
	"strings"
	"time"
)

// claudeAggregate is a transcript's rolled-up metadata, kept without the full message log.
type claudeAggregate struct {
	namedByUser      bool // a custom-title record set the title
	Tokens           TokenUsage
	ToolUsage        map[string]int
	Subagents        int
	Files            map[string]struct{}
	BgTasks          int
	MessageCount     int
	Title            string
	FirstMessage     string
	Model            string
	Mode             string
	LastMessage      string
	LastActivityMs   int64
	FirstActivityMs  int64 // first timestamp in the transcript → Session.StartedAt when seeding
	Spawns           []SpawnRef
	StartedWithClear bool // transcript began with a /clear (a continuation)
}

// scanClaudeJSONL returns the transcript aggregate; a non-nil detailOut also gets up to maxMessages parsed messages.
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
		agg.addLine(sc.Bytes(), detailOut, pendingTools)
	}

	if detailOut != nil {
		// Keep the latest maxMessages messages and tool calls, not the first ones.
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

func (agg *claudeAggregate) addLine(line []byte, detailOut *SessionDetail, pendingTools map[string]*DetailTool) {
	var j map[string]any
	if err := json.Unmarshal(line, &j); err != nil {
		return
	}
	kind, _ := j["type"].(string)
	ts := parseTimestamp(j["timestamp"])
	if ts > 0 && (agg.FirstActivityMs == 0 || ts < agg.FirstActivityMs) {
		agg.FirstActivityMs = ts
	}

	switch kind {
	case "ai-title":
		if t, _ := j["aiTitle"].(string); t != "" && !agg.namedByUser {
			agg.Title = t
		}
		return
	case "custom-title": // a name the user or the desktop app set wins over the generated one
		if t, _ := j["customTitle"].(string); t != "" {
			agg.Title, agg.namedByUser = t, true
		}
		return
	case "permission-mode":
		if m, _ := j["permissionMode"].(string); m != "" {
			agg.Mode = m
		}
		return
	case "queue-operation":
		// Skip remove/enqueue lifecycle ops: they have no summary.
		content, _ := j["content"].(string)
		summary := extractTagValue(content, "summary")
		if summary == "" {
			return
		}
		agg.BgTasks++
		if detailOut != nil {
			op, _ := j["operation"].(string)
			detailOut.BgTasks = append(detailOut.BgTasks, DetailBgTask{
				Ts:      ts,
				ID:      extractTagValue(content, "task-id"),
				Status:  firstOf(extractTagValue(content, "status"), op),
				Summary: summary,
			})
		}
		return
	}

	msg, _ := j["message"].(map[string]any)
	if msg == nil {
		return
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
	// Title from the first real prompt: keep slash-command args, skip caveats, hooks and stdout scaffolding.
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
				// Spawn edge: keep the prompt so linkage can match the child session.
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

// firstNonEmptyLine returns the first non-blank, trimmed line of s.
func firstNonEmptyLine(s string) string {
	for _, ln := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(ln); t != "" {
			return t
		}
	}
	return ""
}

// claudePromptText is the titling text of a user turn: slash-command args or the /goal condition; "" for scaffolding.
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
		"<local-command-stdout>", "<local-command-stderr>", "<local-command-caveat>", "<user-prompt-submit-hook>",
		"<system-reminder>", "caveat:", "the messages below were generated",
		"a session-scoped stop hook", "goal set:",
		"<task-notification>", "[request interrupted", "<bash-input>", "<bash-stdout>", "<bash-stderr>",
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

// extractTagValue returns the text inside <key>...</key>.
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
