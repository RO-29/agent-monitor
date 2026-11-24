package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strings"
	"time"
)

var (
	sessionEndWait    = 10 * time.Second
	sessionEndCtrlC   = 3 * time.Second
	sessionEndPoll    = 250 * time.Millisecond
	sessionInterrupt  = 600 * time.Millisecond
	errSessionNotLive = errors.New("this session has no running agent")
)

// agentPidOf is the pid of the agent that owns a session's terminal, or 0.
func agentPidOf(id string) int {
	if h := hostForSession(id); h != nil && h.AgentPid > 0 {
		return h.AgentPid
	}
	if paneRegistry != nil {
		if reg := paneRegistry.Get(id); reg != nil && reg.Pid > 0 {
			return reg.Pid
		}
	}
	if s := store.Get(id); s != nil {
		return s.Pid
	}
	return 0
}

func waitPidGone(pid int, d time.Duration) bool {
	for end := time.Now().Add(d); time.Now().Before(end); time.Sleep(sessionEndPoll) {
		if !pidAlive(pid) {
			return true
		}
	}
	return !pidAlive(pid)
}

// endSession stops a running turn, types /exit, and presses Ctrl-C twice if the agent is still there.
func endSession(id string) (bool, int, error) {
	s := store.Get(id)
	if s == nil {
		return false, http.StatusNotFound, errors.New("session not found")
	}
	if s.Tool != ToolClaude && s.Tool != ToolCodex {
		return false, http.StatusBadRequest, errors.New("only Claude Code and Codex sessions can be ended here")
	}
	defer cliLock(id)()
	pid := agentPidOf(id)
	_, code, err := withTerminal(id, func(t string) error {
		scr, err := targetCapture(t, 0, true)
		if err != nil {
			return err
		}
		if panePromptOpen(scr) {
			return errPromptOpen
		}
		if boxDraft(scr) {
			return errBoxDraft
		}
		if s.State == StateRunning {
			if err := targetKeys(t, []string{"Escape"}); err != nil {
				return err
			}
			time.Sleep(sessionInterrupt)
		}
		return targetText(t, "/exit", true)
	})
	if errors.Is(err, errPromptOpen) || errors.Is(err, errBoxDraft) {
		return false, statusRefused, err
	}
	if err != nil {
		if code == 0 {
			code = http.StatusBadGateway
		}
		return false, code, err
	}
	if pid <= 0 {
		return false, http.StatusAccepted, nil // sent; no pid to watch
	}
	if waitPidGone(pid, sessionEndWait) {
		store.SetState(s.Tool, s.SessionID, StateCompleted, "ended from agent-monitor")
		return true, http.StatusOK, nil
	}
	_, _, _ = readTerminal(id, func(t string) error { return targetKeys(t, []string{"C-c", "C-c"}) })
	if waitPidGone(pid, sessionEndCtrlC) {
		store.SetState(s.Tool, s.SessionID, StateCompleted, "ended from agent-monitor")
		return true, http.StatusOK, nil
	}
	return false, http.StatusAccepted, nil
}

var (
	errPromptOpen = errors.New("a prompt is open in the terminal; answer or dismiss it first")
	errBoxDraft   = errors.New("the terminal's input box holds a draft; send or clear it first")
)

// boxDraft is true when Claude's input box holds typed text (not empty, not its "Try ..." placeholder).
func boxDraft(screen string) bool {
	b := strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(cliInputBox(screen)), "❯>›"))
	return b != "" && !strings.HasPrefix(b, `Try "`) && !strings.Contains(strings.ToLower(b), cliHint)
}

// resumeSession starts a hosted terminal that resumes an ended Claude or Codex session.
func resumeSession(id, prompt string, attachments []string, origin ...string) (*terminalStarted, int, error) {
	s := store.Get(id)
	if s == nil {
		return nil, http.StatusNotFound, errors.New("session not found")
	}
	if s.Tool != ToolClaude && s.Tool != ToolCodex {
		return nil, http.StatusBadRequest, errors.New("only Claude Code and Codex sessions can be resumed here")
	}
	switch s.State { // a session with no host or pane has no pid; its state is the only sign it runs
	case StateRunning, StateAwaitingInput, StateAwaitingPermission, StateIdle:
		return nil, http.StatusConflict, errors.New("this session looks active; exit it in its terminal or end it first")
	}
	if pid := agentPidOf(id); (pid > 0 && pidAlive(pid)) || hostForSession(id) != nil {
		return nil, http.StatusConflict, errors.New("this session still runs in a terminal; open it there or end it first")
	}
	if o := liveSessionOwner(id); o != nil {
		where := "another terminal"
		if o.Tmux != "" {
			where = "tmux session " + o.Tmux
		}
		return nil, http.StatusConflict, errors.New("this session already runs in " + where + "; open it there")
	}
	if st, err := os.Stat(s.Cwd); s.Cwd == "" || err != nil || !st.IsDir() {
		return nil, http.StatusConflict, errors.New("the session's folder no longer exists")
	}
	codeMode := s.Tool == ToolClaude && transcriptCodeMode(s.TranscriptPath)
	perm := ""
	if s.Tool == ToolClaude {
		perm = transcriptPermissionMode(s.TranscriptPath)
	}
	started, err := startTerminal(terminalStart{Agent: string(s.Tool), Cwd: s.Cwd, Prompt: prompt, Attachments: attachments, Resume: s.SessionID, PermissionMode: perm, CodeMode: codeMode, Origin: firstOf(origin...)})
	if err != nil {
		var bad errTerminalBadRequest
		if errors.As(err, &bad) {
			return nil, http.StatusBadRequest, err
		}
		return nil, http.StatusInternalServerError, err
	}
	return started, http.StatusCreated, nil
}

func registerSessionLifecycleRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/session/end/{id}", func(w http.ResponseWriter, r *http.Request) {
		ended, code, err := endSession(r.PathValue("id"))
		if err != nil {
			writeJSONStatus(w, code, map[string]any{"error": err.Error()})
			return
		}
		writeJSONStatus(w, code, map[string]any{"ok": true, "ended": ended})
	})
	mux.HandleFunc("POST /api/session/resume/{id}", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Prompt      string   `json:"prompt"`
			Attachments []string `json:"attachments"`
		}
		if r.ContentLength != 0 {
			if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&in); err != nil {
				writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": "bad json"})
				return
			}
		}
		started, code, err := resumeSession(r.PathValue("id"), in.Prompt, in.Attachments, requestOrigin(r))
		if err != nil {
			writeJSONStatus(w, code, map[string]any{"error": err.Error()})
			return
		}
		writeJSONStatus(w, code, map[string]any{"ok": true, "terminal": started})
	})
}
