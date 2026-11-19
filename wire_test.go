package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type wenv struct {
	t     *testing.T
	home  string
	inst  string
	calls [][]string
}

func newWenv(t *testing.T) *wenv {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("AGENT_MONITOR_HOME", filepath.Join(home, ".am"))
	t.Setenv("PATH", t.TempDir())
	e := &wenv{t: t, home: home, inst: filepath.Join(home, "agent-monitor")}
	for _, f := range []string{"claude-hook.sh", "codex-hook.sh", "cursor-hook.sh"} {
		e.write("agent-monitor/bin/"+f, "#!/bin/sh\n", 0o755)
	}
	e.write("agent-monitor/agent-monitor", "bin", 0o755)
	oldRun, oldExe, oldBefore, oldSys := wireRun, wireExe, wireBeforeRename, wireSysBinDirs
	wireSysBinDirs = nil
	wireRun = func(bin string, args ...string) error {
		e.calls = append(e.calls, append([]string{filepath.Base(bin)}, args...))
		return nil
	}
	wireExe = func() (string, error) { return filepath.Join(e.inst, "agent-monitor"), nil }
	t.Cleanup(func() { wireRun, wireExe, wireBeforeRename, wireSysBinDirs = oldRun, oldExe, oldBefore, oldSys })
	return e
}

func (e *wenv) write(rel, content string, mode os.FileMode) {
	e.t.Helper()
	p := filepath.Join(e.home, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), mode); err != nil {
		e.t.Fatal(err)
	}
	_ = os.Chmod(p, mode)
}

func (e *wenv) read(rel string) string {
	b, err := os.ReadFile(filepath.Join(e.home, rel))
	if err != nil {
		return "<missing>"
	}
	return string(b)
}

func (e *wenv) steps(agent, kind string, allow func(string) bool, force bool) []wireResult {
	e.t.Helper()
	c := newWireCtx(false, allow)
	if force {
		c.force[agent] = true
	}
	var out []wireResult
	for _, k := range agentRegistry {
		if k.ID != agent {
			continue
		}
		for _, st := range k.Wire {
			if kind == "" || st.Kind == kind {
				out = append(out, st.Run(c))
			}
		}
	}
	if c.dirty {
		if err := c.led.save(); err != nil {
			e.t.Fatal(err)
		}
	}
	return out
}

func (e *wenv) one(agent, kind string) wireResult {
	e.t.Helper()
	r := e.steps(agent, kind, nil, false)
	if len(r) != 1 {
		e.t.Fatalf("want one result, got %+v", r)
	}
	return r[0]
}

func (e *wenv) unwire(agent string) []wireResult {
	e.t.Helper()
	c := newWireCtx(false, nil)
	out := c.unwire(agent)
	if err := c.led.save(); err != nil {
		e.t.Fatal(err)
	}
	return out
}

func wantAction(t *testing.T, r wireResult, action string) {
	t.Helper()
	if r.Action != action {
		t.Fatalf("action = %q (%s), want %q", r.Action, r.Detail, action)
	}
}

const piFixture = `{
  "mcpServers": {
    "other": {
      "type": "http",
      "url": "https://example.test/mcp",
      "headers": {"Authorization": "Bearer redacted"},
      "exposure": "deferred"
    }
  }
}
`

func TestMCPJSONAddsAndUnwireRestoresBytes(t *testing.T) {
	e := newWenv(t)
	e.write(".pi/agent/mcp.json", piFixture, 0o600)
	wantAction(t, e.one("pi", "mcp"), "added")
	got := e.read(".pi/agent/mcp.json")
	var parsed map[string]map[string]map[string]any
	if err := json.Unmarshal([]byte(got), &parsed); err != nil {
		t.Fatalf("invalid json: %v\n%s", err, got)
	}
	ent := parsed["mcpServers"]["agent-monitor"]
	if ent["command"] != filepath.Join(e.inst, "agent-monitor") || ent["exposure"] != "direct" {
		t.Fatalf("entry = %v", ent)
	}
	if !strings.HasPrefix(got, strings.TrimSuffix(piFixture, "\n  }\n}\n")) {
		t.Fatalf("existing bytes changed:\n%s", got)
	}
	wantAction(t, e.one("pi", "mcp"), "unchanged")
	res := e.unwire("pi")
	if len(res) != 1 || res[0].Action != "removed" {
		t.Fatalf("unwire = %+v", res)
	}
	if e.read(".pi/agent/mcp.json") != piFixture {
		t.Fatalf("bytes not restored:\n%s", e.read(".pi/agent/mcp.json"))
	}
}

