package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// cursorPolicy applies Cursor's own allow and deny lists (~/.cursor/cli-config.json) to a hook request:
// "allow" or "deny" when a list decides, "" when Cursor would ask the user.
func cursorPolicy(toolName string, input map[string]any) string {
	raw, err := os.ReadFile(filepath.Join(homeDir(), ".cursor", "cli-config.json"))
	if err != nil {
		return ""
	}
	var cfg struct {
		Permissions struct {
			Allow []string `json:"allow"`
			Deny  []string `json:"deny"`
		} `json:"permissions"`
	}
	if json.Unmarshal(raw, &cfg) != nil {
		return ""
	}
	kind, subject := "Shell", strings.TrimSpace(str(input["command"]))
	if strings.HasPrefix(toolName, "mcp__") {
		kind, subject = "Mcp", strings.TrimPrefix(toolName, "mcp__")
	}
	match := func(list []string) bool {
		for _, rule := range list {
			if pat, ok := strings.CutPrefix(rule, kind+"("); ok && strings.HasSuffix(pat, ")") && cursorGlob(strings.TrimSuffix(pat, ")"), subject) {
				return true
			}
		}
		return false
	}
	switch {
	case match(cfg.Permissions.Deny):
		return "deny"
	case match(cfg.Permissions.Allow):
		return "allow"
	}
	return ""
}

// cursorGlob matches Cursor's permission patterns: * and ** match any run of characters.
func cursorGlob(pat, s string) bool {
	if pat == "*" || pat == "**" {
		return true
	}
	re := "^" + strings.ReplaceAll(regexp.QuoteMeta(strings.ReplaceAll(pat, "**", "*")), `\*`, ".*") + "$"
	ok, _ := regexp.MatchString(re, s)
	return ok
}
