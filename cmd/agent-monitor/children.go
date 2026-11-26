package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Children of a watched session: Claude and Codex subagents, agents started by a shell command, talk_to_agent targets.

// SessionChild is one row of GET /api/session/{id}/children.
type SessionChild struct {
	Key           string  `json:"key"`  // stable within the parent: tool call id or child session id
	Kind          string  `json:"kind"` // subagent | agent | talk
	Agent         string  `json:"agent"`
	SessionID     string  `json:"sessionId,omitempty"` // child store id when it has its own thread
	AgentID       string  `json:"agentId,omitempty"`   // claude subagent id; its transcript is served by /children/{agentId}/items
	ParentAgentID string  `json:"parentAgentId,omitempty"`
	ToolUseID     string  `json:"toolUseId,omitempty"`
	ParentItemID  string  `json:"parentItemId,omitempty"`
	ParentRunID   string  `json:"parentRunId,omitempty"`
	ParentMsgID   string  `json:"parentMessageId,omitempty"`
	ParentPrompt  string  `json:"parentPrompt,omitempty"`
	By            string  `json:"by,omitempty"` // "user" for a ! command
	Name          string  `json:"name,omitempty"`
	SubagentType  string  `json:"subagentType,omitempty"`
	Description   string  `json:"description,omitempty"`
	Prompt        string  `json:"prompt,omitempty"`
	Command       string  `json:"command,omitempty"`
	Cwd           string  `json:"cwd,omitempty"`
	Model         string  `json:"model,omitempty"`
	Pane          string  `json:"pane,omitempty"`
	HostID        string  `json:"hostId,omitempty"`
	Status        string  `json:"status"` // running | completed | failed | killed | stopped
	Background    bool    `json:"background,omitempty"`
	StartedAt     int64   `json:"startedAt"`
	EndedAt       int64   `json:"endedAt,omitempty"`
	InTokens      int64   `json:"inTokens,omitempty"`
	OutTokens     int64   `json:"outTokens,omitempty"`
	CostUSD       float64 `json:"costUsd,omitempty"`
	LinkBasis     string  `json:"linkBasis"`
	HasTranscript bool    `json:"hasTranscript,omitempty"`
}

// SessionChildren is the GET /api/session/{id}/children answer.
type SessionChildren struct {
	ID       string          `json:"id"`
	Agent    Tool            `json:"agent"`
	Pane     string          `json:"pane,omitempty"`
	HostID   string          `json:"hostId,omitempty"`
	Count    int             `json:"count"`
	Children []*SessionChild `json:"children"`
}

var (
	reCmdHead     = `(?:^|&&|\|\||[;|(\n]|\$\()\s*(?:(?:command|exec|nohup|env|time|timeout\s+\S+|[A-Za-z_][A-Za-z0-9_]*=\S*)\s+)*(?:[~\w./-]*/)?`
	reFlags       = `(?:\s+-[-\w]+(?:[ =](?:"[^"]*"|'[^']*'|[^-\s]\S*))?)*`
	reCodexRun    = regexp.MustCompile(reCmdHead + `codex` + reFlags + `\s+(?:exec|e|review)(?:\s|$)`)
	reClaudeRun   = regexp.MustCompile(reCmdHead + `claude(?:\s+(?:"[^"]*"|'[^']*'|[^\s|;&]+))*?\s+(?:-p|--print)(?:\s|=|$)`)
	reDetach      = regexp.MustCompile(`(?:[^&]&\s*\)?\s*(?:;|\n|$))|\b(?:nohup|setsid)\s`)
	reHeredoc     = regexp.MustCompile(`<<-?\s*['"]?(\w+)['"]?`)
	reQuoted      = regexp.MustCompile(`"((?:[^"\\]|\\.){8,})"|'([^']{8,})'`)
	reNoteToolID  = regexp.MustCompile(`<tool-use-id>([^<]+)</tool-use-id>`)
	reNoteStatus  = regexp.MustCompile(`<status>([^<]+)</status>`)
	bTaskNoteTag  = []byte("task-notification")
	bSubNote      = []byte("subagent_notification")
	bToolUseType  = []byte(`"tool_use"`)
	bNameAgent    = []byte(`"name":"Agent"`)
	bNameTask     = []byte(`"name":"Task"`)
	bNameBash     = []byte(`"name":"Bash"`)
	bTalkTool     = []byte(`talk_to_agent"`)
	bToolUseIDKey = []byte(`"tool_use_id":"`)
	bCwdKey       = []byte(`"cwd":"`)
)

const childLiveWindow = 5 * time.Minute

// stripHeredocs drops heredoc bodies, so a script or message that mentions an agent does not count as running it.
func stripHeredocs(cmd string) string {
	for i := 0; i < 8; i++ {
		loc := reHeredoc.FindStringSubmatchIndex(cmd)
		if loc == nil {
			return cmd
		}
		delim := cmd[loc[2]:loc[3]]
		nl := strings.IndexByte(cmd[loc[1]:], '\n')
		if nl < 0 {
			return cmd[:loc[0]]
		}
		body := cmd[loc[1]+nl+1:]
		end := len(body)
		for off := 0; off < len(body); {
			ln, _, _ := strings.Cut(body[off:], "\n")
			if strings.TrimSpace(ln) == delim {
				end = off + len(ln)
				break
			}
			off += len(ln) + 1
		}
		cmd = cmd[:loc[0]] + body[end:]
	}
	return cmd
}

// agentInCommand names the agent a shell command starts, or "".
func agentInCommand(cmd string) Tool {
	if !strings.Contains(cmd, "codex") && !strings.Contains(cmd, "claude") {
		return ""
	}
	cmd = stripHeredocs(cmd)
	switch {
	case reCodexRun.MatchString(cmd):
		return ToolCodex
	case reClaudeRun.MatchString(cmd):
		return ToolClaude
	}
	return ""
}

// commandPrompt is the first quoted argument after the agent's subcommand, when the prompt is inline.
func commandPrompt(cmd string, tool Tool) string {
	re := reCodexRun
	if tool == ToolClaude {
		re = reClaudeRun
	}
	cmd = stripHeredocs(cmd)
	loc := re.FindStringIndex(cmd)
	if loc == nil {
		return ""
	}
	rest := cmd[loc[1]-1:]
	m := reQuoted.FindStringSubmatchIndex(rest)
	if m == nil || strings.ContainsAny(rest[:m[0]], "|;&") {
		return ""
	}
	q := reQuoted.FindStringSubmatch(rest)
	return strings.ReplaceAll(firstOf(q[1], q[2]), `\"`, `"`)
}

type childCall struct {
	kind, callID, itemID, name, by string
	agent                          Tool
	off, ts, endTs                 int64
	desc, prompt, subType, cmd     string
	cwd, recipient, model          string
	bg, async, done, failed        bool
	agentID                        string // claude agentId or codex child thread id
}

type childNote struct {
	status string
	ts     int64
}

