//go:build !windows

package main

// End-to-end harness for `agent-monitor install`.
//
// The unit tests above cover the merge functions in isolation. This drives the
// real binary against a synthetic HOME and asserts what actually lands on disk,
// because the bugs this file exists to catch live in the wiring rather than in
// the helpers: which bucket an entry ends up in, whether a second run rewrites
// anything, whether a backup is world-readable, whether the path baked into an
// MCP config still exists once install has exited.
//
// The binary is built into <repo>/.e2e-bin rather than a temp dir on purpose —
// install refuses to persist a path under TMPDIR (see isEphemeralExe), which is
// exactly the behaviour we want in production and would defeat the harness.

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

var (
	buildOnce sync.Once
	builtBin  string
	buildErr  error
)

// installerBinary builds the real agent-monitor once per `go test` run and
// returns its path.
func installerBinary(t *testing.T) string {
	t.Helper()
	if testing.Short() {
		t.Skip("e2e harness compiles the binary; skipped under -short")
	}
	buildOnce.Do(func() {
		repo, err := os.Getwd()
		if err != nil {
			buildErr = err
			return
		}
		dir := filepath.Join(repo, ".e2e-bin")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			buildErr = err
			return
		}
		bin := filepath.Join(dir, "agent-monitor")
		out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput()
		if err != nil {
			buildErr = fmt.Errorf("go build: %v\n%s", err, out)
			return
		}
		builtBin = bin
	})
	if buildErr != nil {
		t.Fatalf("building installer: %v", buildErr)
	}
	return builtBin
}

// runInstall invokes `agent-monitor install` with HOME pointed at home. cwd
// stays the checkout, which is how a user runs it.
func runInstall(t *testing.T, home string) (stdout string, exitCode int) {
	t.Helper()
	cmd := exec.Command(installerBinary(t), "install")
	// Drop any inherited HOME/override so the child sees only what we set:
	// Go's Getenv takes the first match, so appending wouldn't shadow it.
	env := make([]string, 0, len(os.Environ())+1)
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if k == "HOME" || k == "AGENT_MONITOR_REPO" {
			continue
		}
		env = append(env, kv)
	}
	cmd.Env = append(env, "HOME="+home)
	out, err := cmd.CombinedOutput()
	if ee, ok := err.(*exec.ExitError); ok {
		exitCode = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("running install: %v\n%s", err, out)
	}
	return string(out), exitCode
}

// newHome builds a HOME containing the files install expects to find.
// A nil/absent value means "don't create this file".
type homeSpec struct {
	settings   string // ~/.claude/settings.json
	claudeJSON string // ~/.claude.json
	codexTOML  string // ~/.codex/config.toml ("" but withCodex => empty dir only)
	withCodex  bool   // create ~/.codex at all
	noSettings bool   // skip ~/.claude/settings.json entirely
}

func newHome(t *testing.T, spec homeSpec) string {
	t.Helper()
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	if !spec.noSettings {
		s := spec.settings
		if s == "" {
			s = "{}\n"
		}
		writeFile(t, filepath.Join(home, ".claude", "settings.json"), s, 0o600)
	}
	if spec.claudeJSON != "" {
		writeFile(t, filepath.Join(home, ".claude.json"), spec.claudeJSON, 0o600)
	}
	if spec.withCodex || spec.codexTOML != "" {
		if err := os.MkdirAll(filepath.Join(home, ".codex"), 0o755); err != nil {
			t.Fatal(err)
		}
		if spec.codexTOML != "" {
			writeFile(t, filepath.Join(home, ".codex", "config.toml"), spec.codexTOML, 0o600)
		}
	}
	return home
}

