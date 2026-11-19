package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"sync"
	"time"
)

// hostOrigins: host id -> who asked agent-monitor to start that terminal, until the host links to its session.
var hostOrigins sync.Map

// requestOrigin names the client that sent r: a paired device by its name, else this Mac.
func requestOrigin(r *http.Request) string {
	if d := requestDevice(r); d != nil {
		return d.Name
	}
	return "this Mac"
}

// PlaceStep is one stretch of a session in one place, from its first transcript record there.
type PlaceStep struct {
	Entry string `json:"entry"`
	Label string `json:"label"`
	At    int64  `json:"at"`
}

// SessionPlace is where a session runs now, who started it, and every place it has run in.
type SessionPlace struct {
	Kind    string      `json:"kind"` // desktop | tmux-host | tmux | terminal | thread | ended
	Label   string      `json:"label"`
	Detail  string      `json:"detail,omitempty"`
	Origin  string      `json:"origin,omitempty"`
	Journey []PlaceStep `json:"journey"`
}

func entryLabel(e string) string {
	switch e {
	case "claude-desktop":
		return "Claude desktop app"
	case "claude-vscode":
		return "VS Code: Claude extension"
	case "cli":
		return "Terminal"
	case "sdk-cli", "sdk-ts", "sdk-py":
		return "agent-monitor thread or SDK"
	case "":
		return "Unknown"
	}
	return e
}

// placeNow decides where a session runs now from its host, tmux pane, driven thread and last entrypoint.
func placeNow(id string, s *Session) (kind, label, detail string) {
	if h := hostForSession(id); h != nil {
		if h.TmuxPane != "" {
			return "tmux-host", "Terminal (tmux, agent-monitor host)", "tmux pane " + h.TmuxPane + " · full control"
		}
		return "tmux-host", "Terminal (agent-monitor host)", "host " + h.HostID[:8] + " · full control"
	}
	if paneRegistry != nil {
		if reg := paneRegistry.Get(id); reg != nil && reg.PaneID != "" {
			return "tmux", "Terminal (tmux)", "tmux pane " + reg.PaneID + " · full control"
		}
	}
	live := s != nil && sessionLive(s, id)
	if orchMgr != nil && s != nil && orchMgr.DrivenID(s.SessionID) != "" && (live || s.Entrypoint == "sdk-cli") {
		return "thread", "agent-monitor thread", "driven by agent-monitor · full control"
	}
	if s == nil || !live {
		last := ""
		if s != nil {
			last = entryLabel(s.Entrypoint)
		}
		return "ended", "Ended", "last in " + last
	}
	switch s.Entrypoint {
	case "claude-desktop":
		return "desktop", "Claude desktop app", "view and approvals here; type in the desktop app"
	case "claude-vscode":
		return "desktop", "VS Code: Claude extension", "view and approvals here; type in VS Code"
	case "sdk-cli", "sdk-ts", "sdk-py":
		return "thread", "SDK run", "a program drives it; view only"
	}
	return "terminal", "Terminal (not tmux)", "view and approvals here; type in that terminal"
}

var journeys = struct {
	sync.Mutex
	m map[string]*journeyScan
}{m: map[string]*journeyScan{}}

type journeyScan struct {
	off   int64
	steps []PlaceStep
}

// journeyOf reads a transcript's entrypoint changes; it reads only what was added since the last call.
func journeyOf(path string) []PlaceStep {
	journeys.Lock()
	defer journeys.Unlock()
	js := journeys.m[path]
	if js == nil {
		js = &journeyScan{}
		journeys.m[path] = js
	}
	f, err := os.Open(path)
	if err != nil {
		return js.steps
	}
	defer f.Close()
	if st, err := f.Stat(); err == nil && st.Size() < js.off {
		js.off, js.steps = 0, nil // the file was replaced
	}
	if _, err := f.Seek(js.off, io.SeekStart); err != nil {
		return js.steps
	}
	r := bufio.NewReaderSize(f, 1<<20)
	key := []byte(`"entrypoint":"`)
	for {
		line, err := r.ReadBytes('\n')
		if err != nil { // an unfinished last line is read again next time
			break
		}
		js.off += int64(len(line))
		i := bytes.Index(line, key)
		if i < 0 {
			continue
		}
		rest := line[i+len(key):]
		e := string(rest[:max(0, bytes.IndexByte(rest, '"'))])
		if n := len(js.steps); n > 0 && js.steps[n-1].Entry == e {
			continue
		}
		var rec struct {
			Timestamp string `json:"timestamp"`
		}
		_ = json.Unmarshal(line, &rec)
		at := int64(0)
		if t, err := time.Parse(time.RFC3339Nano, rec.Timestamp); err == nil {
			at = t.UnixMilli()
		}
		js.steps = append(js.steps, PlaceStep{Entry: e, Label: entryLabel(e), At: at})
	}
	return append([]PlaceStep(nil), js.steps...)
}

func sessionPlace(id string) SessionPlace {
	var s *Session
	if store != nil {
		s = store.Get(id)
	}
	p := SessionPlace{Journey: []PlaceStep{}}
	p.Kind, p.Label, p.Detail = placeNow(id, s)
	if s != nil {
		p.Origin = s.Origin
		if s.TranscriptPath != "" {
			if j := journeyOf(s.TranscriptPath); j != nil {
				p.Journey = j // a transcript with no entrypoint (Cursor) has none; the list stays empty, never null
			}
		}
	}
	// After a restart an ended session has no entrypoint yet; its transcript still knows the last place.
	if n := len(p.Journey); n > 0 && p.Kind == "ended" && p.Detail == "last in Unknown" {
		p.Detail = "last in " + p.Journey[n-1].Label
	}
	return p
}