// childScan is the incremental parse of one parent transcript.
type childScan struct {
	mu      sync.Mutex
	path    string
	fi      os.FileInfo
	ix      *wIndex
	off     int64
	prompts map[int64]string
	tsOff   []int64 // turn starts that carry a timestamp, in file order
	tsVal   []int64 // the timestamp of each tsOff entry
	calls   []*childCall
	byID    map[string]*childCall
	notes   map[string]childNote // claude tool-use-id -> latest task notification
	subNote map[string]childNote // codex child thread id -> latest subagent notification
	cwd     string
	bang    *childCall
	refs    int       // requests holding the scan; guarded by childScans.mu
	lastUse time.Time // guarded by childScans.mu
}

const childScanMax = 64

var childScans = struct {
	mu sync.Mutex
	m  map[string]*childScan
}{m: map[string]*childScan{}}

// childScanFor returns the one scan of path; the caller must call release.
func childScanFor(path string) *childScan {
	childScans.mu.Lock()
	defer childScans.mu.Unlock()
	s := childScans.m[path]
	if s == nil {
		if len(childScans.m) >= childScanMax {
			var old *childScan
			for _, v := range childScans.m {
				if v.refs == 0 && (old == nil || v.lastUse.Before(old.lastUse)) {
					old = v
				}
			}
			if old != nil {
				delete(childScans.m, old.path)
			}
		}
		s = &childScan{path: path}
		childScans.m[path] = s
	}
	s.refs++
	s.lastUse = time.Now()
	return s
}

func (s *childScan) release() {
	childScans.mu.Lock()
	s.refs--
	childScans.mu.Unlock()
}

func (s *childScan) reset() {
	s.fi, s.ix, s.off, s.cwd, s.bang = nil, &wIndex{}, 0, "", nil
	s.prompts, s.byID = map[int64]string{}, map[string]*childCall{}
	s.notes, s.subNote = map[string]childNote{}, map[string]childNote{}
	s.calls, s.tsOff, s.tsVal = nil, nil, nil
}

// update reads what the transcript gained since the last call.
func (s *childScan) update(ref watchedRef) error {
	f, err := os.Open(ref.Path)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	if s.ix == nil || s.fi == nil || !os.SameFile(s.fi, st) || st.Size() < s.off || st.ModTime().Before(s.fi.ModTime()) {
		s.reset()
	}
	s.fi = st
	if s.cwd == "" {
		s.cwd = ref.Cwd
	}
	from := s.ix.off
	if err := s.ix.scan(f, ref.Tool, st.Size()); err != nil {
		return err
	}
	k := sort.Search(len(s.ix.starts), func(i int) bool { return s.ix.starts[i] >= from })
	for _, off := range s.ix.starts[k:] {
		s.prompts[off] = ""
	}
	end, err := forEachLine(f, s.off, s.ix.off, func(off int64, line []byte) {
		if len(line) == 0 {
			return
		}
		if _, ok := s.prompts[off]; ok {
			var ts int64
			s.prompts[off], ts = turnPromptOf(ref.Tool, line)
			if ts > 0 {
				s.tsOff, s.tsVal = append(s.tsOff, off), append(s.tsVal, ts)
			}
		}
		if ref.Tool == ToolClaude {
			s.claudeLine(ref.ID, off, line)
		} else {
			s.codexLine(ref.ID, off, line)
		}
	})
	s.off = end
	return err
}

// turnPromptOf is the prompt and the timestamp of a turn start line.
func turnPromptOf(tool Tool, line []byte) (string, int64) {
	text, ts := "", int64(0)
	if tool == ToolClaude {
		if text, ts = claudePromptLine(line); text == "" {
			if c, cts, ok := claudeSlashLine(line); ok {
				text, ts = c.line(), cts
			}
		}
	} else if text, ts = codexPromptLine(line); text == "" {
		if cmd, _, ok := codexUserShellLine(line); ok {
			text = "!" + cmd
		}
	}
	if ts == 0 {
		var j struct {
			Timestamp string `json:"timestamp"`
		}
		_ = json.Unmarshal(line, &j)
		ts = parseTimestamp(j.Timestamp)
	}
	return text, ts
}

func (s *childScan) add(c *childCall) {
	if c.callID != "" {
		if _, dup := s.byID[c.callID]; dup {
			return
		}
		s.byID[c.callID] = c
	}
	s.calls = append(s.calls, c)
}

// ── claude parent ──

func (s *childScan) claudeLine(thread string, off int64, line []byte) {
	hasUse := bytes.Contains(line, bToolUseType) && (bytes.Contains(line, bNameAgent) || bytes.Contains(line, bNameTask) ||
		bytes.Contains(line, bNameBash) || bytes.Contains(line, bTalkTool))
	hasRes := bytes.Contains(line, bToolResult)
	hasNote := bytes.Contains(line, bTaskNoteTag)
	hasBang := bytes.Contains(line, []byte("bash-input")) || bytes.Contains(line, []byte("bash-stdout")) || bytes.Contains(line, []byte("bash-stderr"))
	if !hasUse && !hasNote && !hasBang && !(hasRes && s.knownResult(line)) {
		if cwd := quickCwd(line); cwd != "" {
			s.cwd = cwd
		}
		return
	}
	var j struct {
		wClaudeLine
		ToolUseResult json.RawMessage `json:"toolUseResult"`
	}
	if json.Unmarshal(line, &j) != nil {
		return
	}
	if j.Cwd != "" {
		s.cwd = j.Cwd
	}
	if j.IsSidechain || j.Message == nil {
		return
	}
	ts := parseTimestamp(j.Timestamp)
	if j.Type == "user" && (hasNote || hasBang) && !hasRes {
		text, _ := claudeUserText(j.Message.Content)
		if hasNote {
			for _, blk := range strings.Split(text, "</task-notification>") {
				if m := reNoteToolID.FindStringSubmatch(blk); m != nil {
					st := ""
					if sm := reNoteStatus.FindStringSubmatch(blk); sm != nil {
						st = strings.TrimSpace(sm[1])
					}
					s.notes[strings.TrimSpace(m[1])] = childNote{status: st, ts: ts}
				}
			}
		}
		if hasBang && !j.IsMeta {
			cmd, in, out, hasOut := bangParts(text)
			if in {
				s.bang = nil
				if a := agentInCommand(cmd); a != "" {
					c := &childCall{kind: "agent", agent: a, by: "user", name: "!", itemID: fmt.Sprintf("%s@%d.0", thread, off), off: off, ts: ts, cmd: cmd, cwd: s.cwd}
					s.add(c)
					s.bang = c
				}
			}
			if hasOut && s.bang != nil {
				s.bang.done, s.bang.endTs = true, ts
				if reBgMoved.MatchString(out) {
					s.bang.done, s.bang.bg = false, true
				}
				s.bang = nil
			}
		}
		return
	}
	var parts []wClaudePart
	if len(j.Message.Content) == 0 || j.Message.Content[0] != '[' || json.Unmarshal(j.Message.Content, &parts) != nil {
		return
	}
	for _, p := range parts {
		switch {
		case j.Type == "assistant" && p.Type == "tool_use" && p.ID != "":
			s.claudeToolUse(thread, off, ts, p)
		case j.Type == "user" && p.Type == "tool_result":
			c := s.byID[p.ToolUseID]
			if c == nil {
				continue
			}
			c.done, c.endTs, c.failed = true, ts, p.IsError
			var r struct {
				Status  string `json:"status"`
				IsAsync bool   `json:"isAsync"`
				AgentID string `json:"agentId"`
			}
			if len(j.ToolUseResult) > 0 && j.ToolUseResult[0] == '{' && json.Unmarshal(j.ToolUseResult, &r) == nil {
				if r.AgentID != "" {
					c.agentID = r.AgentID
				}
				if r.IsAsync || r.Status == "async_launched" {
					c.async = true
				}
			}
			if c.kind == "agent" && !p.IsError {
				res := claudeResultText(p.Content)
				if strings.Contains(res, "running in background") {
					c.bg = true
				} else if mm := reExitCode.FindStringSubmatch(res[:min(len(res), 200)]); mm != nil && mm[1] != "0" {
					c.failed = true
				}
			}
		}
	}
}

