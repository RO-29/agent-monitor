package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// buildClaudeTrace parses one Claude Code transcript (plus its subagent
// transcripts) into segments, spans, learnings, outputs and chapters.
//
// Boundary signals (all already on disk, see DESIGN.md §6):
//   - system/compact_boundary + compactMetadata, followed by the
//     isCompactSummary user message that carries the summary text
//   - attachment hook records named SessionStart:clear|compact|resume
//   - a first user prompt of <command-name>/clear
func buildClaudeTrace(sess *Session) *SessionTrace {
	tr := &SessionTrace{SessionID: sess.ID, Tool: ToolClaude, io: map[string]SpanIO{}}
	p := &claudeTraceParser{tr: tr, sess: sess, curTurn: -1, pendingCompact: -1, seenTool: map[string]bool{}, inFile: map[string]bool{}}
	p.subagentIndex(sess.TranscriptPath, sess.SessionID)
	p.parseFile(sess.TranscriptPath, "", 0)
	p.closeTurn(p.lastAct)
	if tr.FirstTs == 0 {
		tr.FirstTs = sess.StartedAt
	}
	if tr.LastTs == 0 {
		tr.LastTs = sess.LastActivityAt
	}
	if len(tr.Segments) == 0 || tr.Segments[0].Boundary.At > tr.FirstTs {
		start := Boundary{Kind: "start", At: tr.FirstTs}
		if p.startedWithClear {
			start.Kind = "clear"
		}
		tr.Segments = append([]Segment{{Boundary: start}}, tr.Segments...)
	}
	if tr.Model == "" {
		tr.Model = sess.Model
	}
	tr.ContextWindow = contextWindowFor(tr.Model)
	if p.maxPre > 900_000 {
		tr.ContextWindow = 1_000_000
	}
	tr.ContextUsed = p.lastContext
	if p.costUSD > 0 {
		tr.CostUSD = p.costUSD
	} else {
		tr.CostUSD = estimateCost(tr.Model, sess.Tokens)
		tr.CostEstimated = true
	}
	finalizeTrace(tr, p.aways)
	if p.costUSD > 0 {
		// scale segment estimates so they sum to the exact session cost
		var sum float64
		for _, s := range tr.Segments {
			sum += s.UsdEst
		}
		if sum > 0 {
			for i := range tr.Segments {
				tr.Segments[i].UsdEst = tr.Segments[i].UsdEst * p.costUSD / sum
			}
		}
	}
	return tr
}

type claudeTraceParser struct {
	tr   *SessionTrace
	sess *Session

	subagents map[string]string // toolUseId → agent-*.jsonl path

	curTurn          int // index into tr.Spans, -1 = none
	turnCount        int
	lastAct          int64
	lastEditTs       int64
	pendingCompact   int // index into tr.Segments whose boundary awaits its summary, -1 = none
	startedWithClear bool
	sawFirstPrompt   bool
	maxPre           int64
	lastContext      int64
	costUSD          float64
	aways            []IntentChange
	seenTool         map[string]bool // tool_use ids already emitted (fork transcripts replay the parent's history)
	inFile           map[string]bool // subagent files on the current parse stack
}

func (p *claudeTraceParser) turn() *Span {
	if p.curTurn < 0 || p.curTurn >= len(p.tr.Spans) {
		return nil
	}
	return &p.tr.Spans[p.curTurn]
}

// subagentIndex maps every Agent tool_use id to the child transcript that ran
// it, from <projectDir>/<sessionId>/subagents/agent-*.meta.json.
func (p *claudeTraceParser) subagentIndex(transcript, sessionID string) {
	p.subagents = map[string]string{}
	dir := filepath.Join(filepath.Dir(transcript), sessionID, "subagents")
	metas, _ := filepath.Glob(filepath.Join(dir, "agent-*.meta.json"))
	for _, m := range metas {
		raw, err := os.ReadFile(m)
		if err != nil {
			continue
		}
		var meta struct {
			ToolUseID string `json:"toolUseId"`
		}
		if json.Unmarshal(raw, &meta) != nil || meta.ToolUseID == "" {
			continue
		}
		jsonl := strings.TrimSuffix(m, ".meta.json") + ".jsonl"
		if pathExists(jsonl) {
			p.subagents[meta.ToolUseID] = jsonl
		}
	}
}

