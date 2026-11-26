package main

import (
	"encoding/json"
	"path/filepath"
	"regexp"
	"strings"
)

// approvalSummary returns one line: the command, the file, the question or the plan title.
func approvalSummary(kind, tool string, input map[string]any) string {
	return clipString(strings.Join(strings.Fields(approvalText(kind, tool, input)), " "), 300)
}

// approvalText is the unclipped command, path or question; risk and rules read this.
func approvalText(kind, tool string, input map[string]any) string {
	s := ""
	switch {
	case kind == "question":
		if qs, ok := input["questions"].([]any); ok && len(qs) > 0 {
			if q, ok := qs[0].(map[string]any); ok {
				s = str(q["question"])
			}
		}
	case kind == "plan":
		for _, line := range strings.Split(str(input["plan"]), "\n") {
			if t := strings.TrimSpace(strings.TrimLeft(line, "# ")); t != "" {
				s = t
				break
			}
		}
		if s == "" {
			s = "Plan approval"
		}
	case isPatchTool(tool):
		if files := patchFiles(str(input["command"]) + str(input["patch"]) + str(input["input"])); len(files) > 0 {
			s = strings.Join(files, " ")
		} else {
			s = str(input["command"])
		}
	default:
		for _, k := range []string{"command", "cmd", "file_path", "notebook_path", "path", "pattern", "url", "query", "description"} {
			if v, ok := input[k]; ok {
				if arr, ok := v.([]any); ok {
					parts := make([]string, 0, len(arr))
					for _, p := range arr {
						parts = append(parts, str(p))
					}
					s = strings.Join(parts, " ")
				} else {
					s = str(v)
				}
				if s != "" {
					break
				}
			}
		}
		if s == "" && len(input) > 0 {
			b, _ := json.Marshal(input)
			s = string(b)
		}
	}
	if strings.TrimSpace(s) == "" {
		s = tool
	}
	return s
}