func TestMCPJSONOddOrderAndEscapesKeepBytes(t *testing.T) {
	e := newWenv(t)
	odd := "{\"z\":1,\"mcpServers\":{\"b<>&\":{\"command\":\"x\\u003c\"},\"a\":\"t\\tab\"},\"a\":[1,2]}"
	e.write(".pi/agent/mcp.json", odd, 0o644)
	wantAction(t, e.one("pi", "mcp"), "added")
	got := e.read(".pi/agent/mcp.json")
	if !strings.Contains(got, "\"b<>&\":{\"command\":\"x\\u003c\"},\"a\":\"t\\tab\", \"agent-monitor\"") || !strings.HasSuffix(got, "},\"a\":[1,2]}") {
		t.Fatalf("splice moved other bytes:\n%s", got)
	}
	e.unwire("pi")
	if e.read(".pi/agent/mcp.json") != odd {
		t.Fatalf("bytes not restored:\n%s", e.read(".pi/agent/mcp.json"))
	}
}

func TestMCPJSONAbsentAndEmptyFile(t *testing.T) {
	e := newWenv(t)
	e.write(".pi/agent/.keep", "", 0o644)
	wantAction(t, e.one("pi", "mcp"), "added")
	if !json.Valid([]byte(e.read(".pi/agent/mcp.json"))) {
		t.Fatal("new file is not valid json")
	}
	e.unwire("pi")
	if e.read(".pi/agent/mcp.json") != "<missing>" {
		t.Fatal("a file we created must be deleted by unwire")
	}
	e.write(".pi/agent/mcp.json", "\n", 0o644)
	wantAction(t, e.one("pi", "mcp"), "added")
	e.unwire("pi")
	if e.read(".pi/agent/mcp.json") != "\n" {
		t.Fatalf("an empty file must come back as it was: %q", e.read(".pi/agent/mcp.json"))
	}
}

func TestMCPJSONAdoptsEntryWithExtraKeys(t *testing.T) {
	e := newWenv(t)
	bin := filepath.Join(e.inst, "agent-monitor")
	doc := "{\n  \"mcpServers\": {\n    \"agent-monitor\": {\n      \"args\": [\"mcp-perm-server\"],\n      \"command\": \"" + bin + "\",\n      \"exposure\": \"direct\"\n    }\n  }\n}\n"
	e.write(".pi/agent/mcp.json", doc, 0o600)
	wantAction(t, e.one("pi", "mcp"), "adopted")
	if e.read(".pi/agent/mcp.json") != doc {
		t.Fatal("adoption must not touch the file")
	}
	if l := loadLedger(); len(l.Entries) != 1 || l.Entries[0].Action != "adopted" {
		t.Fatalf("ledger = %+v", l.Entries)
	}
	if res := e.unwire("pi"); len(res) != 0 {
		t.Fatalf("unwire touched an adopted entry: %+v", res)
	}
	if e.read(".pi/agent/mcp.json") != doc {
		t.Fatal("unwire changed an adopted entry")
	}
}

func TestMCPJSONUserEditedIsSkipped(t *testing.T) {
	e := newWenv(t)
	doc := "{\"mcpServers\":{\"agent-monitor\":{\"command\":\"/opt/mine/agent-monitor\",\"args\":[\"mcp-perm-server\"]}}}"
	e.write(".pi/agent/mcp.json", doc, 0o644)
	wantAction(t, e.one("pi", "mcp"), "skipped: user-edited")
	e.write(".pi/agent/mcp.json", strings.Replace(doc, `["mcp-perm-server"]`, `["other"]`, 1), 0o644)
	wantAction(t, e.one("pi", "mcp"), "skipped: user-edited")
}

func TestMCPJSONTombstoneAndForce(t *testing.T) {
	e := newWenv(t)
	e.write(".pi/agent/mcp.json", piFixture, 0o644)
	wantAction(t, e.one("pi", "mcp"), "added")
	e.write(".pi/agent/mcp.json", piFixture, 0o644)
	wantAction(t, e.one("pi", "mcp"), "removed by user")
	wantAction(t, e.one("pi", "mcp"), "removed by user")
	if e.read(".pi/agent/mcp.json") != piFixture {
		t.Fatal("a tombstoned entry must not come back")
	}
	wantAction(t, e.steps("pi", "mcp", nil, true)[0], "added")
}

func TestMCPJSONBadFileGivesManualSnippet(t *testing.T) {
	e := newWenv(t)
	e.write(".pi/agent/mcp.json", "{\n  // comment\n  \"mcpServers\": {}\n}\n", 0o644)
	r := e.one("pi", "mcp")
	wantAction(t, r, "manual")
	if !strings.Contains(r.Detail, "agent-monitor") {
		t.Fatalf("snippet = %q", r.Detail)
	}
	e.write(".pi/agent/mcp.json", "[1]", 0o644)
	wantAction(t, e.one("pi", "mcp"), "manual")
}