// addBoundary appends a boundary unless the same kind was already recorded
// within 90 s (two signals describe one event). Returns the segment index.
func (p *claudeTraceParser) addBoundary(b Boundary) int {
	for i, s := range p.tr.Segments {
		if s.Boundary.Kind == b.Kind && abs64(s.Boundary.At-b.At) < 90_000 {
			return i
		}
	}
	p.tr.Segments = append(p.tr.Segments, Segment{Boundary: b})
	return len(p.tr.Segments) - 1
}

func abs64(x int64) int64 {
	if x < 0 {
		return -x
	}
	return x
}

func (p *claudeTraceParser) closeTurn(at int64) {
	t := p.turn()
	if t == nil {
		return
	}
	if at > t.Ts && t.Dur == 0 {
		t.Dur = at - t.Ts
	}
	if t.Dur < 0 {
		t.Dur = 0
	}
	p.curTurn = -1
}

// parseFile walks one JSONL file. parentID/depth are set when the file is a
// subagent transcript nested under an Agent span.
func (p *claudeTraceParser) parseFile(path, parentID string, depth int) {
	if p.inFile[path] {
		return // a replayed Agent call inside a fork must not re-enter its own file
	}
	p.inFile[path] = true
	defer delete(p.inFile, path)
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)

	tr := p.tr
	isChild := parentID != ""
	pendingIdx := map[string]int{} // tool_use id → index into tr.Spans
	var childLast int64

	for sc.Scan() {
		line := sc.Bytes()
		var j map[string]any
		if json.Unmarshal(line, &j) != nil {
			continue
		}
		kind, _ := j["type"].(string)
		ts := parseTimestamp(j["timestamp"])
		if ts > 0 && !isChild {
			if tr.FirstTs == 0 || ts < tr.FirstTs {
				tr.FirstTs = ts
			}
			if ts > tr.LastTs {
				tr.LastTs = ts
			}
		}
		if ts > childLast {
			childLast = ts
		}

		switch kind {
		case "cost-state":
			if !isChild {
				p.costUSD = toFloat(j["totalCostUSD"])
			}
			continue
		case "pr-link":
			if !isChild {
				n := int(toFloat(j["prNumber"]))
				url, _ := j["prUrl"].(string)
				tr.Outputs = append(tr.Outputs, Output{Kind: "pr", Label: fmt.Sprintf("PR #%d", n), Ref: url, Ts: ts})
			}
			continue
		case "frame-link":
			if !isChild {
				title, _ := j["title"].(string)
				url, _ := j["frameUrl"].(string)
				if title == "" {
					title = "artifact"
				}
				tr.Outputs = append(tr.Outputs, Output{Kind: "artifact", Label: clipString(title, 80), Ref: url, Ts: ts})
			}
			continue
		case "system":
			sub, _ := j["subtype"].(string)
			switch sub {
			case "compact_boundary":
				if isChild {
					continue
				}
				b := Boundary{Kind: "compact", At: ts}
				if cm, _ := j["compactMetadata"].(map[string]any); cm != nil {
					b.Trigger, _ = cm["trigger"].(string)
					b.PreTokens = int64(toFloat(cm["preTokens"]))
					b.PostTokens = int64(toFloat(cm["postTokens"]))
					b.CumulativeDropped = int64(toFloat(cm["cumulativeDroppedTokens"]))
					b.DurationMs = int64(toFloat(cm["durationMs"]))
					b.DroppedTokens = b.PreTokens - b.PostTokens
					if b.PreTokens > p.maxPre {
						p.maxPre = b.PreTokens
					}
				}
				p.closeTurn(p.lastAct)
				p.pendingCompact = p.addBoundary(b)
			case "turn_duration":
				if !isChild && p.turn() != nil {
					p.closeTurn(ts)
				}
			case "away_summary":
				if !isChild {
					c, _ := j["content"].(string)
					c = strings.TrimSpace(strings.Split(c, "(disable recaps")[0])
					if c != "" {
						p.aways = append(p.aways, IntentChange{Ts: ts, Text: c})
						tr.Learnings = append(tr.Learnings, Learning{ID: spanID("lrn", "away"+fmt.Sprint(ts)), Source: "summary", Text: clipString(c, 300), Evidence: "away_summary", Ts: ts})
					}
				}
			}
			continue
		case "attachment":
			if isChild {
				continue
			}
			att, _ := j["attachment"].(map[string]any)
			hn, _ := att["hookName"].(string)
			switch {
			case strings.HasPrefix(hn, "SessionStart:clear"):
				if len(tr.Segments) == 0 && !p.sawFirstPrompt {
					p.startedWithClear = true
				} else {
					p.closeTurn(p.lastAct)
					p.addBoundary(Boundary{Kind: "clear", At: ts})
				}
			case strings.HasPrefix(hn, "SessionStart:resume"):
				p.closeTurn(p.lastAct)
				p.addBoundary(Boundary{Kind: "resume", At: ts})
			case strings.HasPrefix(hn, "SessionStart:compact"):
				p.closeTurn(p.lastAct)
				idx := p.addBoundary(Boundary{Kind: "compact", At: ts, Trigger: "auto"})
				if p.pendingCompact < 0 {
					p.pendingCompact = idx
				}
			}
			continue
		}

		msg, _ := j["message"].(map[string]any)
		if msg == nil {
			continue
		}
		model, _ := msg["model"].(string)
		if model != "" && !isChild {
			tr.Model = model
		}

		// Compact summary line: attach to the pending boundary, never a prompt.
		if isSum, _ := j["isCompactSummary"].(bool); isSum {
			text, _, _, _ := extractClaudeContent(j)
			if p.pendingCompact >= 0 && p.pendingCompact < len(tr.Segments) {
				b := &tr.Segments[p.pendingCompact].Boundary
				b.Summary = clipString(text, 16000)
				b.Sections = parseSummarySections(text)
				for _, t := range bullets(sectionLike(b.Sections, "errors and fixes", "error", "problem solving"), 6, 240) {
					tr.Learnings = append(tr.Learnings, Learning{ID: spanID("lrn", t), Source: "summary", Text: t, Evidence: "compact summary · errors and fixes", Ts: b.At})
				}
				p.pendingCompact = -1
			}
			continue
		}
		if meta, _ := j["isMeta"].(bool); meta {
			continue
		}

		text, isToolResult, isUserText, isAssistantText := extractClaudeContent(j)

		// Token usage on assistant turns → current turn + live context size.
		if usage, _ := msg["usage"].(map[string]any); usage != nil && !isChild {
			in := int64(toFloat(usage["input_tokens"]))
			out := int64(toFloat(usage["output_tokens"]))
			cr := int64(toFloat(usage["cache_read_input_tokens"]))
			cc := int64(toFloat(usage["cache_creation_input_tokens"]))
			if t := p.turn(); t != nil {
				if t.Tokens == nil {
					t.Tokens = &TokenUsage{}
				}
				t.Tokens.Input += in
				t.Tokens.Output += out
				t.Tokens.CacheRead += cr
				t.Tokens.CacheCreate += cc
				t.Model = model
			}
			if in+cr+cc > 0 {
				p.lastContext = in + cr + cc
			}
		}

		// Real user prompt → close the previous turn, open a new one.
		if isUserText && !isToolResult && !isChild {
			prompt := claudePromptText(text)
			if strings.HasPrefix(strings.TrimSpace(text), "<command-name>/clear") && !p.sawFirstPrompt {
				p.startedWithClear = true
			}
			if prompt != "" {
				p.sawFirstPrompt = true
				p.closeTurn(p.lastAct)
				u := Span{ID: spanID("u", fmt.Sprint(ts)+prompt[:min(len(prompt), 40)]), Kind: "user", Name: "you", Ts: ts, Depth: 0, Fam: "user", Text: clipString(prompt, 400)}
				if isCorrection(prompt, ts, p.lastEditTs) {
					u.Flag = "correction"
					tr.Learnings = append(tr.Learnings, Learning{ID: spanID("lrn", u.ID), Source: "correction", Text: clipString(prompt, 240),
						Evidence: "user turn · " + fmtClock(ts), Ts: ts, Heuristic: true, Ref: u.ID})
				}
				tr.Spans = append(tr.Spans, u)
				p.turnCount++
				tr.Spans = append(tr.Spans, Span{ID: spanID("t", fmt.Sprint(ts)), Kind: "turn", Name: fmt.Sprintf("turn %d", p.turnCount), Ts: ts, Depth: 0, Fam: "model"})
				p.curTurn = len(tr.Spans) - 1
				p.lastAct = ts
			}
		}
		if isAssistantText && !isChild {
			p.lastAct = ts
			if p.turn() == nil {
				// Assistant work after a compaction (or after turn_duration closed
				// the turn) continues the same request: open a continuation turn
				// so every tool span has a parent row.
				p.turnCount++
				tr.Spans = append(tr.Spans, Span{ID: spanID("t", fmt.Sprint(ts)), Kind: "turn", Name: fmt.Sprintf("turn %d · continued", p.turnCount), Ts: ts, Depth: 0, Fam: "model", Model: model})
				p.curTurn = len(tr.Spans) - 1
			}
			if t := p.turn(); t != nil && t.Text == "" && text != "" && !strings.HasPrefix(text, "→ ") {
				t.Text = clipString(strings.Join(strings.Fields(text), " "), 400)
			}
		}

		// Tool parts.
		c, _ := msg["content"].([]any)
		for _, part := range c {
			pm, _ := part.(map[string]any)
			if pm == nil {
				continue
			}
			ptype, _ := pm["type"].(string)
			switch ptype {
			case "tool_use":
				name, _ := pm["name"].(string)
				id, _ := pm["id"].(string)
				if name == "" || id == "" {
					continue
				}
				// A forked subagent's transcript starts with a copy of the parent's
				// history, so its early tool_use ids are replays: emit each id once.
				if p.seenTool[id] {
					continue
				}
				p.seenTool[id] = true
				input, _ := pm["input"].(map[string]any)
				fam := toolFamily(name)
				sp := Span{ID: spanID("s", id), Kind: "tool", Name: name, Res: claudeToolResource(name, input), Ts: ts, Depth: depth + 1, Fam: fam}
				if isChild {
					sp.Parent = parentID
				} else if t := p.turn(); t != nil {
					sp.Parent = t.ID
				}
				if fam == "agent" {
					sp.Kind = "agent"
				}
				argsJSON, _ := json.Marshal(input)
				tr.io[sp.ID] = SpanIO{Args: clipString(string(argsJSON), 20000)}
				tr.Spans = append(tr.Spans, sp)
				pendingIdx[id] = len(tr.Spans) - 1
				if !isChild {
					p.lastAct = ts
				}
				// side effects: files, outputs, memory learnings
				if fam == "edit" {
					fp, _ := input["file_path"].(string)
					if !isChild {
						p.lastEditTs = ts
					}
					if fp != "" {
						if strings.Contains(fp, "/memory/") && strings.Contains(fp, "/.claude/projects/") && !strings.HasSuffix(fp, "MEMORY.md") {
							tr.Learnings = append(tr.Learnings, memoryLearning(fp, ts, 0, name))
						} else if reHandoffDoc.MatchString(fp) && name == "Write" {
							tr.Outputs = append(tr.Outputs, Output{Kind: "doc", Label: filepath.Base(fp), Ref: fp, Ts: ts})
						}
					}
				}
				if fam == "bash" {
					cmd, _ := input["command"].(string)
					if reGitCommit.MatchString(cmd) {
						tr.Outputs = append(tr.Outputs, Output{Kind: "commit", Label: "git commit", Ref: clipString(firstLine(cmd), 160), Ts: ts})
					}
				}
				// Subagent transcript → nested spans.
				if fam == "agent" && depth < 3 {
					if child, ok := p.subagents[id]; ok {
						p.parseFile(child, sp.ID, depth+1)
					}
				}
			case "tool_result":
				id, _ := pm["tool_use_id"].(string)
				idx, ok := pendingIdx[id]
				if !ok || idx >= len(tr.Spans) {
					continue
				}
				sp := &tr.Spans[idx]
				if ts > sp.Ts {
					sp.Dur = ts - sp.Ts
				}
				if e, _ := pm["is_error"].(bool); e {
					sp.Err = true
				}
				res := extractToolResultText(pm)
				head := res[:min(len(res), 200)]
				if !sp.Err && sp.Fam == "bash" && strings.Contains(head, "Exit code") && !strings.Contains(head, "Exit code 0") {
					sp.Err = true
				}
				io := tr.io[sp.ID]
				io.Result = clipString(res, 20000)
				tr.io[sp.ID] = io
				if !isChild {
					p.lastAct = ts
				}
			}
		}
	}
	// A subagent's parent span ends when its file ends if the tool_result is missing.
	if isChild && parentID != "" {
		for i := range tr.Spans {
			if tr.Spans[i].ID == parentID && tr.Spans[i].Dur == 0 && childLast > tr.Spans[i].Ts {
				tr.Spans[i].Dur = childLast - tr.Spans[i].Ts
			}
		}
	}
}

func fmtClock(ms int64) string {
	if ms == 0 {
		return ""
	}
	return msToTime(ms).Format("15:04")
}