func str(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func isPatchTool(tool string) bool {
	return tool == "apply_patch" || tool == "ApplyPatch"
}

func isFileWriteTool(tool string) bool {
	switch tool {
	case "Write", "Edit", "MultiEdit", "NotebookEdit":
		return true
	}
	return isPatchTool(tool)
}

func isShellTool(tool string) bool {
	switch tool {
	case "Bash", "shell", "shell_command", "container.exec", "exec_command", "local_shell", "unified_exec":
		return true
	}
	return false
}

var patchFileRe = regexp.MustCompile(`(?m)^\*\*\* (?:Update|Add|Delete) File: (.+)$`)

func patchFiles(patch string) []string {
	var out []string
	for _, m := range patchFileRe.FindAllStringSubmatch(patch, -1) {
		out = append(out, strings.TrimSpace(m[1]))
	}
	return out
}

var (
	highRisk = []struct {
		re     *regexp.Regexp
		reason string
	}{
		{regexp.MustCompile(`--project[= ]+\S*(backend|prd|prod)`), "targets a production project"},
		{regexp.MustCompile(`--context[= ]+\S*(prd|prod)`), "targets a production cluster"},
		{regexp.MustCompile(`\brm\s+(-\w*r\w*f|-\w*f\w*r|-r\s+-f|-f\s+-r|--recursive\s+--force|--force\s+--recursive)\b`), "recursive forced delete"},
		{regexp.MustCompile(`\bgit\s+push\b.*(\s--force\b|\s-f\b|\s--force-with-lease\b)`), "force push"},
		{regexp.MustCompile(`\bkubectl\s+(\S+\s+)*delete\b`), "kubectl delete"},
		{regexp.MustCompile(`\bgcloud\s.*\b(delete|update)\b`), "gcloud delete or update"},
		{regexp.MustCompile(`(^|[\s;&|(])sudo\s`), "runs as root"},
		{regexp.MustCompile(`\blaunchctl\b`), "changes launchd"},
		{regexp.MustCompile(`\bbrew\s+install\b`), "installs software"},
	}
	networkRe  = regexp.MustCompile(`(^|[\s;&|(])(curl|wget|ssh|scp|rsync|nc|telnet|gh|git\s+(push|fetch|pull|clone)|npm\s+(install|i|publish)|pip3?\s+install|go\s+get)\b`)
	segSplitRe = regexp.MustCompile(`\s*(?:&&|\|\||;|\|)\s*`)
	readOnly   = map[string]bool{"ls": true, "rg": true, "grep": true, "cat": true, "head": true, "tail": true, "wc": true,
		"pwd": true, "echo": true, "which": true, "file": true, "stat": true, "tree": true, "find": true, "less": true,
		"sort": true, "uniq": true, "jq": true, "date": true, "whoami": true, "du": true, "df": true, "env": true, "true": true}
	readOnlyGit = map[string]bool{"status": true, "diff": true, "log": true, "show": true, "branch": true, "blame": true, "rev-parse": true, "remote": true}
)

// shownRisk is classifyRisk without a reason that only repeats the card's verb ("wants to run" and "runs a command").
func shownRisk(kind, tool, summary string) (string, string) {
	r, why := classifyRisk(kind, tool, summary)
	switch why {
	case "runs a command", "writes files", "tool call", "MCP tool", "question for you", "plan approval", "read-only tool", "read-only command":
		why = ""
	}
	return r, why
}

// classifyRisk returns low, medium or high with a short reason.
func classifyRisk(kind, tool, summary string) (string, string) {
	switch kind {
	case "question":
		return "low", "question for you"
	case "plan":
		return "low", "plan approval"
	}
	for _, h := range highRisk {
		if h.re.MatchString(summary) {
			return "high", h.reason
		}
	}
	switch tool {
	case "Read", "Grep", "Glob", "LS", "WebSearch", "TodoWrite":
		return "low", "read-only tool"
	}
	if isFileWriteTool(tool) {
		return "medium", "writes files"
	}
	if tool == "WebFetch" {
		return "medium", "network access"
	}
	if isShellTool(tool) {
		if networkRe.MatchString(summary) {
			return "medium", "network command"
		}
		if commandReadOnly(summary) {
			return "low", "read-only command"
		}
		return "medium", "runs a command"
	}
	if strings.HasPrefix(tool, "mcp__") {
		return "medium", "MCP tool"
	}
	return "medium", "tool call"
}

func commandReadOnly(cmd string) bool {
	if strings.ContainsAny(cmd, ">`") || strings.Contains(cmd, "$(") {
		return false
	}
	segs := segSplitRe.Split(strings.TrimSpace(cmd), -1)
	if len(segs) == 0 {
		return false
	}
	for _, seg := range segs {
		f := strings.Fields(seg)
		if len(f) == 0 {
			return false
		}
		if f[0] == "cd" || readOnly[f[0]] {
			if f[0] == "find" && (strings.Contains(seg, "-delete") || strings.Contains(seg, "-exec")) {
				return false
			}
			continue
		}
		if f[0] == "git" && len(f) > 1 && readOnlyGit[f[1]] {
			continue
		}
		return false
	}
	return true
}

// globMatch matches s against a glob where * spans any text (slashes and spaces too).
func globMatch(pattern, s string) bool {
	var b strings.Builder
	b.WriteString("^")
	for _, r := range pattern {
		switch r {
		case '*':
			b.WriteString(".*")
		case '?':
			b.WriteString(".")
		default:
			b.WriteString(regexp.QuoteMeta(string(r)))
		}
	}
	b.WriteString("$")
	re, err := regexp.Compile(b.String())
	return err == nil && re.MatchString(s)
}

func ruleMatches(r Rule, a *Approval) bool {
	if r.Agent != "" && r.Agent != a.Agent {
		return false
	}
	if r.Tool != "" && !strings.EqualFold(r.Tool, a.ToolName) {
		return false
	}
	if r.CwdPrefix != "" && !pathWithin(a.Cwd, r.CwdPrefix) {
		return false
	}
	if r.Action == "allow" && r.Pattern == "" && strings.HasPrefix(r.Note, "always, from ") {
		return false // an old Always rule without a pattern matched every call of its tool
	}
	if r.Pattern == "" {
		return true
	}
	if !isFileWriteTool(a.ToolName) {
		return globMatch(r.Pattern, a.Summary)
	}
	// File tools match per path: an allow needs every path, a deny or hold any path.
	paths := []string{a.Summary}
	if isPatchTool(a.ToolName) {
		if files := patchFiles(str(a.Input["command"]) + str(a.Input["patch"]) + str(a.Input["input"])); len(files) > 0 {
			paths = files
		}
	}
	for _, p := range paths {
		ok := globMatch(r.Pattern, cleanAbs(p))
		if r.Action == "allow" && (!ok || hasDotDot(p)) {
			return false
		}
		if r.Action != "allow" && ok {
			return true
		}
	}
	return r.Action == "allow"
}

// pathWithin reports whether p is root or inside it, compared as cleaned paths.
func pathWithin(p, root string) bool {
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(p))
	return err == nil && (rel == "." || filepath.IsLocal(rel))
}

