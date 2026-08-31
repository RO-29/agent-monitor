package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

type ClaudeHookPayload struct {
	SessionID      string `json:"session_id"`
	TranscriptPath string `json:"transcript_path"`
	Cwd            string `json:"cwd"`
	HookEventName  string `json:"hook_event_name"`
	Message        string `json:"message"`
	ToolName       string `json:"tool_name"`
	Reason         string `json:"reason"`
}

var permissionRe = regexp.MustCompile(`(?i)permission|approve|allow|confirm`)

var (
	claudeTailMu sync.Mutex
	claudeTails  = map[string]func(){} // sessionId -> closer
)

func handleClaudeHook(s *Store, p ClaudeHookPayload) {
	if p.SessionID == "" {
		return
	}
	cwd := p.Cwd
	evt := p.HookEventName
	msg := p.Message
	if msg == "" {
		msg = p.Reason
	}

	base := UpsertInput{
		Tool: ToolClaude, SessionID: p.SessionID,
		Cwd: cwd, HasCwd: true,
	}
	// Persist the transcript path from every hook so /api/session/:id/full can
	// read the JSONL. Without this, live hook-tracked sessions carry an empty
	// TranscriptPath and the detail view renders no messages/tools.
	if p.TranscriptPath != "" {
		base.TranscriptPath, base.HasTranscript = p.TranscriptPath, true
	}

	switch evt {
	case "SessionStart":
		in := base
		in.State, in.HasState = StateRunning, true
		in.EventKind, in.EventText = evt, msg
		s.Upsert(in)
		ensureClaudeTail(s, p.TranscriptPath, p.SessionID, cwd)

	case "UserPromptSubmit", "PreToolUse", "PostToolUse":
		txt := p.ToolName
		if txt == "" {
			txt = msg
		}
		in := base
		in.State, in.HasState = StateRunning, true
		in.EventKind, in.EventText = evt, txt
		s.Upsert(in)

	case "Notification":
		isPerm := permissionRe.MatchString(msg)
		state := StateAwaitingInput
		if isPerm {
			state = StateAwaitingPermission
		}
		in := base
		in.State, in.HasState = state, true
		in.Message, in.HasMessage = msg, true
		if isPerm {
			in.PermissionMessage, in.HasPerm = msg, true
		}
		in.EventKind, in.EventText = evt, msg
		s.Upsert(in)

	case "Stop", "SubagentStop":
		in := base
		in.State, in.HasState = StateAwaitingInput, true
		in.EventKind, in.EventText = evt, msg
		s.Upsert(in)

	case "SessionEnd":
		s.SetState(ToolClaude, p.SessionID, StateCompleted, msg)

	default:
		kind := evt
		if kind == "" {
			kind = "event"
		}
		in := base
		in.State, in.HasState = StateRunning, true
		in.EventKind, in.EventText = kind, msg
		s.Upsert(in)
	}
}

func ensureClaudeTail(s *Store, transcriptPath, sessionID, cwd string) {
	if transcriptPath == "" {
		return
	}
	claudeTailMu.Lock()
	if _, ok := claudeTails[sessionID]; ok {
		claudeTailMu.Unlock()
		return
	}
	closer := TailFile(transcriptPath, func(line string) {
		var j map[string]any
		if err := json.Unmarshal([]byte(line), &j); err != nil {
			return
		}
		kind, _ := j["type"].(string)
		if kind == "" {
			kind = "event"
		}
		in := UpsertInput{
			Tool: ToolClaude, SessionID: sessionID,
			Cwd: cwd, HasCwd: true,
			State: StateRunning, HasState: true,
		}
		// Sidecar metadata records: title, permission mode. These live on their
		// own JSONL lines and don't change session state, so handle and return.
		switch kind {
		case "ai-title":
			if t, _ := j["aiTitle"].(string); t != "" {
				in.Title, in.HasTitle = t, true
				in.EventKind, in.EventText = kind, t
				s.Upsert(in)
			}
			return
		case "permission-mode":
			if m, _ := j["permissionMode"].(string); m != "" {
				in.Mode, in.HasMode = m, true
				in.EventKind, in.EventText = kind, m
				s.Upsert(in)
			}
			return
		case "last-prompt", "file-history-snapshot", "attachment":
			// Noisy bookkeeping records — record as event but don't bump counters.
			in.EventKind, in.EventText = kind, ""
			s.Upsert(in)
			return
		}

		text, isToolResult, isUserText, isAssistantText := extractClaudeContent(j)
		if model, _ := getNested(j, "message", "model").(string); model != "" {
			in.Model, in.HasModel = model, true
		}
		if isUserText || isAssistantText {
			in.IncrementMsg = true
		}
		// Capture the first real user prompt as title fallback.
		if isUserText && !isToolResult {
			in.FirstMessage, in.HasFirst = clipString(text, 240), true
		}
		if text != "" {
			in.Message, in.HasMessage = text, true
		}
		in.EventKind, in.EventText = kind, text
		s.Upsert(in)
	})
	claudeTails[sessionID] = closer
	claudeTailMu.Unlock()
}

