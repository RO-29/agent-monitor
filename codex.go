package main

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// No time cap on codex scan — the user wants full history. We track which
// rollout files we've already parsed in codexTailing so the periodic re-scan
// only opens new files. Scanning all 74-ish rollouts at startup costs ~few
// hundred ms for typical sizes; subsequent ticks are nearly free.
const codexScanAge = 365 * 24 * time.Hour

var (
	codexMu          sync.Mutex
	codexTailing     = map[string]func(){}                         // file path -> closer
	codexSessionMeta = map[string]struct{ SessionID, Cwd string }{} // file path -> meta
	codexApprovalRe  = regexp.MustCompile(`(?i)approval|approve|permission`)
)

func startCodexAdapter(s *Store) {
	root := filepath.Join(homeDir(), ".codex", "sessions")
	if !pathExists(root) {
		return
	}
	scan := func() {
		cutoff := time.Now().Add(-codexScanAge)
		// Walk every dated subdir under sessions/. Codex organises rollouts
		// as YYYY/MM/DD/rollout-*.jsonl. codexStartTail dedupes by path so
		// re-walking the tree is cheap once everything is registered.
		_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil || info == nil || info.IsDir() {
				return nil
			}
			name := info.Name()
			if !strings.HasPrefix(name, "rollout-") || !strings.HasSuffix(name, ".jsonl") {
				return nil
			}
			if info.ModTime().Before(cutoff) {
				return nil
			}
			codexStartTail(s, path)
			return nil
		})
	}
	scan()
	go func() {
		t := time.NewTicker(15 * time.Second)
		defer t.Stop()
		for range t.C {
			scan()
		}
	}()
}

func codexStartTail(s *Store, fp string) {
	codexMu.Lock()
	if _, ok := codexTailing[fp]; ok {
		codexMu.Unlock()
		return
	}
	// Mark before the IO so concurrent ticks can't double-tail.
	codexTailing[fp] = nil
	codexMu.Unlock()

	codexReadSessionMeta(s, fp)
	closer := TailFile(fp, func(line string) { codexHandleLine(s, fp, line) })

	codexMu.Lock()
	codexTailing[fp] = closer
	codexMu.Unlock()
}

func codexReadSessionMeta(s *Store, fp string) {
	// Rollout files have session_meta as the first line. Read just that line so
	// we know sessionId + cwd before tailing for new events.
	f, err := os.Open(fp)
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	if sc.Scan() {
		codexHandleLine(s, fp, sc.Text())
	}
}

