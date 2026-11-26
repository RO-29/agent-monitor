package main

import (
	"regexp"
	"strings"
)

const (
	statusMaxLines = 4
	statusMaxRunes = 200
)

var (
	reStatusANSI  = regexp.MustCompile("\x1b\\[[0-9;?]*[ -/]*[@-~]|\x1b\\][^\x07]*\x07")
	reSlashChoice = regexp.MustCompile(`^\s*(❯\s*)?(/\S+\s{2,}\S|@\S+\s*$)`)
)

var (
	// A spinner glyph, then "Thinking… (12s · ↓ 300 tokens)" (Claude) or "Working (8s • esc to interrupt)" (Codex).
	reWorkingLine  = regexp.MustCompile(`^(?:[·✢✳✶✻✽]\s+(\pL+…(?:\s*\([^)]*\))?)$|\S\s+(\pL+\s+\([^)]*esc to interrupt[^)]*\)))`)
	reEscInterrupt = regexp.MustCompile(`(?i)\s*[·•]?\s*esc to interrupt`)
	reEmptyParens  = regexp.MustCompile(`\(\s*\)`)
)

// paneActivity is the agent's working line ("✻ Thinking… (12s · esc to interrupt)") as "Thinking… (12s)"; "" when idle.
func paneActivity(screen string) string {
	lines := strings.Split(strings.TrimRight(reStatusANSI.ReplaceAllString(screen, ""), "\n"), "\n")
	busy := false
	for i := len(lines) - 1; i >= 0 && i >= len(lines)-30; i-- {
		t := strings.TrimSpace(lines[i])
		if m := reWorkingLine.FindStringSubmatch(t); m != nil {
			return clipRunes(strings.TrimSpace(reEmptyParens.ReplaceAllString(strings.ReplaceAll(reEscInterrupt.ReplaceAllString(m[1]+m[2], ""), "( ", "("), "")), 120)
		}
		// The footer ("auto mode on · esc to interrupt") only says a turn runs.
		if strings.Contains(strings.ToLower(t), "esc to interrupt") {
			busy = true
		}
	}
	if busy {
		return "Working…"
	}
	return ""
}

// paneStatusLine returns the footer an agent's terminal draws under its input box: model, context, mode.
func paneStatusLine(tool Tool, screen string) []string {
	screen = reStatusANSI.ReplaceAllString(screen, "")
	switch tool {
	case ToolClaude:
		return claudeStatusLine(screen)
	case ToolCodex:
		return codexStatusLine(screen)
	}
	return nil
}

func statusClean(lines []string) []string {
	var out []string
	for _, ln := range lines {
		if len(out) == statusMaxLines {
			break
		}
		out = append(out, clipRunes(strings.TrimSpace(ln), statusMaxRunes))
	}
	return out
}

func claudeStatusLine(screen string) []string {
	lines := strings.Split(strings.TrimRight(screen, "\n"), "\n")
	_, b := claudeBoxRules(lines)
	if b < 0 {
		return nil
	}
	var below []string
	for _, ln := range lines[b+1:] {
		t := strings.TrimSpace(ln)
		if isBoxRule(t) {
			break
		}
		if t != "" {
			below = append(below, t)
		}
	}
	if len(below) == 0 {
		return nil
	}
	choices := 0
	for _, t := range below {
		if reSlashChoice.MatchString(t) {
			choices++
		}
	}
	if choices >= 2 {
		return nil
	}
	return statusClean(below)
}

func codexStatusLine(screen string) []string {
	if panePromptOpen(screen) {
		return nil
	}
	lines := strings.Split(strings.TrimRight(screen, " \n\t"), "\n")
	var rev []string
	for i := len(lines) - 1; i >= 0; i-- {
		t := strings.TrimSpace(lines[i])
		if t == "" || strings.HasPrefix(t, "›") {
			break
		}
		rev = append(rev, t)
	}
	for i, j := 0, len(rev)-1; i < j; i, j = i+1, j-1 {
		rev[i], rev[j] = rev[j], rev[i]
	}
	return statusClean(rev)
}
