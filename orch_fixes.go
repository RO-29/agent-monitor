package main

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// ── D1: close cascades to descendants ──

// CloseTree closes a thread and, with cascade, every descendant that is not closed yet (deepest first).
func (m *OrchManager) CloseTree(id string, cascade bool) error {
	if _, err := m.store.Thread(id); err != nil {
		return err
	}
	if cascade {
		desc, err := m.store.Subtree(id)
		if err != nil {
			return err
		}
		sort.SliceStable(desc, func(i, j int) bool { return desc[i].Depth > desc[j].Depth })
		for _, d := range desc {
			if d.Status != "closed" {
				_ = m.Close(d.ID)
			}
		}
	}
	return m.Close(id)
}

// ── D2: Continue here keeps the session's model ──

// transcriptModel reads the newest model id from a Claude or Codex transcript tail.
func transcriptModel(agent, path string) string {
	if path == "" {
		return ""
	}
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	const tail = 512 << 10
	if st, err := f.Stat(); err == nil && st.Size() > tail {
		_, _ = f.Seek(st.Size()-tail, io.SeekStart)
	}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	model := ""
	for sc.Scan() {
		line := sc.Bytes()
		if !strings.Contains(string(line), `"model"`) {
			continue
		}
		var l struct {
			Type    string `json:"type"`
			Message struct {
				Model string `json:"model"`
			} `json:"message"`
			Payload struct {
				Model string `json:"model"`
			} `json:"payload"`
		}
		if json.Unmarshal(line, &l) != nil {
			continue
		}
		switch {
		case agent == "claude" && l.Type == "assistant" && l.Message.Model != "" && !strings.HasPrefix(l.Message.Model, "<"):
			model = l.Message.Model
		case agent == "codex" && l.Type == "turn_context" && l.Payload.Model != "":
			model = l.Payload.Model
		}
	}
	return model
}

// ── D4: read-only Bash allowlist for read-only Claude children ──

// roFlags lists the flags one program may take; every other flag sends the command to the user.
type roFlags struct {
	short    string          // boolean short flags, clusters allowed
	shortArg string          // short flags with a value (attached or the next word)
	long     map[string]bool // long flag -> takes a value
	numeric  bool            // -NUM (head -5, git log -3)
}

func longs(names ...string) map[string]bool {
	m := map[string]bool{}
	for _, n := range names {
		name, arg := strings.CutSuffix(n, "=")
		m[name] = arg
	}
	return m
}

var (
	roGitLog = roFlags{short: "pusw", shortArg: "nU", numeric: true, long: longs("--stat", "--numstat", "--shortstat", "--name-only",
		"--name-status", "--cached", "--staged", "--oneline", "--graph", "--decorate", "--all", "--patch", "--no-color", "--format=",
		"--pretty=", "--since=", "--until=", "--author=", "--max-count=", "--follow", "--no-ext-diff", "--no-textconv", "--reverse",
		"--first-parent", "--no-merges", "--merges", "--abbrev-commit", "--date=", "--unified=", "--summary", "--ignore-all-space")}
	roPrograms = map[string]roFlags{
		"ls":       {short: "1aAdFGhilpRrStT"},
		"cat":      {short: "bnsv"},
		"head":     {shortArg: "nc", numeric: true},
		"tail":     {shortArg: "nc", numeric: true, short: "r"},
		"wc":       {short: "clmw"},
		"pwd":      {short: "LP"},
		"stat":     {short: "Llnqrsx", shortArg: "f"},
		"du":       {short: "acHhkLmsx", shortArg: "d"},
		"df":       {short: "hHikl"},
		"which":    {short: "as"},
		"cut":      {short: "ns", shortArg: "bcdf"},
		"diff":     {short: "abBiNqrstuw", shortArg: "U", long: longs("--brief", "--recursive", "--unified=", "--ignore-all-space")},
		"cmp":      {short: "ls"},
		"basename": {short: "a", shortArg: "s"},
		"dirname":  {},
		"realpath": {short: "q"},
		"readlink": {short: "fn"},
		"grep": {short: "cEFHhiIlLnoqRrsvwxz", shortArg: "ABCem", numeric: true, long: longs("--color=", "--include=", "--exclude=",
			"--exclude-dir=", "--line-number", "--ignore-case", "--recursive", "--files-with-matches", "--count", "--fixed-strings",
			"--extended-regexp", "--word-regexp", "--context=")},
		"rg": {short: "cFHiIlLnNosSuUvwx", shortArg: "ABCegmMtT", long: longs("--hidden", "--no-ignore", "--files", "--files-with-matches",
			"--count", "--glob=", "--type=", "--fixed-strings", "--ignore-case", "--smart-case", "--line-number", "--no-heading", "--json",
			"--max-count=", "--context=", "--word-regexp", "--max-depth=", "--sort=")},
		"echo": {short: "neE"},
		"sort": {short: "bdfghinrRsuV", shortArg: "kt"},
		"true": {},
	}
	roGit = map[string]roFlags{
		"status":    {short: "bsz", long: longs("--short", "--branch", "--porcelain", "--ignored")},
		"diff":      roGitLog,
		"log":       roGitLog,
		"show":      roGitLog,
		"blame":     {short: "elsw", shortArg: "L"},
		"rev-parse": {long: longs("--show-toplevel", "--abbrev-ref", "--short", "--git-dir", "--is-inside-work-tree", "--verify", "--symbolic-full-name", "--show-prefix")},
		"ls-files":  {short: "cdmosz", long: longs("--others", "--cached", "--modified", "--deleted", "--exclude-standard", "--full-name")},
	}
	// find primaries; true = takes one argument.
	roFind = longs("-name=", "-iname=", "-path=", "-ipath=", "-type=", "-maxdepth=", "-mindepth=", "-newer=", "-size=", "-mtime=",
		"-mmin=", "-regex=", "-iregex=", "-perm=", "-print", "-print0", "-prune", "-not", "-o", "-a", "-and", "-or", "-empty",
		"-depth", "-xdev", "-true", "-false", "-L", "-H", "-P")
)