// knownResult reports whether the line answers a tool call this scan tracks.
func (s *childScan) knownResult(line []byte) bool {
	for rest := line; ; {
		i := bytes.Index(rest, bToolUseIDKey)
		if i < 0 {
			return false
		}
		rest = rest[i+len(bToolUseIDKey):]
		if j := bytes.IndexByte(rest, '"'); j > 0 && s.byID[string(rest[:j])] != nil {
			return true
		}
	}
}

// quickCwd reads the first "cwd" string of a Claude line without decoding the line; top-level cwd comes before message.
func quickCwd(line []byte) string {
	i := bytes.Index(line, bCwdKey)
	if i < 0 {
		return ""
	}
	rest := line[i+len(bCwdKey)-1:]
	esc := false
	for j := 1; j < len(rest); j++ {
		switch {
		case esc:
			esc = false
		case rest[j] == '\\':
			esc = true
		case rest[j] == '"':
			var v string
			if json.Unmarshal(rest[:j+1], &v) == nil {
				return v
			}
			return ""
		}
	}
	return ""
}

func (s *childScan) claudeToolUse(thread string, off, ts int64, p wClaudePart) {
	in := p.Input
	c := &childCall{callID: p.ID, itemID: thread + "/" + p.ID, name: p.Name, off: off, ts: ts, cwd: s.cwd}
	switch {
	case p.Name == "Agent" || p.Name == "Task":
		c.kind, c.agent = "subagent", ToolClaude
		c.desc, c.prompt, c.subType, c.model = claudeStr(in, "description"), claudeStr(in, "prompt"), claudeStr(in, "subagent_type"), claudeStr(in, "model")
		c.bg = in["run_in_background"] == true || in["run_in_background"] == "true"
	case p.Name == "Bash":
		cmd := shellCommandOf(in)
		a := agentInCommand(cmd)
		if a == "" {
			return
		}
		c.kind, c.agent, c.cmd, c.desc = "agent", a, cmd, claudeStr(in, "description")
		c.bg = in["run_in_background"] == true || reDetach.MatchString(stripHeredocs(cmd))
	case strings.HasSuffix(p.Name, "talk_to_agent"):
		c.kind, c.recipient, c.prompt = "talk", claudeStr(in, "recipient"), claudeStr(in, "message")
	default:
		return
	}
	s.add(c)
}

// ── codex parent ──

func (s *childScan) codexLine(thread string, off int64, line []byte) {
	var rec codexLineRec
	if json.Unmarshal(line, &rec) != nil || len(rec.Payload) == 0 || rec.Payload[0] != '{' || codexSkipTop[rec.Type] {
		return
	}
	var pl wCodexPayload
	if json.Unmarshal(rec.Payload, &pl) != nil {
		return
	}
	ts := parseTimestamp(rec.Timestamp)
	switch {
	case rec.Type == "turn_context" || rec.Type == "session_meta":
		if pl.Cwd != "" {
			s.cwd = pl.Cwd
		}
	case rec.Type == "response_item" && (pl.Type == "function_call" || pl.Type == "custom_tool_call"):
		s.codexCall(thread, off, ts, &pl)
	case rec.Type == "response_item" && (pl.Type == "function_call_output" || pl.Type == "custom_tool_call_output"):
		c := s.byID[pl.CallID]
		if c == nil {
			return
		}
		out := ""
		if len(pl.Output) > 0 && pl.Output[0] == '"' {
			_ = json.Unmarshal(pl.Output, &out)
		} else {
			out = codexFlattenContent(rawToAny(pl.Output))
		}
		if c.kind == "subagent" {
			var r struct {
				AgentID string `json:"agent_id"`
			}
			if json.Unmarshal([]byte(out), &r) == nil && r.AgentID != "" {
				c.agentID = r.AgentID
			} else {
				c.failed = true
			}
			c.done, c.endTs = true, ts
			return
		}
		if !c.done {
			c.done, c.endTs = true, ts
			var meta struct {
				Metadata struct {
					ExitCode *int `json:"exit_code"`
				} `json:"metadata"`
			}
			if json.Unmarshal([]byte(out), &meta) == nil && meta.Metadata.ExitCode != nil {
				c.failed = *meta.Metadata.ExitCode != 0
			} else if mm := reExitCode.FindStringSubmatch(out[:min(len(out), 200)]); mm != nil {
				c.failed = mm[1] != "0"
			}
		}
	case rec.Type == "event_msg" && pl.Type == "exec_command_end":
		if c := s.byID[pl.CallID]; c != nil {
			c.done, c.endTs = true, ts
			c.failed = pl.ExitCode != nil && *pl.ExitCode != 0
		}
	case rec.Type == "event_msg" && pl.Type == "item_completed" && pl.Item != nil && pl.Item.Type == "CommandExecution":
		ci := pl.Item
		cmd := codexCommandString(ci.Command)
		a := agentInCommand(cmd)
		if a == "" {
			return
		}
		start, end := pl.StartedAtMs, pl.CompletedAtMs
		if start == 0 {
			start = ts - ci.Duration.ms()
		}
		if end == 0 {
			end = ts
		}
		c := &childCall{kind: "agent", agent: a, callID: ci.ID, itemID: thread + "/" + ci.ID, name: "shell", off: off, ts: start, endTs: end, done: true,
			cmd: cmd, cwd: firstOf(strings.TrimPrefix(ci.Cwd, "file://"), s.cwd), failed: ci.ExitCode != nil && *ci.ExitCode != 0}
		if ci.Source == "user_shell" {
			c.by, c.name = "user", "!"
		}
		s.add(c)
	case rec.Type == "response_item" && pl.Type == "message" && pl.Role == "user" && bytes.Contains(line, bSubNote):
		s.codexSubNote(codexFlattenContent(rawToAny(pl.Content)), ts)
	}
}

