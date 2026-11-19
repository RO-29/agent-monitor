package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// acpAgent is one registry entry that speaks the Agent Client Protocol over stdio.
type acpAgent struct {
	ID      string            `json:"id"`
	Name    string            `json:"name"`
	Command string            `json:"command"`
	Args    []string          `json:"args"`
	Env     map[string]string `json:"env,omitempty"`
}

// orchAgentInfo is one row of GET /api/orch/agents.
type orchAgentInfo struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	Installed bool   `json:"installed"`
	Version   string `json:"version,omitempty"`
	Path      string `json:"path,omitempty"`
	Models    []any  `json:"models,omitempty"`
}

// cursor-agent 2026.11.05 has a hidden `acp` subcommand (loadSession false).
var acpBuiltins = []acpAgent{
	{ID: "opencode", Name: "OpenCode", Command: "opencode", Args: []string{"acp"}},
	{ID: "gemini", Name: "Gemini CLI", Command: "gemini", Args: []string{"--experimental-acp"}},
	{ID: "cursor", Name: "Cursor Agent", Command: "cursor-agent", Args: []string{"acp"}},
}

var acpIDRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,39}$`)

const orchAgentCacheTTL = 10 * time.Minute

type orchAgentProbe struct {
	at      time.Time
	path    string
	version string
}

type orchAgents struct {
	mu     sync.Mutex
	probes map[string]orchAgentProbe
	models map[string][]any
	// images records whether an ACP agent advertised promptCapabilities.image at initialize.
	images map[string]bool
	// version runs `<path> --version`; tests replace it.
	version func(path string) string
}

func newOrchAgents() *orchAgents {
	return &orchAgents{probes: map[string]orchAgentProbe{}, models: map[string][]any{}, images: map[string]bool{}, version: binaryVersion}
}

func (a *orchAgents) setImages(id string, ok bool) {
	a.mu.Lock()
	a.images[id] = ok
	a.mu.Unlock()
}

// imageSupport reports what an ACP agent advertised; known is false before its first initialize.
func (a *orchAgents) imageSupport(id string) (ok, known bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	ok, known = a.images[id]
	return ok, known
}

func (a *orchAgents) setModels(id string, models []any) {
	if len(models) == 0 {
		return
	}
	a.mu.Lock()
	a.models[id] = models
	a.mu.Unlock()
}

// acpRegistry returns the built-in entries overlaid by stateDir/acp-agents.json.
func (m *OrchManager) acpRegistry() []acpAgent {
	out := append([]acpAgent(nil), acpBuiltins...)
	b, err := os.ReadFile(filepath.Join(m.stateDir, "acp-agents.json"))
	if err != nil {
		return out
	}
	var user []acpAgent
	if json.Unmarshal(b, &user) != nil {
		return out
	}
	for _, u := range user {
		if !acpIDRe.MatchString(u.ID) || u.ID == "claude" || u.ID == "codex" || u.Command == "" {
			continue
		}
		if u.Name == "" {
			u.Name = u.ID
		}
		replaced := false
		for i := range out {
			if out[i].ID == u.ID {
				out[i], replaced = u, true
			}
		}
		if !replaced {
			out = append(out, u)
		}
	}
	return out
}

func (m *OrchManager) acpAgent(id string) (acpAgent, bool) {
	for _, a := range m.acpRegistry() {
		if a.ID == id {
			return a, true
		}
	}
	return acpAgent{}, false
}

// validAgent accepts claude, codex and any registry id.
func (m *OrchManager) validAgent(id string) bool {
	if id == "claude" || id == "codex" {
		return true
	}
	_, ok := m.acpAgent(id)
	return ok
}

func (m *OrchManager) agentError() error {
	ids := []string{"claude", "codex"}
	for _, a := range m.acpRegistry() {
		ids = append(ids, a.ID)
	}
	return orchBadRequest("agent must be one of " + strings.Join(ids, ", "))
}

// resolveBinary finds a command on PATH or in the usual user bin folders the daemon PATH may lack.
func resolveBinary(cmd string) string {
	if filepath.IsAbs(cmd) {
		if pathExists(cmd) {
			return cmd
		}
		return ""
	}
	if p, err := exec.LookPath(cmd); err == nil {
		return p
	}
	for _, dir := range []string{filepath.Join(homeDir(), ".local", "bin"), "/opt/homebrew/bin", "/usr/local/bin"} {
		if p := filepath.Join(dir, cmd); pathExists(p) {
			return p
		}
	}
	return ""
}

func binaryVersion(path string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "--version")
	cmd.Stdin = nil
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return clipString(strings.TrimSpace(firstLine(strings.TrimSpace(string(out)))), 80)
}

// probe returns the cached path and version of one binary.
func (a *orchAgents) probe(key, command string) orchAgentProbe {
	a.mu.Lock()
	p, ok := a.probes[key]
	a.mu.Unlock()
	if ok && time.Since(p.at) < orchAgentCacheTTL {
		return p
	}
	p = orchAgentProbe{at: time.Now(), path: resolveBinary(command)}
	if p.path != "" {
		p.version = a.version(p.path)
	}
	a.mu.Lock()
	a.probes[key] = p
	a.mu.Unlock()
	return p
}

// Agents lists claude, codex and the ACP registry with install state.
func (m *OrchManager) Agents() []orchAgentInfo {
	reg := m.acpRegistry()
	out := []orchAgentInfo{
		{ID: "claude", Name: "Claude Code", Kind: "claude", Models: []any{"opus", "sonnet", "haiku"}},
		{ID: "codex", Name: "Codex", Kind: "codex"},
	}
	cmds := []string{claudeBinary(), codexBinary()}
	for _, r := range reg {
		out = append(out, orchAgentInfo{ID: r.ID, Name: r.Name, Kind: "acp"})
		cmds = append(cmds, r.Command)
	}
	var wg sync.WaitGroup
	for i := range out {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			p := m.agents.probe(out[i].ID+"\x00"+cmds[i], cmds[i])
			out[i].Installed, out[i].Path, out[i].Version = p.path != "", p.path, p.version
		}(i)
	}
	wg.Wait()
	m.agents.mu.Lock()
	for i := range out {
		if ms := m.agents.models[out[i].ID]; len(ms) > 0 {
			out[i].Models = ms
		}
	}
	m.agents.mu.Unlock()
	return out
}

func (m *OrchManager) agentRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/orch/agents", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, m.Agents())
	})
}

func acpCommandFor(a acpAgent) (*exec.Cmd, error) {
	path := resolveBinary(a.Command)
	if path == "" {
		return nil, fmt.Errorf("%s is not installed (%s not found)", a.Name, a.Command)
	}
	return exec.Command(path, a.Args...), nil
}
