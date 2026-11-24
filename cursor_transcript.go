package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
)

// Cursor agent transcripts: ~/.cursor/projects/<ws>/agent-transcripts/<id>/<id>.jsonl, one message per line.
// Lines carry no time; a prompt's <timestamp> dates its turn, and the file time ends the last one.

type cursorTLine struct {
	Type    string `json:"type"`
	Role    string `json:"role"`
	Message *struct {
		Content []struct {
			Type  string          `json:"type"`
			Text  string          `json:"text"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		} `json:"content"`
	} `json:"message"`
}

var cursorTCache = struct {
	mu sync.Mutex
	m  map[string]cursorParsed
}{m: map[string]cursorParsed{}}

func isCursorTranscript(path string) bool {
	return strings.HasSuffix(path, ".jsonl") && strings.Contains(path, "/agent-transcripts/")
}

// cursorTranscriptItems reads a Cursor agent transcript into items and runs; cached until the file changes.
func cursorTranscriptItems(threadID, path string) ([]*OItem, []*ORun, error) {
	st, err := os.Stat(path)
	if err != nil {
		return nil, nil, err
	}
	stamp := fmt.Sprintf("%d/%d", st.Size(), st.ModTime().UnixNano())
	cursorTCache.mu.Lock()
	if c, ok := cursorTCache.m[path]; ok && c.stamp == stamp {
		cursorTCache.mu.Unlock()
		return c.items, c.runs, nil
	}
	cursorTCache.mu.Unlock()
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	items, runs := cursorTranscriptBuild(threadID, bufio.NewScanner(f), st.ModTime().UnixMilli())
	cursorTCache.mu.Lock()
	if len(cursorTCache.m) > 32 {
		cursorTCache.m = map[string]cursorParsed{}
	}
	cursorTCache.m[path] = cursorParsed{stamp: stamp, items: items, runs: runs}
	cursorTCache.mu.Unlock()
	return items, runs, nil
}

func cursorTranscriptBuild(threadID string, sc *bufio.Scanner, fileAt int64) ([]*OItem, []*ORun) {
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	var items []*OItem
	var runs []*ORun
	var run *ORun
	ts := fileAt
	n := 0
	add := func(it *OItem) {
		n++
		it.ID, it.ThreadID, it.StartedAt = fmt.Sprintf("%s@t%d", threadID, n), threadID, ts
		if it.EndedAt == 0 && it.Status == "completed" {
			it.EndedAt = ts
		}
		if run != nil {
			it.RunID = run.ID
		}
		items = append(items, it)
	}
	end := func(at int64) {
		if run != nil && run.Status == "running" {
			run.Status, run.EndedAt = "completed", max(at, run.StartedAt)
		}
	}
	for sc.Scan() {
		var l cursorTLine
		if json.Unmarshal(sc.Bytes(), &l) != nil {
			continue
		}
		if l.Type == "turn_ended" {
			end(ts)
			continue
		}
		if l.Message == nil {
			continue
		}
		switch l.Role {
		case "user":
			var text string
			for _, p := range l.Message.Content {
				if p.Type == "text" {
					text += p.Text
				}
			}
			q := reCursorQuery.FindStringSubmatch(text)
			if q == nil {
				continue // context Cursor adds, not something the user typed
			}
			if t := reCursorTime.FindStringSubmatch(text); t != nil {
				if at := parseCursorTime(t[1]); at > 0 {
					ts = at
				}
			}
			end(ts)
			run = &ORun{ID: fmt.Sprintf("%s@r%d", threadID, len(runs)), ThreadID: threadID, Index: len(runs), Prompt: q[1], Status: "running", StartedAt: ts}
			runs = append(runs, run)
			add(&OItem{Kind: "user", Status: "completed", Text: q[1]})
		case "assistant":
			for _, p := range l.Message.Content {
				switch p.Type {
				case "text":
					text := p.Text
					if th := reCursorThink.FindStringSubmatch(text); th != nil {
						if t := strings.TrimSpace(th[1]); t != "" {
							add(&OItem{Kind: "reasoning", Status: "completed", Text: t})
						}
						text = text[len(th[0]):]
					}
					if t := strings.TrimSpace(text); t != "" {
						add(&OItem{Kind: "agent", Status: "completed", Text: t})
					}
				case "tool_use":
					it := cursorToolItem(cursorPart{ToolName: p.Name, Args: p.Input})
					it.Status = "completed" // the transcript keeps no results; a later line means the call returned
					if p.Name == "StrReplace" || p.Name == "Delete" {
						it.Kind = "fileChange"
					}
					add(it)
				}
			}
		}
	}
	// The newest turn ends at the file's last write once Cursor marked it ended.
	if run != nil && run.Status == "completed" && run.EndedAt == run.StartedAt && fileAt > run.EndedAt {
		run.EndedAt = fileAt
	}
	return items, runs
}
