package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

func (c *wireCtx) cliAt(name string) string {
	p := filepath.Join(c.home, ".local", "bin", name)
	if st, err := os.Stat(p); err == nil && !st.IsDir() {
		return p
	}
	return ""
}

func mcpSnapshot(b []byte) map[string]string {
	out := map[string]string{}
	root, err := parseJSONSpans(b)
	if err != nil {
		return out
	}
	if m := root.get("mcpServers"); m != nil && m.kind == '{' {
		for i, k := range m.keys {
			out[k] = string(b[m.vals[i].start:m.vals[i].end])
		}
	}
	return out
}

// claudeMCPStep reads ~/.claude.json but writes only through the claude CLI, because live sessions rewrite that file.
func (c *wireCtx) claudeMCPStep() wireResult {
	path := filepath.Join(c.home, ".claude.json")
	res := c.res("claude", "mcp", path)
	cmd, args := c.bin(), mcpArgs
	snippet := "run: claude mcp add --scope user " + wireName + " -- " + cmd + " " + strings.Join(args, " ")
	raw, _ := os.ReadFile(path)
	prev := c.led.find("claude", res.File, wireName)
	if c.force["claude"] && prev != nil && prev.Removed {
		c.dropLedger("claude", res.File, wireName)
		prev = nil
	}
	if len(raw) > 0 {
		root, err := parseJSONSpans(raw)
		if err != nil || root.kind != '{' {
			res.Action, res.Detail = "manual", snippet
			return res
		}
		if ent := root.at([]string{"mcpServers", wireName}); ent != nil {
			ec, ea, ok := "", []string(nil), ent.kind == '{'
			if ok {
				ec, ea, ok = readCommandArgs(raw, ent)
			}
			if !ok || !c.sameCmd(ec, ea, cmd, args) {
				return skipped(res, "user-edited")
			}
			return c.adopt(res, prev, wireName, "mcp")
		}
	}
	if prev != nil {
		return c.tombstone(res, prev)
	}
	res.Action = "added"
	res.Detail = snippet
	if !c.allow("mcp") {
		return c.held(res)
	}
	if c.dry {
		return res
	}
	cli := c.cliAt("claude")
	if cli == "" {
		res.Detail = ""
		return skipped(res, "claude CLI not found at ~/.local/bin/claude")
	}
	if len(raw) > 0 {
		c.backupOnce(path, raw, modeOf(path))
	}
	before := mcpSnapshot(raw)
	if err := wireRun(cli, "mcp", "add", "--scope", "user", wireName, "--", cmd, args[0]); err != nil {
		res.Detail = ""
		return c.failed(res, err, snippet)
	}
	after, _ := os.ReadFile(path)
	snap := mcpSnapshot(after)
	if _, ok := snap[wireName]; !ok {
		return c.failed(res, errors.New("entry missing after claude mcp add"), snippet)
	}
	for k, v := range before {
		if snap[k] != v {
			return c.failed(res, errors.New("claude mcp add changed "+k), snippet)
		}
	}
	c.setLedger(ledgerEntry{Agent: "claude", Kind: "mcp", File: res.File, ID: wireName, Action: "added", Via: "cli"})
	res.Detail = ""
	c.restartFlag(&res)
	return res
}

// textMCP describes an agent whose MCP entry lives in a TOML or YAML file that we only append to.
type textMCP struct {
	agent      string
	file       string
	entry      func(src string) (found bool, cmd string, args []string, ok bool)
	elsewhere  func(src string) bool // our name or a conflicting table is already there
	block      func(cmd string, args []string) string
	cliName    string // CLI that adds the entry, tried first
	unverified bool
}

func (c *wireCtx) textMCPStep(s textMCP) wireResult {
	path := filepath.Join(c.home, s.file)
	res := c.res(s.agent, "mcp", path)
	res.Unverified = s.unverified
	cmd, args := c.bin(), mcpArgs
	block := s.block(cmd, args)
	snippet := "add to " + c.rel(path) + ":\n" + block
	raw, rerr := os.ReadFile(path)
	exists := rerr == nil
	src := string(raw)
	prev := c.led.find(s.agent, res.File, wireName)
	if c.force[s.agent] && prev != nil && prev.Removed {
		c.dropLedger(s.agent, res.File, wireName)
		prev = nil
	}
	found, ec, ea, ok := s.entry(src)
	switch {
	case found && ok && c.sameCmd(ec, ea, cmd, args):
		return c.adopt(res, prev, wireName, "mcp")
	case found && ok:
		return skipped(res, "user-edited")
	case found || s.elsewhere(src):
		res.Action, res.Detail = "manual", snippet
		return res
	case prev != nil:
		return c.tombstone(res, prev)
	}
	res.Action = "added"
	if !c.allow("mcp") {
		return c.held(res)
	}
	if c.dry {
		return res
	}
	if cli := ""; s.cliName != "" {
		if cli = c.cliAt(s.cliName); cli != "" {
			if exists {
				c.backupOnce(path, raw, modeOf(path))
			}
			if err := wireRun(cli, "mcp", "add", wireName, "--", cmd, args[0]); err == nil {
				after, _ := os.ReadFile(path)
				if f, _, _, _ := s.entry(string(after)); f {
					c.setLedger(ledgerEntry{Agent: s.agent, Kind: "mcp", File: res.File, ID: wireName, Action: "added", Via: "cli"})
					c.restartFlag(&res)
					return res
				}
				res.Action, res.Detail = "manual", "the CLI reported success but the entry is not in the file; check it: "+snippet
				return res
			}
		}
	}
	var written string
	err := c.modify(path, func(old []byte, ex bool) ([]byte, error) {
		o := string(old)
		if !ex || strings.TrimSpace(o) == "" {
			written = block
			return []byte(block), nil
		}
		sep := ""
		if !strings.HasSuffix(o, "\n") {
			sep = "\n"
		}
		written = sep + "\n" + block
		return []byte(o + written), nil
	})
	if err != nil {
		return c.failed(res, err, snippet)
	}
	e := ledgerEntry{Agent: s.agent, Kind: "mcp", File: res.File, ID: wireName, Action: "added", Via: "append", Written: written}
	if !exists {
		e.Fresh = "absent"
	} else if strings.TrimSpace(src) == "" {
		e.Fresh, e.Orig = "empty", src
	}
	c.setLedger(e)
	c.restartFlag(&res)
	return res
}

// manualMCPStep is for an installed agent whose config shape we only know from its docs.
func (c *wireCtx) manualMCPStep(agent, file, snippet string) wireResult {
	res := c.res(agent, "mcp", filepath.Join(c.home, file))
	res.Action, res.Detail, res.Unverified = "manual", snippet, true
	return res
}