func TestOpencodeCommandArray(t *testing.T) {
	e := newWenv(t)
	doc := "{\n  \"$schema\": \"https://opencode.ai/config.json\",\n  \"permission\": {\n    \"read\": {\"a\": \"allow\"}\n  }\n}\n"
	e.write(".config/opencode/opencode.json", doc, 0o644)
	wantAction(t, e.one("opencode", "mcp"), "added")
	got := e.read(".config/opencode/opencode.json")
	var m map[string]any
	if err := json.Unmarshal([]byte(got), &m); err != nil {
		t.Fatal(err)
	}
	ent := m["mcp"].(map[string]any)["agent-monitor"].(map[string]any)
	cmd := ent["command"].([]any)
	if ent["type"] != "local" || len(cmd) != 2 || cmd[1] != "mcp-perm-server" {
		t.Fatalf("entry = %v", ent)
	}
	wantAction(t, e.one("opencode", "mcp"), "unchanged")
	e.unwire("opencode")
	if e.read(".config/opencode/opencode.json") != doc {
		t.Fatalf("bytes not restored:\n%s", e.read(".config/opencode/opencode.json"))
	}
}

const claudeHooksFixture = `{
  "model": "x",
  "hooks": {
    "Notification": [
      {"matcher": "", "hooks": [{"type": "command", "command": "%[1]s/agent-monitor/bin/claude-hook.sh", "timeout": 2000}]}
    ],
    "SessionStart": [
      {"matcher": "", "hooks": [{"type": "command", "command": "tool status"}, {"type": "command", "command": "/old/agent-monitor-x/bin/claude-hook.sh", "timeout": 2000}]}
    ],
    "Stop": [
      {"matcher": "", "hooks": [{"type": "command", "command": "'%[1]s/agent-monitor/bin/claude-hook.sh'"}]},
      {"matcher": "", "hooks": [{"type": "command", "command": "/usr/bin/python3 /h/tab.py save", "timeout": 5}]}
    ],
    "PermissionRequest": [
      {"matcher": "", "hooks": [{"type": "http", "url": "http://127.0.0.1:7777/api/approvals/hook/claude-permission"}]}
    ]
  }
}
`

func TestClaudeHooksDedupeByScriptPath(t *testing.T) {
	e := newWenv(t)
	doc := strings.ReplaceAll(claudeHooksFixture, "%[1]s", e.home)
	e.write(".claude/settings.json", doc, 0o644)
	r := e.one("claude", "hook")
	wantAction(t, r, "added")
	for _, en := range r.Entries {
		if en == "Stop: added" || en == "SessionStart: added" || en == "Notification: added" {
			t.Fatalf("existing hooks were added again: %v", r.Entries)
		}
	}
	got := e.read(".claude/settings.json")
	if n := strings.Count(got, "claude-hook.sh"); n != 3+3 {
		t.Fatalf("claude-hook.sh occurs %d times, want 6:\n%s", n, got)
	}
	if !json.Valid([]byte(got)) || !strings.Contains(got, `"PermissionRequest"`) {
		t.Fatal("settings broken")
	}
	wantAction(t, e.one("claude", "hook"), "unchanged")
	e.unwire("claude")
	if e.read(".claude/settings.json") != doc {
		t.Fatalf("bytes not restored:\n%s", e.read(".claude/settings.json"))
	}
	for _, en := range loadLedger().Entries {
		if en.Action == "added" && !en.Removed {
			t.Fatalf("unwire must leave only adopted rows and tombstones: %+v", en)
		}
	}
}

func TestClaudeHooksNoHooksKeyAndUnwire(t *testing.T) {
	e := newWenv(t)
	doc := "{\n  \"model\": \"x\"\n}\n"
	e.write(".claude/settings.json", doc, 0o600)
	wantAction(t, e.one("claude", "hook"), "added")
	if n := strings.Count(e.read(".claude/settings.json"), "claude-hook.sh"); n != 6 {
		t.Fatalf("hooks = %d", n)
	}
	e.unwire("claude")
	if e.read(".claude/settings.json") != doc {
		t.Fatalf("bytes not restored:\n%s", e.read(".claude/settings.json"))
	}
}

func TestCodexHooksNeedTrustAndLeaveConfigAlone(t *testing.T) {
	e := newWenv(t)
	cfg := "[hooks.state.\"x:session_start:0:0\"]\ntrusted_hash = \"sha256:abc\"\n"
	e.write(".codex/config.toml", cfg, 0o600)
	doc := "{\n  \"hooks\": {\n    \"Stop\": [\n      {\"hooks\": [{\"command\": \"/x/agent-monitor/bin/codex-hook.sh\", \"timeout\": 2000, \"type\": \"command\"}]}\n    ]\n  }\n}\n"
	e.write(".codex/hooks.json", doc, 0o644)
	r := e.one("codex", "hook")
	wantAction(t, r, "needs trust in Codex")
	if e.read(".codex/config.toml") != cfg {
		t.Fatal("the writer must never touch trusted_hash")
	}
	got := e.read(".codex/hooks.json")
	if n := strings.Count(got, "codex-hook.sh"); n != 6 {
		t.Fatalf("codex-hook.sh occurs %d times, want 6:\n%s", n, got)
	}
	if !strings.HasPrefix(got, "{\n  \"hooks\": {\n    \"Stop\": [\n      {\"hooks\": [{\"command\": \"/x/agent-monitor/bin/codex-hook.sh\", \"timeout\": 2000, \"type\": \"command\"}]}\n    ],") {
		t.Fatalf("existing entries were reordered or rewritten:\n%s", got)
	}
	e.unwire("codex")
	if e.read(".codex/hooks.json") != doc {
		t.Fatalf("bytes not restored:\n%s", e.read(".codex/hooks.json"))
	}
}

