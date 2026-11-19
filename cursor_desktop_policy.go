package main

import (
	"database/sql"
	"encoding/json"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// The CLI reports versions like 2026.11.17-f532c45; the desktop app like 3.30.14.
var cursorCLIVersion = regexp.MustCompile(`^\d{4}\.\d{1,2}\.\d{1,2}`)

func cursorFromCLI(version string) bool { return cursorCLIVersion.MatchString(version) }

const cursorDesktopBundle = "com.todesktop.230313mzl4w4u92"

type cursorDesktopRules struct {
	UseYoloMode             bool     `json:"useYoloMode"`
	YoloEnableRunEverything bool     `json:"yoloEnableRunEverything"`
	YoloCommandAllowlist    []string `json:"yoloCommandAllowlist"`
	YoloCommandDenylist     []string `json:"yoloCommandDenylist"`
	YoloMcpToolsDisabled    bool     `json:"yoloMcpToolsDisabled"`
	MCPAllowedTools         []string `json:"mcpAllowedTools"`
}

var cursorDesktopRulesFn = readCursorDesktopRules

// readCursorDesktopRules reads the desktop app's run settings from its own state database; nil when unknown.
func readCursorDesktopRules() *cursorDesktopRules {
	path := filepath.Join(homeDir(), "Library", "Application Support", "Cursor", "User", "globalStorage", "state.vscdb")
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro&_pragma=busy_timeout(2000)")
	if err != nil {
		return nil
	}
	defer db.Close()
	var raw string
	if db.QueryRow(`SELECT value FROM ItemTable WHERE key = 'src.vs.platform.reactivestorage.browser.reactiveStorageServiceImpl.persistentStorage.applicationUser'`).Scan(&raw) != nil {
		return nil
	}
	var v struct {
		ComposerState cursorDesktopRules `json:"composerState"`
	}
	if json.Unmarshal([]byte(raw), &v) != nil {
		return nil
	}
	return &v.ComposerState
}

// cursorDesktopPolicy is "allow" when the desktop app would run the call by itself, else "" (it asks the user).
func cursorDesktopPolicy(toolName string, input map[string]any) string {
	r := cursorDesktopRulesFn()
	if r == nil || !r.UseYoloMode {
		return ""
	}
	if server, tool, ok := strings.Cut(strings.TrimPrefix(toolName, "mcp__"), "__"); ok && strings.HasPrefix(toolName, "mcp__") {
		if !r.YoloMcpToolsDisabled && (r.YoloEnableRunEverything || slices.Contains(r.MCPAllowedTools, server+":"+tool)) {
			return "allow"
		}
		return ""
	}
	cmd := strings.TrimSpace(str(input["command"]))
	for _, d := range r.YoloCommandDenylist {
		if d != "" && strings.HasPrefix(cmd, d) {
			return ""
		}
	}
	if r.YoloEnableRunEverything {
		return "allow"
	}
	for _, a := range r.YoloCommandAllowlist {
		if a != "" && (cmd == a || strings.HasPrefix(cmd, a+" ")) && !strings.ContainsAny(cmd, ";&|`$<>") {
			return "allow"
		}
	}
	return ""
}

// cursorDesktopInFront: the user sits at Cursor, so its own prompt is the fastest place to answer.
var cursorDesktopInFront = func() bool {
	out, err := macFrontBundle()
	return err == nil && strings.EqualFold(parseBundleID(out), cursorDesktopBundle)
}