// extractClaudeContent inspects a JSONL turn and returns:
//   - the user-facing text (first text part / tool name / tool result marker)
//   - isToolResult: turn is a tool_result (don't count as a user message)
//   - isUserText: turn has user role and at least one text part (real prompt)
//   - isAssistantText: turn has assistant role with text or tool_use
//
// The flags drive message counting and title fallback in the caller.
func extractClaudeContent(j map[string]any) (text string, isToolResult, isUserText, isAssistantText bool) {
	msg, ok := j["message"].(map[string]any)
	if !ok {
		return "", false, false, false
	}
	role, _ := msg["role"].(string)
	c := msg["content"]
	switch v := c.(type) {
	case string:
		text = v
		if role == "user" {
			isUserText = true
		}
		if role == "assistant" {
			isAssistantText = true
		}
	case []any:
		hasText := false
		for _, p := range v {
			pm, ok := p.(map[string]any)
			if !ok {
				continue
			}
			t, _ := pm["type"].(string)
			switch t {
			case "text":
				if s, _ := pm["text"].(string); s != "" && text == "" {
					text = s
				}
				hasText = true
			case "tool_use":
				if n, _ := pm["name"].(string); n != "" && text == "" {
					text = "→ " + n
				}
			case "tool_result":
				isToolResult = true
				if text == "" {
					text = "← tool_result"
				}
			}
		}
		if role == "user" && hasText && !isToolResult {
			isUserText = true
		}
		if role == "assistant" {
			isAssistantText = true
		}
	}
	return
}

// seedClaudeFromTranscript scans an existing JSONL once at startup and
// populates the Session with title, first prompt, model, mode, token totals,
// tool histogram, files touched, and bg-task count. Heavy on big transcripts
// (multi-MB) but only runs once per session at startup.
//
// fileModMs is the transcript's mtime; we anchor lastActivity there so the
// session doesn't appear "just now" simply because we got around to seeding it.
func seedClaudeFromTranscript(s *Store, path, sessionID, cwd string, state State, fileModMs int64) {
	agg, err := scanClaudeJSONL(path, nil, 0)
	if err != nil {
		return
	}
	in := UpsertInput{
		Tool: ToolClaude, SessionID: sessionID,
		Cwd: cwd, HasCwd: true,
		State: state, HasState: true,
		EventKind: "seed", EventText: "seeded from " + filepathBase(path),
		TranscriptPath: path, HasTranscript: true,
		LastActivityAtOverride: fileModMs,
		// Anchor startedAt to the transcript's first record, not the seed time —
		// thread linking (/clear within 30 min) depends on real start times.
		StartedAtOverride: agg.FirstActivityMs,
	}
	if agg.Title != "" {
		in.Title, in.HasTitle = agg.Title, true
	}
	if agg.FirstMessage != "" {
		in.FirstMessage, in.HasFirst = agg.FirstMessage, true
	}
	if agg.Model != "" {
		in.Model, in.HasModel = agg.Model, true
	}
	if agg.Mode != "" {
		in.Mode, in.HasMode = agg.Mode, true
	}
	if agg.LastMessage != "" {
		in.Message, in.HasMessage = agg.LastMessage, true
	}
	tokens := agg.Tokens
	in.Tokens = &tokens
	in.ToolUsageDelta = agg.ToolUsage
	in.SubagentDelta = agg.Subagents
	in.FilesTouchedSet = len(agg.Files)
	in.HasFiles = true
	in.BgTasksSet = agg.BgTasks
	in.HasBgTasks = true
	in.MessageCountSet, in.HasMessageCount = agg.MessageCount, true
	setSpawns("claude:"+sessionID, agg.Spawns)
	setStartedWithClear("claude:"+sessionID, agg.StartedWithClear)
	s.Upsert(in)
}

func filepathBase(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[i+1:]
	}
	return p
}

func getNested(m map[string]any, keys ...string) any {
	cur := any(m)
	for _, k := range keys {
		mm, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = mm[k]
	}
	return cur
}

// startClaudeAdapter seeds sessions from recently-modified JSONL files so the
// dashboard shows long-running CC processes that started before the hook was
// installed.
func startClaudeAdapter(s *Store) {
	root := filepath.Join(homeDir(), ".claude", "projects")
	if !pathExists(root) {
		return
	}
	cutoff := time.Now().Add(-365 * 24 * time.Hour).UnixMilli()
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		projectPath := filepath.Join(root, e.Name())
		cwd := decodeClaudeProjectDir(e.Name())
		files, err := os.ReadDir(projectPath)
		if err != nil {
			continue
		}
		for _, f := range files {
			name := f.Name()
			if !strings.HasSuffix(name, ".jsonl") {
				continue
			}
			fp := filepath.Join(projectPath, name)
			st, err := os.Stat(fp)
			if err != nil || st.ModTime().UnixMilli() < cutoff {
				continue
			}
			sessionID := strings.TrimSuffix(name, ".jsonl")
			ageMs := time.Since(st.ModTime()).Milliseconds()
			state := ClassifyAge(ageMs)
			seedClaudeFromTranscript(s, fp, sessionID, cwd, state, st.ModTime().UnixMilli())
			ensureClaudeTail(s, fp, sessionID, cwd)
		}
	}
}

func decodeClaudeProjectDir(dir string) string {
	return decodePathFromDashEncoded(dir, true)
}
