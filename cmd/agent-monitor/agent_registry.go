package main

import (
	"os"
	"os/exec"
	"path/filepath"
)

type promptMatcher struct{ Keys map[string]string } // terminal prompt shape; filled in phase 5 to 7

type wireStep struct {
	Kind string // mcp | hook | extension
	Run  func(c *wireCtx) wireResult
}

type agentKind struct {
	ID         string
	Binaries   []string
	Dirs       []string // config dirs under home that show the agent is used here
	NeedBinary bool     // wire only when the binary exists, a config dir is not enough
	Wire       []wireStep
	Prompt     *promptMatcher
	ACP        *acpAgent
}

func mcpStep(s mcpJSON) wireStep {
	return wireStep{"mcp", func(c *wireCtx) wireResult { return c.mcpJSONStep(s) }}
}

func hookStep(s hookJSON) wireStep {
	return wireStep{"hook", func(c *wireCtx) wireResult { return c.hookStep(s) }}
}

func textStep(s textMCP) wireStep {
	return wireStep{"mcp", func(c *wireCtx) wireResult { return c.textMCPStep(s) }}
}

func manualStep(agent, file, snippet string) wireStep {
	return wireStep{"mcp", func(c *wireCtx) wireResult { return c.manualMCPStep(agent, file, snippet) }}
}

func bucketElem(timeout func(ev string) int) func(ev, cmd string) any {
	return func(ev, cmd string) any {
		return obj{{"matcher", ""}, {"hooks", []any{obj{{"type", "command"}, {"command", cmd}, {"timeout", timeout(ev)}}}}}
	}
}

var claudeEvents = []string{"Notification", "Stop", "SubagentStop", "SessionStart", "SessionEnd", "UserPromptSubmit"}

func plainMCP(agent, file string, path ...string) wireStep {
	return mcpStep(mcpJSON{agent: agent, file: file, path: path, unverified: true, build: buildCommandArgs, read: readCommandArgs})
}

