package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// PromptItem is one prompt you typed, read from the agents' own history files.
type PromptItem struct {
	Text      string `json:"text"`
	At        int64  `json:"at"`
	Agent     string `json:"agent"`
	SessionID string `json:"sessionId,omitempty"` // "claude:<uuid>" or "codex:<uuid>"
	Cwd       string `json:"cwd,omitempty"`
	Bash      bool   `json:"bash,omitempty"`
}

const (
	promptTailBytes = 1 << 20 // only the end of each history file is read
	promptMax       = 3000
)

type promptFile struct {
	size  int64
	mod   time.Time
	items []PromptItem
}

var promptCache = struct {
	mu    sync.Mutex
	files map[string]*promptFile
}{files: map[string]*promptFile{}}

func promptFiles() map[string]string {
	return map[string]string{
		"claude": filepath.Join(claudeConfigDir(), "history.jsonl"),
		"codex":  filepath.Join(codexHome(), "history.jsonl"),
	}
}

// readPromptFile parses the tail of one history file; the cache holds until its size or mtime changes.
func readPromptFile(agent, path string) []PromptItem {
	st, err := os.Stat(path)
	if err != nil {
		return nil
	}
	promptCache.mu.Lock()
	defer promptCache.mu.Unlock()
	if c := promptCache.files[path]; c != nil && c.size == st.Size() && c.mod.Equal(st.ModTime()) {
		return c.items
	}
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	off := max(st.Size()-promptTailBytes, 0)
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return nil
	}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	first := off > 0
	var out []PromptItem
	for sc.Scan() {
		line := sc.Bytes()
		if first { // a partial line at the cut
			first = false
			continue
		}
		if it, ok := parseHistoryLine(agent, line); ok {
			out = append(out, it)
		}
	}
	if len(out) > promptMax {
		out = out[len(out)-promptMax:]
	}
	promptCache.files[path] = &promptFile{size: st.Size(), mod: st.ModTime(), items: out}
	return out
}

func parseHistoryLine(agent string, line []byte) (PromptItem, bool) {
	line = bytes.TrimSpace(line)
	if len(line) == 0 {
		return PromptItem{}, false
	}
	var r struct {
		Display   string `json:"display"`
		Timestamp int64  `json:"timestamp"`
		Project   string `json:"project"`
		Session   string `json:"sessionId"`
		CodexSess string `json:"session_id"`
		Ts        int64  `json:"ts"`
		Text      string `json:"text"`
	}
	if json.Unmarshal(line, &r) != nil {
		return PromptItem{}, false
	}
	it := PromptItem{Agent: agent}
	if agent == "codex" {
		it.Text, it.At = r.Text, r.Ts*1000
		if r.CodexSess != "" {
			it.SessionID = "codex:" + r.CodexSess
		}
	} else {
		it.Text, it.At, it.Cwd = r.Display, r.Timestamp, r.Project
		if r.Session != "" {
			it.SessionID = "claude:" + r.Session
		}
	}
	// The note agent-monitor adds for attachments is not part of what you typed.
	if i := strings.Index(it.Text, "Attached files (open them with your file tools):"); i >= 0 {
		it.Text = it.Text[:i]
	}
	it.Text = strings.TrimSpace(it.Text)
	if it.Text == "" || it.Text == "exit" || it.Text == "/exit" {
		return PromptItem{}, false
	}
	it.Bash = strings.HasPrefix(it.Text, "!")
	return it, true
}

// prompts merges both files newest first, drops repeats of the same text, and applies the filters.
func prompts(session, query string, bash string, limit int) []PromptItem {
	var all []PromptItem
	for agent, p := range promptFiles() {
		all = append(all, readPromptFile(agent, p)...)
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].At > all[j].At })
	q := strings.ToLower(strings.TrimSpace(query))
	seen := map[string]bool{}
	out := make([]PromptItem, 0, min(limit, 64))
	for _, it := range all {
		if session != "" && it.SessionID != session {
			continue
		}
		if (bash == "only" && !it.Bash) || (bash == "no" && it.Bash) {
			continue
		}
		if q != "" && !strings.Contains(strings.ToLower(it.Text), q) && !strings.Contains(strings.ToLower(it.Cwd), q) {
			continue
		}
		if seen[it.Text] {
			continue
		}
		seen[it.Text] = true
		out = append(out, it)
		if len(out) >= limit {
			break
		}
	}
	return out
}

// GET /api/prompts?session=claude:<id>&q=&bash=only|no&limit= lists prompts you typed, newest first.
func registerPromptRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/prompts", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		limit := cmdLimit(r, 100)
		writeJSON(w, map[string]any{"items": prompts(resolvePromptSession(q.Get("session")), q.Get("q"), q.Get("bash"), limit)})
	})
}

// resolvePromptSession turns a thread or session id into the "agent:uuid" key the history files use.
func resolvePromptSession(id string) string {
	if id == "" || strings.HasPrefix(id, "claude:") || strings.HasPrefix(id, "codex:") {
		return id
	}
	if ref := resolveWatched(id); ref.Resolved {
		return ref.ID
	}
	return id
}