func writeFile(t *testing.T, path, body string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// hookCommandsFor returns every command registered for one event, flattened
// across buckets — what actually fires, regardless of file shape.
func hookCommandsFor(t *testing.T, home, event string) []string {
	t.Helper()
	var settings struct {
		Hooks map[string][]struct {
			Matcher string `json:"matcher"`
			Hooks   []struct {
				Command string `json:"command"`
				Timeout int    `json:"timeout"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	raw := readFile(t, filepath.Join(home, ".claude", "settings.json"))
	if err := json.Unmarshal([]byte(raw), &settings); err != nil {
		t.Fatalf("settings.json is not valid JSON after install: %v\n%s", err, raw)
	}
	var out []string
	for _, bucket := range settings.Hooks[event] {
		for _, h := range bucket.Hooks {
			out = append(out, h.Command)
		}
	}
	return out
}

func ourHookCount(cmds []string) int {
	n := 0
	for _, c := range cmds {
		if isOurHookCommand(c) {
			n++
		}
	}
	return n
}

var installedEvents = []string{
	"Notification", "Stop", "SubagentStop",
	"SessionStart", "SessionEnd", "UserPromptSubmit",
}

func TestE2EFreshInstall(t *testing.T) {
	home := newHome(t, homeSpec{})
	out, code := runInstall(t, home)
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}

	repo, _ := os.Getwd()
	wantHook := filepath.Join(repo, "bin", "claude-hook.sh")
	for _, evt := range installedEvents {
		cmds := hookCommandsFor(t, home, evt)
		if len(cmds) != 1 || cmds[0] != wantHook {
			t.Errorf("%s hooks = %q, want exactly [%s]", evt, cmds, wantHook)
		}
	}

	// The MCP command must be a path that still exists now that install has
	// exited — the whole point of refusing go-run temp paths.
	bin := claudeMCPCommand(t, home)
	if !pathExists(bin) {
		t.Fatalf("MCP command %q doesn't exist after install", bin)
	}
	if bin != installerBinary(t) {
		t.Errorf("MCP command = %q, want the running binary %q", bin, installerBinary(t))
	}
}

func claudeMCPCommand(t *testing.T, home string) string {
	t.Helper()
	var cfg struct {
		MCPServers map[string]struct {
			Command string   `json:"command"`
			Args    []string `json:"args"`
		} `json:"mcpServers"`
	}
	raw := readFile(t, filepath.Join(home, ".claude.json"))
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatalf(".claude.json is not valid JSON after install: %v", err)
	}
	return cfg.MCPServers["agent-monitor"].Command
}

// The regression this PR exists for: a checkout that has moved, with the stale
// entry sitting in a bucket other than the first.
func TestE2EMovedCheckout(t *testing.T) {
	home := newHome(t, homeSpec{
		settings: `{
  "model": "opus",
  "hooks": {
    "Stop": [
      {"matcher": "", "hooks": [{"type": "command", "command": "/somewhere/else/dr_learning_hook.sh", "timeout": 1000}]},
      {"matcher": "", "hooks": [{"type": "command", "command": "/old/checkout/bin/claude-hook.sh", "timeout": 4242}]}
    ],
    "SessionStart": [
      {"matcher": "*", "hooks": [{"type": "command", "command": "/older/checkout/bin/claude-hook.sh", "timeout": 2000}]}
    ]
  }
}`,
		codexTOML: "[mcp_servers.agent-monitor]\ncommand = \"/old/checkout/agent-monitor\"\ncommand_timeout = 30\nargs    = [\"mcp-perm-server\"]\n\n[mcp_servers.other]\ncommand = \"/opt/other\"\n",
	})
	out, code := runInstall(t, home)
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}

	repo, _ := os.Getwd()
	wantHook := filepath.Join(repo, "bin", "claude-hook.sh")
	for _, evt := range installedEvents {
		cmds := hookCommandsFor(t, home, evt)
		if n := ourHookCount(cmds); n != 1 {
			t.Errorf("%s: %d agent-monitor hooks registered, want 1: %q", evt, n, cmds)
		}
		for _, c := range cmds {
			if isOurHookCommand(c) && c != wantHook {
				t.Errorf("%s: stale hook %q survived, want %q", evt, c, wantHook)
			}
		}
	}

	// Unrelated hook and unrelated settings keys must survive.
	stop := hookCommandsFor(t, home, "Stop")
	if !contains(stop, "/somewhere/else/dr_learning_hook.sh") {
		t.Errorf("unrelated Stop hook was dropped: %q", stop)
	}
	if !strings.Contains(readFile(t, filepath.Join(home, ".claude", "settings.json")), `"model"`) {
		t.Error("unrelated top-level settings key was dropped")
	}

	// Codex: our command repointed, neighbours untouched.
	toml := readFile(t, filepath.Join(home, ".codex", "config.toml"))
	if strings.Contains(toml, "/old/checkout/agent-monitor") {
		t.Errorf("stale codex command survived:\n%s", toml)
	}
	if !strings.Contains(toml, "command_timeout = 30") {
		t.Errorf("neighbouring command_timeout key was clobbered:\n%s", toml)
	}
	if !strings.Contains(toml, `command = "/opt/other"`) {
		t.Errorf("unrelated MCP server was clobbered:\n%s", toml)
	}
	// Exactly two `command` keys: ours and the other server's. A third would
	// mean command_timeout got rewritten into a duplicate key, which makes the
	// file unparseable and takes every MCP server down with it.
	if n := strings.Count(toml, "\ncommand = "); n != 2 {
		t.Errorf("expected 2 command keys (ours, the other server's), got %d:\n%s", n, toml)
	}
}

// A second run must report no changes and leave both files byte-identical.
func TestE2EIdempotent(t *testing.T) {
	home := newHome(t, homeSpec{withCodex: true})
	if _, code := runInstall(t, home); code != 0 {
		t.Fatal("first install failed")
	}
	settingsPath := filepath.Join(home, ".claude", "settings.json")
	codexPath := filepath.Join(home, ".codex", "config.toml")
	beforeSettings := readFile(t, settingsPath)
	beforeCodex := readFile(t, codexPath)

	out, code := runInstall(t, home)
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if !strings.Contains(out, "+0 hooks") {
		t.Errorf("second run should add nothing, got:\n%s", out)
	}
	if got := readFile(t, settingsPath); got != beforeSettings {
		t.Errorf("settings.json changed on re-run:\n--- before ---\n%s\n--- after ---\n%s", beforeSettings, got)
	}
	if got := readFile(t, codexPath); got != beforeCodex {
		t.Errorf("config.toml changed on re-run:\n--- before ---\n%s\n--- after ---\n%s", beforeCodex, got)
	}
}

// Backups sit next to files Claude Code keeps at 0600 and hold the same bytes,
// so they must not be readable by other accounts on the machine.
func TestE2EBackupsArePrivate(t *testing.T) {
	home := newHome(t, homeSpec{
		claudeJSON: `{"oauthAccount": {"emailAddress": "someone@example.com"}}`,
		codexTOML:  "[mcp_servers.agent-monitor]\ncommand = \"/old/agent-monitor\"\n",
	})
	if _, code := runInstall(t, home); code != 0 {
		t.Fatal("install failed")
	}

	var backups []string
	for _, dir := range []string{home, filepath.Join(home, ".claude"), filepath.Join(home, ".codex")} {
		found, err := filepath.Glob(filepath.Join(dir, "*.bak-*"))
		if err != nil {
			t.Fatal(err)
		}
		backups = append(backups, found...)
	}
	if len(backups) == 0 {
		t.Fatal("install made no backups at all")
	}
	for _, b := range backups {
		info, err := os.Stat(b)
		if err != nil {
			t.Fatal(err)
		}
		if mode := info.Mode().Perm(); mode&0o077 != 0 {
			t.Errorf("backup %s is mode %04o, want no group/other access", filepath.Base(b), mode)
		}
	}
}

// Codex not installed is a normal state, not an error.
func TestE2ECodexAbsent(t *testing.T) {
	home := newHome(t, homeSpec{})
	out, code := runInstall(t, home)
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if !strings.Contains(out, "Codex not detected") {
		t.Errorf("expected a Codex-skipped notice, got:\n%s", out)
	}
	if pathExists(filepath.Join(home, ".codex", "config.toml")) {
		t.Error("install created a codex config despite codex not being installed")
	}
}

// Hook wiring needs settings.json, but MCP registration doesn't. A failure in
// the first must not cost you the second — while still failing the exit code.
func TestE2EHookFailureStillRegistersMCP(t *testing.T) {
	home := newHome(t, homeSpec{noSettings: true, withCodex: true})
	out, code := runInstall(t, home)
	if code == 0 {
		t.Fatalf("expected a non-zero exit, got 0:\n%s", out)
	}
	if !strings.Contains(out, "settings.json") {
		t.Errorf("error should name the missing file, got:\n%s", out)
	}
	if bin := claudeMCPCommand(t, home); bin != installerBinary(t) {
		t.Errorf("Claude MCP not registered despite hooks failing: command = %q\n%s", bin, out)
	}
	toml := readFile(t, filepath.Join(home, ".codex", "config.toml"))
	if !strings.Contains(toml, "[mcp_servers.agent-monitor]") {
		t.Errorf("Codex MCP not registered despite hooks failing:\n%s", toml)
	}
}

// Repointing our MCP entry must not discard fields somebody added to it.
func TestE2EPreservesCustomMCPFields(t *testing.T) {
	home := newHome(t, homeSpec{
		claudeJSON: `{
  "mcpServers": {
    "agent-monitor": {
      "command": "/old/checkout/agent-monitor",
      "args": ["mcp-perm-server"],
      "env": {"AGENT_MONITOR_PORT": "7788"},
      "timeout": 30000
    },
    "other-server": {"command": "/opt/other", "args": []}
  }
}`,
	})
	if _, code := runInstall(t, home); code != 0 {
		t.Fatal("install failed")
	}

	var cfg struct {
		MCPServers map[string]struct {
			Command string            `json:"command"`
			Args    []string          `json:"args"`
			Env     map[string]string `json:"env"`
			Timeout int               `json:"timeout"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(readFile(t, filepath.Join(home, ".claude.json"))), &cfg); err != nil {
		t.Fatal(err)
	}
	ours := cfg.MCPServers["agent-monitor"]
	if ours.Command != installerBinary(t) {
		t.Errorf("command = %q, want %q", ours.Command, installerBinary(t))
	}
	if ours.Env["AGENT_MONITOR_PORT"] != "7788" {
		t.Errorf("custom env was dropped: %v", ours.Env)
	}
	if ours.Timeout != 30000 {
		t.Errorf("custom timeout was dropped: %d", ours.Timeout)
	}
	if _, ok := cfg.MCPServers["other-server"]; !ok {
		t.Error("unrelated MCP server was dropped")
	}
}

// A section left without a command key is a server codex can't spawn; install
// should repair it rather than report success.
func TestE2ECodexSectionMissingCommand(t *testing.T) {
	home := newHome(t, homeSpec{
		codexTOML: "[mcp_servers.agent-monitor]\nargs    = [\"mcp-perm-server\"]\n",
	})
	if _, code := runInstall(t, home); code != 0 {
		t.Fatal("install failed")
	}
	toml := readFile(t, filepath.Join(home, ".codex", "config.toml"))
	if !strings.Contains(toml, fmt.Sprintf("command = %q", installerBinary(t))) {
		t.Errorf("command key was not inserted:\n%s", toml)
	}
}

// An array spread over several lines must not be mistaken for a section break.
func TestE2ECodexMultilineArray(t *testing.T) {
	home := newHome(t, homeSpec{
		codexTOML: "[mcp_servers.agent-monitor]\nenv_pairs = [\n  [\"A\", \"1\"],\n]\ncommand = \"/old/checkout/agent-monitor\"\n",
	})
	if _, code := runInstall(t, home); code != 0 {
		t.Fatal("install failed")
	}
	toml := readFile(t, filepath.Join(home, ".codex", "config.toml"))
	if strings.Contains(toml, "/old/checkout/agent-monitor") {
		t.Errorf("stale command after a multi-line array was not updated:\n%s", toml)
	}
	if !strings.Contains(toml, `["A", "1"],`) {
		t.Errorf("array contents were mangled:\n%s", toml)
	}
}

func contains(hay []string, needle string) bool {
	for _, s := range hay {
		if s == needle {
			return true
		}
	}
	return false
}