func (s *childScan) codexCall(thread string, off, ts int64, pl *wCodexPayload) {
	args := pl.Input
	if args == "" && len(pl.Arguments) > 0 {
		if pl.Arguments[0] == '"' {
			_ = json.Unmarshal(pl.Arguments, &args)
		} else {
			args = string(pl.Arguments)
		}
	}
	var in map[string]any
	_ = json.Unmarshal([]byte(args), &in)
	if in == nil {
		in = map[string]any{}
	}
	c := &childCall{callID: pl.CallID, name: pl.Name, off: off, ts: ts, cwd: s.cwd}
	if pl.CallID != "" {
		c.itemID = thread + "/" + pl.CallID
	}
	switch {
	case pl.Name == "spawn_agent":
		c.kind, c.agent = "subagent", ToolCodex
		c.subType, c.prompt, c.model = claudeStr(in, "agent_type"), claudeStr(in, "message", "prompt"), claudeStr(in, "model")
	case codexShellTools[pl.Name]:
		cmd := shellCommandOf(in)
		a := agentInCommand(cmd)
		if a == "" {
			return
		}
		c.kind, c.agent, c.cmd, c.cwd = "agent", a, cmd, firstOf(claudeStr(in, "workdir"), s.cwd)
	case strings.HasSuffix(pl.Name, "talk_to_agent"):
		c.kind, c.recipient, c.prompt = "talk", claudeStr(in, "recipient"), claudeStr(in, "message")
	default:
		return
	}
	s.add(c)
}

// codexSubNote reads <subagent_notification>{"agent_path":..,"status":{"completed":..}}.
func (s *childScan) codexSubNote(text string, ts int64) {
	for _, blk := range strings.Split(text, "<subagent_notification>")[1:] {
		body, _, _ := strings.Cut(blk, "</subagent_notification>")
		var n struct {
			AgentPath string                     `json:"agent_path"`
			Status    map[string]json.RawMessage `json:"status"`
		}
		if json.Unmarshal([]byte(strings.TrimSpace(body)), &n) != nil || n.AgentPath == "" {
			continue
		}
		for k := range n.Status {
			s.subNote[filepath.Base(n.AgentPath)] = childNote{status: k, ts: ts}
		}
	}
}

// ── assembly ──

func sessionChildren(ref watchedRef) (*SessionChildren, error) {
	s := childScanFor(ref.Path)
	defer s.release()
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.update(ref); err != nil {
		return nil, err
	}
	out := &SessionChildren{ID: ref.ID, Agent: ref.Tool, Children: []*SessionChild{}}
	out.Pane, out.HostID = paneOf(ref.ID)
	now := time.Now().UnixMilli()
	used := map[string]bool{ref.ID: true}
	var agentDir string
	if ref.Tool == ToolClaude {
		agentDir = filepath.Join(filepath.Dir(ref.Path), strings.TrimSuffix(filepath.Base(ref.Path), ".jsonl"), "subagents")
	}
	metas := readSubagentMetas(agentDir)
	byToolUse := map[string]*subagentMeta{}
	for _, m := range metas {
		if m.ToolUseID != "" {
			byToolUse[m.ToolUseID] = m
		}
	}
	topByAgent := map[string]*SessionChild{}
	for _, c := range s.calls {
		ch := &SessionChild{Key: c.callID, Kind: c.kind, Agent: string(c.agent), ToolUseID: c.callID, ParentItemID: c.itemID, By: c.by, Name: c.name,
			SubagentType: c.subType, Description: c.desc, Prompt: clipString(strings.TrimSpace(c.prompt), 600), Command: clipString(c.cmd, 600),
			Cwd: c.cwd, Model: c.model, Background: c.bg, StartedAt: c.ts, EndedAt: c.endTs}
		if ch.Key == "" {
			ch.Key = c.itemID
		}
		s.setParent(ref.ID, ch, c.off)
		switch {
		case c.kind == "subagent" && c.agent == ToolClaude:
			m := byToolUse[c.callID]
			if m == nil && c.agentID != "" {
				m = metaForAgent(metas, c.agentID)
			}
			s.claudeSubagent(ch, c, m, now)
			if ch.AgentID != "" {
				topByAgent[ch.AgentID] = ch
			}
		case c.kind == "subagent":
			s.codexSubagent(ch, c, used, now)
		case c.kind == "agent":
			s.crossAgent(ref, ch, c, used, now)
		case c.kind == "talk":
			talkTarget(ch, c)
		}
		out.Children = append(out.Children, ch)
	}
	out.Children = append(out.Children, s.nestedSubagents(ref.ID, metas, topByAgent, now)...)
	if ref.Tool == ToolCodex {
		out.Children = append(out.Children, s.codexOrphans(ref, used, now)...)
	}
	out.Children = append(out.Children, paneChildren(ref, out.Pane, used)...)
	sort.SliceStable(out.Children, func(i, j int) bool { return out.Children[i].StartedAt < out.Children[j].StartedAt })
	out.Count = len(out.Children)
	return out, nil
}

// setParent fills the turn (run and user message) that holds the transcript offset.
func (s *childScan) setParent(thread string, ch *SessionChild, off int64) {
	starts := s.ix.starts
	k := sort.Search(len(starts), func(i int) bool { return starts[i] > off })
	if k == 0 {
		return
	}
	st := starts[k-1]
	ch.ParentRunID = fmt.Sprintf("%s@%d", thread, st)
	ch.ParentMsgID = fmt.Sprintf("%s@%d.0", thread, st)
	ch.ParentPrompt = clipString(firstLine(s.prompts[st]), 200)
}

// setParentByTs places a child with no tool call (an orphan) in the last turn that started at or before ts.
func (s *childScan) setParentByTs(thread string, ch *SessionChild, ts int64) {
	k := sort.Search(len(s.tsVal), func(i int) bool { return s.tsVal[i] > ts })
	if k > 0 {
		s.setParent(thread, ch, s.tsOff[k-1])
	}
}

func noteStatus(st string) string {
	switch strings.ToLower(st) {
	case "completed", "complete", "done", "finished", "success":
		return "completed"
	case "failed", "error", "errored":
		return "failed"
	case "killed", "stopped", "cancelled", "canceled", "interrupted", "shutdown":
		return "killed"
	case "running", "pending_init", "in_progress":
		return "running"
	}
	return ""
}

func (s *childScan) claudeSubagent(ch *SessionChild, c *childCall, m *subagentMeta, now int64) {
	ch.LinkBasis = "tool_use id"
	if c.agentID != "" {
		ch.AgentID = c.agentID
	}
	var st *subagentStats
	if m != nil {
		ch.AgentID = firstOf(ch.AgentID, m.AgentID)
		ch.SubagentType = firstOf(ch.SubagentType, m.AgentType)
		ch.Description = firstOf(ch.Description, m.Description)
		ch.LinkBasis = "tool_use id + subagent meta"
		st = subagentStatsFor(m.Path)
		ch.HasTranscript = st != nil
		if m.RequestShape == "background" {
			ch.Background = true
		}
	}
	if st != nil {
		ch.Model = firstOf(st.model, ch.Model, m.Model)
		ch.InTokens, ch.OutTokens = st.tok.Input+st.tok.CacheRead+st.tok.CacheCreate, st.tok.Output
		ch.CostUSD = estimateCost(ch.Model, st.tok)
		if st.firstTs > 0 && (ch.StartedAt == 0 || st.firstTs < ch.StartedAt) {
			ch.StartedAt = st.firstTs
		}
	}
	live := st != nil && now-st.mod < childLiveWindow.Milliseconds()
	n, hasNote := s.notes[c.callID]
	switch {
	case hasNote && noteStatus(n.status) != "":
		ch.Status, ch.EndedAt = noteStatus(n.status), n.ts
		if live && st.lastTs > n.ts+2000 {
			ch.Status, ch.EndedAt = "running", 0
		}
	case c.done && c.failed:
		ch.Status, ch.EndedAt = "failed", c.endTs
	case c.done && !c.async && !ch.Background:
		ch.Status, ch.EndedAt = "completed", c.endTs
		if c.failed {
			ch.Status = "failed"
		}
	case m != nil && m.Finished:
		ch.Status = "completed"
		if st != nil {
			ch.EndedAt = st.lastTs
		}
	case live || (st == nil && !c.done && now-c.ts < childLiveWindow.Milliseconds()):
		ch.Status, ch.EndedAt = "running", 0
	default:
		ch.Status = "stopped"
		if st != nil {
			ch.EndedAt = st.lastTs
		}
	}
	if ch.Status != "running" && ch.EndedAt == 0 && st != nil {
		ch.EndedAt = st.lastTs
	}
}