// shellWords splits a command the way sh does for the subset we allow; ok is false for anything
// that expands, redirects, chains or globs. Stages are separated by a plain pipe.
func shellWords(cmd string) (stages [][]string, ok bool) {
	var (
		cur    []string
		word   strings.Builder
		inWord bool
		quote  rune
	)
	end := func() {
		if inWord {
			cur = append(cur, word.String())
		}
		word.Reset()
		inWord = false
	}
	rs := []rune(cmd)
	for i := 0; i < len(rs); i++ {
		c := rs[i]
		if c < 0x20 && c != '\t' {
			return nil, false
		}
		switch quote {
		case '\'':
			if c == '\'' {
				quote = 0
			} else {
				word.WriteRune(c)
			}
			continue
		case '"':
			switch c {
			case '"':
				quote = 0
			case '$', '`', '\\', '!':
				return nil, false
			default:
				word.WriteRune(c)
			}
			continue
		}
		switch {
		case c == ' ' || c == '\t':
			end()
		case c == '\'' || c == '"':
			quote, inWord = c, true
		case c == '|':
			if i+1 < len(rs) && rs[i+1] == '|' {
				return nil, false
			}
			end()
			if len(cur) == 0 {
				return nil, false
			}
			stages, cur = append(stages, cur), nil
		case strings.ContainsRune(";&<>()$`\\*?[]{}#!^", c):
			return nil, false
		case c == '=' && !inWord:
			return nil, false
		case c == '~' && (i == 0 || !unicode.IsLetter(rs[i-1]) && !unicode.IsDigit(rs[i-1])):
			return nil, false
		default:
			word.WriteRune(c)
			inWord = true
		}
	}
	if quote != 0 {
		return nil, false
	}
	end()
	if len(cur) == 0 {
		return nil, false
	}
	return append(stages, cur), true
}

// readOnlyBash reports whether a Bash command only reads inside cwd: each pipeline stage is an
// allowlisted program with allowlisted flags, and no operand names a path outside cwd.
func readOnlyBash(cmd, cwd string) bool {
	stages, ok := shellWords(strings.TrimSpace(cmd))
	if !ok {
		return false
	}
	for _, args := range stages {
		if !readOnlyStage(args, cwd) {
			return false
		}
	}
	return true
}

func readOnlyStage(args []string, cwd string) bool {
	name := args[0]
	if strings.ContainsAny(name, "/=") {
		return false
	}
	switch name {
	case "git":
		i := 1
		if i < len(args) && args[i] == "--no-pager" {
			i++
		}
		if i >= len(args) {
			return false
		}
		spec, ok := roGit[args[i]]
		return ok && roArgsOK(spec, args[i+1:], cwd)
	case "find":
		return roFindOK(args[1:], cwd)
	}
	spec, ok := roPrograms[name]
	return ok && roArgsOK(spec, args[1:], cwd)
}

// roOperand refuses a path that leaves cwd: an absolute path outside it, or any .. element.
func roOperand(s, cwd string) bool {
	for _, el := range strings.Split(s, "/") {
		if el == ".." {
			return false
		}
	}
	if !strings.HasPrefix(s, "/") {
		return true
	}
	return cwd != "" && pathWithin(s, cwd)
}

