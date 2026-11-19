package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

// codexLinks indexes subagent rollouts (children) and plain continuations (parents) by parent_thread_id.
var codexLinks = struct {
	mu       sync.Mutex
	children map[string][]codexChildRef // parent thread id → children
	parentOf map[string]string          // child session id → parent (continuation)
}{children: map[string][]codexChildRef{}, parentOf: map[string]string{}}

type codexChildRef struct {
	SessionID string
	Path      string
	Nickname  string
	Role      string
	Depth     int
	FirstTs   int64
}

func codexRecordLinks(sessionID, path string, payload map[string]any, ts int64) {
	parent, _ := payload["parent_thread_id"].(string)
	if parent == "" {
		return
	}
	codexLinks.mu.Lock()
	defer codexLinks.mu.Unlock()
	src, _ := payload["source"].(map[string]any)
	if sub, _ := src["subagent"].(map[string]any); sub != nil {
		spawn, _ := sub["thread_spawn"].(map[string]any)
		ref := codexChildRef{SessionID: sessionID, Path: path, FirstTs: ts}
		if spawn != nil {
			ref.Nickname, _ = spawn["agent_nickname"].(string)
			ref.Role, _ = spawn["agent_role"].(string)
			ref.Depth = int(toFloat(spawn["depth"]))
		}
		for _, c := range codexLinks.children[parent] {
			if c.SessionID == sessionID {
				return
			}
		}
		codexLinks.children[parent] = append(codexLinks.children[parent], ref)
		return
	}
	codexLinks.parentOf[sessionID] = parent
}

func codexParentOf(sessionID string) string {
	codexLinks.mu.Lock()
	defer codexLinks.mu.Unlock()
	return codexLinks.parentOf[sessionID]
}

func codexChildrenOf(sessionID string) []codexChildRef {
	codexLinks.mu.Lock()
	defer codexLinks.mu.Unlock()
	return append([]codexChildRef(nil), codexLinks.children[sessionID]...)
}

// buildCodexTrace parses a Codex rollout; boundaries are session_meta, compacted and context_compacted.
func buildCodexTrace(sess *Session) *SessionTrace { return buildCodexTraceIO(sess, false) }