func TestCursorHooksCreateFileAndUnwireDeletesIt(t *testing.T) {
	e := newWenv(t)
	e.write(".cursor/.keep", "", 0o644)
	wantAction(t, e.one("cursor-agent", "hook"), "added")
	got := e.read(".cursor/hooks.json")
	var m struct {
		Version int
		Hooks   map[string][]struct {
			Command string
			Timeout int
		}
	}
	if err := json.Unmarshal([]byte(got), &m); err != nil || m.Version != 1 {
		t.Fatalf("hooks.json = %v\n%s", err, got)
	}
	if len(m.Hooks["beforeShellExecution"]) != 1 || m.Hooks["beforeMCPExecution"][0].Timeout != 180 {
		t.Fatalf("hooks = %+v", m.Hooks)
	}
	e.unwire("cursor-agent")
	if e.read(".cursor/hooks.json") != "<missing>" {
		t.Fatal("a created hooks file must be deleted by unwire")
	}
}

func TestCursorHooksKeepForeignEntries(t *testing.T) {
	e := newWenv(t)
	doc := "{\n  \"version\": 1,\n  \"hooks\": {\n    \"beforeShellExecution\": [\n      {\"command\": \"/usr/bin/python3 adapter.py\", \"timeout\": 25},\n      {\"command\": \"" + e.inst + "/bin/cursor-hook.sh\", \"timeout\": 180}\n    ]\n  }\n}\n"
	e.write(".cursor/hooks.json", doc, 0o644)
	r := e.one("cursor-agent", "hook")
	wantAction(t, r, "added")
	got := e.read(".cursor/hooks.json")
	if strings.Count(got, "cursor-hook.sh") != 2 || !strings.Contains(got, "adapter.py") {
		t.Fatalf("hooks.json:\n%s", got)
	}
	e.unwire("cursor-agent")
	if e.read(".cursor/hooks.json") != doc {
		t.Fatalf("bytes not restored:\n%s", e.read(".cursor/hooks.json"))
	}
}

func TestHookScriptMissingIsSkipped(t *testing.T) {
	e := newWenv(t)
	_ = os.Remove(filepath.Join(e.inst, "bin", "claude-hook.sh"))
	e.write(".claude/settings.json", "{}", 0o644)
	if r := e.one("claude", "hook"); !strings.HasPrefix(r.Action, "skipped:") {
		t.Fatalf("action = %q", r.Action)
	}
}

const codexTomlFixture = `model = "x"

[projects."%[1]s/agent-monitor"]
trust_level = "trusted"

[mcp_servers.other]
command = "tool"
`

func TestCodexTomlAppendOnlyAndRestores(t *testing.T) {
	e := newWenv(t)
	doc := strings.ReplaceAll(codexTomlFixture, "%[1]s", e.home)
	e.write(".codex/config.toml", doc, 0o600)
	r := e.one("codex", "mcp")
	wantAction(t, r, "added")
	got := e.read(".codex/config.toml")
	if !strings.HasPrefix(got, doc) || !strings.Contains(got, "[mcp_servers.agent-monitor]\ncommand = ") {
		t.Fatalf("not appended:\n%s", got)
	}
	if st, _ := os.Stat(filepath.Join(e.home, ".codex/config.toml")); st.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v", st.Mode())
	}
	wantAction(t, e.one("codex", "mcp"), "unchanged")
	e.unwire("codex")
	if e.read(".codex/config.toml") != doc {
		t.Fatalf("bytes not restored:\n%s", e.read(".codex/config.toml"))
	}
}

func TestCodexTomlPrefersCLIWhenPresent(t *testing.T) {
	e := newWenv(t)
	e.write(".codex/config.toml", "model = \"x\"\n", 0o600)
	e.write(".local/bin/codex", "#!/bin/sh\n", 0o755)
	wireRun = func(bin string, args ...string) error {
		e.calls = append(e.calls, append([]string{filepath.Base(bin)}, args...))
		e.write(".codex/config.toml", "model = \"x\"\n\n[mcp_servers.agent-monitor]\ncommand = \"c\"\n", 0o600)
		return nil
	}
	r := e.one("codex", "mcp")
	wantAction(t, r, "added")
	if !r.Unverified || len(e.calls) != 1 || strings.Join(e.calls[0], " ") != "codex mcp add agent-monitor -- "+filepath.Join(e.inst, "agent-monitor")+" mcp-perm-server" {
		t.Fatalf("calls = %v unverified=%v", e.calls, r.Unverified)
	}
	e.calls = nil
	e.unwire("codex")
	if len(e.calls) != 1 || strings.Join(e.calls[0], " ") != "codex mcp remove agent-monitor" {
		t.Fatalf("calls = %v", e.calls)
	}
}