var agentRegistry = []agentKind{
	{ID: "claude", Binaries: []string{"claude"}, Dirs: []string{".claude"}, Wire: []wireStep{
		{"mcp", func(c *wireCtx) wireResult { return c.claudeMCPStep() }},
		hookStep(hookJSON{agent: "claude", file: ".claude/settings.json", events: claudeEvents, script: "claude-hook.sh",
			elem: bucketElem(func(string) int { return 2000 }), fresh: "{\n}\n"}),
	}},
	{ID: "codex", Binaries: []string{"codex"}, Dirs: []string{".codex"}, Wire: []wireStep{
		textStep(textMCP{agent: "codex", file: ".codex/config.toml", entry: tomlEntry,
			elsewhere: tomlNamed, block: tomlMCPBlock, cliName: "codex", unverified: true}),
		hookStep(hookJSON{agent: "codex", file: ".codex/hooks.json", codex: true, script: "codex-hook.sh", fresh: "{\n}\n",
			events: append([]string{"PermissionRequest"}, claudeEvents[1:]...),
			elem: bucketElem(func(ev string) int {
				if ev == "PermissionRequest" {
					return 150
				}
				return 2000
			})}),
	}},
	{ID: "cursor-agent", Binaries: []string{"cursor-agent"}, Dirs: []string{".cursor"}, Wire: []wireStep{
		mcpStep(mcpJSON{agent: "cursor-agent", file: ".cursor/mcp.json", path: []string{"mcpServers"}, build: buildCommandArgs, read: readCommandArgs}),
		hookStep(hookJSON{agent: "cursor-agent", file: ".cursor/hooks.json", events: []string{"beforeShellExecution", "beforeMCPExecution"},
			script: "cursor-hook.sh", fresh: "{\n  \"version\": 1,\n  \"hooks\": {}\n}\n",
			elem: func(_, cmd string) any { return obj{{"command", cmd}, {"timeout", 180}} }}),
	}},
	{ID: "pi", Binaries: []string{"pi"}, Dirs: []string{".pi"}, Wire: []wireStep{
		mcpStep(mcpJSON{agent: "pi", file: ".pi/agent/mcp.json", path: []string{"mcpServers"}, read: readCommandArgs,
			build: func(cmd string, args []string) any {
				return obj{{"command", cmd}, {"args", args}, {"exposure", "direct"}}
			}}),
	}},
	{ID: "opencode", Binaries: []string{"opencode"}, Dirs: []string{".config/opencode"}, Wire: []wireStep{
		mcpStep(mcpJSON{agent: "opencode", file: ".config/opencode/opencode.json", path: []string{"mcp"}, read: readCommandArray, unverified: true,
			build: func(cmd string, args []string) any {
				return obj{{"type", "local"}, {"command", append([]string{cmd}, args...)}, {"enabled", true}}
			}}),
	}},
	{ID: "hermes", Binaries: []string{"hermes"}, Dirs: []string{".hermes"}, Wire: []wireStep{
		textStep(textMCP{agent: "hermes", file: ".hermes/config.yaml", entry: yamlEntry,
			elsewhere: func(s string) bool { return yamlTopRe.MatchString(s) }, block: yamlMCPBlock, unverified: true}),
	}},
	{ID: "gemini", Binaries: []string{"gemini"}, NeedBinary: true, Wire: []wireStep{plainMCP("gemini", ".gemini/settings.json", "mcpServers")}},
	{ID: "amp", Binaries: []string{"amp"}, NeedBinary: true, Wire: []wireStep{plainMCP("amp", ".config/amp/settings.json", "amp.mcpServers")}},
	{ID: "qwen", Binaries: []string{"qwen"}, NeedBinary: true, Wire: []wireStep{plainMCP("qwen", ".qwen/settings.json", "mcpServers")}},
	{ID: "kiro", Binaries: []string{"kiro-cli", "kiro"}, NeedBinary: true, Wire: []wireStep{plainMCP("kiro", ".kiro/settings/mcp.json", "mcpServers")}},
	{ID: "crush", Binaries: []string{"crush"}, NeedBinary: true, Wire: []wireStep{
		mcpStep(mcpJSON{agent: "crush", file: ".config/crush/crush.json", path: []string{"mcp"}, unverified: true, read: readCommandArgs,
			build: func(cmd string, args []string) any { return obj{{"type", "stdio"}, {"command", cmd}, {"args", args}} }}),
	}},
	{ID: "goose", Binaries: []string{"goose"}, NeedBinary: true, Wire: []wireStep{
		manualStep("goose", ".config/goose/config.yaml", "add an extension named "+wireName+" of type stdio with cmd <installDir>/agent-monitor and args [mcp-perm-server]"),
	}},
	{ID: "grok", Binaries: []string{"grok"}, NeedBinary: true, Wire: []wireStep{
		manualStep("grok", ".grok", "add an MCP server named "+wireName+" with command <installDir>/agent-monitor and args [mcp-perm-server]"),
	}},
}

// registryIDs lists every agent ID the registry knows, for rules and the helper.
func registryIDs() []string {
	ids := make([]string, len(agentRegistry))
	for i, k := range agentRegistry {
		ids[i] = k.ID
	}
	return ids
}

func isRegistryID(id string) bool {
	for _, k := range agentRegistry {
		if k.ID == id {
			return true
		}
	}
	return false
}

var wireSysBinDirs = []string{"/opt/homebrew/bin", "/usr/local/bin"}

var binDirs = []string{".local/bin", ".cargo/bin", "go/bin", ".bun/bin", ".npm-global/bin", ".opencode/bin"}

func (c *wireCtx) installed(k agentKind) bool {
	for _, b := range k.Binaries {
		if _, err := exec.LookPath(b); err == nil {
			return true
		}
		for _, d := range agentBinDirs(c.home, append(append([]string{}, wireSysBinDirs...), prefixAll(c.home, binDirs)...)...) {
			if st, err := os.Stat(filepath.Join(d, b)); err == nil && !st.IsDir() {
				return true
			}
		}
	}
	if k.NeedBinary {
		return false
	}
	for _, d := range k.Dirs {
		if st, err := os.Stat(filepath.Join(c.home, d)); err == nil && st.IsDir() {
			return true
		}
	}
	return false
}

func prefixAll(home string, l []string) []string {
	out := make([]string, len(l))
	for i, d := range l {
		out[i] = filepath.Join(home, d)
	}
	return out
}