// nestedSubagents lists subagents started by another subagent, and any whose call is not in the parent transcript.
func (s *childScan) nestedSubagents(thread string, metas []*subagentMeta, top map[string]*SessionChild, now int64) []*SessionChild {
	var out []*SessionChild
	calls := map[string]*nestedCall{}
	for _, m := range metas {
		if st := subagentStatsFor(m.Path); st != nil {
			for id, nc := range st.calls {
				cp := *nc // st is shared through subagentStatCache, so it stays read-only
				cp.owner = m.AgentID
				calls[id] = &cp
			}
		}
	}
	seen := map[string]bool{}
	for id := range top {
		seen[id] = true
	}
	for _, m := range metas {
		if seen[m.AgentID] {
			continue
		}
		seen[m.AgentID] = true
		ch := &SessionChild{Key: firstOf(m.ToolUseID, "agent-"+m.AgentID), Kind: "subagent", Agent: string(ToolClaude), ToolUseID: m.ToolUseID, Description: m.Description, SubagentType: m.AgentType, LinkBasis: "subagents dir"}
		c := &childCall{callID: m.ToolUseID, agentID: m.AgentID}
		if nc := calls[m.ToolUseID]; nc != nil {
			ch.ParentAgentID, ch.Prompt, c.ts, c.bg = nc.owner, clipString(nc.prompt, 600), nc.ts, nc.bg
			ch.StartedAt = nc.ts
			if p := top[nc.owner]; p != nil {
				ch.ParentItemID, ch.ParentRunID, ch.ParentMsgID, ch.ParentPrompt = p.ParentItemID, p.ParentRunID, p.ParentMsgID, p.ParentPrompt
			}
		}
		s.claudeSubagent(ch, c, m, now)
		if ch.ParentAgentID != "" {
			ch.LinkBasis = "tool_use id in subagent " + ch.ParentAgentID
		} else if ch.ParentRunID == "" {
			s.setParentByTs(thread, ch, ch.StartedAt)
		}
		out = append(out, ch)
	}
	return out
}

func (s *childScan) codexSubagent(ch *SessionChild, c *childCall, used map[string]bool, now int64) {
	ch.LinkBasis = "spawn_agent call"
	if c.agentID == "" {
		ch.Status = "running"
		if c.failed {
			ch.Status = "failed"
		} else if now-c.ts > childLiveWindow.Milliseconds() {
			ch.Status = "stopped"
		}
		return
	}
	ch.SessionID = sid(ToolCodex, c.agentID)
	ch.Key = firstOf(ch.Key, ch.SessionID)
	used[ch.SessionID] = true
	ch.LinkBasis = "spawn_agent call + agent_id"
	sess := sessionFor(ch.SessionID)
	fillFromSession(ch, sess)
	if n, ok := s.subNote[c.agentID]; ok && noteStatus(n.status) != "" {
		ch.Status, ch.EndedAt = noteStatus(n.status), n.ts
	} else if ch.Status == "" {
		ch.Status = "running"
		if sess == nil || now-sess.LastActivityAt > childLiveWindow.Milliseconds() {
			ch.Status = "stopped"
		}
	}
}

// codexOrphans adds Codex subagents known only by the child's parent_thread_id.
func (s *childScan) codexOrphans(ref watchedRef, used map[string]bool, now int64) []*SessionChild {
	var out []*SessionChild
	for _, r := range codexChildrenOf(ref.RawID) {
		id := sid(ToolCodex, r.SessionID)
		if used[id] {
			continue
		}
		used[id] = true
		ch := &SessionChild{Key: id, Kind: "subagent", Agent: string(ToolCodex), SessionID: id, SubagentType: r.Role, Description: r.Nickname, StartedAt: r.FirstTs, LinkBasis: "parent_thread_id"}
		sess := sessionFor(id)
		fillFromSession(ch, sess)
		if n, ok := s.subNote[r.SessionID]; ok && noteStatus(n.status) != "" {
			ch.Status, ch.EndedAt = noteStatus(n.status), n.ts
		} else if ch.Status == "" {
			ch.Status = "stopped"
		}
		s.setParentByTs(ref.ID, ch, ch.StartedAt)
		out = append(out, ch)
	}
	return out
}

func sessionFor(id string) *Session {
	if store == nil {
		return nil
	}
	return store.Get(id)
}

// fillFromSession copies what the store knows about a child session.
func fillFromSession(ch *SessionChild, sess *Session) {
	if sess == nil {
		return
	}
	ch.Cwd = firstOf(sess.Cwd, ch.Cwd)
	ch.Model = firstOf(sess.Model, ch.Model)
	if ch.StartedAt == 0 {
		ch.StartedAt = sess.StartedAt
	}
	if ch.Prompt == "" {
		ch.Prompt = clipString(firstOf(sess.FirstMessage, sess.Title), 600)
	}
	t := sess.Tokens
	ch.InTokens, ch.OutTokens = t.Input+t.CacheRead+t.CacheCreate, t.Output
	ch.CostUSD = estimateCost(ch.Model, t)
	switch sess.State {
	case StateRunning, StateAwaitingPermission:
		ch.Status = "running"
	}
	ch.Pane, ch.HostID = paneOf(sess.ID)
}

// paneOf reads the host and pane registries without asking tmux.
func paneOf(id string) (pane, host string) {
	if h := hostForSession(id); h != nil {
		return h.TmuxPane, h.HostID
	}
	if paneRegistry != nil {
		if reg := paneRegistry.Get(id); reg != nil {
			return reg.PaneID, ""
		}
	}
	return "", ""
}

func talkTarget(ch *SessionChild, c *childCall) {
	ch.Status, ch.LinkBasis = "completed", "talk_to_agent recipient"
	if !c.done {
		ch.Status = "running"
	} else if c.failed {
		ch.Status = "failed"
	}
	ch.Description = c.recipient
	if paneRegistry != nil {
		if reg := paneRegistry.ResolveRecipient(c.recipient); reg != nil {
			ch.SessionID, ch.Agent, ch.Pane = reg.AgentID, string(reg.Tool), reg.PaneID
			ch.LinkBasis = "talk_to_agent recipient + pane registry"
			return
		}
	}
	if ref := resolveWatched(c.recipient); ref.Resolved {
		ch.SessionID, ch.Agent = ref.ID, string(ref.Tool)
	}
}