func codexHandleLine(s *Store, fp, line string) {
	var j map[string]any
	if err := json.Unmarshal([]byte(line), &j); err != nil {
		return
	}

	if j["type"] == "session_meta" {
		payload, _ := j["payload"].(map[string]any)
		if payload == nil {
			return
		}
		sessionID, _ := payload["id"].(string)
		cwd, _ := payload["cwd"].(string)
		if sessionID == "" {
			return
		}
		codexMu.Lock()
		codexSessionMeta[fp] = struct{ SessionID, Cwd string }{sessionID, cwd}
		codexMu.Unlock()
		// Classify state by the rollout file's mtime — otherwise every old
		// rollout we tail at startup gets seeded as "running" (since the
		// session_meta line triggers an upsert and decay only kicks in after
		// 5 minutes). Without this, three-month-old rollouts appear live.
		state := StateCompleted
		var startedAt int64
		if st, err := os.Stat(fp); err == nil {
			ageMs := time.Since(st.ModTime()).Milliseconds()
			state = ClassifyAge(ageMs)
			startedAt = st.ModTime().UnixMilli()
		}
		in := UpsertInput{
			Tool: ToolCodex, SessionID: sessionID,
			Cwd: cwd, HasCwd: true,
			State: state, HasState: true,
			EventKind: "session_meta", EventText: "started in " + cwd,
			TranscriptPath: fp, HasTranscript: true,
		}
		if startedAt > 0 {
			in.StartedAtOverride = startedAt
			in.LastActivityAtOverride = startedAt
		}
		// Pull aggregates (token totals, tool histogram, files, first/last
		// message, title-fallback) from the rollout the same way we do for
		// Claude transcripts. Cheap because scanCodexJSONL is single-pass.
		if agg, err := scanCodexJSONL(fp, nil, 0); err == nil {
			t := agg.Tokens
			in.Tokens = &t
			in.ToolUsageDelta = agg.ToolUsage
			if agg.FirstMessage != "" {
				in.FirstMessage, in.HasFirst = agg.FirstMessage, true
			}
			if agg.LastMessage != "" {
				in.Message, in.HasMessage = agg.LastMessage, true
			}
			if agg.Mode != "" {
				in.Mode, in.HasMode = agg.Mode, true
			}
			if agg.Model != "" {
				in.Model, in.HasModel = agg.Model, true
			}
			in.FilesTouchedSet, in.HasFiles = len(agg.Files), true
			in.MessageCountSet, in.HasMessageCount = agg.MessageCount, true
		}
		s.Upsert(in)
		return
	}

	codexMu.Lock()
	meta, ok := codexSessionMeta[fp]
	codexMu.Unlock()
	if !ok {
		return
	}

	base := UpsertInput{
		Tool: ToolCodex, SessionID: meta.SessionID,
		Cwd: meta.Cwd, HasCwd: true,
	}

	payload, _ := j["payload"].(map[string]any)
	innerType, _ := j["type"].(string)
	if payload != nil {
		if t, ok := payload["type"].(string); ok && t != "" {
			innerType = t
		}
	}

	// turn_context carries the model + approval policy — codex doesn't emit it
	// as an event_msg, so capture it here so the session shows which model it's
	// on (like Claude sessions do).
	if j["type"] == "turn_context" && payload != nil {
		in := base
		changed := false
		if m, ok := payload["model"].(string); ok && m != "" {
			in.Model, in.HasModel = m, true
			changed = true
		}
		if ap, ok := payload["approval_policy"].(string); ok && ap != "" {
			in.Mode, in.HasMode = ap, true
			changed = true
		}
		if changed {
			s.Upsert(in)
		}
		return
	}

	if j["type"] == "event_msg" && payload != nil {
		pType, _ := payload["type"].(string)
		// token_count → refresh aggregate tokens. Codex emits this every
		// turn, so live token totals stay current.
		if pType == "token_count" {
			if info, _ := payload["info"].(map[string]any); info != nil {
				if tot, _ := info["total_token_usage"].(map[string]any); tot != nil {
					var t TokenUsage
					if v, ok := tot["input_tokens"].(float64); ok {
						t.Input = int64(v)
					}
					if v, ok := tot["output_tokens"].(float64); ok {
						t.Output = int64(v)
					}
					if v, ok := tot["cached_input_tokens"].(float64); ok {
						t.CacheRead = int64(v)
					}
					in := base
					in.Tokens = &t
					in.EventKind, in.EventText = "token_count", ""
					s.Upsert(in)
					return
				}
			}
		}
		switch pType {
		case "task_started":
			in := base
			in.State, in.HasState = StateRunning, true
			in.EventKind, in.EventText = "task_started", ""
			s.Upsert(in)
			return
		case "task_complete":
			last, _ := payload["last_agent_message"].(string)
			last = clipString(last, 240)
			in := base
			in.State, in.HasState = StateAwaitingInput, true
			in.Message, in.HasMessage = last, true
			in.EventKind, in.EventText = "task_complete", last
			s.Upsert(in)
			return
		case "user_message":
			txt, _ := payload["message"].(string)
			in := base
			in.State, in.HasState = StateRunning, true
			in.EventKind, in.EventText = "user_message", clipString(txt, 200)
			s.Upsert(in)
			return
		}
		if codexApprovalRe.MatchString(pType) {
			perm := ""
			if v, ok := payload["command"].(string); ok {
				perm = v
			} else if v, ok := payload["message"].(string); ok {
				perm = v
			} else {
				perm = pType
			}
			if b, err := json.Marshal(payload); err == nil {
				in := base
				in.State, in.HasState = StateAwaitingPermission, true
				in.PermissionMessage, in.HasPerm = perm, true
				in.EventKind, in.EventText = pType, clipString(string(b), 300)
				s.Upsert(in)
				return
			}
		}
	}

	// Heartbeat: any other line means the session is making progress.
	text := ""
	if payload != nil {
		if v, ok := payload["message"].(string); ok {
			text = clipString(v, 200)
		} else if v, ok := payload["text"].(string); ok {
			text = clipString(v, 200)
		}
	}
	in := base
	in.State, in.HasState = StateRunning, true
	in.EventKind, in.EventText = innerType, text
	s.Upsert(in)
}