func hasDotDot(p string) bool {
	for _, el := range strings.Split(filepath.ToSlash(p), "/") {
		if el == ".." {
			return true
		}
	}
	return false
}

// cleanAbs cleans an absolute path and leaves anything else as it is.
func cleanAbs(p string) string {
	if strings.HasPrefix(p, "/") {
		return filepath.Clean(p)
	}
	return p
}

var shellInterpreters = map[string]bool{"python": true, "python3": true, "node": true, "bash": true, "sh": true, "zsh": true, "env": true,
	"xargs": true, "ruby": true, "perl": true, "deno": true, "bun": true, "npx": true, "uvx": true, "sudo": true, "eval": true, "exec": true,
	"osascript": true, "awk": true, "command": true, "nohup": true, "time": true, "timeout": true, "watch": true, "sbatch": true}

// alwaysPattern derives a rule pattern from an approval: a command prefix, a directory or the exact call.
// "" means Always is not offered: no rule may cover more than the user saw.
func alwaysPattern(a *Approval) string {
	if isShellTool(a.ToolName) {
		f := strings.Fields(a.Summary)
		var keep []string
		for _, t := range f {
			if len(keep) == 3 || strings.HasPrefix(t, "-") || strings.ContainsAny(t, "/=$'\"&|;*?") {
				break
			}
			keep = append(keep, t)
		}
		// An interpreter runs any code its arguments carry: only the exact command.
		exact := len(keep) == 0 || len(keep) == len(f) || len(keep) == 1 && shellInterpreters[keep[0]]
		if exact {
			if strings.ContainsAny(a.Summary, "*?") || len(a.Summary) >= 300 {
				return ""
			}
			return a.Summary
		}
		return strings.Join(keep, " ") + " *" // a word boundary: "go test *" never matches "go testx"
	}
	if a.ToolName == "request_permissions" {
		return ""
	}
	if isFileWriteTool(a.ToolName) {
		path := a.Summary
		if isPatchTool(a.ToolName) {
			files := patchFiles(str(a.Input["command"]) + str(a.Input["patch"]) + str(a.Input["input"]))
			if len(files) != 1 {
				return ""
			}
			path = files[0]
		}
		if !strings.HasPrefix(path, "/") || hasDotDot(path) || strings.ContainsAny(path, "*?") {
			return ""
		}
		return filepath.Dir(filepath.Clean(path)) + "/*"
	}
	if a.Summary == "" || strings.ContainsAny(a.Summary, "*?") {
		return ""
	}
	return a.Summary
}

// isCompoundShell is true when a command chains, pipes, redirects or substitutes; prefix rules never allow those.
func isCompoundShell(cmd string) bool {
	for _, op := range []string{"&&", "||", ";", "|", "`", "$(", ">", "<", "\n", "&"} {
		if strings.Contains(cmd, op) {
			return true
		}
	}
	return strings.Contains(cmd, "\n")
}