// ── agents started by a shell command ──

type childCand struct {
	id, cwd, prompt, originator string
	tool                        Tool
	started                     int64
	sess                        *Session
}

// crossHit is a cached command-to-child match; a miss (id "") expires at until, a hit never does.
type crossHit struct {
	id, cwd, basis string
	started, until int64
}

var crossMatchCache = struct {
	mu sync.Mutex
	m  map[string]crossHit
}{m: map[string]crossHit{}}

// crossStartWindow caps how long after its command a child may start; a child starts within seconds.
const crossStartWindow = 60_000

func (s *childScan) crossAgent(ref watchedRef, ch *SessionChild, c *childCall, used map[string]bool, now int64) {
	ch.Prompt = firstOf(ch.Prompt, commandPrompt(c.cmd, c.agent))
	ch.Description = firstOf(ch.Description, firstLine(ch.Prompt))
	dir := firstOf(cmdDir(c.cmd, c.cwd), c.cwd)
	end := c.endTs
	if !c.done || c.bg {
		if n, ok := s.notes[c.callID]; ok {
			end = n.ts
		} else {
			end = now
		}
	}
	to := min(end, c.ts+crossStartWindow)
	cacheKey := ref.Path + "|" + ch.Key
	var best *childCand
	basis := ""
	crossMatchCache.mu.Lock()
	h, hit := crossMatchCache.m[cacheKey]
	crossMatchCache.mu.Unlock()
	if hit && h.id == "" && now >= h.until {
		hit = false
	}
	if hit {
		if h.id != "" {
			best = &childCand{id: h.id, tool: c.agent, cwd: h.cwd, started: h.started, sess: sessionFor(h.id)}
			basis = h.basis
		}
	} else {
		best, basis = matchChild(crossCandidates(c.agent, dir, c.ts, to), used, dir, ch.Prompt, c.ts, to)
		h = crossHit{until: now + 60_000}
		if best != nil {
			h = crossHit{id: best.id, cwd: best.cwd, basis: basis, started: best.started}
		}
		crossMatchCache.mu.Lock()
		if len(crossMatchCache.m) > 2048 {
			crossMatchCache.m = map[string]crossHit{}
		}
		crossMatchCache.m[cacheKey] = h
		crossMatchCache.mu.Unlock()
	}
	ch.Status = "completed"
	switch {
	case c.failed:
		ch.Status = "failed"
	case !c.done || c.bg:
		ch.Status = "running"
		if n, ok := s.notes[c.callID]; ok && noteStatus(n.status) != "" {
			ch.Status, ch.EndedAt = noteStatus(n.status), n.ts
		} else if now-c.ts > 24*time.Hour.Milliseconds() {
			ch.Status = "stopped"
		}
	}
	ch.LinkBasis = "command"
	if best == nil {
		return
	}
	used[best.id] = true
	ch.SessionID, ch.LinkBasis = best.id, basis
	ch.Cwd = firstOf(best.cwd, ch.Cwd)
	if best.started > 0 {
		ch.StartedAt = best.started
	}
	sess := best.sess
	if sess == nil {
		sess = sessionFor(best.id)
	}
	callStatus := ch.Status
	ch.Status = ""
	fillFromSession(ch, sess)
	// The child's own transcript decides: a detached run outlives the command that started it.
	var mod int64
	if p := resolveWatched(best.id).Path; p != "" {
		if fi, err := os.Stat(p); err == nil {
			mod = fi.ModTime().UnixMilli()
		}
	}
	switch {
	case ch.Status == "running" || sess == nil && mod > 0 && now-mod < childLiveWindow.Milliseconds() && (c.bg || !c.done):
		ch.Status, ch.EndedAt = "running", 0
	case callStatus == "running":
		ch.Status, ch.EndedAt = "completed", max(mod, ch.EndedAt)
	default:
		ch.Status = callStatus
		ch.EndedAt = max(ch.EndedAt, mod)
	}
	if pp, _ := paneOf(ref.ID); ch.Pane != "" && ch.Pane == pp {
		ch.LinkBasis += " + pane"
	}
}

// matchChild prefers a matching inline prompt, then the command's folder, then the closest start.
func matchChild(cands []*childCand, used map[string]bool, dir, prompt string, from, to int64) (*childCand, string) {
	np := normPrompt(prompt)
	dir = strings.TrimRight(dir, "/")
	var best *childCand
	bestScore, bestDelta, inWindow := -1, int64(1<<62), 0
	for _, c := range cands {
		if used[c.id] || c.started < from-2000 || c.started > to+5000 {
			continue
		}
		inWindow++
		score := 0
		if np != "" && len(np) >= 8 {
			if nc := normPrompt(c.prompt); nc != "" && (strings.HasPrefix(nc, np[:min(len(np), len(nc))]) || strings.HasPrefix(np, nc[:min(len(np), len(nc))])) {
				score += 4
			}
		}
		if dir != "" && strings.TrimRight(c.cwd, "/") == dir {
			score += 2
		}
		d := abs64(c.started - from)
		if score > bestScore || score == bestScore && d < bestDelta {
			best, bestScore, bestDelta = c, score, d
		}
	}
	if best == nil {
		return nil, ""
	}
	if bestScore == 0 && (inWindow > 1 || best.started-from > 10_000) {
		return nil, ""
	}
	basis := "command + time"
	if bestScore&2 != 0 {
		basis += " + cwd"
	}
	if bestScore&4 != 0 {
		basis += " + prompt"
	}
	return best, basis
}

// crossCandidates lists sessions of the tool that started in [from, to]: the store first, then the transcripts on disk.
func crossCandidates(tool Tool, dir string, from, to int64) []*childCand {
	var out []*childCand
	seen := map[string]bool{}
	if store != nil {
		for _, s := range store.All() {
			if s.Tool == tool && s.StartedAt >= from-2000 && s.StartedAt <= to+5000 {
				seen[s.ID] = true
				out = append(out, &childCand{id: s.ID, tool: tool, cwd: s.Cwd, prompt: firstOf(s.FirstMessage, s.Title), started: s.StartedAt, sess: s})
			}
		}
	}
	var disk []*childCand
	if tool == ToolCodex {
		disk = codexRolloutsBetween(from-2000, to+5000)
	} else if dir != "" {
		disk = claudeTranscriptsBetween(dir, from-2000, to+5000)
	}
	for _, c := range disk {
		if !seen[c.id] {
			out = append(out, c)
		}
	}
	return out
}

var reRolloutName = regexp.MustCompile(`^rollout-(\d{4}-\d{2}-\d{2}T\d{2}-\d{2}-\d{2})-(.+)\.jsonl$`)

