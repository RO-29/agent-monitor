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
	byTask    map[string]string // task id (b…/agent id) to tool-use id; TaskStop writes no finish notice
	skipFirst bool              // a tail scan starts mid-line
}

var bgScan = struct {
	sync.Mutex
	m map[string]*bgState
}{m: map[string]*bgState{}}

const (
	bgTailBytes = 8 << 20 // a first scan reads at most the last 8 MB
	bgMaxAge    = 6 * time.Hour
)

var (
	reToolUseID = regexp.MustCompile(`<tool-use-id>([^<]+)</tool-use-id>`)
	reTaskID    = regexp.MustCompile(`^(?:Command running in background with ID: |Async agent launched[^\n]*\nagentId: )([A-Za-z0-9_-]+)`)
)

func (st *bgState) drop(toolUseID string) {
	delete(st.open, toolUseID)
	for t, u := range st.byTask {
		if u == toolUseID {
			delete(st.byTask, t)
		}
	}
}

// resultText reads a tool_result content that is a string or a list of text blocks.
func resultText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Text string `json:"text"`
	}
	_ = json.Unmarshal(raw, &parts)
	for _, p := range parts {
		s += p.Text
	}
	return s
}

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
		st = &bgState{off: off, open: map[string]BgRun{}, byTask: map[string]string{}, skipFirst: off > 0}
		bgScan.m[path] = st
	}
	if fi.Size() > st.off {
		bgRead(path, st)
	}
	cut := time.Now().Add(-bgMaxAge).UnixMilli()
	out := make([]BgRun, 0, len(st.open))
	for id, b := range st.open {
		if b.StartedAt < cut {
			st.drop(id)
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
	launched := bytes.Contains(line, []byte(`running in background with ID: `)) || bytes.Contains(line, []byte(`agentId: `))
	stopped := bytes.Contains(line, []byte(`Successfully stopped task`))
	if !start && !done && !failed && !launched && !stopped {
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
				st.drop(m[1])
			}
			return
		}
	}
	var blocks []struct {
		Type      string          `json:"type"`
		ID        string          `json:"id"`
		Name      string          `json:"name"`
		Input     map[string]any  `json:"input"`
		ToolUseID string          `json:"tool_use_id"`
		IsError   bool            `json:"is_error"`
		Content   json.RawMessage `json:"content"`
	}
	if json.Unmarshal(j.Message.Content, &blocks) != nil {
		return
	}
	if j.Message.Role == "user" {
		for _, b := range blocks {
			if b.Type != "tool_result" {
				continue
			}
			if b.IsError { // a refused or failed start never sends a finish notice
				st.drop(b.ToolUseID)
				continue
			}
			txt := strings.TrimSpace(resultText(b.Content))
			if _, ok := st.open[b.ToolUseID]; ok {
				if m := reTaskID.FindStringSubmatch(txt); m != nil {
					st.byTask[m[1]] = b.ToolUseID
				}
				continue
			}
			var stop struct {
				Message string `json:"message"`
				TaskID  string `json:"task_id"`
			}
			if strings.HasPrefix(txt, `{"message":"Successfully stopped task`) && json.Unmarshal([]byte(txt), &stop) == nil {
				if u, ok := st.byTask[stop.TaskID]; ok {
					st.drop(u)
				}
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