func TestCodexTomlAdoptsExistingWithToolSubtables(t *testing.T) {
	e := newWenv(t)
	bin := filepath.Join(e.inst, "agent-monitor")
	doc := "[mcp_servers.agent-monitor]\ncommand = \"" + bin + "\"\nargs = [\"mcp-perm-server\"]\n\n[mcp_servers.agent-monitor.tools.list_agents]\napproval_mode = \"approve\"\n"
	e.write(".codex/config.toml", doc, 0o600)
	wantAction(t, e.one("codex", "mcp"), "adopted")
	if e.read(".codex/config.toml") != doc {
		t.Fatal("adoption changed the file")
	}
	e.write(".codex/config.toml", strings.Replace(doc, bin, "/elsewhere/agent-monitor", 1), 0o600)
	wantAction(t, e.one("codex", "mcp"), "skipped: user-edited")
}

func TestCodexTomlNameElsewhereIsManual(t *testing.T) {
	e := newWenv(t)
	for _, doc := range []string{
		"agent-monitor = { command = \"x\" }\n",
		"mcp_servers = { other = { command = \"x\" } }\n",
		"[mcp_servers.agent-monitor.env]\nA = \"b\"\n",
		"[mcp_servers.agent-monitor]\ncommand = [\n",
		"[mcp_servers.agent-monitor] # mine\ncommand = \"x\"\n",
		"[mcp_servers.'agent-monitor']\ncommand = \"x\"\n",
		"mcp_servers.agent-monitor = { command = \"/x\" }\n",
		"[mcp_servers]\n'agent-monitor' = { command = \"/x\" }\n",
	} {
		e.write(".codex/config.toml", doc, 0o644)
		r := e.one("codex", "mcp")
		wantAction(t, r, "manual")
		if !strings.Contains(r.Detail, "[mcp_servers.agent-monitor]") || e.read(".codex/config.toml") != doc {
			t.Fatalf("doc %q: detail %q file %q", doc, r.Detail, e.read(".codex/config.toml"))
		}
	}
}

func TestHermesYAMLAppendAndPresent(t *testing.T) {
	e := newWenv(t)
	doc := "model:\n  name: x\nagent:\n  max: 3\n"
	e.write(".hermes/config.yaml", doc, 0o644)
	wantAction(t, e.one("hermes", "mcp"), "added")
	got := e.read(".hermes/config.yaml")
	if !strings.HasPrefix(got, doc) || !strings.Contains(got, "mcp_servers:\n  agent-monitor:\n    command: ") {
		t.Fatalf("yaml:\n%s", got)
	}
	wantAction(t, e.one("hermes", "mcp"), "unchanged")
	e.unwire("hermes")
	if e.read(".hermes/config.yaml") != doc {
		t.Fatalf("bytes not restored:\n%s", e.read(".hermes/config.yaml"))
	}
	e.write(".hermes/config.yaml", doc+"mcp_servers:\n  other:\n    command: tool\n", 0o644)
	r := e.one("hermes", "mcp")
	wantAction(t, r, "manual")
	bin := filepath.Join(e.inst, "agent-monitor")
	same := doc + "mcp_servers:\n  agent-monitor:\n    command: \"" + bin + "\"\n    args:\n      - mcp-perm-server\n"
	e.write(".hermes/config.yaml", same, 0o644)
	wantAction(t, e.one("hermes", "mcp"), "adopted")
	e.write(".hermes/config.yaml", strings.Replace(same, bin, "/other/x", 1), 0o644)
	wantAction(t, e.one("hermes", "mcp"), "skipped: user-edited")
}

