package main

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// terminalStart is a request to start an agent in a new tmux session under a session host.
type terminalStart struct {
	Agent          string   `json:"agent"`
	Cwd            string   `json:"cwd"`
	Prompt         string   `json:"prompt"`
	Model          string   `json:"model,omitempty"`
	PermissionMode string   `json:"permissionMode,omitempty"`
	Sandbox        string   `json:"sandbox,omitempty"`
	ApprovalPolicy string   `json:"approvalPolicy,omitempty"`
	Args           []string `json:"args,omitempty"` // raw flags, loopback callers only
	Attachments    []string `json:"attachments,omitempty"`
	Resume         string   `json:"resume,omitempty"` // a Claude or Codex session id to resume
	CodeMode       bool     `json:"-"`                // the resumed Claude session started in Code Mode
	Origin         string   `json:"-"`                // who asked agent-monitor to start it: "this Mac" or a device name
}

type terminalStarted struct {
	Session string `json:"session"`
	HostID  string `json:"hostId"`
	Attach  string `json:"attach"`
}

// typeWhenReadyFn types the first prompt once the host is up; tests swap it.
var typeWhenReadyFn = typeWhenReady

var terminalResumePattern = regexp.MustCompile(`^[0-9a-fA-F-]{36}$`)

var terminalModelPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/\[\]-]{0,99}$`)

// terminalFlags maps the allow-listed options to CLI flags; any value outside the lists fails.
func terminalFlags(in terminalStart) ([]string, error) {
	var out []string
	if in.Model != "" {
		if !terminalModelPattern.MatchString(in.Model) {
			return nil, errors.New("model has characters that are not allowed")
		}
		out = append(out, "--model", in.Model)
	}
	switch in.Agent {
	case "claude":
		if in.Sandbox != "" || in.ApprovalPolicy != "" {
			return nil, errors.New("sandbox and approvalPolicy are Codex options")
		}
		switch in.PermissionMode {
		case "", "manual":
		case "acceptEdits", "plan", "auto":
			out = append(out, "--permission-mode", in.PermissionMode)
		default:
			return nil, errors.New("permissionMode must be manual, acceptEdits, plan or auto")
		}
		if in.Resume != "" {
			if !terminalResumePattern.MatchString(in.Resume) {
				return nil, errors.New("resume must be a session id")
			}
			out = append(out, "--resume", in.Resume)
		}
	case "codex":
		if in.Resume != "" && !terminalResumePattern.MatchString(in.Resume) {
			return nil, errors.New("resume must be a session id")
		}
		if in.PermissionMode != "" {
			return nil, errors.New("permissionMode is a Claude option")
		}
		switch in.Sandbox {
		case "":
		case "read-only", "workspace-write":
			out = append(out, "-s", in.Sandbox)
		default:
			return nil, errors.New("sandbox must be read-only or workspace-write")
		}
		switch in.ApprovalPolicy {
		case "":
		case "untrusted", "on-request", "never":
			out = append(out, "-a", in.ApprovalPolicy)
		default:
			return nil, errors.New("approvalPolicy must be untrusted, on-request or never")
		}
	default:
		return nil, errors.New("agent must be claude or codex")
	}
	if in.Agent == "codex" && in.Resume != "" { // codex resume [OPTIONS] <id>
		out = append(append([]string{"resume"}, out...), in.Resume)
	}
	return out, nil
}

// errTerminalBadRequest marks a start request the caller can fix.
type errTerminalBadRequest struct{ msg string }

func (e errTerminalBadRequest) Error() string { return e.msg }

// startTerminal runs `agent-monitor run <agent>` in a detached tmux session am-<id> and types the prompt when ready.
func startTerminal(in terminalStart) (*terminalStarted, error) {
	flags, err := terminalFlags(in)
	if err != nil {
		return nil, errTerminalBadRequest{err.Error()}
	}
	in.Prompt = stripControl(in.Prompt)
	cwd := in.Cwd
	if strings.HasPrefix(cwd, "~/") {
		cwd = filepath.Join(homeDir(), cwd[2:])
	}
	if st, err := os.Stat(cwd); !filepath.IsAbs(cwd) || err != nil || !st.IsDir() {
		return nil, errTerminalBadRequest{"cwd must be an existing absolute directory"}
	}
	cwd = filepath.Clean(cwd)
	if len(in.Attachments) > 0 {
		if orchMgr == nil {
			return nil, errTerminalBadRequest{"this daemon keeps no uploads"}
		}
		ups, err := orchMgr.resolveUploads(in.Attachments)
		if err != nil {
			return nil, errTerminalBadRequest{err.Error()}
		}
		in.Prompt = withFilePaths(in.Prompt, ups)
	}
	exe, err := agentMonitorExe()
	if err != nil {
		return nil, err
	}
	id := newHostID()
	name := "am-" + id[:8]
	if in.Origin != "" {
		hostOrigins.Store(id, in.Origin)
	}
	parts := []string{"AM_HOST_ID_ASSIGN=" + id}
	if in.CodeMode && in.Agent == "claude" { // a Code Mode session resumes only with Code Mode on
		parts = append(parts, "CLAUDE_CODE_EXPERIMENTAL_CODE_MODE=1")
	}
	parts = append(parts, "exec", shellQuote(helperFor(exe)), "run", shellQuote(in.Agent))
	for _, a := range append(flags, in.Args...) {
		parts = append(parts, shellQuote(a))
	}
	if err := tmuxNewSessionFn(name, cwd, strings.Join(parts, " ")); err != nil {
		return nil, err
	}
	if strings.TrimSpace(in.Prompt) != "" {
		go func() {
			if err := typeWhenReadyFn(id, in.Prompt); err != nil {
				log.Printf("terminal %s: first prompt not sent: %v", id, err)
				orchMgr.notify(notifyEvent{Kind: "error", Title: "The first prompt did not reach " + in.Agent, Body: "Open the terminal and send it again: " + err.Error()})
			} else {
				log.Printf("terminal %s: first prompt sent", id)
			}
		}()
	}
	return &terminalStarted{Session: name, HostID: id, Attach: "tmux attach -t " + name}, nil
}

func serveOrchTerminal(w http.ResponseWriter, r *http.Request) {
	var in terminalStart
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&in); err != nil {
		writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": "bad json"})
		return
	}
	if len(in.Args) > 0 && !localCaller(r) {
		writeJSONStatus(w, http.StatusForbidden, map[string]any{"error": "args are accepted from this machine only"})
		return
	}
	in.Origin = requestOrigin(r)
	st, err := startTerminal(in)
	if err != nil {
		code := http.StatusInternalServerError
		if errors.As(err, new(errTerminalBadRequest)) {
			code = http.StatusBadRequest
		}
		writeJSONStatus(w, code, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, map[string]any{"ok": true, "session": st.Session, "hostId": st.HostID, "attach": st.Attach})
}

// orchLaunched is a headless thread or a terminal session, whichever the request asked for.
type orchLaunched struct {
	Thread   *OThread         `json:"thread,omitempty"`
	Terminal *terminalStarted `json:"terminal,omitempty"`
}

// launch starts in a terminal when asked and possible; fan-out and other agents always run headless.
func (m *OrchManager) launch(in orchCreate) (*orchLaunched, error) {
	if in.Where != "terminal" || len(in.Children) > 0 || (in.Agent != "claude" && in.Agent != "codex") {
		th, err := m.CreateThread(in)
		if err != nil {
			return nil, err
		}
		return &orchLaunched{Thread: th}, nil
	}
	ts := terminalStart{Agent: in.Agent, Cwd: in.Cwd, Prompt: in.Prompt, Model: in.Model, ApprovalPolicy: in.ApprovalPolicy, Attachments: in.Attachments}
	if in.Agent == "claude" {
		ts.PermissionMode = in.PermissionMode
		if in.Workspace == "read-only" && (ts.PermissionMode == "" || ts.PermissionMode == "manual") {
			ts.PermissionMode = "plan"
		}
	} else {
		ts.Sandbox = in.Sandbox
		if in.Workspace == "read-only" {
			ts.Sandbox = "read-only"
		}
	}
	st, err := startTerminal(ts)
	if err != nil {
		var bad errTerminalBadRequest
		if errors.As(err, &bad) {
			return nil, orchBadRequest(bad.msg)
		}
		return nil, err
	}
	return &orchLaunched{Terminal: st}, nil
}

// stripControl drops control bytes other than newline and tab: a CR or ESC typed into the pty acts as a key.
func stripControl(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' || (r >= 0x20 && r != 0x7f && !(r >= 0x80 && r < 0xa0)) {
			return r
		}
		return -1
	}, strings.ReplaceAll(s, "\r\n", "\n"))
}
