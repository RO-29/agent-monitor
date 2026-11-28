package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// BgRun is a background command or agent that started and has not sent its finish notice yet.
type BgRun struct {
	ID        string `json:"id"`
	Label     string `json:"label"`
	Tool      string `json:"tool"`
	StartedAt int64  `json:"startedAt"`
}

type bgState struct {
	off       int64
	open      map[string]BgRun
	skipFirst bool // a tail scan starts mid-line
}

var bgScan = struct {
	sync.Mutex
	m map[string]*bgState
}{m: map[string]*bgState{}}

const (
	bgTailBytes = 8 << 20 // a first scan reads at most the last 8 MB
	bgMaxAge    = 6 * time.Hour
)

var reToolUseID = regexp.MustCompile(`<tool-use-id>([^<]+)</tool-use-id>`)

// runningBg reads only the bytes added since the last call, so a poll costs one stat.
func runningBg(path string) []BgRun {
	if path == "" {
		return nil
	}
	bgScan.Lock()
	defer bgScan.Unlock()
	st := bgScan.m[path]
	fi, err := os.Stat(path)
	if err != nil {
		delete(bgScan.m, path)
		return nil
	}
	if st == nil || fi.Size() < st.off {
		off := max(0, fi.Size()-bgTailBytes)
		st = &bgState{off: off, open: map[string]BgRun{}, skipFirst: off > 0}
		bgScan.m[path] = st
	}
	if fi.Size() > st.off {
		bgRead(path, st)
	}
	cut := time.Now().Add(-bgMaxAge).UnixMilli()
	out := make([]BgRun, 0, len(st.open))
	for id, b := range st.open {
		if b.StartedAt < cut {
			delete(st.open, id)
			continue
		}
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt < out[j].StartedAt })
	return out
}

func bgRead(path string, st *bgState) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	if _, err := f.Seek(st.off, io.SeekStart); err != nil {
		return
	}
	r := bufio.NewReaderSize(f, 256<<10)
	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			return // a partial last line waits for the next call
		}
		st.off += int64(len(line))
		if st.skipFirst {
			st.skipFirst = false
			continue
		}
		bgLine(line, st)
	}
}

func bgLine(line []byte, st *bgState) {
	start := bytes.Contains(line, []byte(`"run_in_background":true`))
	done := bytes.Contains(line, []byte(`<task-notification>`))
	failed := bytes.Contains(line, []byte(`"is_error":true`))
	if !start && !done && !failed {
		return
	}
	var j struct {
		Type      string `json:"type"`
		Timestamp string `json:"timestamp"`
		Content   string `json:"content"`
		Message   struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(line, &j) != nil {
		return
	}
	if done {
		txt := j.Content
		if txt == "" {
			_ = json.Unmarshal(j.Message.Content, &txt)
		}
		if strings.HasPrefix(strings.TrimSpace(txt), "<task-notification>") {
			for _, m := range reToolUseID.FindAllStringSubmatch(txt, -1) {
				delete(st.open, m[1])
			}
			return
		}
	}
	var blocks []struct {
		Type      string         `json:"type"`
		ID        string         `json:"id"`
		Name      string         `json:"name"`
		Input     map[string]any `json:"input"`
		ToolUseID string         `json:"tool_use_id"`
		IsError   bool           `json:"is_error"`
	}
	if json.Unmarshal(j.Message.Content, &blocks) != nil {
		return
	}
	if j.Message.Role == "user" { // a refused or failed start never sends a finish notice
		for _, b := range blocks {
			if b.Type == "tool_result" && b.IsError {
				delete(st.open, b.ToolUseID)
			}
		}
		return
	}
	if !start || j.Message.Role != "assistant" {
		return
	}
	ts := time.Now().UnixMilli()
	if t, err := time.Parse(time.RFC3339Nano, j.Timestamp); err == nil {
		ts = t.UnixMilli()
	}
	for _, b := range blocks {
		if b.Type != "tool_use" || b.Input["run_in_background"] != true || b.ID == "" {
			continue
		}
		label, _ := b.Input["description"].(string)
		if label == "" {
			c, _ := b.Input["command"].(string)
			label = firstNonEmptyLine(c)
		}
		st.open[b.ID] = BgRun{ID: b.ID, Label: label, Tool: b.Name, StartedAt: ts}
	}
}