func TestClaudeMCPUsesCLIOnly(t *testing.T) {
	e := newWenv(t)
	cj := "{\n  \"numStartups\": 3,\n  \"mcpServers\": {\n    \"drawio\": {\"command\": \"npx\"}\n  }\n}\n"
	e.write(".claude.json", cj, 0o600)
	r := e.one("claude", "mcp")
	if !strings.HasPrefix(r.Action, "skipped:") || e.read(".claude.json") != cj {
		t.Fatalf("no CLI: action %q", r.Action)
	}
	e.write(".local/bin/claude", "#!/bin/sh\n", 0o755)
	wireRun = func(bin string, args ...string) error {
		e.calls = append(e.calls, append([]string{filepath.Base(bin)}, args...))
		e.write(".claude.json", strings.Replace(cj, "\"drawio\"", "\"agent-monitor\": {\"command\": \"c\"},\n    \"drawio\"", 1), 0o600)
		return nil
	}
	wantAction(t, e.one("claude", "mcp"), "added")
	want := "claude mcp add --scope user agent-monitor -- " + filepath.Join(e.inst, "agent-monitor") + " mcp-perm-server"
	if len(e.calls) != 1 || strings.Join(e.calls[0], " ") != want {
		t.Fatalf("calls = %v", e.calls)
	}
	if _, err := os.Stat(filepath.Join(e.home, ".claude.json.bak-am-"+time.Now().UTC().Format("2006-01-02"))); err != nil {
		t.Fatalf("no backup: %v", err)
	}
	e.calls = nil
	e.unwire("claude")
	if len(e.calls) != 1 || strings.Join(e.calls[0], " ") != "claude mcp remove agent-monitor --scope user" {
		t.Fatalf("calls = %v", e.calls)
	}
}

func TestClaudeMCPAdoptsExisting(t *testing.T) {
	e := newWenv(t)
	cj := "{\"mcpServers\":{\"agent-monitor\":{\"command\":\"" + filepath.Join(e.inst, "agent-monitor") + "\",\"args\":[\"mcp-perm-server\"]}}}"
	e.write(".claude.json", cj, 0o600)
	wantAction(t, e.one("claude", "mcp"), "adopted")
	if len(e.calls) != 0 || e.read(".claude.json") != cj {
		t.Fatal("adoption must not run the CLI or write the file")
	}
}

func TestBackupModeDailyAndKeepFive(t *testing.T) {
	e := newWenv(t)
	p := filepath.Join(e.home, ".pi/agent/mcp.json")
	e.write(".pi/agent/mcp.json", piFixture, 0o600)
	for i := 1; i <= 7; i++ {
		_ = os.WriteFile(p+".bak-am-2020-01-0"+string(rune('0'+i)), []byte("old"), 0o600)
	}
	_ = os.WriteFile(p+".bak-2026-01-01", []byte("legacy"), 0o644)
	wantAction(t, e.one("pi", "mcp"), "added")
	today := p + ".bak-am-" + time.Now().UTC().Format("2006-01-02")
	st, err := os.Stat(today)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("backup: %v %v", err, st)
	}
	if b, _ := os.ReadFile(today); string(b) != piFixture {
		t.Fatal("backup does not hold the original")
	}
	all, _ := filepath.Glob(p + ".bak-am-*")
	if len(all) != 5 {
		t.Fatalf("kept %d backups: %v", len(all), all)
	}
	if _, err := os.Stat(p + ".bak-2026-01-01"); err != nil {
		t.Fatal("old installer backups must stay")
	}
	if got := legacyBackups(p); len(got) != 1 {
		t.Fatalf("legacy = %v", got)
	}
	if st, _ := os.Stat(p); st.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v", st.Mode())
	}
	e.unwire("pi")
	all, _ = filepath.Glob(p + ".bak-am-*")
	if len(all) != 5 {
		t.Fatalf("a second write the same day must reuse today's backup: %v", all)
	}
}

func TestFileChangedDuringWrite(t *testing.T) {
	e := newWenv(t)
	p := filepath.Join(e.home, ".pi/agent/mcp.json")
	e.write(".pi/agent/mcp.json", piFixture, 0o644)
	n := 0
	wireBeforeRename = func() {
		n++
		if n == 1 {
			_ = os.WriteFile(p, []byte(strings.Replace(piFixture, "deferred", "direct", 1)), 0o644)
			future := time.Now().Add(time.Hour)
			_ = os.Chtimes(p, future, future)
		}
	}
	wantAction(t, e.one("pi", "mcp"), "added")
	if got := e.read(".pi/agent/mcp.json"); !strings.Contains(got, "\"exposure\": \"direct\"\n    }") || !strings.Contains(got, "agent-monitor") {
		t.Fatalf("the retry must keep the concurrent change:\n%s", got)
	}
	e.unwire("pi")
	e.write(".pi/agent/mcp.json", piFixture, 0o644)
	_ = os.Remove(ledgerPath())
	c := 0
	wireBeforeRename = func() {
		c++
		fut := time.Now().Add(time.Duration(c) * time.Hour)
		_ = os.WriteFile(p, []byte(piFixture+" "), 0o644)
		_ = os.Chtimes(p, fut, fut)
	}
	wantAction(t, e.one("pi", "mcp"), "skipped: changed during write")
}

