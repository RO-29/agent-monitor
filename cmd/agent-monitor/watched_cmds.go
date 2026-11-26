package main

import (
	"bytes"
	"encoding/json"
	"html"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// User commands in a transcript: slash commands, and ! commands Claude Code moved to the background.

// slashCmd is one <command-name> line. Prompt commands (skills, custom commands) write
// <command-message> first; local commands (/model, /mcp, /compact) write <command-name> first.
type slashCmd struct {
	Name, Args string
	Prompt     bool
}

func (c slashCmd) line() string { return strings.TrimSpace(c.Name + " " + c.Args) }

func parseSlashCmd(text string) (slashCmd, bool) {
	t := strings.TrimSpace(text)
	if !strings.HasPrefix(t, "<command-name>") && !strings.HasPrefix(t, "<command-message>") {
		return slashCmd{}, false
	}
	name := strings.TrimSpace(html.UnescapeString(extractTagValue(t, "command-name")))
	if name == "" {
		return slashCmd{}, false
	}
	if !strings.HasPrefix(name, "/") {
		name = "/" + name
	}
	args := strings.TrimSpace(html.UnescapeString(extractTagValue(t, "command-args")))
	return slashCmd{Name: name, Args: args, Prompt: strings.HasPrefix(t, "<command-message>")}, true
}

// claudeSlashLine reads the slash command of a raw user line, if it is one.
func claudeSlashLine(line []byte) (slashCmd, int64, bool) {
	if !bytes.Contains(line, bCommandName) || !bytes.Contains(line, bClaudeUser) || bytes.Contains(line, bIsMeta) || bytes.Contains(line, bIsSidechain) {
		return slashCmd{}, 0, false
	}
	var j struct {
		Timestamp string `json:"timestamp"`
		Message   struct {
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(line, &j) != nil {
		return slashCmd{}, 0, false
	}
	text, res := claudeUserText(j.Message.Content)
	if res {
		return slashCmd{}, 0, false
	}
	c, ok := parseSlashCmd(text)
	return c, parseTimestamp(j.Timestamp), ok
}

var (
	bCommandName = []byte("command-name")
	bLocalCmd    = []byte("command-")
	bTaskNote    = []byte("task-notification")
)

var reANSI = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(\x07|\x1b\\)|\x1b[@-Z\\-_]`)

// cleanCmdOutput unescapes HTML entities and removes ANSI escapes from recorded terminal output.
func cleanCmdOutput(s string) string {
	s = reANSI.ReplaceAllString(html.UnescapeString(s), "")
	s = strings.TrimRight(s, " \n\r\t")
	if strings.TrimSpace(s) == "(no content)" {
		return ""
	}
	return s
}

// localCmdOutput returns the <local-command-stdout>/<local-command-stderr> text of a user line.
func localCmdOutput(text string) (out string, isErr, ok bool) {
	t := strings.TrimSpace(text)
	if !strings.HasPrefix(t, "<local-command-stdout>") && !strings.HasPrefix(t, "<local-command-stderr>") {
		return "", false, false
	}
	o := cleanCmdOutput(extractTagValue(t, "local-command-stdout"))
	e := cleanCmdOutput(extractTagValue(t, "local-command-stderr"))
	return strings.TrimSpace(strings.Join(nonEmpty(o, e), "\n")), e != "", true
}

func nonEmpty(ss ...string) []string {
	var out []string
	for _, s := range ss {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

const skillBasePrefix = "Base directory for this skill:"

// ── ! commands moved to the background ──

var reBgMoved = regexp.MustCompile(`moved to the background \(ID: ([A-Za-z0-9_-]+)\)\. Output is being written to: (\S+?\.output)\b`)

var reBgExit = regexp.MustCompile(`exit code (-?\d+)`)

var (
	bgReadCap   int64 = 1 << 20
	bgFollow          = 2 * time.Hour
	bgIdle            = 10 * time.Minute
	bgNow             = time.Now
	bgTempRoots       = defaultBgTempRoots
)

type wBg struct {
	id, path  string
	notified  bool
	final     bool
	size      int64
	mtime     time.Time
	notStatus string
	notExit   *int
	notTs     int64
}

func defaultBgTempRoots() []string {
	roots := []string{"/tmp", "/private/tmp", os.TempDir()}
	if d := os.Getenv("TMPDIR"); d != "" {
		roots = append(roots, d)
	}
	if d := os.Getenv("CLAUDE_CODE_TMPDIR"); d != "" {
		roots = append(roots, d)
	}
	var out []string
	for _, r := range roots {
		r = filepath.Clean(r)
		out = append(out, r)
		if e, err := filepath.EvalSymlinks(r); err == nil && e != r {
			out = append(out, e)
		}
	}
	return out
}

// bgPathAllowed accepts only <temp root>/claude-<uid>/<project>/<session>/tasks/<id>.output, as Claude Code prints it.
func bgPathAllowed(p, id string) bool {
	if !filepath.IsAbs(p) || filepath.Clean(p) != p || filepath.Base(p) != id+".output" {
		return false
	}
	want := "claude-" + strconv.Itoa(os.Getuid())
	under := func(path string) bool {
		for _, r := range bgTempRoots() {
			rel, err := filepath.Rel(r, path)
			if err != nil || strings.HasPrefix(rel, "..") {
				continue
			}
			parts := strings.Split(rel, string(filepath.Separator))
			if len(parts) == 5 && parts[0] == want && parts[3] == "tasks" {
				return true
			}
		}
		return false
	}
	if !under(p) {
		return false
	}
	dir, err := filepath.EvalSymlinks(filepath.Dir(p))
	return err == nil && under(filepath.Join(dir, filepath.Base(p)))
}

// readBgOutput reads the last bgReadCap bytes of a validated output file owned by this user.
func readBgOutput(p, id string) (string, os.FileInfo, bool) {
	if !bgPathAllowed(p, id) {
		return "", nil, false
	}
	st, err := os.Lstat(p)
	if err != nil || !st.Mode().IsRegular() {
		return "", nil, false
	}
	if sys, ok := st.Sys().(*syscall.Stat_t); !ok || int(sys.Uid) != os.Getuid() {
		return "", nil, false
	}
	f, err := os.OpenFile(p, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return "", nil, false
	}
	defer f.Close()
	if fst, err := f.Stat(); err != nil || !os.SameFile(fst, st) {
		return "", nil, false
	}
	from := max(0, st.Size()-bgReadCap)
	b, err := io.ReadAll(io.NewSectionReader(f, from, st.Size()-from))
	if err != nil {
		return "", nil, false
	}
	return string(b), st, true
}

// bgTail keeps the end of a long output, which is what a live view needs.
func bgTail(s string, cut bool) string {
	s = cleanCmdOutput(s)
	if len(s) > orchOutputCap {
		i := len(s) - orchOutputCap
		for i < len(s) && s[i]&0xC0 == 0x80 {
			i++
		}
		s, cut = s[i:], true
	}
	if cut {
		if nl := strings.IndexByte(s, '\n'); nl >= 0 && nl < 200 {
			s = s[nl+1:]
		}
		s = "…\n" + s
	}
	return s
}

// refreshBg fills the output of background ! commands from their files and ends them when done.
func (p *wParser) refreshBg() {
	now := bgNow()
	for _, wi := range p.bgs {
		bg := wi.bg
		if bg.final {
			continue
		}
		it := wi.it
		changed := false
		out, st, ok := readBgOutput(bg.path, bg.id)
		if ok && (st.Size() != bg.size || !st.ModTime().Equal(bg.mtime)) {
			bg.size, bg.mtime = st.Size(), st.ModTime()
			it.Output = bgTail(out, st.Size() > bgReadCap)
			changed = true
		}
		start := time.UnixMilli(it.StartedAt)
		last := start
		if bg.mtime.After(last) {
			last = bg.mtime
		}
		switch {
		case bg.notified:
			it.Status, it.ExitCode, it.EndedAt = bg.notStatus, bg.notExit, bg.notTs
			bg.final, changed = true, true
		case now.Sub(last) > bgIdle || now.Sub(start) > bgFollow:
			it.Status, it.EndedAt = "completed", max(last.UnixMilli(), it.StartedAt)
			bg.final, changed = true, true
		}
		if changed {
			wi.mod = p.off
		}
	}
}

// claudeTaskNotification ends the background ! command a <task-notification> names.
func (p *wParser) claudeTaskNotification(off, ts int64, text string) bool {
	t := strings.TrimSpace(text)
	if !strings.HasPrefix(t, "<task-notification>") {
		return false
	}
	wi := p.bgByID[strings.TrimSpace(extractTagValue(t, "task-id"))]
	if wi == nil {
		return false
	}
	bg := wi.bg
	status := strings.TrimSpace(extractTagValue(t, "status"))
	summary := extractTagValue(t, "summary")
	bg.notified, bg.notTs, bg.notStatus = true, ts, "completed"
	if m := reBgExit.FindStringSubmatch(summary); m != nil {
		code, _ := strconv.Atoi(m[1])
		bg.notExit = &code
		if code != 0 {
			bg.notStatus = "failed"
		}
	}
	if status != "" && status != "completed" {
		bg.notStatus = "failed"
	}
	bg.final = false
	wi.mod = off
	return true
}

// ── Codex ! commands ──

var bUserShell = []byte(`"user_shell"`)

// codexCommandString reads a Codex command, a string or an argv that may wrap a shell.
func codexCommandString(raw json.RawMessage) string {
	if len(raw) > 0 && raw[0] == '"' {
		var s string
		_ = json.Unmarshal(raw, &s)
		return s
	}
	var parts []string
	_ = json.Unmarshal(raw, &parts)
	if s := unwrapShell(parts); s != "" {
		return s
	}
	return strings.Join(parts, " ")
}

// codexUserShell reports the command of a completed ! command record.
func codexUserShell(top string, pl *wCodexPayload) (string, bool) {
	if top != "event_msg" || pl.Type != "item_completed" || pl.Item == nil || pl.Item.Type != "CommandExecution" || pl.Item.Source != "user_shell" {
		return "", false
	}
	return codexCommandString(pl.Item.Command), true
}

// codexUserShellLine is codexUserShell for a raw rollout line; it also returns the item.
func codexUserShellLine(line []byte) (string, *wCodexItem, bool) {
	if !bytes.Contains(line, bUserShell) {
		return "", nil, false
	}
	var rec codexLineRec
	if json.Unmarshal(line, &rec) != nil || len(rec.Payload) == 0 || rec.Payload[0] != '{' {
		return "", nil, false
	}
	var pl wCodexPayload
	if json.Unmarshal(rec.Payload, &pl) != nil {
		return "", nil, false
	}
	cmd, ok := codexUserShell(rec.Type, &pl)
	return cmd, pl.Item, ok
}

// ── Claude ! commands and one parse of a user command line ──

// bangParts reads the <bash-input>, <bash-stdout> and <bash-stderr> tags of a Claude ! command line.
func bangParts(text string) (cmd string, hasIn bool, out string, hasOut bool) {
	hasIn = strings.HasPrefix(strings.TrimSpace(text), "<bash-input>")
	if hasIn {
		cmd = html.UnescapeString(strings.TrimSpace(extractTagValue(text, "bash-input")))
	}
	if strings.Contains(text, "<bash-stdout>") || strings.Contains(text, "<bash-stderr>") {
		hasOut = true
		out = html.UnescapeString(strings.TrimRight(extractTagValue(text, "bash-stdout"), "\n"))
		if e := html.UnescapeString(strings.TrimSpace(extractTagValue(text, "bash-stderr"))); e != "" {
			out = strings.TrimSpace(out + "\n" + e)
		}
	}
	return
}

// userCmdLine is what one Claude user line says about a user command: a slash or ! command, its output, or both.
type userCmdLine struct {
	Name, Command string // Name is the slash command or "!"; empty when the line only carries output
	Prompt        bool   // a prompt command (skill, custom command): a prompt follows
	Output        string
	HasOutput     bool
	IsErr         bool
}

// claudeUserCmdLine reads a slash command, local command output, or ! command from a user line's text.
func claudeUserCmdLine(text string) (userCmdLine, bool) {
	if c, ok := parseSlashCmd(text); ok {
		return userCmdLine{Name: c.Name, Command: c.line(), Prompt: c.Prompt}, true
	}
	if out, isErr, ok := localCmdOutput(text); ok {
		return userCmdLine{Output: out, HasOutput: true, IsErr: isErr}, true
	}
	cmd, in, out, hasOut := bangParts(text)
	if !in && !hasOut {
		return userCmdLine{}, false
	}
	u := userCmdLine{Output: out, HasOutput: hasOut}
	if in {
		u.Name, u.Command = "!", cmd
	}
	return u, true
}
