package main

import (
	"net/http"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// sessionOwner is the live process that already runs a Claude or Codex session, and its tmux session.
type sessionOwner struct {
	Pid  int    `json:"pid"`
	Tmux string `json:"tmux,omitempty"`
	Pane string `json:"pane,omitempty"`
	Host string `json:"hostId,omitempty"`
}

// psCommandsFn lists "pid command" lines; tests swap it.
var psCommandsFn = func() string {
	out, _ := exec.Command("ps", "-axo", "pid=,command=").Output()
	return string(out)
}

// tmuxPanesFn lists tmux panes; tests swap it.
var tmuxPanesFn = listTmuxPanes

// resumePids are the agent processes whose command line resumes raw: `claude --resume <id>` or `codex resume <id>`.
// A fork writes a new session, so it does not count.
func resumePids(tool Tool, raw string) []int {
	var out []int
	for _, line := range strings.Split(psCommandsFn(), "\n") {
		f := strings.Fields(line)
		if len(f) < 3 || filepath.Base(f[1]) != string(tool) || slices.Contains(f, "--fork-session") {
			continue
		}
		for i := 2; i < len(f); i++ {
			hit := f[i] == "--resume="+raw || (i+1 < len(f) && f[i+1] == raw && (f[i] == "--resume" || f[i] == "-r" || f[i] == "resume"))
			if hit {
				if pid, err := strconv.Atoi(f[0]); err == nil {
					out = append(out, pid)
				}
				break
			}
		}
	}
	return out
}

// liveSessionOwner finds a running copy of a session: a linked host first, then any process resuming it.
func liveSessionOwner(agentID string) *sessionOwner {
	tool, raw := splitAgentID(agentID)
	if raw == "" || (tool != ToolClaude && tool != ToolCodex) {
		return nil
	}
	var hosts []HostInfo
	if hostRegistry != nil {
		hosts = hostRegistry.List()
	}
	var o *sessionOwner
	if h := hostForSession(agentID); h != nil {
		o = &sessionOwner{Pid: h.AgentPid, Host: h.HostID}
	}
	if o == nil {
		// argv goes stale: a process started with --resume A that moved to B (/clear, /resume) owns B now.
		moved := map[int]bool{}
		for _, h := range hosts {
			if h.AgentPid > 0 && h.SessionID != "" && h.SessionID != raw {
				moved[h.AgentPid] = true
			}
		}
		for _, pid := range resumePids(tool, raw) {
			if !moved[pid] && pidAlive(pid) {
				o = &sessionOwner{Pid: pid}
				break
			}
		}
	}
	if o == nil {
		return nil
	}
	pane := ""
	for _, h := range hosts {
		if (o.Host != "" && h.HostID == o.Host) || (o.Pid > 0 && h.AgentPid == o.Pid) {
			o.Host, pane = h.HostID, h.TmuxPane
			break
		}
	}
	if panes, err := tmuxPanesFn(); err == nil {
		tree := procTree()
		for _, p := range panes {
			if (pane != "" && p.PaneID == pane) || (pane == "" && o.Pid > 0 && isDescendant(tree, p.PanePID, o.Pid)) {
				o.Tmux, o.Pane = p.SessionName, p.PaneID
				break
			}
		}
	}
	return o
}

func registerSessionOwnerRoutes(mux *http.ServeMux) {
	// The CLI wrapper asks this before `claude --resume <id>`, so a session never runs twice.
	mux.HandleFunc("GET /api/session/owner/{id}", func(w http.ResponseWriter, r *http.Request) {
		o := liveSessionOwner(r.PathValue("id"))
		writeJSON(w, map[string]any{"live": o != nil, "owner": o})
	})
}
