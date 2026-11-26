package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// The user's ! commands per transcript, read incrementally, for /api/session/{id}/commands and /api/commands/recent.

const (
	cmdOutputMax    = 2048
	cmdPerFileMax   = 500
	cmdCacheMax     = 64
	cmdRecentAge    = 14 * 24 * time.Hour
	cmdRecentFiles  = 40
	cmdSessionLimit = 200
	cmdRecentLimit  = 50
)

// CmdItem is one ! command: newest first in every answer.
type CmdItem struct {
	Cmd       string `json:"cmd"`
	At        int64  `json:"at"`
	ExitCode  *int   `json:"exitCode,omitempty"`
	Output    string `json:"output"`
	Bg        bool   `json:"bg,omitempty"`
	SessionID string `json:"sessionId,omitempty"`
	Cwd       string `json:"cwd,omitempty"`
}

type cmdFile struct {
	fi      os.FileInfo
	off     int64
	items   []*CmdItem // oldest first, at most cmdPerFileMax
	pending *CmdItem   // claude: a ! command whose output may come on a later line
	bg      map[string]*CmdItem
	lastUse time.Time
}

var cmdCache = struct {
	mu sync.Mutex
	m  map[string]*cmdFile
}{m: map[string]*cmdFile{}}

var (
	bBashInput = []byte("bash-input")
	bBashOut   = []byte("bash-std")
)

// transcriptCmds returns the ! commands of one transcript, oldest first, reading only new bytes.
func transcriptCmds(path string, tool Tool) []CmdItem {
	cmdCache.mu.Lock()
	cf := cmdCache.m[path]
	if cf == nil {
		cf = &cmdFile{bg: map[string]*CmdItem{}}
		cmdCache.m[path] = cf
		if len(cmdCache.m) > cmdCacheMax {
			var oldK string
			var old time.Time
			for k, v := range cmdCache.m {
				if v != cf && (oldK == "" || v.lastUse.Before(old)) {
					oldK, old = k, v.lastUse
				}
			}
			delete(cmdCache.m, oldK)
		}
	}
	cf.lastUse = time.Now()
	cmdCache.mu.Unlock()

	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil
	}
	cmdCache.mu.Lock()
	defer cmdCache.mu.Unlock()
	if cf.fi == nil || !os.SameFile(cf.fi, st) || st.Size() < cf.off {
		*cf = cmdFile{bg: map[string]*CmdItem{}, lastUse: cf.lastUse}
	}
	cf.fi = st
	if st.Size() > cf.off {
		off, err := forEachLine(f, cf.off, st.Size(), func(_ int64, line []byte) {
			if tool == ToolCodex {
				cf.codexLine(line)
			} else {
				cf.claudeLine(line)
			}
		})
		if err == nil {
			cf.off = off
		}
	}
	out := make([]CmdItem, len(cf.items))
	for i, it := range cf.items {
		out[i] = *it
	}
	return out
}

func (cf *cmdFile) push(it *CmdItem) {
	cf.items = append(cf.items, it)
	if len(cf.items) > cmdPerFileMax {
		cf.items = append([]*CmdItem(nil), cf.items[len(cf.items)-cmdPerFileMax:]...)
	}
}