func TestAdoptionNeedsNoCardAndAddNeedsConfirmation(t *testing.T) {
	e := newWenv(t)
	bin := filepath.Join(e.inst, "agent-monitor")
	adopted := "{\"mcpServers\":{\"agent-monitor\":{\"command\":\"" + bin + "\",\"args\":[\"mcp-perm-server\"]}}}"
	e.write(".pi/agent/mcp.json", adopted, 0o600)
	wireTick()
	wireState.Lock()
	pend := wireState.Pending
	wireState.Unlock()
	for _, p := range pend {
		for _, it := range p.Items {
			if it.Agent == "pi" {
				t.Fatalf("an adopted entry opened a card: %+v", it)
			}
		}
	}
	if e.read(".pi/agent/mcp.json") != adopted {
		t.Fatal("adoption wrote the agent file")
	}
	found := false
	for _, le := range loadLedger().Entries {
		found = found || (le.Agent == "pi" && le.Action == "adopted")
	}
	if !found {
		t.Fatal("adoption must be recorded in the ledger")
	}

	e.write(".pi/agent/mcp.json", piFixture, 0o600)
	_ = os.Remove(ledgerPath())
	wireTick()
	if e.read(".pi/agent/mcp.json") != piFixture {
		t.Fatal("the daemon wrote a real file before the admin tapped the card")
	}
	wireState.Lock()
	pend = wireState.Pending
	wireState.Unlock()
	var card *wirePending
	for i := range pend {
		if pend[i].Kind == "mcp" {
			card = &pend[i]
		}
	}
	if card == nil {
		t.Fatalf("no mcp card: %+v", pend)
	}
	hit := false
	for _, it := range card.Items {
		if it.Agent == "pi" && it.Action == "added" && it.File == "~/.pi/agent/mcp.json" {
			hit = true
		}
	}
	if !hit {
		t.Fatalf("the card does not list the pi file: %+v", card.Items)
	}
	if err := setWireMode("mcp", "on"); err != nil {
		t.Fatal(err)
	}
	wireTick()
	if !strings.Contains(e.read(".pi/agent/mcp.json"), "agent-monitor") {
		t.Fatal("approved kind was not written")
	}
	if err := setWireMode("mcp", "maybe"); err == nil {
		t.Fatal("bad mode accepted")
	}
	if err := setWireMode("bogus", "on"); err == nil {
		t.Fatal("bad kind accepted")
	}
}

func TestOffKindNeverWrites(t *testing.T) {
	e := newWenv(t)
	e.write(".pi/agent/mcp.json", piFixture, 0o600)
	if err := setWireMode("mcp", "off"); err != nil {
		t.Fatal(err)
	}
	wireTick()
	if e.read(".pi/agent/mcp.json") != piFixture {
		t.Fatal("off must not write")
	}
	wireState.Lock()
	pend := wireState.Pending
	wireState.Unlock()
	for _, p := range pend {
		if p.Kind == "mcp" {
			t.Fatal("off must not ask again")
		}
	}
}

func TestWireOnlyRunsFromInstallDir(t *testing.T) {
	e := newWenv(t)
	e.write(".pi/agent/mcp.json", piFixture, 0o600)
	_ = setWireMode("mcp", "on")
	wireExe = func() (string, error) { return filepath.Join(e.home, "am-build", "agent-monitor"), nil }
	_ = os.MkdirAll(filepath.Join(e.home, "am-build"), 0o755)
	if out := wireTick(); out != nil {
		t.Fatalf("test daemon wired: %+v", out)
	}
	if e.read(".pi/agent/mcp.json") != piFixture {
		t.Fatal("a daemon outside installDir wrote a file")
	}
	if ok, why := wireActive(); ok || !strings.Contains(why, "not in") {
		t.Fatalf("active = %v %q", ok, why)
	}
}

func TestNotInstalledAgentsAreSkippedAndOptionalNeedBinary(t *testing.T) {
	e := newWenv(t)
	e.write(".gemini/settings.json", "{}", 0o644)
	var gem, pi *wireResult
	rs := wireAll(true)
	for i := range rs {
		switch rs[i].Agent {
		case "gemini":
			gem = &rs[i]
		case "pi":
			pi = &rs[i]
		}
	}
	if gem == nil || gem.Action != "skipped: not installed" || pi == nil || pi.Action != "skipped: not installed" {
		t.Fatalf("results = %+v", rs)
	}
	e.write(".local/bin/gemini", "#!/bin/sh\n", 0o755)
	e.write(".pi/agent/mcp.json", piFixture, 0o644)
	for _, r := range wireAll(true) {
		if r.Agent == "gemini" && (r.Action != "added" || !r.Dry || !r.Unverified) {
			t.Fatalf("gemini = %+v", r)
		}
		if r.Agent == "pi" && (r.Action != "added" || !r.Dry) {
			t.Fatalf("pi = %+v", r)
		}
	}
	if e.read(".pi/agent/mcp.json") != piFixture || e.read(".gemini/settings.json") != "{}" {
		t.Fatal("a dry run wrote a file")
	}
	if _, err := os.Stat(ledgerPath()); err == nil {
		t.Fatal("a dry run wrote the ledger")
	}
}