// buildCodexTraceIO keeps every span's IO when all is set.
func buildCodexTraceIO(sess *Session, all bool) *SessionTrace {
	tr := &SessionTrace{SessionID: sess.ID, Tool: ToolCodex, io: spanIOMap{}, ioAll: all}
	f, err := os.Open(sess.TranscriptPath)
	if err != nil {
		return nil
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)

	curTurn := -1 // index into tr.Spans
	turnCount := 0
	lastAct := int64(0)
	lastEditTs := int64(0)
	pendingIdx := map[string]int{}
	var aways []IntentChange
	var lastTurnTokens *TokenUsage
	var window int64
	var lastTotal int64
	var prevTotal *TokenUsage

	closeTurn := func(at int64) {
		if curTurn < 0 {
			return
		}
		t := &tr.Spans[curTurn]
		if at > t.Ts && t.Dur == 0 {
			t.Dur = at - t.Ts
		}
		curTurn = -1
	}
	openTurn := func(ts int64, model string) {
		closeTurn(lastAct)
		turnCount++
		tr.Spans = append(tr.Spans, Span{ID: spanID("t", fmt.Sprint(ts)), Kind: "turn", Name: fmt.Sprintf("turn %d", turnCount), Ts: ts, Depth: 0, Fam: "model", Model: model})
		curTurn = len(tr.Spans) - 1
		lastAct = ts
	}

	for sc.Scan() {
		var j map[string]any
		if json.Unmarshal(sc.Bytes(), &j) != nil {
			continue
		}
		top, _ := j["type"].(string)
		ts := parseTimestamp(j["timestamp"])
		if cmd, it, ok := codexUserShellLine(sc.Bytes()); ok {
			start := ts - it.Duration.ms()
			if curTurn < 0 {
				openTurn(start, tr.Model)
			}
			sp := Span{ID: spanID("c", it.ID+cmd), Kind: "tool", Name: "!", Res: clipString(cmd, 400), Ts: start, Dur: ts - start, Depth: 1, Fam: "bash", By: "user", Parent: tr.Spans[curTurn].ID}
			if it.ExitCode != nil && *it.ExitCode != 0 {
				sp.Err = true
			}
			tr.setIO(&sp, SpanIO{Args: clipString(cmd, 20000), Result: clipString(it.AggregatedOutput, 20000)})
			tr.Spans = append(tr.Spans, sp)
			lastAct = ts
			continue
		}
		payload, _ := j["payload"].(map[string]any)
		if ts > 0 {
			if tr.FirstTs == 0 || ts < tr.FirstTs {
				tr.FirstTs = ts
			}
			if ts > tr.LastTs {
				tr.LastTs = ts
			}
		}
		switch top {
		case "session_meta":
			if payload != nil {
				id, _ := payload["id"].(string)
				codexRecordLinks(id, sess.TranscriptPath, payload, ts)
			}
			kind := "start"
			if payload != nil {
				if p, _ := payload["parent_thread_id"].(string); p != "" {
					if src, _ := payload["source"].(map[string]any); src == nil || src["subagent"] == nil {
						kind = "resume"
					}
				}
			}
			tr.Segments = append(tr.Segments, Segment{Boundary: Boundary{Kind: kind, At: ts}})
			continue
		case "compacted":
			closeTurn(lastAct)
			b := Boundary{Kind: "compact", At: ts, Trigger: "auto", PreTokens: lastTotal}
			if payload != nil {
				var parts []string
				if hist, _ := payload["replacement_history"].([]any); hist != nil {
					for _, h := range hist {
						hm, _ := h.(map[string]any)
						role, _ := hm["role"].(string)
						if role == "" || role == "developer" || role == "system" {
							continue
						}
						if t := codexFlattenContent(hm["content"]); t != "" && !codexLooksLikeSystemPrompt(t) {
							parts = append(parts, strings.ToUpper(role[:1])+role[1:]+": "+clipString(t, 2000))
						}
					}
				}
				if m, _ := payload["message"].(string); m != "" {
					parts = append([]string{m}, parts...)
				}
				b.Summary = clipString(strings.Join(parts, "\n\n"), 16000)
				b.Sections = parseSummarySections(b.Summary)
			}
			tr.Segments = append(tr.Segments, Segment{Boundary: b})
			continue
		case "turn_context":
			model, _ := payload["model"].(string)
			if model != "" {
				tr.Model = model
			}
			openTurn(ts, model)
			continue
		}

		if payload == nil {
			continue
		}
		inner, _ := payload["type"].(string)
		if top == "event_msg" {
			switch inner {
			case "context_compacted":
				// fallback when no `compacted` record preceded it
				dup := false
				for _, s := range tr.Segments {
					if s.Boundary.Kind == "compact" && abs64(s.Boundary.At-ts) < 5000 {
						dup = true
					}
				}
				if !dup {
					closeTurn(lastAct)
					tr.Segments = append(tr.Segments, Segment{Boundary: Boundary{Kind: "compact", At: ts, Trigger: "auto", PreTokens: lastTotal}})
				}
			case "user_message":
				msg, _ := payload["message"].(string)
				if msg == "" || codexLooksLikeSystemPrompt(msg) {
					continue
				}
				u := Span{ID: spanID("u", fmt.Sprint(ts)+msg[:min(len(msg), 40)]), Kind: "user", Name: "you", Ts: ts, Depth: 0, Fam: "user", Text: clipString(msg, 400)}
				if isCorrection(msg, ts, lastEditTs) {
					u.Flag = "correction"
					tr.Learnings = append(tr.Learnings, Learning{ID: spanID("lrn", u.ID), Source: "correction", Text: clipString(msg, 240), Evidence: "user turn · " + fmtClock(ts), Ts: ts, Heuristic: true, Ref: u.ID})
				}
				tr.Spans = append(tr.Spans, u)
				if curTurn < 0 || tr.Spans[curTurn].Ts < ts-2000 && tr.Spans[curTurn].Text != "" {
					// a prompt without a preceding turn_context starts a turn
					if curTurn < 0 {
						openTurn(ts, tr.Model)
					}
				}
			case "agent_message":
				msg, _ := payload["message"].(string)
				lastAct = ts
				if curTurn >= 0 && tr.Spans[curTurn].Text == "" && msg != "" {
					tr.Spans[curTurn].Text = clipString(strings.Join(strings.Fields(msg), " "), 400)
				}
			case "task_complete":
				lastAct = ts
				if last, _ := payload["last_agent_message"].(string); last != "" && curTurn >= 0 && tr.Spans[curTurn].Text == "" {
					tr.Spans[curTurn].Text = clipString(strings.Join(strings.Fields(last), " "), 400)
				}
				closeTurn(ts)
			case "turn_aborted":
				if curTurn >= 0 {
					tr.Spans[curTurn].Flag = "aborted"
				}
				closeTurn(ts)
			case "token_count":
				info, _ := payload["info"].(map[string]any)
				if info == nil {
					continue
				}
				if w := int64(toFloat(info["model_context_window"])); w > 0 {
					window = w
				}
				if last, _ := info["last_token_usage"].(map[string]any); last != nil {
					t := codexUsageDelta(info, &prevTotal)
					lastTurnTokens = t
					if curTurn >= 0 {
						if tr.Spans[curTurn].Tokens == nil {
							tr.Spans[curTurn].Tokens = &TokenUsage{}
						}
						tt := tr.Spans[curTurn].Tokens
						tt.Input += t.Input
						tt.Output += t.Output
						tt.CacheRead += t.CacheRead
					}
					if used := int64(toFloat(last["input_tokens"])); used > 0 {
						tr.ContextUsed = used
					}
				}
				if tot, _ := info["total_token_usage"].(map[string]any); tot != nil {
					lastTotal = int64(toFloat(tot["total_tokens"]))
				}
			case "exec_command_end", "mcp_tool_call_end", "patch_apply_end", "web_search_end":
				callID, _ := payload["call_id"].(string)
				if idx, ok := pendingIdx[callID]; ok {
					sp := &tr.Spans[idx]
					if ts > sp.Ts {
						sp.Dur = ts - sp.Ts
					}
					out := ""
					switch inner {
					case "exec_command_end":
						agg, _ := payload["aggregated_output"].(string)
						stdout, _ := payload["stdout"].(string)
						stderr, _ := payload["stderr"].(string)
						out = firstOf(agg, stdout)
						if stderr != "" {
							out += "\n--- STDERR ---\n" + stderr
						}
						if code := toFloat(payload["exit_code"]); code != 0 {
							sp.Err = true
						}
					case "mcp_tool_call_end":
						res, _ := payload["result"].(map[string]any)
						out = codexExtractMCPResult(res)
						if _, isErr := res["Err"]; isErr {
							sp.Err = true
						}
					case "patch_apply_end":
						stdout, _ := payload["stdout"].(string)
						out = stdout
						if ok, has := payload["success"].(bool); has && !ok {
							sp.Err = true
						}
					case "web_search_end":
						q, _ := payload["query"].(string)
						out = "query: " + q
					}
					tr.setIOResult(sp, clipString(out, 20000))
					lastAct = ts
				}
			}
			continue
		}
		if top == "response_item" {
			switch inner {
			case "function_call", "custom_tool_call", "web_search_call":
				name, _ := payload["name"].(string)
				if inner == "web_search_call" {
					name = "web_search"
				}
				if name == "" {
					continue
				}
				callID, _ := payload["call_id"].(string)
				args, _ := payload["arguments"].(string)
				if args == "" {
					args, _ = payload["input"].(string)
				}
				if args == "" {
					if b, err := json.Marshal(payload["arguments"]); err == nil && string(b) != "null" {
						args = string(b)
					}
				}
				fam := toolFamily(name)
				res := codexToolResource(name, args)
				sp := Span{ID: spanID("s", callID+name), Kind: "tool", Name: name, Res: res, Ts: ts, Depth: 1, Fam: fam}
				if curTurn >= 0 {
					sp.Parent = tr.Spans[curTurn].ID
				}
				tr.setIO(&sp, SpanIO{Args: clipString(args, 20000)})
				tr.Spans = append(tr.Spans, sp)
				pendingIdx[callID] = len(tr.Spans) - 1
				lastAct = ts
				if fam == "edit" {
					lastEditTs = ts
					for _, fp := range codexPatchFiles(args) {
						if reHandoffDoc.MatchString(fp) {
							tr.Outputs = append(tr.Outputs, Output{Kind: "doc", Label: fp[strings.LastIndex(fp, "/")+1:], Ref: fp, Ts: ts})
						}
					}
				}
				if fam == "bash" && reGitCommit.MatchString(args) {
					tr.Outputs = append(tr.Outputs, Output{Kind: "commit", Label: "git commit", Ref: clipString(res, 160), Ts: ts})
				}
			case "function_call_output", "custom_tool_call_output":
				callID, _ := payload["call_id"].(string)
				if idx, ok := pendingIdx[callID]; ok {
					sp := &tr.Spans[idx]
					if ts > sp.Ts && sp.Dur == 0 {
						sp.Dur = ts - sp.Ts
					}
					out, _ := payload["output"].(string)
					if tr.io.result(sp.ID) == "" {
						tr.setIOResult(sp, clipString(out, 20000))
					}
					lastAct = ts
				}
			case "message":
				role, _ := payload["role"].(string)
				if role == "assistant" {
					lastAct = ts
					if curTurn >= 0 && tr.Spans[curTurn].Text == "" {
						if t := codexFlattenContent(payload["content"]); t != "" {
							tr.Spans[curTurn].Text = clipString(strings.Join(strings.Fields(t), " "), 400)
						}
					}
				}
			}
		}
	}
	closeTurn(lastAct)
	_ = lastTurnTokens

	// Subagent rollouts → agent spans under the turn active when they started.
	for _, c := range codexChildrenOf(sess.SessionID) {
		first, last := codexFileSpan(c.Path)
		if first == 0 {
			continue
		}
		sp := Span{ID: spanID("a", c.SessionID), Kind: "agent", Name: "Agent", Res: strings.TrimSpace(c.Nickname + " · " + c.Role), Ts: first, Dur: last - first, Depth: 1, Fam: "agent", Child: "codex:" + c.SessionID}
		for i := len(tr.Spans) - 1; i >= 0; i-- {
			if tr.Spans[i].Kind == "turn" && tr.Spans[i].Ts <= first {
				sp.Parent = tr.Spans[i].ID
				break
			}
		}
		tr.Spans = append(tr.Spans, sp)
	}

	if tr.Model == "" {
		tr.Model = sess.Model
	}
	tr.ContextWindow = window
	if tr.ContextWindow == 0 {
		tr.ContextWindow = contextWindowFor(tr.Model)
	}
	tr.CostUSD = estimateCost(tr.Model, sess.Tokens)
	tr.CostEstimated = true
	if len(tr.Segments) == 0 {
		tr.Segments = append(tr.Segments, Segment{Boundary: Boundary{Kind: "start", At: tr.FirstTs}})
	}
	finalizeTrace(tr, aways)
	return tr
}