func codexRolloutsBetween(from, to int64) []*childCand {
	if to-from > 7*24*time.Hour.Milliseconds() {
		from = to - 7*24*time.Hour.Milliseconds()
	}
	var out []*childCand
	days := map[string]bool{}
	for t := time.UnixMilli(from).Add(-24 * time.Hour); !t.After(time.UnixMilli(to).Add(24 * time.Hour)); t = t.Add(24 * time.Hour) {
		days[t.Format("2006/01/02")] = true
	}
	for d := range days {
		ents, _ := os.ReadDir(filepath.Join(codexHome(), "sessions", filepath.FromSlash(d)))
		for _, e := range ents {
			m := reRolloutName.FindStringSubmatch(e.Name())
			if m == nil {
				continue
			}
			t, err := time.ParseInLocation("2006-01-02T15-04-05", m[1], time.Local)
			if err != nil || t.UnixMilli() < from-1000 || t.UnixMilli() > to {
				continue
			}
			if c := codexCandFromFile(filepath.Join(codexHome(), "sessions", filepath.FromSlash(d), e.Name())); c != nil {
				out = append(out, c)
			}
		}
	}
	return out
}

func codexCandFromFile(path string) *childCand {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	var c *childCand
	for i := 0; i < 40 && sc.Scan(); i++ {
		line := sc.Bytes()
		if c == nil {
			var rec codexLineRec
			if json.Unmarshal(line, &rec) != nil || rec.Type != "session_meta" {
				return nil
			}
			var meta struct {
				ID         string `json:"id"`
				Cwd        string `json:"cwd"`
				Originator string `json:"originator"`
				Timestamp  string `json:"timestamp"`
			}
			if json.Unmarshal(rec.Payload, &meta) != nil || meta.ID == "" {
				return nil
			}
			c = &childCand{id: sid(ToolCodex, meta.ID), tool: ToolCodex, cwd: meta.Cwd, originator: meta.Originator, started: firstNonZero(parseTimestamp(meta.Timestamp), parseTimestamp(rec.Timestamp))}
			continue
		}
		if t, _ := codexPromptLine(line); t != "" {
			c.prompt = t
			break
		}
	}
	return c
}

func claudeTranscriptsBetween(dir string, from, to int64) []*childCand {
	pd := claudeProjectDir(dir)
	ents, _ := os.ReadDir(pd)
	var out []*childCand
	for _, e := range ents {
		if !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		if info, err := e.Info(); err != nil || info.ModTime().UnixMilli() < from {
			continue
		}
		c := claudeCandFromFile(filepath.Join(pd, e.Name()))
		if c != nil && c.started >= from && c.started <= to {
			out = append(out, c)
		}
	}
	return out
}

func claudeCandFromFile(path string) *childCand {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	c := &childCand{id: sid(ToolClaude, strings.TrimSuffix(filepath.Base(path), ".jsonl")), tool: ToolClaude}
	for i := 0; i < 60 && sc.Scan(); i++ {
		var j struct {
			Timestamp string `json:"timestamp"`
			Cwd       string `json:"cwd"`
		}
		line := sc.Bytes()
		if json.Unmarshal(line, &j) != nil {
			continue
		}
		if c.started == 0 {
			c.started = parseTimestamp(j.Timestamp)
		}
		c.cwd = firstOf(c.cwd, j.Cwd)
		if t, _ := claudePromptLine(line); t != "" {
			c.prompt = t
			break
		}
	}
	if c.started == 0 {
		return nil
	}
	return c
}

func firstNonZero(v ...int64) int64 {
	for _, x := range v {
		if x != 0 {
			return x
		}
	}
	return 0
}

// paneChildren lists other agents registered on the parent's tmux pane while the parent ran.
func paneChildren(ref watchedRef, pane string, used map[string]bool) []*SessionChild {
	if pane == "" || paneRegistry == nil {
		return nil
	}
	parent := sessionFor(ref.ID)
	if parent == nil {
		return nil
	}
	var out []*SessionChild
	for _, reg := range paneRegistry.List() {
		if reg.PaneID != pane || used[reg.AgentID] || reg.AgentID == ref.ID {
			continue
		}
		if reg.RegisteredAt < parent.StartedAt || reg.RegisteredAt > parent.LastActivityAt {
			continue
		}
		used[reg.AgentID] = true
		ch := &SessionChild{Key: reg.AgentID, Kind: "agent", Agent: string(reg.Tool), SessionID: reg.AgentID, Pane: pane, Cwd: reg.Cwd, StartedAt: reg.RegisteredAt, Status: "stopped", LinkBasis: "pane + time"}
		fillFromSession(ch, sessionFor(reg.AgentID))
		out = append(out, ch)
	}
	return out
}

// ── claude subagent files ──

type subagentMeta struct {
	AgentID      string
	Path         string // agent-<id>.jsonl
	AgentType    string `json:"agentType"`
	Description  string `json:"description"`
	ToolUseID    string `json:"toolUseId"`
	RequestShape string `json:"requestShape"`
	Model        string `json:"pickedEffortModel"`
	Finished     bool   `json:"finished"`
}

func readSubagentMetas(dir string) []*subagentMeta {
	if dir == "" {
		return nil
	}
	files, _ := filepath.Glob(filepath.Join(dir, "agent-*.jsonl"))
	var out []*subagentMeta
	for _, jf := range files {
		m := &subagentMeta{Path: jf, AgentID: strings.TrimPrefix(strings.TrimSuffix(filepath.Base(jf), ".jsonl"), "agent-")}
		if raw, err := os.ReadFile(strings.TrimSuffix(jf, ".jsonl") + ".meta.json"); err == nil {
			_ = json.Unmarshal(raw, m)
		}
		out = append(out, m)
	}
	return out
}

func metaForAgent(metas []*subagentMeta, agentID string) *subagentMeta {
	for _, m := range metas {
		if m.AgentID == agentID {
			return m
		}
	}
	return nil
}

type nestedCall struct {
	owner, prompt string
	ts            int64
	bg            bool
}

type subagentStats struct {
	size, mod       int64
	firstTs, lastTs int64
	model           string
	tok             TokenUsage
	calls           map[string]*nestedCall
}

var subagentStatCache = struct {
	mu sync.Mutex
	m  map[string]*subagentStats
}{m: map[string]*subagentStats{}}