func (cf *cmdFile) claudeLine(line []byte) {
	isBang := bytes.Contains(line, bBashInput) || bytes.Contains(line, bBashOut)
	isNote := bytes.Contains(line, bTaskNote)
	if (!isBang && !isNote) || !bytes.Contains(line, bClaudeUser) || bytes.Contains(line, bIsSidechain) {
		return
	}
	var j struct {
		Type      string `json:"type"`
		Timestamp string `json:"timestamp"`
		Message   struct {
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(line, &j) != nil || j.Type != "user" {
		return
	}
	text, res := claudeUserText(j.Message.Content)
	if res {
		return
	}
	ts := parseTimestamp(j.Timestamp)
	if t := strings.TrimSpace(text); strings.HasPrefix(t, "<task-notification>") {
		it := cf.bg[strings.TrimSpace(extractTagValue(t, "task-id"))]
		if it == nil {
			return
		}
		if m := reBgExit.FindStringSubmatch(extractTagValue(t, "summary")); m != nil {
			code, _ := strconv.Atoi(m[1])
			it.ExitCode = &code
		}
		delete(cf.bg, strings.TrimSpace(extractTagValue(t, "task-id")))
		return
	}
	cmd, in, out, hasOut := bangParts(text)
	if in {
		cf.pending = &CmdItem{Cmd: cmd, At: ts}
		cf.push(cf.pending)
	}
	if it := cf.pending; it != nil && hasOut {
		cf.pending = nil
		if m := reBgMoved.FindStringSubmatch(out); m != nil {
			it.Bg = true
			if len(cf.bg) > 100 {
				cf.bg = map[string]*CmdItem{}
			}
			cf.bg[m[1]] = it
		}
		it.Output = cmdOutput(out)
	}
}

func (cf *cmdFile) codexLine(line []byte) {
	cmd, item, ok := codexUserShellLine(line)
	if !ok || item == nil {
		return
	}
	var j struct {
		Timestamp string `json:"timestamp"`
	}
	_ = json.Unmarshal(line, &j)
	it := &CmdItem{Cmd: cmd, At: parseTimestamp(j.Timestamp), Output: cmdOutput(item.AggregatedOutput)}
	if item.ExitCode != nil {
		code := int(*item.ExitCode)
		it.ExitCode = &code
	}
	cf.push(it)
}

// cmdOutput strips ANSI escapes (bangParts already unescaped HTML) and keeps the first 2 KB.
func cmdOutput(s string) string {
	return clipHead(strings.TrimRight(reANSI.ReplaceAllString(s, ""), " \n\r\t"), cmdOutputMax)
}

// clipHead keeps the first n bytes of s on a rune boundary.
func clipHead(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

func cmdLimit(r *http.Request, def int) int {
	n, err := strconv.Atoi(r.URL.Query().Get("limit"))
	if err != nil || n <= 0 {
		return def
	}
	return min(n, cmdPerFileMax)
}

func newestFirst(items []CmdItem) {
	sort.SliceStable(items, func(i, j int) bool { return items[i].At > items[j].At })
}

func serveSessionCommands(w http.ResponseWriter, r *http.Request, id string) {
	ref := resolveWatched(id)
	if !ref.Resolved || ref.Path == "" {
		writeJSONStatus(w, http.StatusNotFound, map[string]any{"error": "no transcript for session"})
		return
	}
	if ref.Tool != ToolClaude && ref.Tool != ToolCodex {
		writeJSONStatus(w, http.StatusNotImplemented, map[string]any{"error": "! commands are only read for claude and codex"})
		return
	}
	items := transcriptCmds(ref.Path, ref.Tool)
	newestFirst(items)
	if n := cmdLimit(r, cmdSessionLimit); len(items) > n {
		items = items[:n]
	}
	if items == nil {
		items = []CmdItem{}
	}
	writeJSON(w, map[string]any{"items": items})
}

// recentCmds merges the ! commands of the most recently active claude and codex sessions, newest per command text.
func recentCmds(limit int) []CmdItem {
	out := []CmdItem{}
	if store == nil {
		return out
	}
	cut := time.Now().Add(-cmdRecentAge).UnixMilli()
	var sess []*Session
	for _, s := range store.All() {
		if (s.Tool == ToolClaude || s.Tool == ToolCodex) && s.TranscriptPath != "" && s.LastActivityAt >= cut {
			sess = append(sess, s)
		}
	}
	sort.Slice(sess, func(i, j int) bool { return sess[i].LastActivityAt > sess[j].LastActivityAt })
	if len(sess) > cmdRecentFiles {
		sess = sess[:cmdRecentFiles]
	}
	var all []CmdItem
	for _, s := range sess {
		for _, it := range transcriptCmds(s.TranscriptPath, s.Tool) {
			it.SessionID, it.Cwd = s.ID, s.Cwd
			all = append(all, it)
		}
	}
	newestFirst(all)
	seen := map[string]bool{}
	for _, it := range all {
		k := strings.TrimSpace(it.Cmd)
		if k == "" || seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, it)
		if len(out) >= limit {
			break
		}
	}
	return out
}

func registerCommandRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/commands/recent", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"items": recentCmds(cmdLimit(r, cmdRecentLimit))})
	})
}