// codexFileSpan returns the first and last timestamps of a rollout file.
func codexFileSpan(path string) (int64, int64) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	var first, last int64
	for sc.Scan() {
		var j struct {
			Timestamp string `json:"timestamp"`
		}
		if json.Unmarshal(sc.Bytes(), &j) != nil || j.Timestamp == "" {
			continue
		}
		if t, err := time.Parse(time.RFC3339Nano, j.Timestamp); err == nil {
			ms := t.UnixMilli()
			if first == 0 || ms < first {
				first = ms
			}
			if ms > last {
				last = ms
			}
		}
	}
	return first, last
}

func codexToolResource(name, args string) string {
	var obj map[string]any
	if json.Unmarshal([]byte(args), &obj) == nil {
		cmd, ok := obj["command"]
		if !ok {
			cmd, ok = obj["cmd"] // exec_command uses "cmd"
		}
		if ok {
			switch v := cmd.(type) {
			case string:
				return clipString(firstLine(v), 140)
			case []any:
				var parts []string
				for _, p := range v {
					if s, ok := p.(string); ok {
						parts = append(parts, s)
					}
				}
				return clipString(strings.Join(parts, " "), 140)
			}
		}
		for _, k := range []string{"path", "file_path", "query", "pattern", "url"} {
			if v, ok := obj[k].(string); ok && v != "" {
				return clipString(v, 120)
			}
		}
	}
	if files := codexPatchFiles(args); len(files) > 0 {
		return clipString(strings.Join(files, ", "), 140)
	}
	return clipString(firstLine(args), 120)
}

