package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// installAll wires MCP and hook entries through the shared writers, then runs the two manual permission installers.
// Running install is your decision, so no confirmation card is needed.
func installAll() {
	fmt.Println("agent-monitor install: wiring into your agent CLIs")
	fmt.Println()
	printWire(wireAll(false))
	fmt.Println()
	installClaudePermissions()
	fmt.Println()
	installCodexApproval()
	fmt.Println()
	fmt.Println("Done. Restart your agents INSIDE tmux for the changes to take effect:")
	fmt.Println("    tmux new -s work")
	fmt.Println("    agent-monitor run claude       # or codex / cursor-agent / opencode")
	fmt.Println()
	fmt.Println("Once running, agents can call MCP tools natively:")
	fmt.Println("    who_am_i, list_agents, read_pane, talk_to_agent")
}

// installClaudePermissions seeds ~/.claude/settings.json with permissions.allow
// entries for every agent-monitor MCP tool. Without these, Claude prompts the
// user before each tool call (talk_to_agent / reply_to_talk / read_pane) — which
// is fine interactively but breaks scripted demos and noticeably slows down
// agent-to-agent flows.
//
// Idempotent: if all entries are already present, nothing changes.
func installClaudePermissions() {
	settingsPath := filepath.Join(homeDir(), ".claude", "settings.json")
	if !pathExists(settingsPath) {
		fmt.Fprintf(os.Stderr, "✗ skip Claude permissions: %s not found\n", settingsPath)
		return
	}
	raw, err := os.ReadFile(settingsPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "✗ Claude permissions: read failed: %v\n", err)
		return
	}
	var settings map[string]any
	if err := json.Unmarshal(raw, &settings); err != nil {
		fmt.Fprintf(os.Stderr, "✗ Claude permissions: parse failed: %v\n", err)
		return
	}
	if settings == nil {
		settings = map[string]any{}
	}
	perms, _ := settings["permissions"].(map[string]any)
	if perms == nil {
		perms = map[string]any{}
		settings["permissions"] = perms
	}
	allow, _ := perms["allow"].([]any)

	// Only our own MCP tools: allowing Bash or Edit here would skip every approval card.
	want := []string{"mcp__agent-monitor"}
	added := 0
	for _, w := range want {
		present := false
		for _, e := range allow {
			if s, ok := e.(string); ok && s == w {
				present = true
				break
			}
		}
		if !present {
			allow = append(allow, w)
			added++
		}
	}
	perms["allow"] = allow

	if added == 0 {
		fmt.Printf("✓ Claude permissions already include agent-monitor tools (no change to %s)\n", settingsPath)
		return
	}
	out, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "✗ Claude permissions: marshal failed: %v\n", err)
		return
	}
	tmp := settingsPath + ".am-tmp"
	err = os.WriteFile(tmp, append(out, '\n'), 0o644)
	if err == nil {
		err = os.Rename(tmp, settingsPath)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "✗ Claude permissions: write failed: %v\n", err)
		return
	}
	fmt.Printf("✓ Patched %s — Claude auto-allows agent-monitor MCP tools (no more prompts)\n", settingsPath)
}

// installCodexApproval flips approval_mode from "approve" (codex's default,
// which prompts) to "auto" (auto-allow without asking) for the agent-monitor
// MCP tools. Codex's valid values are auto | prompt | approve. We previously
// tried "never" — codex rejects it with `unknown variant`.
//
// Tiny string-level edit (no TOML parser dep). Idempotent.
func installCodexApproval() {
	cfgPath := filepath.Join(homeDir(), ".codex", "config.toml")
	if !pathExists(cfgPath) {
		fmt.Printf("• Codex not detected (%s missing) — skipping\n", cfgPath)
		return
	}
	body, err := os.ReadFile(cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "✗ Codex approval: read failed: %v\n", err)
		return
	}
	src := string(body)
	if !strings.Contains(src, "[mcp_servers.agent-monitor.tools.") {
		fmt.Printf("• Codex agent-monitor tool blocks not present yet — skipping (run install again after first MCP call)\n")
		return
	}
	// Replace any approve/on_request/untrusted/on_failure under our blocks
	// with `never`. We only want this to affect lines inside our tool sections,
	// so we walk line-by-line and track the current section header.
	lines := strings.Split(src, "\n")
	inOurBlock := false
	changed := 0
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "[") {
			inOurBlock = strings.HasPrefix(trimmed, "[mcp_servers.agent-monitor.tools.")
			continue
		}
		if !inOurBlock {
			continue
		}
		if strings.HasPrefix(trimmed, "approval_mode") && !strings.Contains(line, `"auto"`) {
			lines[i] = `approval_mode = "auto"`
			changed++
		}
	}
	if changed == 0 {
		fmt.Printf("✓ Codex tools already auto-approve (no change to %s)\n", cfgPath)
		return
	}
	stamp := time.Now().UTC().Format("2006-01-02")
	backup := fmt.Sprintf("%s.bak-%s", cfgPath, stamp)
	if !pathExists(backup) {
		_ = os.WriteFile(backup, body, 0o644)
	}
	out := strings.Join(lines, "\n")
	if err := os.WriteFile(cfgPath, []byte(out), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "✗ Codex approval: write failed: %v\n", err)
		return
	}
	fmt.Printf("✓ Patched %s — agent-monitor tools auto-approve in Codex (%d entries)\n", cfgPath, changed)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}
