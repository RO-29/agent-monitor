package main

import (
	"net/http"
	"path/filepath"
	"strings"
)

// watchedRef identifies a watched (terminal) session, whatever prefix the caller used.
type watchedRef struct {
	ID       string // store id, e.g. claude:<uuid>
	Tool     Tool
	RawID    string
	Path     string // transcript
	Cwd      string
	Model    string
	Resolved bool
}

var watchedTools = []Tool{ToolClaude, ToolCodex, ToolCursorAgent, ToolOpencode}

// resolveWatched finds a session by store id or raw id: store first, then the
// transcript on disk, so a session that decayed out of the store still resolves.
func resolveWatched(id string) watchedRef {
	claimed, raw := splitAgentID(id)
	if store != nil {
		cands := []string{id}
		for _, t := range watchedTools {
			if t != claimed {
				cands = append(cands, sid(t, raw))
			}
		}
		for _, c := range cands {
			if s := store.Get(c); s != nil {
				return watchedRef{ID: s.ID, Tool: s.Tool, RawID: s.SessionID, Path: s.TranscriptPath, Cwd: s.Cwd, Model: s.Model, Resolved: true}
			}
		}
	}
	if raw == "" || strings.ContainsAny(raw, "/*?[") {
		return watchedRef{ID: id, Tool: claimed, RawID: raw}
	}
	if m, _ := filepath.Glob(filepath.Join(claudeConfigDir(), "projects", "*", raw+".jsonl")); len(m) > 0 {
		return watchedRef{ID: sid(ToolClaude, raw), Tool: ToolClaude, RawID: raw, Path: m[0], Resolved: true}
	}
	if p := codexRolloutPath(raw); p != "" {
		return watchedRef{ID: sid(ToolCodex, raw), Tool: ToolCodex, RawID: raw, Path: p, Resolved: true}
	}
	return watchedRef{ID: id, Tool: claimed, RawID: raw}
}

// listPanesFn is swapped by tests.
var listPanesFn = listTmuxPanes

var shellCommands = map[string]bool{"zsh": true, "-zsh": true, "bash": true, "-bash": true, "sh": true, "fish": true, "login": true, "tmux": true}

// WatchedPane is the GET /api/session/{id}/pane answer.
type WatchedPane struct {
	Registered bool   `json:"registered"`
	Sendable   bool   `json:"sendable"`
	Live       bool   `json:"live"`
	AgentID    string `json:"agentId,omitempty"`
	Agent      Tool   `json:"agent,omitempty"`
	PaneID     string `json:"paneId,omitempty"`
	TmuxTarget string `json:"tmuxTarget,omitempty"`
	Pid        int    `json:"pid,omitempty"`
	Command    string `json:"command,omitempty"`
	HostID     string `json:"hostId,omitempty"`
}

// Target is where input goes: the session host when there is one, else the tmux pane.
func (p WatchedPane) Target() string {
	if p.HostID != "" {
		return hostTargetPrefix + p.HostID
	}
	return p.PaneID
}

func watchedPane(id string) WatchedPane {
	var out WatchedPane
	if h := hostForSession(id); h != nil {
		out = WatchedPane{Registered: true, Sendable: true, Live: true, AgentID: id, Agent: h.Tool, HostID: h.HostID, PaneID: h.TmuxPane, Pid: h.AgentPid, Command: h.Agent}
		if ref := resolveWatched(id); ref.Resolved {
			out.AgentID = ref.ID
		}
		return out
	}
	var reg *PaneRegistration
	if paneRegistry != nil {
		reg = paneRegistry.Get(id)
		// Cursor CLI registers its pane through the Claude Code hook before its chat folder exists.
		if raw, ok := strings.CutPrefix(id, string(ToolCursor)+":"); reg == nil && ok {
			reg = paneRegistry.Get(sid(ToolClaude, raw))
		}
	}
	if reg == nil {
		ref := resolveWatched(id)
		out.Agent = ref.Tool
		if ref.Resolved {
			out.AgentID = ref.ID
		}
		return out
	}
	out.Registered, out.AgentID, out.Agent, out.PaneID, out.Pid = true, reg.AgentID, reg.Tool, reg.PaneID, reg.Pid
	panes, err := listPanesFn()
	if err != nil {
		return out
	}
	for _, p := range panes {
		if p.PaneID != reg.PaneID {
			continue
		}
		out.Live, out.Command = true, p.CurrentCommand
		out.TmuxTarget = p.SessionName + ":" + p.WindowID + "." + p.PaneID
		if out.Pid == 0 {
			out.Pid = p.PanePID
		}
		// Hook registrations carry no pid; then a non-shell foreground command means the agent still runs.
		if reg.Pid > 0 {
			out.Sendable = pidAlive(reg.Pid)
		} else {
			out.Sendable = p.CurrentCommand != "" && !shellCommands[p.CurrentCommand]
		}
	}
	return out
}

// watchedSessionRoute serves /api/session/{id}/pane, /items, /commands, /children, /question, /answer and /cli-queue; false for any other path.
func watchedSessionRoute(w http.ResponseWriter, r *http.Request, path string) bool {
	if strings.Contains(path, "/cli-queue") && serveCLIQueue(w, r, path) {
		return true
	}
	if strings.Contains(path, "/children") && childrenRoute(w, r, path) {
		return true
	}
	switch {
	case strings.HasSuffix(path, "/pane"):
		writeJSON(w, watchedPane(strings.TrimSuffix(path, "/pane")))
	case strings.HasSuffix(path, "/commands"):
		serveSessionCommands(w, r, strings.TrimSuffix(path, "/commands"))
	case strings.HasSuffix(path, "/items"):
		serveWatchedItems(w, r, strings.TrimSuffix(path, "/items"))
	case strings.HasSuffix(path, "/question"):
		serveTermQuestion(w, strings.TrimSuffix(path, "/question"))
	case strings.HasSuffix(path, "/answer"):
		serveTermAnswer(w, r, strings.TrimSuffix(path, "/answer"))
	default:
		return false
	}
	return true
}