func codexPatchFiles(args string) []string {
	var out []string
	for _, line := range strings.Split(args, "\n") {
		for _, marker := range []string{"*** Update File: ", "*** Add File: ", "*** Delete File: "} {
			if strings.HasPrefix(line, marker) {
				out = append(out, strings.TrimSpace(line[len(marker):]))
			}
		}
	}
	return out
}

func msToTime(ms int64) time.Time { return time.UnixMilli(ms) }

// codexUsageDelta returns the tokens one token_count event adds, from the
// cumulative total so repeated events count once. Input excludes cached input.
func codexUsageDelta(info map[string]any, prev **TokenUsage) *TokenUsage {
	read := func(m map[string]any) TokenUsage {
		in, cached := int64(toFloat(m["input_tokens"])), int64(toFloat(m["cached_input_tokens"]))
		return TokenUsage{Input: in - cached, CacheRead: cached, Output: int64(toFloat(m["output_tokens"]))}
	}
	tot, _ := info["total_token_usage"].(map[string]any)
	if tot == nil {
		last, _ := info["last_token_usage"].(map[string]any)
		u := read(last)
		return &u
	}
	cur := read(tot)
	if *prev == nil || cur.Input+cur.CacheRead+cur.Output < (*prev).Input+(*prev).CacheRead+(*prev).Output {
		*prev = &cur
		last, _ := info["last_token_usage"].(map[string]any)
		u := read(last)
		return &u
	}
	d := TokenUsage{Input: cur.Input - (*prev).Input, CacheRead: cur.CacheRead - (*prev).CacheRead, Output: cur.Output - (*prev).Output}
	*prev = &cur
	return &d
}
