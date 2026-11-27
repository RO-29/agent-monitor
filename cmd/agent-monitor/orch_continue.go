package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
)

// orchWatched is the watched session that Continue here takes over.
type orchWatched struct {
	Agent    string
	NativeID string
	Cwd      string
	Title    string
	Model    string
	Live     bool
	// Transcript is the session file; Continue reads the model from it.
	Transcript string
}

// lookupWatched resolves a watched v3 thread or session id to its newest session.
func lookupWatched(id string) (*orchWatched, error) {
	if store == nil {
		return nil, errOrchNotFound
	}
	v := loadThreads()
	t := v.thread(id)
	if t == nil || len(t.Sessions) == 0 {
		return nil, errOrchNotFound
	}
	sid := t.Sessions[len(t.Sessions)-1]
	agent, native, ok := strings.Cut(sid, ":")
	if !ok || (agent != string(ToolClaude) && agent != string(ToolCodex)) {
		return nil, orchBadRequest("only claude and codex sessions can be continued")
	}
	w := &orchWatched{Agent: agent, NativeID: native, Cwd: t.Cwd, Title: t.Title, Model: t.Model}
	if s := v.byID[sid]; s != nil {
		w.Cwd = firstOf(s.Cwd, w.Cwd)
		w.Title = firstOf(t.Title, s.Title, firstLine(s.FirstMessage))
		w.Transcript = s.TranscriptPath
	}
	for _, s := range t.Sessions {
		if sessionLive(v.byID[s], s) {
			w.Live = true
		}
	}
	return w, nil
}

// errContinueInTerminal: a Code Mode session cannot run under -p, so it resumes in a terminal instead.
var errContinueInTerminal = errors.New("code mode session resumes in a terminal")

// transcriptCodeMode is true when Claude recorded the session as started in Code Mode.
func transcriptCodeMode(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	sc := bufio.NewScanner(io.LimitReader(f, 4<<20))
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		if b := sc.Bytes(); bytes.Contains(b, []byte(`"type":"repl-mode"`)) {
			return bytes.Contains(b, []byte(`"replMode":true`))
		}
	}
	return false
}

var transcriptPermPattern = regexp.MustCompile(`"permissionMode":"(acceptEdits|plan|auto|default|bypassPermissions)"`)

// transcriptPermissionMode is the last permission mode Claude recorded, for a resume flag; "" when none applies.
func transcriptPermissionMode(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	const tail = 2 << 20
	if st, err := f.Stat(); err == nil && st.Size() > tail {
		_, _ = f.Seek(st.Size()-tail, io.SeekStart)
	}
	b, _ := io.ReadAll(io.LimitReader(f, tail))
	all := transcriptPermPattern.FindAllSubmatch(b, -1)
	if len(all) == 0 {
		return ""
	}
	switch m := string(all[len(all)-1][1]); m {
	case "acceptEdits", "plan", "auto":
		return m
	}
	return ""
}

// sessionLive is true when a CLI process still owns the session: a live pid, a pane, or a recent active state.
func sessionLive(s *Session, storeID string) bool {
	if s != nil && s.Pid > 0 && pidAlive(s.Pid) {
		return true
	}
	if paneRegistry != nil {
		if reg := paneRegistry.Get(storeID); reg != nil && reg.PaneID != "" && (reg.Pid == 0 || pidAlive(reg.Pid)) {
			return true
		}
	}
	if s == nil {
		return false
	}
	switch s.State {
	case StateRunning, StateAwaitingPermission, StateAwaitingInput:
		return true
	}
	return false
}

type orchContinue struct {
	SessionID      string `json:"sessionId"`
	Prompt         string `json:"prompt,omitempty"`
	Model          string `json:"model,omitempty"`
	PermissionMode string `json:"permissionMode,omitempty"`
}

// Continue returns the driven thread for a watched session, creating it when the session has no live owner.
func (m *OrchManager) Continue(in orchContinue) (*OThread, bool, error) {
	if strings.TrimSpace(in.SessionID) == "" {
		return nil, false, orchBadRequest("sessionId is required")
	}
	w, err := m.watched(in.SessionID)
	if err != nil {
		return nil, false, err
	}
	created := false
	id := m.DrivenID(w.NativeID)
	if id == "" {
		if w.Live {
			return nil, false, orchConflict("live")
		}
		if w.Agent == string(ToolClaude) && transcriptCodeMode(w.Transcript) {
			return nil, false, errContinueInTerminal
		}
		model := firstOf(in.Model, transcriptModel(w.Agent, w.Transcript))
		if model == "" && w.Agent == "codex" {
			model = w.Model
		}
		th, err := m.newThread(orchCreate{Agent: w.Agent, Cwd: w.Cwd, Model: model, PermissionMode: in.PermissionMode}, w.NativeID)
		if err != nil {
			return nil, false, err
		}
		th.Title = firstOf(orchTitle(w.Title), orchTitle(in.Prompt))
		if err := m.insertThread(th); err != nil {
			return nil, false, err
		}
		id, created = th.ID, true
	}
	if strings.TrimSpace(in.Prompt) != "" {
		if _, err := m.Send(id, in.Prompt); err != nil {
			return nil, created, err
		}
	}
	th, err := m.store.Thread(id)
	return th, created, err
}

func (m *OrchManager) continueRoute(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/orch/continue", func(w http.ResponseWriter, r *http.Request) {
		var in orchContinue
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&in); err != nil {
			orchWriteErr(w, orchBadRequest("bad json"))
			return
		}
		if denyNoApprovals(w, r, "", in.PermissionMode, "") {
			return
		}
		th, created, err := m.Continue(in)
		if errors.Is(err, errContinueInTerminal) {
			w2, werr := m.watched(in.SessionID)
			if werr != nil {
				orchWriteErr(w, werr)
				return
			}
			started, terr := startTerminal(terminalStart{Agent: "claude", Cwd: w2.Cwd, Prompt: in.Prompt, Model: in.Model, PermissionMode: in.PermissionMode, Resume: w2.NativeID, CodeMode: true})
			if terr != nil {
				orchWriteErr(w, terr)
				return
			}
			writeJSONStatus(w, http.StatusAccepted, map[string]any{"terminal": started})
			return
		}
		if err != nil {
			if errors.Is(err, errOrchNotFound) {
				writeJSONStatus(w, http.StatusNotFound, map[string]any{"error": "session not found"})
				return
			}
			orchWriteErr(w, err)
			return
		}
		code := http.StatusOK
		if created {
			code = http.StatusCreated
		}
		writeJSONStatus(w, code, map[string]any{"thread": th})
	})
}