func roArgsOK(spec roFlags, args []string, cwd string) bool {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			for _, op := range args[i+1:] {
				if !roOperand(op, cwd) {
					return false
				}
			}
			return true
		case strings.HasPrefix(a, "--"):
			name, val, hasVal := strings.Cut(a, "=")
			takes, ok := spec.long[name]
			if !ok || (hasVal && !takes) {
				return false
			}
			if takes && !hasVal {
				if i+1 >= len(args) || strings.HasPrefix(args[i+1], "-") {
					return false
				}
				i++
				val = args[i]
			}
			if takes && !roOperand(val, cwd) {
				return false
			}
		case len(a) > 1 && a[0] == '-':
			if spec.numeric && strings.Trim(a[1:], "0123456789") == "" {
				continue
			}
			for j := 1; j < len(a); j++ {
				c := rune(a[j])
				if strings.ContainsRune(spec.short, c) {
					continue
				}
				if !strings.ContainsRune(spec.shortArg, c) {
					return false
				}
				val := a[j+1:]
				if val == "" {
					// A dash word is never taken as a value: the flag may take an optional one (git -U).
					if i+1 >= len(args) || strings.HasPrefix(args[i+1], "-") {
						return false
					}
					i++
					val = args[i]
				}
				if !roOperand(val, cwd) {
					return false
				}
				break
			}
		default:
			if !roOperand(a, cwd) {
				return false
			}
		}
	}
	return true
}

func roFindOK(args []string, cwd string) bool {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") {
			if !roOperand(a, cwd) {
				return false
			}
			continue
		}
		takes, ok := roFind[a]
		if !ok {
			return false
		}
		if takes {
			if i+1 >= len(args) || !roOperand(args[i+1], cwd) {
				return false
			}
			i++
		}
	}
	return true
}

// ── D6: continue in the parent checkout after the worktree is gone ──

// claudeProjectDir is where Claude keeps the transcripts of sessions started in cwd.
func claudeProjectDir(cwd string) string {
	return filepath.Join(claudeConfigDir(), "projects", regexp.MustCompile(`[^a-zA-Z0-9]`).ReplaceAllString(cwd, "-"))
}

// copyClaudeSession puts the session transcript under the new cwd's project so --resume finds it there.
func copyClaudeSession(native, cwd string) {
	if native == "" {
		return
	}
	dst := filepath.Join(claudeProjectDir(cwd), native+".jsonl")
	if pathExists(dst) {
		return
	}
	matches, _ := filepath.Glob(filepath.Join(claudeConfigDir(), "projects", "*", native+".jsonl"))
	if len(matches) == 0 {
		return
	}
	b, err := os.ReadFile(matches[0])
	if err != nil {
		return
	}
	_ = os.MkdirAll(filepath.Dir(dst), 0o755)
	_ = os.WriteFile(dst, b, 0o600)
}

// ContinueInParent moves a thread whose worktree was removed back to the checkout it came from.
func (m *OrchManager) ContinueInParent(id string) (*OThread, error) {
	th, err := m.store.Thread(id)
	if err != nil {
		return nil, err
	}
	if th.Worktree == nil || !th.Worktree.Removed {
		return nil, orchBadRequest("thread has no removed worktree")
	}
	if activeStatus(th.Status) {
		return nil, orchConflict("a run is active")
	}
	dest := th.Worktree.Origin
	if dest == "" && th.ParentID != "" {
		if p, err := m.store.Thread(th.ParentID); err == nil {
			dest = p.Cwd
		}
	}
	if dest == "" {
		return nil, orchConflict("the parent checkout is not known for this thread")
	}
	if st, err := os.Stat(dest); err != nil || !st.IsDir() {
		return nil, orchConflict("the parent checkout " + dest + " does not exist")
	}
	m.mu.Lock()
	d := m.drivers[id]
	delete(m.drivers, id)
	m.mu.Unlock()
	if d != nil {
		d.shutdown()
	}
	if th.Agent == "claude" {
		copyClaudeSession(th.NativeID, dest)
	}
	old := th.Cwd
	repo, branch := gitInfo(dest)
	out, err := m.UpdateMeta(id, func(t *OThread, now int64) {
		t.Cwd, t.Repo, t.Branch, t.Workspace = dest, repo, branch, "here"
	})
	if err != nil {
		return nil, err
	}
	m.gitNote(id, "cwd", "Continued in the parent checkout "+dest+" (was "+old+")", true)
	return out, nil
}

// ── D8: ACP silent turn timeout ──

func (m *OrchManager) acpTurnTimeoutOr() time.Duration {
	if m.acpTurnTimeout != nil {
		return m.acpTurnTimeout()
	}
	n := getSettings().AcpTurnTimeoutMin
	if n <= 0 {
		n = 20
	}
	return time.Duration(n) * time.Minute
}

// ── D9: diff for watched sessions ──