func TestDryRunCardListsExactFilesAndEntries(t *testing.T) {
	e := newWenv(t)
	e.write(".claude/settings.json", "{}", 0o644)
	e.write(".cursor/hooks.json", "{\"version\":1,\"hooks\":{}}", 0o644)
	var claudeHook, cursorHook *wireResult
	rs := wireAll(true)
	for i := range rs {
		if rs[i].Kind == "hook" && rs[i].Agent == "claude" {
			claudeHook = &rs[i]
		}
		if rs[i].Kind == "hook" && rs[i].Agent == "cursor-agent" {
			cursorHook = &rs[i]
		}
	}
	if claudeHook == nil || claudeHook.File != "~/.claude/settings.json" || len(claudeHook.Entries) != 6 || claudeHook.Entries[0] != "Notification: added" {
		t.Fatalf("claude hook = %+v", claudeHook)
	}
	if cursorHook == nil || cursorHook.File != "~/.cursor/hooks.json" || len(cursorHook.Entries) != 2 {
		t.Fatalf("cursor hook = %+v", cursorHook)
	}
	if e.read(".claude/settings.json") != "{}" {
		t.Fatal("dry run wrote")
	}
}

func TestRouteLevelWiring(t *testing.T) {
	if got := routeLevel("POST", "/api/agents/wiring/confirm"); got != LevelAdmin {
		t.Fatalf("confirm needs admin, got %s", got)
	}
	listed := false
	for _, r := range accessRules {
		if r.prefix == "/api/agents/wiring/confirm$" && r.level == LevelAdmin {
			listed = true
		}
	}
	if !listed {
		t.Fatal("the confirm route needs its own admin row, not the default")
	}
	if got := routeLevel("GET", "/api/agents/wiring"); got != LevelControl {
		t.Fatalf("wiring read = %s", got)
	}
}

func TestJSONScannerSpans(t *testing.T) {
	src := []byte(" {\"a\": [1, {\"b\": \"x\\\"y\"}], \"c\": null }\n")
	n, err := parseJSONSpans(src)
	if err != nil || n.kind != '{' || len(n.keys) != 2 {
		t.Fatalf("parse: %v %+v", err, n)
	}
	if got := string(src[n.get("a").start:n.get("a").end]); got != "[1, {\"b\": \"x\\\"y\"}]" {
		t.Fatalf("span = %q", got)
	}
	for _, bad := range []string{"{", "{\"a\":}", "{\"a\":1,}", "// c\n{}", "{} x", "{'a':1}"} {
		if _, err := parseJSONSpans([]byte(bad)); err == nil {
			t.Fatalf("%q parsed", bad)
		}
	}
}

func TestUnwireIsNotUndoneByTheNextTick(t *testing.T) {
	e := newWenv(t)
	e.write(".pi/agent/mcp.json", piFixture, 0o600)
	wantAction(t, e.one("pi", "mcp"), "added")
	e.unwire("pi")
	wantAction(t, e.one("pi", "mcp"), "removed by user")
	if e.read(".pi/agent/mcp.json") != piFixture {
		t.Fatal("the next run added the entry back after unwire")
	}
}

func TestReaddedByHandAfterTombstoneIsAdopted(t *testing.T) {
	e := newWenv(t)
	e.write(".pi/agent/mcp.json", piFixture, 0o600)
	wantAction(t, e.one("pi", "mcp"), "added")
	withEntry := e.read(".pi/agent/mcp.json")
	e.write(".pi/agent/mcp.json", piFixture, 0o600)
	wantAction(t, e.one("pi", "mcp"), "removed by user")
	e.write(".pi/agent/mcp.json", withEntry, 0o600)
	wantAction(t, e.one("pi", "mcp"), "adopted")
	e.unwire("pi")
	if e.read(".pi/agent/mcp.json") != withEntry {
		t.Fatal("unwire removed an entry the user added by hand")
	}
}

func TestMissingBinaryWritesNothing(t *testing.T) {
	e := newWenv(t)
	e.write(".pi/agent/mcp.json", piFixture, 0o600)
	if err := os.Remove(filepath.Join(e.inst, "agent-monitor")); err != nil {
		t.Fatal(err)
	}
	r := newWireCtx(false, nil).runAll()
	if len(r) != 1 || !strings.HasPrefix(r[0].Action, "skipped: agent-monitor not found") {
		t.Fatalf("results = %+v", r)
	}
	if e.read(".pi/agent/mcp.json") != piFixture {
		t.Fatal("wrote an entry for a missing binary")
	}
}

func TestHookCmdIsExpandsHome(t *testing.T) {
	e := newWenv(t)
	if !hookCmdIs("$HOME/am/bin/codex-hook.sh", "codex-hook.sh", filepath.Join(e.inst, "bin", "codex-hook.sh")) {
		t.Fatal("a $HOME path to our script must count as ours")
	}
	if hookCmdIs("/usr/bin/other.sh", "codex-hook.sh", filepath.Join(e.inst, "bin", "codex-hook.sh")) {
		t.Fatal("a foreign script matched")
	}
}