// subagentStatsFor sums a subagent transcript's usage, cached by size and mtime.
func subagentStatsFor(path string) *subagentStats {
	st, err := os.Stat(path)
	if err != nil {
		return nil
	}
	subagentStatCache.mu.Lock()
	c := subagentStatCache.m[path]
	subagentStatCache.mu.Unlock()
	if c != nil && c.size == st.Size() && c.mod == st.ModTime().UnixMilli() {
		return c
	}
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	c = &subagentStats{size: st.Size(), mod: st.ModTime().UnixMilli(), calls: map[string]*nestedCall{}}
	msgTok := map[string]TokenUsage{}
	_, _ = forEachLine(f, 0, st.Size(), func(_ int64, line []byte) {
		var j wClaudeLine
		if len(line) == 0 || json.Unmarshal(line, &j) != nil {
			return
		}
		if ts := parseTimestamp(j.Timestamp); ts > 0 {
			if c.firstTs == 0 {
				c.firstTs = ts
			}
			c.lastTs = max(c.lastTs, ts)
		}
		m := j.Message
		if m == nil || j.Type != "assistant" {
			return
		}
		if m.Model != "" && m.Model != "<synthetic>" {
			c.model = m.Model
		}
		if u := m.Usage; u != nil {
			msgTok[firstOf(m.ID, strconv.Itoa(len(msgTok)))] = TokenUsage{Input: u.Input, Output: u.Output, CacheRead: u.CacheRead, CacheCreate: u.CacheCreate}
		}
		if bytes.Contains(line, []byte(`"name":"Agent"`)) || bytes.Contains(line, []byte(`"name":"Task"`)) {
			var parts []wClaudePart
			_ = json.Unmarshal(m.Content, &parts)
			for _, p := range parts {
				if p.Type == "tool_use" && (p.Name == "Agent" || p.Name == "Task") {
					c.calls[p.ID] = &nestedCall{prompt: firstOf(claudeStr(p.Input, "prompt"), claudeStr(p.Input, "description")), ts: parseTimestamp(j.Timestamp), bg: p.Input["run_in_background"] == true}
				}
			}
		}
	})
	for _, u := range msgTok {
		c.tok.Input += u.Input
		c.tok.Output += u.Output
		c.tok.CacheRead += u.CacheRead
		c.tok.CacheCreate += u.CacheCreate
	}
	subagentStatCache.mu.Lock()
	if len(subagentStatCache.m) > 512 {
		subagentStatCache.m = map[string]*subagentStats{}
	}
	subagentStatCache.m[path] = c
	subagentStatCache.mu.Unlock()
	return c
}

// subItemsCap caps the items of one subagent transcript response; More says older ones were left out.
const subItemsCap = 400

// subTranscript is the incremental parse of one subagent transcript.
type subTranscript struct {
	mu      sync.Mutex
	fi      os.FileInfo
	p       *wParser
	lastUse time.Time // guarded by subTranscripts.mu
}

var subTranscripts = struct {
	mu sync.Mutex
	m  map[string]*subTranscript
}{m: map[string]*subTranscript{}}

func subTranscriptFor(path string) *subTranscript {
	subTranscripts.mu.Lock()
	defer subTranscripts.mu.Unlock()
	s := subTranscripts.m[path]
	if s == nil {
		if len(subTranscripts.m) >= watchedCacheMax {
			oldK := ""
			var old time.Time
			for k, v := range subTranscripts.m {
				if oldK == "" || v.lastUse.Before(old) {
					oldK, old = k, v.lastUse
				}
			}
			delete(subTranscripts.m, oldK)
		}
		s = &subTranscript{}
		subTranscripts.m[path] = s
	}
	s.lastUse = time.Now()
	return s
}

// subagentItems parses a Claude subagent transcript into chat items; its lines are sidechain lines.
// after>=0 returns only what changed at or after that byte cursor, like /items.
func subagentItems(ref watchedRef, agentID string, after int64) (*WatchedItems, error) {
	if ref.Tool != ToolClaude || strings.ContainsAny(agentID, `/\.`) || agentID == "" {
		return nil, os.ErrNotExist
	}
	path := filepath.Join(filepath.Dir(ref.Path), strings.TrimSuffix(filepath.Base(ref.Path), ".jsonl"), "subagents", "agent-"+agentID+".jsonl")
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	thread := ref.ID + "/agent-" + agentID
	s := subTranscriptFor(path)
	s.mu.Lock()
	defer s.mu.Unlock()
	reset := false
	if s.p == nil || s.fi == nil || !os.SameFile(s.fi, st) || st.Size() < s.p.off || st.ModTime().Before(s.fi.ModTime()) {
		reset = s.p != nil
		s.p = &wParser{tool: ToolClaude, thread: thread, ix: &wIndex{}, pending: map[string]*wItem{}, cwd: ref.Cwd, dup: map[string]int64{}, bgByID: map[string]*wItem{}}
		s.fi = nil
	}
	if s.fi == nil || st.Size() != s.fi.Size() || !st.ModTime().Equal(s.fi.ModTime()) {
		p := s.p
		end, err := forEachLine(f, p.off, st.Size(), func(off int64, line []byte) {
			line = bytes.Replace(line, bIsSidechain, []byte(`"isSidechain":false`), 1)
			if t, _ := claudePromptLine(line); t != "" {
				p.ix.starts = append(p.ix.starts, off)
			}
			p.line(off, line)
		})
		if err != nil {
			s.p = nil
			return nil, err
		}
		p.off, p.ix.off = end, end
	}
	s.fi = st
	out := &WatchedItems{ID: thread, Agent: ToolClaude, Items: []*OItem{}, Runs: []*ORun{}, Cursor: s.p.off}
	if after > s.p.off || (reset && after >= 0) {
		out.Reset, after = true, -1
	}
	s.p.collect(out, after, -1)
	if len(out.Items) > subItemsCap {
		out.Items, out.More = out.Items[len(out.Items)-subItemsCap:], true
		out.Reset = out.Reset || after >= 0
	}
	// Every run goes out each time: a subagent has few, and the quiet check below changes them without a new line.
	out.Runs = out.Runs[:0]
	for _, t := range s.p.turns {
		r := *t.run
		out.Runs = append(out.Runs, &r)
	}
	// A subagent ends with a hand-back call, not end_turn; a quiet transcript is a finished one.
	if time.Since(st.ModTime()) > childLiveWindow {
		for _, r := range out.Runs {
			if r.Status == "running" {
				r.Status, r.EndedAt = "completed", max(r.StartedAt, st.ModTime().UnixMilli())
			}
		}
	}
	return out, nil
}

func serveSessionChildren(w http.ResponseWriter, id string) {
	ref := resolveWatched(id)
	if !ref.Resolved || ref.Path == "" {
		writeJSONStatus(w, http.StatusNotFound, map[string]any{"error": "no transcript for session"})
		return
	}
	if ref.Tool != ToolClaude && ref.Tool != ToolCodex {
		writeJSON(w, &SessionChildren{ID: ref.ID, Agent: ref.Tool, Children: []*SessionChild{}})
		return
	}
	res, err := sessionChildren(ref)
	if err != nil {
		writeJSONStatus(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, res)
}

func serveSubagentItems(w http.ResponseWriter, r *http.Request, id, agentID string) {
	after := int64(-1)
	if v := r.URL.Query().Get("after"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": "after must be a byte offset"})
			return
		}
		after = n
	}
	ref := resolveWatched(id)
	if !ref.Resolved || ref.Path == "" {
		writeJSONStatus(w, http.StatusNotFound, map[string]any{"error": "no transcript for session"})
		return
	}
	res, err := subagentItems(ref, agentID, after)
	if err != nil {
		writeJSONStatus(w, http.StatusNotFound, map[string]any{"error": "no transcript for subagent"})
		return
	}
	writeJSON(w, res)
}

// childrenRoute serves {id}/children and {id}/children/{agentId}/items.
func childrenRoute(w http.ResponseWriter, r *http.Request, path string) bool {
	if id, ok := strings.CutSuffix(path, "/children"); ok {
		serveSessionChildren(w, id)
		return true
	}
	i := strings.LastIndex(path, "/children/")
	if i < 0 || !strings.HasSuffix(path, "/items") {
		return false
	}
	agentID := strings.TrimSuffix(path[i+len("/children/"):], "/items")
	serveSubagentItems(w, r, path[:i], agentID)
	return true
}