var shaRe = regexp.MustCompile(`^[0-9a-f]{7,40}$`)

// WatchedDiff diffs head or a sha against the work tree of any repository (no checkpoints).
func WatchedDiff(cwd, from, to, path string) (*orchDiff, error) {
	if !filepath.IsAbs(cwd) {
		return nil, orchBadRequest("cwd must be an absolute path")
	}
	root := gitTop(cwd)
	if root == "" {
		return nil, orchConflict("not a git repository")
	}
	from, to = firstOf(from, "head"), firstOf(to, "worktree")
	ctx, cancel := context.WithTimeout(context.Background(), orchGitTimeout)
	defer cancel()
	resolve := func(spec string) (string, error) {
		switch {
		case spec == "worktree":
			return snapshotTree(ctx, root)
		case spec == "head":
			if h := headSha(ctx, root); h != "" {
				return treeOf(ctx, root, h)
			}
			return emptyTree(ctx, root), nil
		case strings.HasPrefix(spec, "since:"):
			// since:<ms>[:<session id>]: the session's recorded start tree when there is one, else the last commit before ms.
			msText, session, _ := strings.Cut(strings.TrimPrefix(spec, "since:"), ":")
			ms, err := strconv.ParseInt(msText, 10, 64)
			if err != nil || ms <= 0 {
				return "", orchBadRequest("since needs a time in ms")
			}
			if t := sessionBaseTree(ctx, session, root); session != "" && t != "" {
				return t, nil
			}
			// The last commit before the time; none means the repo started later.
			if h, _ := git(ctx, root, "rev-list", "-1", "--before=@"+strconv.FormatInt(ms/1000, 10), "HEAD"); h != "" {
				return treeOf(ctx, root, h)
			}
			return emptyTree(ctx, root), nil
		case shaRe.MatchString(spec):
			if t, err := treeOf(ctx, root, spec); err == nil {
				return t, nil
			}
			return "", &orchHTTPError{404, "unknown ref " + spec}
		}
		return "", orchBadRequest("from and to must be head, worktree, since:<ms>[:<session>] or a commit sha")
	}
	a, err := resolve(from)
	if err != nil {
		return nil, err
	}
	b, err := resolve(to)
	if err != nil {
		return nil, err
	}
	if path != "" {
		if path = repoPaths(root, cwd, path); path == "" {
			return &orchDiff{From: from, To: to, Files: []orchDiffFile{}}, nil
		}
	}
	files, truncated, err := diffTrees(ctx, root, a, b, path, true)
	if err != nil {
		return nil, err
	}
	return &orchDiff{From: from, To: to, Files: files, Truncated: truncated}, nil
}

// relInside is p relative to root when p lies inside it, comparing real paths too (/var is /private/var).
func relInside(root, p string) (string, bool) {
	in := func(root, p string) (string, bool) {
		r, err := filepath.Rel(root, p)
		return r, err == nil && r != ".." && !strings.HasPrefix(r, "../")
	}
	if r, ok := in(root, p); ok {
		return r, true
	}
	rr, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", false
	}
	rp, err := filepath.EvalSymlinks(filepath.Dir(p))
	if err != nil {
		return "", false
	}
	return in(rr, filepath.Join(rp, filepath.Base(p)))
}

// repoPaths keeps the newline-separated paths that lie inside root, relative to it; relative ones start at cwd.
func repoPaths(root, cwd, paths string) string {
	var out []string
	for _, p := range strings.Split(paths, "\n") {
		if p = strings.TrimSpace(p); p == "" {
			continue
		}
		if !filepath.IsAbs(p) {
			p = filepath.Join(cwd, p)
		}
		r, ok := relInside(root, p)
		if !ok || r == "." {
			continue
		}
		out = append(out, r)
	}
	return strings.Join(out, "\n")
}

func (m *OrchManager) fixRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/orch/git/diff", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if c := q.Get("cwd"); filepath.IsAbs(c) && !knownCwd(c) {
			orchWriteErr(w, &orchHTTPError{http.StatusForbidden, "cwd is not the folder of a watched session or a driven thread"})
			return
		}
		d, err := WatchedDiff(q.Get("cwd"), q.Get("from"), q.Get("to"), strings.Join(q["path"], "\n"))
		if err != nil {
			gitWriteErr(w, gitFailOr(err))
			return
		}
		writeJSON(w, d)
	})
	mux.HandleFunc("POST /api/orch/threads/{id}/worktree/parent", func(w http.ResponseWriter, r *http.Request) {
		th, err := m.ContinueInParent(r.PathValue("id"))
		if err != nil {
			orchWriteErr(w, err)
			return
		}
		writeJSON(w, map[string]any{"ok": true, "thread": th})
	})
}
