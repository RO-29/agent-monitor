package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Line types that never carry searchable conversation text.
var claudeSkipTypes = map[string]bool{
	"api-request-blob": true, "api-request": true, "api-request-shape": true, "file-history-snapshot": true,
	"file-history-delta": true, "progress": true, "attachment": true, "queue-operation": true, "last-prompt": true,
	"mode": true, "permission-mode": true, "session-owner": true, "atis-latch": true, "ai-title": true,
	"tools-used": true, "cost-state": true, "pr-link": true, "frame-link": true, "summary": true, "custom-title": true,
}

// sniffType reads the first "type" value near the start of a JSONL line.
func sniffType(line []byte) string {
	head := line[:min(len(line), 600)]
	i := bytes.Index(head, []byte(`"type":"`))
	if i < 0 {
		return ""
	}
	rest := head[i+8:]
	j := bytes.IndexByte(rest, '"')
	if j < 0 {
		return ""
	}
	return string(rest[:j])
}

type claudeLineRec struct {
	Type             string `json:"type"`
	Timestamp        string `json:"timestamp"`
	Cwd              string `json:"cwd"`
	IsMeta           bool   `json:"isMeta"`
	IsCompactSummary bool   `json:"isCompactSummary"`
	Message          *struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

type claudePartRec struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Name      string          `json:"name"`
	ID        string          `json:"id"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
}

var reSystemReminder = regexp.MustCompile(`(?s)<system-reminder>.*?</system-reminder>`)

func cleanPrompt(t string) string {
	t = reSystemReminder.ReplaceAllString(t, "")
	return strings.TrimSpace(claudePromptText(t))
}

func (ix *searchIndex) parseClaudeLine(st *searchFileState, line []byte, ops []searchOp) []searchOp {
	if t := sniffType(line); claudeSkipTypes[t] {
		return ops
	}
	var rec claudeLineRec
	if json.Unmarshal(line, &rec) != nil {
		return ops
	}
	if rec.Cwd != "" && rec.Cwd != st.cwd {
		st.cwd = rec.Cwd
		st.repo = ix.repoFor(rec.Cwd)
	}
	if rec.Message == nil || rec.IsMeta || (rec.Type != "user" && rec.Type != "assistant") {
		return ops
	}
	ts := parseTimestamp(rec.Timestamp)
	base := searchDoc{Session: st.session, Agent: "claude", Cwd: st.cwd, Repo: st.repo, Ts: ts, Turn: st.turn}
	var str string
	var parts []claudePartRec
	c := rec.Message.Content
	if len(c) > 0 && c[0] == '"' {
		_ = json.Unmarshal(c, &str)
	} else {
		_ = json.Unmarshal(c, &parts)
	}
	if rec.IsCompactSummary {
		text := str
		for _, p := range parts {
			if p.Type == "text" {
				text += p.Text
			}
		}
		d := base
		d.Kind, d.Text = "summary", clipString(text, 16000)
		return append(ops, searchOp{doc: &d})
	}
	if rec.Message.Role == "user" {
		var texts []string
		hasResult := false
		if str != "" {
			texts = append(texts, str)
		}
		for _, p := range parts {
			switch p.Type {
			case "text":
				texts = append(texts, p.Text)
			case "tool_result":
				hasResult = true
				res := extractToolResultText(map[string]any{"content": rawToAny(p.Content)})
				ops = append(ops, searchOp{res: &spanResult{Ref: p.ToolUseID, Ts: ts, IsErr: p.IsError, Code: -1, Text: res, Turn: st.turn, Detect: true}})
			}
		}
		if !hasResult && len(texts) > 0 && !st.sub {
			ops = ix.claudeUserCmdOps(st, base, strings.Join(texts, "\n"), ops)
		}
		if !hasResult && len(texts) > 0 && !st.sub {
			if p := cleanPrompt(strings.Join(texts, "\n")); p != "" {
				st.turn++
				d := base
				d.Turn, d.Kind, d.Text = st.turn, "prompt", clipString(p, 8192)
				ops = append(ops, searchOp{doc: &d})
			}
		}
		return ops
	}
	var prose []string
	if str != "" {
		prose = append(prose, str)
	}
	for _, p := range parts {
		switch p.Type {
		case "text":
			prose = append(prose, p.Text)
		case "tool_use":
			if p.ID == "" || p.Name == "" {
				continue
			}
			var input map[string]any
			_ = json.Unmarshal(p.Input, &input)
			ops = append(ops, ix.toolOp(st, base, p.ID, spanID("s", p.ID), p.Name, input, string(p.Input)))
		}
	}
	if t := strings.TrimSpace(strings.Join(prose, "\n")); t != "" {
		d := base
		d.Kind, d.Text = "reply", clipString(t, 8192)
		ops = append(ops, searchOp{doc: &d})
	}
	return ops
}

func rawToAny(r json.RawMessage) any {
	if len(r) == 0 {
		return nil
	}
	var v any
	_ = json.Unmarshal(r, &v)
	return v
}

// toolOp builds the span insert and its command/file/tool doc for one tool call.
func (ix *searchIndex) toolOp(st *searchFileState, base searchDoc, ref, id, name string, input map[string]any, rawArgs string) searchOp {
	str := func(k string) string { v, _ := input[k].(string); return v }
	fam := toolFamily(name)
	sp := &searchSpan{ID: id, Session: st.session, Agent: st.agent, Cwd: st.cwd, Repo: st.repo, Ts: base.Ts, Tool: name, Fam: fam, Ref: ref}
	d := base
	d.Tool, d.Span = name, id
	switch {
	case fam == "bash":
		cmd := shellCommandOf(input)
		sp.Cmd, sp.CmdKey = clipString(cmd, 300), commandKey(cmd)
		d.Kind, d.Text = "command", clipString(cmd, 2048)
		if desc := str("description"); desc != "" {
			d.Text += "\n# " + clipString(desc, 200)
		}
	case fam == "edit" || name == "Read" || name == "NotebookRead":
		paths := []string{}
		for _, k := range []string{"file_path", "notebook_path", "path"} {
			if v := str(k); v != "" {
				paths = append(paths, v)
			}
		}
		if name == "apply_patch" {
			paths = append(paths, codexPatchFiles(rawArgs)...)
		}
		d.Kind, d.Path = "file", strings.Join(paths, " ")
		body := firstOf(str("new_string"), str("content"))
		if name == "apply_patch" {
			body = rawArgs
		}
		d.Text = clipString(name+" "+d.Path+"\n"+body, 1024)
		sp.Cmd = clipString(d.Path, 300)
	default:
		res := claudeToolResource(name, input)
		if input == nil {
			res = codexToolResource(name, rawArgs)
		}
		sp.Cmd = clipString(res, 300)
		if fam == "agent" || strings.HasPrefix(name, "mcp__") {
			sp.CmdKey = name
		}
		if v := firstOf(str("path"), str("pattern")); v != "" && strings.Contains(v, "/") {
			d.Path = v
		}
		d.Kind, d.Text = "tool", clipString(name+" "+res+"\n"+rawArgs, 1024)
	}
	if sp.CmdKey == "" {
		sp.CmdKey = name
	}
	if fam == "agent" && st.agent == "claude" {
		sp.Child = ix.subagentFor(st, ref)
	}
	return searchOp{span: sp, docs: []searchDoc{d}}
}

// shellCommandOf reads the command from Bash / exec_command / shell inputs.
func shellCommandOf(input map[string]any) string {
	for _, k := range []string{"command", "cmd"} {
		switch v := input[k].(type) {
		case string:
			return v
		case []any:
			var parts []string
			for _, p := range v {
				if s, ok := p.(string); ok {
					parts = append(parts, s)
				}
			}
			return unwrapShell(parts)
		}
	}
	return ""
}

// unwrapShell turns ["/bin/zsh","-lc","git status"] into "git status".
func unwrapShell(parts []string) string {
	if len(parts) == 3 && (strings.HasSuffix(parts[0], "sh") || strings.HasSuffix(parts[0], "zsh")) && strings.HasPrefix(parts[1], "-") {
		return parts[2]
	}
	return strings.Join(parts, " ")
}

// subagentFor finds the agent-*.jsonl whose meta names this Task tool_use id.
func (ix *searchIndex) subagentFor(st *searchFileState, toolUseID string) string {
	if st.sub {
		return ""
	}
	dir := strings.TrimSuffix(st.path, ".jsonl") + "/subagents"
	metas := globSorted(dir + "/agent-*.meta.json")
	for _, m := range metas {
		jsonl := strings.TrimSuffix(m, ".meta.json") + ".jsonl"
		tid, ok := ix.metaTool[jsonl]
		if !ok {
			var meta struct {
				ToolUseID string `json:"toolUseId"`
			}
			if raw, err := readSmallFile(m); err == nil {
				_ = json.Unmarshal(raw, &meta)
			}
			tid = meta.ToolUseID
			ix.metaTool[jsonl] = tid
		}
		if tid == toolUseID {
			b := jsonl[strings.LastIndex(jsonl, "/")+1:]
			return "subagent:" + strings.TrimSuffix(b, ".jsonl")
		}
	}
	return ""
}

// ── Codex ─────────────────────────────────────────────────────────────────

type codexLineRec struct {
	Type      string          `json:"type"`
	Timestamp string          `json:"timestamp"`
	Payload   json.RawMessage `json:"payload"`
}

type codexDur struct {
	Secs  float64 `json:"secs"`
	Nanos float64 `json:"nanos"`
}

func (d *codexDur) ms() int64 {
	if d == nil {
		return 0
	}
	return int64(d.Secs*1000 + d.Nanos/1e6)
}

type codexPayloadRec struct {
	Type               string          `json:"type"`
	ID                 string          `json:"id"`
	Role               string          `json:"role"`
	Name               string          `json:"name"`
	CallID             string          `json:"call_id"`
	Arguments          json.RawMessage `json:"arguments"`
	Input              string          `json:"input"`
	Output             json.RawMessage `json:"output"`
	Content            json.RawMessage `json:"content"`
	Message            string          `json:"message"`
	Cwd                string          `json:"cwd"`
	ExitCode           *float64        `json:"exit_code"`
	AggregatedOutput   string          `json:"aggregated_output"`
	Stdout             string          `json:"stdout"`
	Stderr             string          `json:"stderr"`
	Success            *bool           `json:"success"`
	Result             json.RawMessage `json:"result"`
	ReplacementHistory json.RawMessage `json:"replacement_history"`
	StartedAtMs        int64           `json:"started_at_ms"`
	Item               *codexItemRec   `json:"item"`
}

type codexItemRec struct {
	Type             string                     `json:"type"`
	ID               string                     `json:"id"`
	Command          json.RawMessage            `json:"command"`
	Cwd              string                     `json:"cwd"`
	ExitCode         *float64                   `json:"exit_code"`
	AggregatedOutput string                     `json:"aggregated_output"`
	Duration         *codexDur                  `json:"duration"`
	Content          json.RawMessage            `json:"content"`
	Server           string                     `json:"server"`
	Tool             string                     `json:"tool"`
	Arguments        json.RawMessage            `json:"arguments"`
	Result           json.RawMessage            `json:"result"`
	Error            json.RawMessage            `json:"error"`
	Changes          map[string]json.RawMessage `json:"changes"`
	Source           string                     `json:"source"` // "user_shell" for a ! command
}

var codexSkipTop = map[string]bool{"token_usage_record": true, "world_state": true, "inter_agent_communication_metadata": true}

func (ix *searchIndex) parseCodexLine(st *searchFileState, line []byte, ops []searchOp) []searchOp {
	if codexSkipTop[sniffType(line)] {
		return ops
	}
	var rec codexLineRec
	if json.Unmarshal(line, &rec) != nil || len(rec.Payload) == 0 || rec.Payload[0] != '{' {
		return ops
	}
	if rec.Type == "event_msg" && bytes.Contains(rec.Payload[:min(len(rec.Payload), 60)], []byte(`"token_count"`)) {
		return ops
	}
	var p codexPayloadRec
	if json.Unmarshal(rec.Payload, &p) != nil {
		return ops
	}
	ts := parseTimestamp(rec.Timestamp)
	setCwd := func(c string) {
		c = strings.TrimPrefix(c, "file://")
		if c != "" && c != st.cwd {
			st.cwd, st.repo = c, ix.repoFor(c)
		}
	}
	base := func() searchDoc {
		return searchDoc{Session: st.session, Agent: "codex", Cwd: st.cwd, Repo: st.repo, Ts: ts, Turn: st.turn}
	}
	switch rec.Type {
	case "session_meta", "turn_context":
		setCwd(p.Cwd)
		return ops
	case "compacted":
		var parts []string
		if p.Message != "" {
			parts = append(parts, p.Message)
		}
		var hist []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		}
		_ = json.Unmarshal(p.ReplacementHistory, &hist)
		for _, h := range hist {
			if h.Role == "user" || h.Role == "assistant" {
				if t := codexFlattenContent(rawToAny(h.Content)); t != "" && !codexLooksLikeSystemPrompt(t) {
					parts = append(parts, clipString(t, 2000))
				}
			}
		}
		if len(parts) > 0 {
			d := base()
			d.Kind, d.Text = "summary", clipString(strings.Join(parts, "\n\n"), 16000)
			ops = append(ops, searchOp{doc: &d})
		}
		return ops
	case "event_msg":
		switch p.Type {
		case "user_message":
			return ix.codexPrompt(st, base(), p.Message, ops)
		case "agent_message":
			return codexReply(st, base(), p.Message, ops)
		case "exec_command_end":
			out := firstOf(p.AggregatedOutput, p.Stdout)
			if p.Stderr != "" {
				out += "\n" + p.Stderr
			}
			code := -1
			if p.ExitCode != nil {
				code = int(*p.ExitCode)
			}
			return append(ops, searchOp{res: &spanResult{Ref: p.CallID, Ts: ts, Code: code, Text: out, Turn: st.turn, Detect: true}})
		case "mcp_tool_call_end":
			var res map[string]any
			_ = json.Unmarshal(p.Result, &res)
			_, isErr := res["Err"]
			return append(ops, searchOp{res: &spanResult{Ref: p.CallID, Ts: ts, IsErr: isErr, Code: -1, Text: codexExtractMCPResult(res), Turn: st.turn}})
		case "patch_apply_end":
			isErr := p.Success != nil && !*p.Success
			return append(ops, searchOp{res: &spanResult{Ref: p.CallID, Ts: ts, IsErr: isErr, Code: -1, Text: p.Stdout + p.Stderr, Turn: st.turn}})
		case "item_completed":
			if p.Item != nil {
				return ix.codexItem(st, base(), p.Item, p.StartedAtMs, ops, setCwd)
			}
		}
		return ops
	case "response_item":
		switch p.Type {
		case "message":
			t := codexFlattenContent(rawToAny(p.Content))
			if p.Role == "user" {
				return ix.codexPrompt(st, base(), t, ops)
			}
			if p.Role == "assistant" {
				return codexReply(st, base(), t, ops)
			}
		case "function_call", "custom_tool_call":
			if p.Name == "" || p.Name == "exec" {
				return ops // "exec" wraps a script; its commands arrive as CommandExecution items
			}
			args := p.Input
			if args == "" && len(p.Arguments) > 0 {
				if p.Arguments[0] == '"' {
					_ = json.Unmarshal(p.Arguments, &args)
				} else {
					args = string(p.Arguments)
				}
			}
			var input map[string]any
			_ = json.Unmarshal([]byte(args), &input)
			if w, _ := input["workdir"].(string); w != "" {
				setCwd(w)
			}
			b := base()
			op := ix.toolOp(st, b, p.CallID, spanID("s", p.CallID+p.Name), p.Name, input, args)
			return append(ops, op)
		case "function_call_output", "custom_tool_call_output":
			out := ""
			if len(p.Output) > 0 && p.Output[0] == '"' {
				_ = json.Unmarshal(p.Output, &out)
			} else {
				out = codexFlattenContent(rawToAny(p.Output))
			}
			return append(ops, searchOp{res: &spanResult{Ref: p.CallID, Ts: ts, Code: -1, Text: out, Turn: st.turn, Detect: true}})
		}
	}
	return ops
}

func (ix *searchIndex) codexPrompt(st *searchFileState, d searchDoc, msg string, ops []searchOp) []searchOp {
	msg = strings.TrimSpace(msg)
	if msg == "" || codexLooksLikeSystemPrompt(msg) || strings.HasPrefix(msg, "<turn_aborted>") || strings.HasPrefix(msg, "<subagent_notification>") {
		return ops
	}
	key := clipString(msg, 200)
	if key == st.lastPrompt && d.Ts-st.lastPromptTs < 10_000 {
		return ops
	}
	st.lastPrompt, st.lastPromptTs = key, d.Ts
	st.turn++
	d.Turn, d.Kind, d.Text = st.turn, "prompt", clipString(msg, 8192)
	return append(ops, searchOp{doc: &d})
}

func codexReply(st *searchFileState, d searchDoc, msg string, ops []searchOp) []searchOp {
	msg = strings.TrimSpace(msg)
	if msg == "" {
		return ops
	}
	key := clipString(msg, 200)
	if key == st.lastReply && d.Ts-st.lastReplyTs < 10_000 {
		return ops
	}
	st.lastReply, st.lastReplyTs = key, d.Ts
	d.Kind, d.Text = "reply", clipString(msg, 8192)
	return append(ops, searchOp{doc: &d})
}

// codexItem handles item_completed records, which carry a whole call with timing.
func (ix *searchIndex) codexItem(st *searchFileState, d searchDoc, it *codexItemRec, startMs int64, ops []searchOp, setCwd func(string)) []searchOp {
	switch it.Type {
	case "UserMessage":
		return ix.codexPrompt(st, d, codexFlattenContent(rawToAny(it.Content)), ops)
	case "AgentMessage":
		return codexReply(st, d, codexFlattenContent(rawToAny(it.Content)), ops)
	}
	if startMs > 0 {
		d.Ts = startMs
	}
	var spanOp searchOp
	var out string
	code := -1
	isErr := false
	switch it.Type {
	case "CommandExecution":
		s := codexCommandString(it.Command)
		setCwd(it.Cwd)
		d.Cwd, d.Repo = st.cwd, st.repo
		spanOp = ix.toolOp(st, d, it.ID, spanID("s", it.ID), "exec_command", map[string]any{"cmd": s}, "")
		if it.Source == "user_shell" {
			markUserDocs(spanOp.docs)
		}
		out = it.AggregatedOutput
		if it.ExitCode != nil {
			code = int(*it.ExitCode)
		}
	case "McpToolCall":
		name := "mcp__" + it.Server + "__" + it.Tool
		var input map[string]any
		_ = json.Unmarshal(it.Arguments, &input)
		spanOp = ix.toolOp(st, d, it.ID, spanID("s", it.ID), name, input, string(it.Arguments))
		var res map[string]any
		_ = json.Unmarshal(it.Result, &res)
		if res != nil {
			out = codexExtractMCPResult(map[string]any{"Ok": res})
		}
		if len(it.Error) > 0 && string(it.Error) != "null" {
			isErr = true
			out = string(it.Error) + "\n" + out
		}
	case "FileChange":
		paths := make([]string, 0, len(it.Changes))
		for p := range it.Changes {
			paths = append(paths, p)
		}
		sort.Strings(paths)
		var body strings.Builder
		for _, p := range paths {
			body.WriteString("*** Update File: " + p + "\n")
			var ch struct {
				Content     string `json:"content"`
				UnifiedDiff string `json:"unified_diff"`
			}
			_ = json.Unmarshal(it.Changes[p], &ch)
			body.WriteString(clipString(firstOf(ch.UnifiedDiff, ch.Content), 600))
			body.WriteString("\n")
		}
		spanOp = ix.toolOp(st, d, it.ID, spanID("s", it.ID), "apply_patch", nil, body.String())
	default:
		return ops
	}
	if spanOp.span != nil {
		spanOp.span.Dur = it.Duration.ms()
	}
	ops = append(ops, spanOp)
	if it.Type == "FileChange" {
		return ops
	}
	return append(ops, searchOp{res: &spanResult{Ref: it.ID, Ts: d.Ts + it.Duration.ms(), Dur: it.Duration.ms(), IsErr: isErr, Code: code, Text: out, Turn: st.turn, Detect: it.Type == "CommandExecution"}})
}

func globSorted(pat string) []string {
	ms, _ := filepath.Glob(pat)
	sort.Strings(ms)
	return ms
}

func readSmallFile(p string) ([]byte, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, 1<<20))
}

// userDocTool marks docs for commands the user ran (slash and ! commands), so tool:user finds them.
const userDocTool = "user"

func markUserDocs(ds []searchDoc) {
	for i := range ds {
		ds[i].Tool = userDocTool
	}
}

// claudeUserCmdOps indexes a slash or ! command line, and its output, as user command docs.
func (ix *searchIndex) claudeUserCmdOps(st *searchFileState, base searchDoc, text string, ops []searchOp) []searchOp {
	u, ok := claudeUserCmdLine(text)
	if !ok {
		return ops
	}
	if u.Command != "" {
		d := base
		d.Kind, d.Tool, d.Text = "command", userDocTool, clipString(u.Command, 2048)
		ops = append(ops, searchOp{doc: &d})
	}
	if u.HasOutput && strings.TrimSpace(u.Output) != "" {
		d := base
		d.Kind, d.Tool, d.Text = "output", userDocTool, clipString(u.Output, 4096)
		if u.IsErr {
			d.Kind = "error"
		}
		ops = append(ops, searchOp{doc: &d})
	}
	return ops
}
