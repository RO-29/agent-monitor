package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	orchGitTimeout       = 60 * time.Second
	orchGitLongTimeout   = 5 * time.Minute
	orchCheckpointBudget = 20 * time.Second
	orchCheckpointKeep   = 30 * 24 * time.Hour
	orchDiffCap          = 400 << 10
	orchDiffFileCap      = 120 << 10
	orchDiffReadCap      = 8 << 20
)

// orchGit holds the git side of the manager: per-thread locks and the injectable launchers.
type orchGit struct {
	gh     string
	launch func(ctx context.Context, dir, name string, args ...string) ([]byte, error)
	look   func(name string) (string, error)
	budget time.Duration

	mu     sync.Mutex
	locks  map[string]*sync.Mutex // per repository, keyed by the git common dir
	off    map[string]bool        // threads whose checkpoints timed out once
	warned map[string]bool        // embedded repositories already reported per thread
}

func newOrchGit() *orchGit {
	return &orchGit{gh: ghBinary(), launch: launchCmd, look: exec.LookPath, budget: orchCheckpointBudget,
		locks: map[string]*sync.Mutex{}, off: map[string]bool{}, warned: map[string]bool{}}
}

func ghBinary() string {
	if v := os.Getenv("AGENT_MONITOR_GH_BIN"); v != "" {
		return v
	}
	if p, err := exec.LookPath("gh"); err == nil {
		return p
	}
	for _, p := range []string{"/opt/homebrew/bin/gh", "/usr/local/bin/gh"} {
		if pathExists(p) {
			return p
		}
	}
	return "gh"
}

func launchCmd(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = gitEnv("GH_PROMPT_DISABLED=1")
	cmd.WaitDelay = 2 * time.Second
	return cmd.CombinedOutput()
}

// lockRepo serializes checkpoint, restore, commit, merge and worktree removal for one repository.
func (g *orchGit) lockRepo(root string) func() {
	key := repoKey(root)
	g.mu.Lock()
	l, ok := g.locks[key]
	if !ok {
		l = &sync.Mutex{}
		g.locks[key] = l
	}
	g.mu.Unlock()
	l.Lock()
	return l.Unlock
}

// repoKey is the absolute git common dir of root, shared by all its worktrees.
func repoKey(root string) string {
	if d := commonDir(root); d != "" {
		return d
	}
	return filepath.Clean(root)
}

func commonDir(dir string) string {
	ctx, cancel := context.WithTimeout(context.Background(), orchGitTimeout)
	defer cancel()
	out, err := git(ctx, dir, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil || out == "" {
		return ""
	}
	return filepath.Clean(out)
}

// repoBusy returns the id of an active thread whose work tree is one of roots, or "".
func (m *OrchManager) repoBusy(roots ...string) string {
	ths, _, err := m.store.Threads()
	if err != nil {
		return ""
	}
	for _, th := range ths {
		if !activeStatus(th.Status) {
			continue
		}
		top := gitTop(th.Cwd)
		for _, r := range roots {
			if top != "" && top == r {
				return th.ID
			}
		}
	}
	return ""
}

func busyErr(id string) error {
	return orchConflict("thread " + id + " runs in this repository; wait for it or interrupt it first")
}

// ── exec ──

type gitError struct {
	args    []string
	out     string
	code    int
	timeout bool
}

func (e *gitError) Error() string {
	if e.timeout {
		return "git " + e.args[0] + ": timed out"
	}
	msg := strings.TrimSpace(e.out)
	if msg == "" {
		msg = "exit " + strconv.Itoa(e.code)
	}
	return "git " + e.args[0] + ": " + clipString(msg, 2000)
}

// gitEnv drops git variables that would redirect a call to another repo or index.
func gitEnv(extra ...string) []string {
	env := make([]string, 0, len(os.Environ())+len(extra)+3)
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		switch k {
		case "GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_OBJECT_DIRECTORY", "GIT_ALTERNATE_OBJECT_DIRECTORIES",
			"GIT_NAMESPACE", "GIT_PREFIX", "GIT_COMMON_DIR", "GIT_TERMINAL_PROMPT", "GIT_OPTIONAL_LOCKS", "LC_ALL":
			continue
		}
		env = append(env, kv)
	}
	return append(append(env, "GIT_TERMINAL_PROMPT=0", "LC_ALL=C", "GIT_OPTIONAL_LOCKS=0"), extra...)
}

// capWriter keeps the first n bytes and drops the rest.
type capWriter struct {
	buf    bytes.Buffer
	n      int
	capped bool
}

func (c *capWriter) Write(p []byte) (int, error) {
	if room := c.n - c.buf.Len(); room > 0 {
		if len(p) > room {
			c.buf.Write(p[:room])
			c.capped = true
		} else {
			c.buf.Write(p)
		}
	} else if len(p) > 0 {
		c.capped = true
	}
	return len(p), nil
}

type gitRun struct {
	ctx     context.Context
	dir     string
	env     []string
	stdin   []byte
	timeout time.Duration
	cap     int
}

// run executes git -C dir args with no shell; stdout is returned, stderr joins it on error.
func (r gitRun) run(args ...string) (string, bool, error) {
	ctx := r.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	to := r.timeout
	if to == 0 {
		to = orchGitTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, to)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", r.dir}, args...)...)
	cmd.Env = gitEnv(r.env...)
	cmd.WaitDelay = 2 * time.Second
	limit := r.cap
	if limit == 0 {
		limit = orchDiffReadCap
	}
	out := &capWriter{n: limit}
	var errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = out, &errb
	if r.stdin != nil {
		cmd.Stdin = bytes.NewReader(r.stdin)
	}
	err := cmd.Run()
	if err != nil {
		ge := &gitError{args: args, out: out.buf.String() + errb.String(), code: -1, timeout: ctx.Err() != nil}
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			ge.code = ee.ExitCode()
		}
		return out.buf.String(), out.capped, ge
	}
	return out.buf.String(), out.capped, nil
}

func git(ctx context.Context, dir string, args ...string) (string, error) {
	out, _, err := gitRun{ctx: ctx, dir: dir}.run(args...)
	return strings.TrimSpace(out), err
}

func isTimeout(err error) bool {
	var ge *gitError
	return (errors.As(err, &ge) && ge.timeout) || errors.Is(err, context.DeadlineExceeded)
}

// tempIndex returns a private index path (seeded from the real index for its stat cache) and a cleanup.
func tempIndex(ctx context.Context, root string, seed bool) (string, func(), error) {
	dir, err := os.MkdirTemp("", "am-idx-")
	if err != nil {
		return "", nil, err
	}
	idx := filepath.Join(dir, "index")
	if seed {
		if real, err := git(ctx, root, "rev-parse", "--path-format=absolute", "--git-path", "index"); err == nil && pathExists(real) {
			_ = copyFile(real, idx)
		}
	}
	return idx, func() { os.RemoveAll(dir) }, nil
}

var embeddedNoCommitRe = regexp.MustCompile(`'([^']+)' does not have a commit checked out`)

// snapshotTree writes the non-ignored work tree to a tree object; the real index is never written.
func snapshotTree(ctx context.Context, root string) (string, error) {
	tree, _, err := snapshotTreeSkip(ctx, root)
	return tree, err
}

// snapshotTreeSkip is snapshotTree that also names embedded repositories without a commit, which it leaves out.
func snapshotTreeSkip(ctx context.Context, root string) (string, []string, error) {
	idx, done, err := tempIndex(ctx, root, true)
	if err != nil {
		return "", nil, err
	}
	defer done()
	env := []string{"GIT_INDEX_FILE=" + idx}
	args := []string{"add", "-A"}
	var skipped []string
	for {
		_, _, err := gitRun{ctx: ctx, dir: root, env: env}.run(args...)
		if err == nil {
			break
		}
		var ge *gitError
		sm := []string(nil)
		if errors.As(err, &ge) {
			sm = embeddedNoCommitRe.FindStringSubmatch(ge.out)
		}
		p := ""
		if sm != nil {
			p = strings.TrimSuffix(sm[1], "/")
		}
		if p == "" || slices.Contains(skipped, p) || len(skipped) >= 50 {
			return "", skipped, err
		}
		if len(skipped) == 0 {
			args = append(args, "--", ".")
		}
		skipped = append(skipped, p)
		args = append(args, ":(exclude,literal)"+p)
	}
	out, _, err := gitRun{ctx: ctx, dir: root, env: env}.run("write-tree")
	return strings.TrimSpace(out), skipped, err
}

func emptyTree(ctx context.Context, root string) string {
	out, _, _ := gitRun{ctx: ctx, dir: root, stdin: []byte{}}.run("hash-object", "-t", "tree", "--stdin")
	return strings.TrimSpace(out)
}

func headSha(ctx context.Context, root string) string {
	out, _ := git(ctx, root, "rev-parse", "--verify", "-q", "HEAD^{commit}")
	return out
}

func treeOf(ctx context.Context, root, rev string) (string, error) {
	return git(ctx, root, "rev-parse", "--verify", "-q", "--end-of-options", rev+"^{tree}")
}

// ── checkpoints ──

type orchCheckpoint struct {
	RunIndex int    `json:"runIndex"`
	Kind     string `json:"kind"`
	Ref      string `json:"ref"`
	Sha      string `json:"sha"`
	At       int64  `json:"at"`
	Prompt   string `json:"prompt"`
	Add      int    `json:"add"`
	Del      int    `json:"del"`
	Files    int    `json:"files"`
	GitDir   string `json:"gitDir,omitempty"` // the repository's common dir, where prune finds the refs
}

func cpPrefix(threadID string) string { return "refs/am/cp/" + threadID + "/" }
func baseRef(threadID string) string  { return "refs/am/base/" + threadID }

func (s *OrchStore) lastCheckpoint(threadID string) *orchCheckpoint {
	var cp *orchCheckpoint
	_ = s.read(func(tx *sql.Tx) error {
		cp, _ = getJSON[orchCheckpoint](tx, `SELECT json FROM checkpoints WHERE thread_id=? ORDER BY rowid DESC LIMIT 1`, threadID)
		return nil
	})
	return cp
}

func (s *OrchStore) checkpoints(threadID string) []orchCheckpoint {
	var out []orchCheckpoint
	_ = s.read(func(tx *sql.Tx) error {
		out, _ = scanJSONRows[orchCheckpoint](tx.Query(`SELECT json FROM checkpoints WHERE thread_id=? ORDER BY rowid`, threadID))
		prompts := map[int]string{}
		runs, _ := scanJSONRows[ORun](tx.Query(`SELECT json FROM runs WHERE thread_id=?`, threadID))
		for _, r := range runs {
			prompts[r.Index] = r.Prompt
		}
		for i := range out {
			out[i].Prompt = clipString(prompts[out[i].RunIndex], 500)
		}
		return nil
	})
	if out == nil {
		out = []orchCheckpoint{}
	}
	return out
}

func (s *OrchStore) putCheckpoint(threadID, root string, cp *orchCheckpoint) error {
	b, _ := json.Marshal(cp)
	return s.write(func(t *orchTx) error {
		_, err := t.tx.Exec(`INSERT INTO checkpoints(ref, thread_id, root, at, json) VALUES(?,?,?,?,?)
			ON CONFLICT(ref) DO UPDATE SET json=excluded.json, at=excluded.at`, cp.Ref, threadID, root, cp.At, string(b))
		return err
	})
}

// checkpointRun snapshots a run's start or end; it never fails the run.
func (m *OrchManager) checkpointRun(threadID, runID, kind string) {
	if m.git == nil || runID == "" {
		return
	}
	var idx int
	err := m.store.read(func(tx *sql.Tx) error {
		r, err := getJSON[ORun](tx, `SELECT json FROM runs WHERE id=?`, runID)
		if err == nil {
			idx = r.Index
		}
		return err
	})
	if err != nil {
		return
	}
	if _, _, err := m.checkpoint(threadID, idx, kind, false); err != nil && !errors.Is(err, errNoRepo) {
		log.Printf("orch: checkpoint %s %d-%s: %v", threadID, idx, kind, err)
	}
}

var errNoRepo = errors.New("not a git repository")

// warnEmbedded notes once per thread that embedded repositories without a commit are not in checkpoints.
func (m *OrchManager) warnEmbedded(threadID, root string, skipped []string) {
	var fresh []string
	m.git.mu.Lock()
	for _, p := range skipped {
		if k := threadID + "\x00" + p; !m.git.warned[k] {
			m.git.warned[k] = true
			fresh = append(fresh, p)
		}
	}
	m.git.mu.Unlock()
	if len(fresh) > 0 {
		m.gitNote(threadID, "checkpoint", fmt.Sprintf("Checkpoints leave out %s in %s: an embedded git repository with no commit. Commit inside it, or its files are not restorable.", strings.Join(fresh, ", "), root), false)
	}
}

// checkpoint takes the lock and snapshots the thread's work tree.
func (m *OrchManager) checkpoint(threadID string, runIndex int, kind string, force bool) (*orchCheckpoint, bool, error) {
	th, err := m.store.Thread(threadID)
	if err != nil {
		return nil, false, err
	}
	root := gitTop(th.Cwd)
	if root == "" {
		return nil, false, errNoRepo
	}
	unlock := m.git.lockRepo(root)
	defer unlock()
	return m.checkpointLocked(th, root, runIndex, kind, force)
}

// checkpointLocked writes refs/am/cp/<thread>/<run>-<kind>; it skips (returns the previous one) when the tree is unchanged, unless force.
func (m *OrchManager) checkpointLocked(th *OThread, root string, runIndex int, kind string, force bool) (*orchCheckpoint, bool, error) {
	m.git.mu.Lock()
	off := m.git.off[th.ID]
	m.git.mu.Unlock()
	if off && !force {
		return nil, true, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), m.git.budget)
	defer cancel()
	cp, skipped, err := m.snapshotCheckpoint(ctx, th, root, runIndex, kind, force)
	if err != nil && (isTimeout(err) || ctx.Err() != nil) && !force {
		m.git.mu.Lock()
		m.git.off[th.ID] = true
		m.git.mu.Unlock()
		m.gitNote(th.ID, "checkpoint", fmt.Sprintf("Checkpoint skipped: git took longer than %s in %s. Checkpoints are off for this thread until the daemon restarts.", m.git.budget, root), false)
	}
	return cp, skipped, err
}

func (m *OrchManager) snapshotCheckpoint(ctx context.Context, th *OThread, root string, runIndex int, kind string, force bool) (*orchCheckpoint, bool, error) {
	head := headSha(ctx, root)
	if head != "" {
		if _, err := git(ctx, root, "rev-parse", "--verify", "-q", baseRef(th.ID)); err != nil {
			if _, err := git(ctx, root, "update-ref", baseRef(th.ID), head); err != nil {
				return nil, false, err
			}
		}
	}
	tree, skipped, err := snapshotTreeSkip(ctx, root)
	if err != nil {
		return nil, false, err
	}
	m.warnEmbedded(th.ID, root, skipped)
	prev := m.store.lastCheckpoint(th.ID)
	var prevTree string
	if prev != nil {
		if prevTree, err = treeOf(ctx, root, prev.Sha); err != nil {
			prev, prevTree = nil, ""
		}
	}
	ref := cpPrefix(th.ID) + strconv.Itoa(runIndex) + "-" + kind
	if kind != "start" && kind != "end" {
		for n := 2; ; n++ {
			if _, err := git(ctx, root, "rev-parse", "--verify", "-q", ref); err != nil {
				break
			}
			ref = cpPrefix(th.ID) + strconv.Itoa(runIndex) + "-" + kind + "-" + strconv.Itoa(n)
		}
	}
	gitDir, _ := git(ctx, root, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if prev != nil && prevTree == tree {
		if !force {
			return prev, true, nil
		}
		if _, err := git(ctx, root, "update-ref", ref, prev.Sha); err != nil {
			return nil, false, err
		}
		cp := &orchCheckpoint{RunIndex: runIndex, Kind: kind, Ref: ref, Sha: prev.Sha, At: time.Now().UnixMilli(), GitDir: gitDir}
		return cp, false, m.store.putCheckpoint(th.ID, root, cp)
	}
	parent, parentTree := head, ""
	if prev != nil {
		parent, parentTree = prev.Sha, prevTree
	} else if head != "" {
		parentTree, _ = treeOf(ctx, root, head)
	}
	if parentTree == "" {
		parentTree = emptyTree(ctx, root)
	}
	args := []string{"commit-tree", "--no-gpg-sign", tree, "-m", fmt.Sprintf("agent-monitor checkpoint %s run %d %s", th.ID, runIndex, kind)}
	if parent != "" {
		args = append(args, "-p", parent)
	}
	idEnv := []string{"GIT_AUTHOR_NAME=agent-monitor", "GIT_AUTHOR_EMAIL=agent-monitor@localhost",
		"GIT_COMMITTER_NAME=agent-monitor", "GIT_COMMITTER_EMAIL=agent-monitor@localhost"}
	out, _, err := gitRun{ctx: ctx, dir: root, env: idEnv}.run(args...)
	if err != nil {
		return nil, false, err
	}
	sha := strings.TrimSpace(out)
	if _, err := git(ctx, root, "update-ref", ref, sha); err != nil {
		return nil, false, err
	}
	cp := &orchCheckpoint{RunIndex: runIndex, Kind: kind, Ref: ref, Sha: sha, At: time.Now().UnixMilli(), GitDir: gitDir}
	if files, err := diffNumstat(ctx, root, parentTree, tree, ""); err == nil {
		cp.Files = len(files)
		for _, f := range files {
			cp.Add += f.Add
			cp.Del += f.Del
		}
	}
	return cp, false, m.store.putCheckpoint(th.ID, root, cp)
}

// PruneCheckpoints deletes the refs and rows of threads closed for longer than the keep window.
// Rows go only after their refs are gone, or when no copy of the repository is left.
func (m *OrchManager) PruneCheckpoints(now time.Time) (int, error) {
	dirs := map[string][]string{}
	var order []string
	cut := now.Add(-orchCheckpointKeep).UnixMilli()
	err := m.store.read(func(tx *sql.Tx) error {
		rows, err := tx.Query(`SELECT c.thread_id, coalesce(json_extract(c.json,'$.gitDir'),''), c.root,
			coalesce(json_extract(t.json,'$.worktree.origin'),'') FROM checkpoints c LEFT JOIN threads t ON t.id=c.thread_id
			WHERE t.id IS NULL OR (json_extract(t.json,'$.status')='closed' AND json_extract(t.json,'$.updatedAt') < ?)`, cut)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id, gitDir, root, origin string
			if err := rows.Scan(&id, &gitDir, &root, &origin); err != nil {
				return err
			}
			if _, ok := dirs[id]; !ok {
				order = append(order, id)
			}
			for _, d := range []string{gitDir, root, origin} {
				if d != "" && !slices.Contains(dirs[id], d) {
					dirs[id] = append(dirs[id], d)
				}
			}
		}
		return rows.Err()
	})
	if err != nil {
		return 0, err
	}
	var done []string
	for _, id := range order {
		if err := pruneRefs(id, dirs[id]); err != nil {
			log.Printf("orch: prune refs %s: %v", id, err)
			continue
		}
		done = append(done, id)
	}
	if len(done) == 0 {
		return 0, nil
	}
	err = m.store.write(func(t *orchTx) error {
		for _, id := range done {
			if _, err := t.tx.Exec(`DELETE FROM checkpoints WHERE thread_id=?`, id); err != nil {
				return err
			}
		}
		return nil
	})
	return len(done), err
}

// pruneRefs deletes a thread's refs in every distinct repository found among dirs.
func pruneRefs(id string, dirs []string) error {
	seen := map[string]bool{}
	for _, d := range dirs {
		if !pathExists(d) {
			continue
		}
		key := commonDir(d)
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		ctx, cancel := context.WithTimeout(context.Background(), orchGitTimeout)
		out, err := git(ctx, key, "for-each-ref", "--format=%(refname)", cpPrefix(id), baseRef(id))
		if err == nil {
			var in strings.Builder
			for _, ref := range strings.Fields(out) {
				in.WriteString("delete " + ref + "\n")
			}
			if in.Len() > 0 {
				_, _, err = gitRun{ctx: ctx, dir: key, stdin: []byte(in.String())}.run("update-ref", "--stdin")
			}
		}
		cancel()
		if err != nil {
			return err
		}
	}
	return nil
}

// ── diff ──

type orchDiffFile struct {
	Path      string `json:"path"`
	OldPath   string `json:"oldPath,omitempty"`
	Status    string `json:"status"`
	Add       int    `json:"add"`
	Del       int    `json:"del"`
	Binary    bool   `json:"binary"`
	Patch     string `json:"patch"`
	Truncated bool   `json:"truncated,omitempty"`
	mode      string
}

type orchDiff struct {
	From      string         `json:"from"`
	To        string         `json:"to"`
	Files     []orchDiffFile `json:"files"`
	Truncated bool           `json:"truncated"`
}

// literalEnv makes a caller's path a literal path, never a glob or magic pathspec.
var literalEnv = []string{"GIT_LITERAL_PATHSPECS=1"}

// revs omits an empty second side, so the diff compares a tree with the work tree.
func revs(a, b string) []string {
	if b == "" {
		return []string{a}
	}
	return []string{a, b}
}

func pathArgs(path string) []string {
	if path == "" {
		return nil
	}
	return []string{"--", path}
}

// diffRaw lists changed files between two trees with status and paths (renames detected).
func diffRaw(ctx context.Context, root, a, b, path string) ([]orchDiffFile, error) {
	args := append(append([]string{"diff", "--no-color", "--no-ext-diff", "--raw", "-z", "-M"}, revs(a, b)...), pathArgs(path)...)
	out, _, err := gitRun{ctx: ctx, dir: root, env: literalEnv}.run(args...)
	if err != nil {
		return nil, err
	}
	parts := strings.Split(out, "\x00")
	var files []orchDiffFile
	for i := 0; i < len(parts); i++ {
		meta := parts[i]
		if !strings.HasPrefix(meta, ":") {
			continue
		}
		fields := strings.Fields(meta)
		if len(fields) < 5 || i+1 >= len(parts) {
			break
		}
		f := orchDiffFile{Status: statusWord(fields[4][:1]), mode: fields[0][1:] + " " + fields[1]}
		if f.Status == "renamed" || f.Status == "copied" {
			if i+2 >= len(parts) {
				break
			}
			f.OldPath, f.Path = parts[i+1], parts[i+2]
			i += 2
		} else {
			f.Path = parts[i+1]
			i++
		}
		files = append(files, f)
	}
	return files, nil
}

func statusWord(c string) string {
	switch c {
	case "A":
		return "added"
	case "M":
		return "modified"
	case "D":
		return "deleted"
	case "R":
		return "renamed"
	case "C":
		return "copied"
	case "T":
		return "typechange"
	}
	return strings.ToLower(c)
}

// diffNumstat returns add/del per file in the same order as diffRaw; binary files report -1.
func diffNumstat(ctx context.Context, root, a, b, path string) ([]orchDiffFile, error) {
	args := append(append([]string{"diff", "--no-color", "--no-ext-diff", "--numstat", "-z", "-M"}, revs(a, b)...), pathArgs(path)...)
	out, _, err := gitRun{ctx: ctx, dir: root, env: literalEnv}.run(args...)
	if err != nil {
		return nil, err
	}
	parts := strings.Split(out, "\x00")
	var files []orchDiffFile
	for i := 0; i < len(parts); i++ {
		if parts[i] == "" {
			continue
		}
		cols := strings.SplitN(parts[i], "\t", 3)
		if len(cols) < 3 {
			continue
		}
		f := orchDiffFile{Path: cols[2]}
		if cols[0] == "-" {
			f.Binary = true
		} else {
			f.Add, _ = strconv.Atoi(cols[0])
			f.Del, _ = strconv.Atoi(cols[1])
		}
		if cols[2] == "" && i+2 < len(parts) {
			f.OldPath, f.Path = parts[i+1], parts[i+2]
			i += 2
		}
		files = append(files, f)
	}
	return files, nil
}

// splitPatch cuts a unified diff at each "diff --git" header.
func splitPatch(patch string) []string {
	var out []string
	start := -1
	for i := 0; i < len(patch); {
		if strings.HasPrefix(patch[i:], "diff --git ") {
			if start >= 0 {
				out = append(out, patch[start:i])
			}
			start = i
		}
		nl := strings.IndexByte(patch[i:], '\n')
		if nl < 0 {
			break
		}
		i += nl + 1
	}
	if start >= 0 {
		out = append(out, patch[start:])
	}
	return out
}

func clipPatch(p string, n int) string {
	if len(p) <= n {
		return p
	}
	p = p[:n]
	if i := strings.LastIndexByte(p, '\n'); i > 0 {
		p = p[:i+1]
	}
	return p
}

// diffTrees builds the file list with patches, honouring the total and per-file caps.
func diffTrees(ctx context.Context, root, a, b, path string, withPatch bool) ([]orchDiffFile, bool, error) {
	files, err := diffRaw(ctx, root, a, b, path)
	if err != nil {
		return nil, false, err
	}
	nums, err := diffNumstat(ctx, root, a, b, path)
	if err != nil {
		return nil, false, err
	}
	for i := range files {
		if i < len(nums) && nums[i].Path == files[i].Path {
			files[i].Add, files[i].Del, files[i].Binary = nums[i].Add, nums[i].Del, nums[i].Binary
		}
	}
	if files == nil {
		files = []orchDiffFile{}
	}
	if !withPatch || len(files) == 0 {
		return files, false, nil
	}
	args := append([]string{"diff", "--no-color", "--no-ext-diff", "--patch", "-M", a, b}, pathArgs(path)...)
	out, capped, err := gitRun{ctx: ctx, dir: root, env: literalEnv}.run(args...)
	if err != nil {
		return nil, false, err
	}
	patches := splitPatch(out)
	truncated := capped
	total := 0
	for i := range files {
		if i >= len(patches) {
			truncated = true
			break
		}
		p := patches[i]
		if len(p) > orchDiffFileCap {
			p = clipPatch(p, orchDiffFileCap)
			files[i].Truncated, truncated = true, true
		}
		if total+len(p) > orchDiffCap {
			files[i].Truncated, truncated = true, true
			continue
		}
		total += len(p)
		files[i].Patch = p
	}
	if capped && len(patches) > 0 && len(patches) <= len(files) {
		files[len(patches)-1].Truncated = true
	}
	return files, truncated, nil
}

// resolveTree turns a diff endpoint into a tree id: worktree, head, base, a checkpoint ref or a sha.
func (m *OrchManager) resolveTree(ctx context.Context, th *OThread, root, spec string) (string, error) {
	switch spec {
	case "", "worktree":
		return snapshotTree(ctx, root)
	case "head":
		if h := headSha(ctx, root); h != "" {
			return treeOf(ctx, root, h)
		}
		return emptyTree(ctx, root), nil
	case "base":
		if t, err := treeOf(ctx, root, baseRef(th.ID)); err == nil {
			return t, nil
		}
		return m.resolveTree(ctx, th, root, "head")
	}
	if strings.HasPrefix(spec, "-") {
		return "", orchBadRequest("bad ref " + spec)
	}
	cand := []string{spec}
	if !strings.HasPrefix(spec, "refs/") {
		cand = append([]string{cpPrefix(th.ID) + spec}, cand...)
	}
	for _, c := range cand {
		if t, err := treeOf(ctx, root, c); err == nil {
			return t, nil
		}
	}
	return "", &orchHTTPError{404, "unknown ref " + spec}
}

// runRange maps a run index to the checkpoints around it (from = before the run, to = after it or the work tree).
func (m *OrchManager) runRange(th *OThread, run int) (string, string) {
	cps := m.store.checkpoints(th.ID)
	from, to := "base", ""
	for _, cp := range cps {
		if cp.RunIndex < run || (cp.RunIndex == run && cp.Kind == "start") {
			from = cp.Ref
		}
		if cp.RunIndex == run && cp.Kind == "end" {
			to = cp.Ref
		}
	}
	if to == "" {
		to = from
		if activeStatus(th.Status) {
			to = "worktree"
		}
	}
	return from, to
}

func (m *OrchManager) Diff(id, from, to, path string, run *int) (*orchDiff, error) {
	th, err := m.store.Thread(id)
	if err != nil {
		return nil, err
	}
	root := gitTop(th.Cwd)
	if root == "" {
		return nil, orchConflict("not a git repository")
	}
	if run != nil {
		from, to = m.runRange(th, *run)
	}
	if from == "" {
		from = "base"
	}
	if to == "" {
		to = "worktree"
	}
	ctx, cancel := context.WithTimeout(context.Background(), orchGitTimeout)
	defer cancel()
	a, err := m.resolveTree(ctx, th, root, from)
	if err != nil {
		return nil, err
	}
	b, err := m.resolveTree(ctx, th, root, to)
	if err != nil {
		return nil, err
	}
	files, truncated, err := diffTrees(ctx, root, a, b, path, true)
	if err != nil {
		return nil, err
	}
	return &orchDiff{From: from, To: to, Files: files, Truncated: truncated}, nil
}

// ── status ──

type orchGitFile struct {
	Path    string `json:"path"`
	OldPath string `json:"oldPath,omitempty"`
	Status  string `json:"status"`
	Add     int    `json:"add"`
	Del     int    `json:"del"`
}

type orchGitInfo struct {
	IsRepo        bool             `json:"isRepo"`
	Repo          string           `json:"repo"`
	Root          string           `json:"root"`
	Branch        string           `json:"branch"`
	Head          string           `json:"head"`
	Upstream      string           `json:"upstream,omitempty"`
	Ahead         int              `json:"ahead"`
	Behind        int              `json:"behind"`
	Dirty         int              `json:"dirty"`
	DefaultBranch string           `json:"defaultBranch"`
	Files         []orchGitFile    `json:"files"`
	Checkpoints   []orchCheckpoint `json:"checkpoints"`
	Worktree      *OWorktree       `json:"worktree,omitempty"`
}

func defaultBranch(ctx context.Context, root string) string {
	if out, err := git(ctx, root, "symbolic-ref", "--short", "-q", "refs/remotes/origin/HEAD"); err == nil && out != "" {
		return strings.TrimPrefix(out, "origin/")
	}
	for _, b := range []string{"main", "master"} {
		if _, err := git(ctx, root, "rev-parse", "--verify", "-q", "refs/heads/"+b); err == nil {
			return b
		}
	}
	return ""
}

func upstreamOf(ctx context.Context, root string) string {
	out, _ := git(ctx, root, "rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{u}")
	return out
}

// gitStatus reads branch, upstream and changes of the repo at cwd; the work tree is compared with HEAD.
func gitStatus(cwd string) (*orchGitInfo, string, error) {
	info := &orchGitInfo{Files: []orchGitFile{}, Checkpoints: []orchCheckpoint{}}
	root := gitTop(cwd)
	if root == "" {
		return info, "", nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), orchGitTimeout)
	defer cancel()
	info.IsRepo, info.Root, info.Repo = true, root, filepath.Base(root)
	info.Branch, _ = git(ctx, root, "rev-parse", "--abbrev-ref", "HEAD")
	info.Head = headSha(ctx, root)
	info.DefaultBranch = defaultBranch(ctx, root)
	if up := upstreamOf(ctx, root); up != "" {
		info.Upstream = up
		if out, err := git(ctx, root, "rev-list", "--left-right", "--count", "HEAD...@{u}"); err == nil {
			if f := strings.Fields(out); len(f) == 2 {
				info.Ahead, _ = strconv.Atoi(f[0])
				info.Behind, _ = strconv.Atoi(f[1])
			}
		}
	}
	// Read only: the diff reads the real index without refreshing it (GIT_OPTIONAL_LOCKS=0) and writes no object.
	base := emptyTree(ctx, root)
	if info.Head != "" {
		base = info.Head
	}
	files, err := diffRaw(ctx, root, base, "", "")
	if err != nil {
		return nil, root, err
	}
	nums, err := diffNumstat(ctx, root, base, "", "")
	if err != nil {
		return nil, root, err
	}
	for i := range files {
		if i < len(nums) && nums[i].Path == files[i].Path {
			files[i].Add, files[i].Del = nums[i].Add, nums[i].Del
		}
	}
	for _, f := range files {
		info.Files = append(info.Files, orchGitFile{Path: f.Path, OldPath: f.OldPath, Status: f.Status, Add: f.Add, Del: f.Del})
	}
	if out, _, err := (gitRun{ctx: ctx, dir: root}).run("ls-files", "-z", "--others", "--exclude-standard"); err == nil {
		for _, p := range strings.Split(out, "\x00") {
			if p != "" {
				info.Files = append(info.Files, orchGitFile{Path: p, Status: "untracked", Add: countLines(filepath.Join(root, p))})
			}
		}
	}
	info.Dirty = len(info.Files)
	return info, root, nil
}

// countLines counts the lines of a small text file; large or binary files count 0.
func countLines(path string) int {
	fi, err := os.Lstat(path)
	if err != nil || !fi.Mode().IsRegular() || fi.Size() > 1<<20 {
		return 0
	}
	b, err := os.ReadFile(path)
	if err != nil || bytes.IndexByte(b, 0) >= 0 {
		return 0
	}
	n := bytes.Count(b, []byte("\n"))
	if len(b) > 0 && b[len(b)-1] != '\n' {
		n++
	}
	return n
}

func (m *OrchManager) GitInfo(id string) (*orchGitInfo, error) {
	th, err := m.store.Thread(id)
	if err != nil {
		return nil, err
	}
	info, _, err := gitStatus(th.Cwd)
	if err != nil {
		return nil, err
	}
	if info.IsRepo {
		info.Checkpoints = m.store.checkpoints(id)
	}
	info.Worktree = th.Worktree
	return info, nil
}

// ── restore ──

// orchRestoreReadCap bounds one git listing during a restore; a longer listing aborts the restore.
var orchRestoreReadCap = 512 << 20

var errTruncated = errors.New("git output was truncated; nothing was changed")

// gitList runs a -z listing and refuses a truncated or unterminated result.
func gitList(ctx context.Context, root string, env []string, stdin []byte, args ...string) ([]string, error) {
	out, capped, err := gitRun{ctx: ctx, dir: root, env: env, stdin: stdin, cap: orchRestoreReadCap, timeout: orchGitLongTimeout}.run(args...)
	if err != nil {
		return nil, err
	}
	if capped || (out != "" && !strings.HasSuffix(out, "\x00")) {
		return nil, errTruncated
	}
	if out == "" {
		return nil, nil
	}
	return strings.Split(strings.TrimSuffix(out, "\x00"), "\x00"), nil
}

func removeEmptyParents(root, path string) {
	for dir := filepath.Dir(path); dir != root && strings.HasPrefix(dir, root+string(filepath.Separator)); dir = filepath.Dir(dir) {
		if os.Remove(dir) != nil {
			return
		}
	}
}

// checkIgnored lists which paths the ignore rules match; any failure other than "none matched" is an error.
func checkIgnored(ctx context.Context, dir string, env []string, paths []string) (map[string]bool, error) {
	out := map[string]bool{}
	if len(paths) == 0 {
		return out, nil
	}
	in := []byte(strings.Join(paths, "\x00") + "\x00")
	res, capped, err := gitRun{ctx: ctx, dir: dir, env: env, stdin: in, cap: orchRestoreReadCap, timeout: orchGitLongTimeout}.run("check-ignore", "--no-index", "-z", "--stdin")
	var ge *gitError
	if err != nil && !(errors.As(err, &ge) && ge.code == 1 && !ge.timeout) {
		return nil, fmt.Errorf("git check-ignore: %w", err)
	}
	if capped {
		return nil, errTruncated
	}
	for _, p := range strings.Split(res, "\x00") {
		if p != "" {
			out[p] = true
		}
	}
	return out, nil
}

// targetIgnored applies the .gitignore files of tree (plus info/exclude and core.excludesFile) to paths.
func targetIgnored(ctx context.Context, root, tree string, paths []string) (map[string]bool, error) {
	if len(paths) == 0 {
		return map[string]bool{}, nil
	}
	ents, err := gitList(ctx, root, nil, nil, "ls-tree", "-r", "-z", "--full-tree", tree)
	if err != nil {
		return nil, err
	}
	var files []string
	for _, e := range ents {
		meta, p, ok := strings.Cut(e, "\t")
		if ok && filepath.Base(p) == ".gitignore" && !strings.HasPrefix(meta, "120000") && strings.Contains(meta, " blob ") {
			files = append(files, p)
		}
	}
	tmp, err := os.MkdirTemp("", "am-ign-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)
	wt := filepath.Join(tmp, "wt")
	if err := os.Mkdir(wt, 0o700); err != nil {
		return nil, err
	}
	if len(files) > 0 {
		env := []string{"GIT_INDEX_FILE=" + filepath.Join(tmp, "index")}
		if _, _, err := (gitRun{ctx: ctx, dir: root, env: env, timeout: orchGitLongTimeout}).run("read-tree", tree); err != nil {
			return nil, err
		}
		in := []byte(strings.Join(files, "\x00") + "\x00")
		if _, _, err := (gitRun{ctx: ctx, dir: root, env: env, stdin: in, timeout: orchGitLongTimeout}).run("checkout-index", "-z", "--stdin", "--prefix="+wt+"/"); err != nil {
			return nil, err
		}
	}
	gitDir, err := git(ctx, root, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return nil, err
	}
	return checkIgnored(ctx, wt, []string{"GIT_DIR=" + gitDir, "GIT_WORK_TREE=" + wt}, paths)
}

// realCase returns rel spelled as it is on disk, so a case-insensitive volume does not hide a rename.
type caseCache map[string][]string

func (c caseCache) realCase(root, rel string) string {
	parts := strings.Split(rel, "/")
	dir := ""
	for i, name := range parts {
		names, ok := c[dir]
		if !ok {
			ents, _ := os.ReadDir(filepath.Join(root, filepath.FromSlash(dir)))
			for _, e := range ents {
				names = append(names, e.Name())
			}
			c[dir] = names
		}
		if !slices.Contains(names, name) {
			for _, n := range names {
				if strings.EqualFold(n, name) {
					parts[i] = n
					break
				}
			}
		}
		dir = strings.Join(parts[:i+1], "/")
	}
	return strings.Join(parts, "/")
}

type orchRestoreResult struct {
	SafetyRef string   `json:"safetyRef"`
	Written   int      `json:"written"`
	Deleted   int      `json:"deleted"`
	Kept      []string `json:"kept"` // ignored paths that the restore left in place
}

type restorePlan struct {
	dels, adds, mods, emptyDirs, kept, conflicts []string
}

// planRestore lists every change from cur to target and refuses any that would touch content no checkpoint holds.
func planRestore(ctx context.Context, root, cur, target string) (*restorePlan, error) {
	raw, err := gitList(ctx, root, nil, nil, "diff", "--no-color", "--no-ext-diff", "--raw", "-z", "--no-renames", cur, target)
	if err != nil {
		return nil, err
	}
	if len(raw)%2 != 0 {
		return nil, errTruncated
	}
	plan := &restorePlan{}
	var dels []string
	for i := 0; i < len(raw); i += 2 {
		f := strings.Fields(raw[i])
		p := raw[i+1]
		if len(f) < 5 || !strings.HasPrefix(f[0], ":") || !filepath.IsLocal(p) {
			return nil, fmt.Errorf("unexpected git diff entry %q %q", raw[i], p)
		}
		if f[0] == ":160000" || f[1] == "160000" {
			continue
		}
		switch f[4][:1] {
		case "D":
			dels = append(dels, p)
		case "A":
			plan.adds = append(plan.adds, p)
		default:
			plan.mods = append(plan.mods, p)
		}
	}
	ign, err := checkIgnored(ctx, root, nil, dels)
	if err != nil {
		return nil, err
	}
	tign, err := targetIgnored(ctx, root, target, dels)
	if err != nil {
		return nil, err
	}
	curList, err := gitList(ctx, root, nil, nil, "ls-tree", "-r", "-z", "--name-only", "--full-tree", cur)
	if err != nil {
		return nil, err
	}
	inCur := map[string]bool{}
	for _, p := range curList {
		inCur[p] = true
	}
	cc := caseCache{}
	delSet := map[string]bool{}
	for _, p := range dels {
		if ign[p] || tign[p] {
			plan.kept = append(plan.kept, p)
			continue
		}
		if q := symlinkLead(root, p); q != "" {
			plan.conflicts = append(plan.conflicts, q+" (a symlink in the path of "+p+")")
			continue
		}
		plan.dels = append(plan.dels, p)
		delSet[p] = true
	}
	gone := func(p string) bool { return delSet[cc.realCase(root, p)] }
	for _, p := range append(append([]string{}, plan.adds...), plan.mods...) {
		parts := strings.Split(p, "/")
		for i := range parts {
			q := strings.Join(parts[:i+1], "/")
			fi, err := os.Lstat(filepath.Join(root, filepath.FromSlash(q)))
			if err != nil {
				break
			}
			last := i == len(parts)-1
			switch {
			case fi.IsDir() && !last:
				continue
			case fi.IsDir():
				if bad := dirNotGone(root, q, cc, delSet); bad != "" {
					plan.conflicts = append(plan.conflicts, bad)
				} else {
					plan.emptyDirs = append(plan.emptyDirs, q)
				}
			case !last:
				if !gone(q) {
					plan.conflicts = append(plan.conflicts, q+" (in the way of "+p+")")
				}
			default:
				if real := cc.realCase(root, q); !delSet[real] && !(inCur[real] && real == p) {
					plan.conflicts = append(plan.conflicts, q)
				}
			}
			break
		}
	}
	return plan, nil
}

// symlinkLead returns the first leading component of rel that is a symlink or not a directory, or "".
func symlinkLead(root, rel string) string {
	parts := strings.Split(rel, "/")
	for i := 0; i < len(parts)-1; i++ {
		q := strings.Join(parts[:i+1], "/")
		fi, err := os.Lstat(filepath.Join(root, filepath.FromSlash(q)))
		if err != nil {
			return ""
		}
		if !fi.IsDir() {
			return q
		}
	}
	return ""
}

// dirNotGone names a file under dir that the delete step keeps, or "" when only empty directories remain.
func dirNotGone(root, dir string, cc caseCache, delSet map[string]bool) string {
	bad := ""
	_ = filepath.WalkDir(filepath.Join(root, filepath.FromSlash(dir)), func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			bad = dir + " (unreadable)"
			return filepath.SkipAll
		}
		if d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		if !delSet[cc.realCase(root, rel)] {
			bad = rel + " (inside " + dir + ", which the target needs as a file)"
			return filepath.SkipAll
		}
		return nil
	})
	return bad
}

// removeEmptyTree deletes dir and its subdirectories, bottom up, only while they are empty.
func removeEmptyTree(dir string) error {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range ents {
		if !e.IsDir() {
			return fmt.Errorf("%s is not empty", dir)
		}
		if err := removeEmptyTree(filepath.Join(dir, e.Name())); err != nil {
			return err
		}
	}
	return os.Remove(dir)
}

// Restore makes the work tree equal to a checkpoint after a safety checkpoint.
// It never deletes ignored paths or files no checkpoint holds, and checks every path before it changes one.
func (m *OrchManager) Restore(id, ref string) (*orchRestoreResult, error) {
	th, err := m.store.Thread(id)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(ref) == "" || ref == "worktree" {
		return nil, orchBadRequest("ref is required")
	}
	if activeStatus(th.Status) {
		return nil, orchConflict("a run is active; interrupt it before restoring")
	}
	root := gitTop(th.Cwd)
	if root == "" {
		return nil, orchConflict("not a git repository")
	}
	unlock := m.git.lockRepo(root)
	defer unlock()
	if busy := m.repoBusy(root); busy != "" {
		return nil, busyErr(busy)
	}
	ctx, cancel := context.WithTimeout(context.Background(), orchGitLongTimeout)
	defer cancel()
	target, err := m.resolveTree(ctx, th, root, ref)
	if err != nil {
		return nil, err
	}
	idx := 0
	if d, err := m.store.ThreadDetail(id); err == nil && len(d.Runs) > 0 {
		idx = d.Runs[len(d.Runs)-1].Index
	}
	safety, _, err := m.checkpointLocked(th, root, idx, "restore", true)
	if err != nil {
		return nil, fmt.Errorf("safety checkpoint: %w", err)
	}
	res := &orchRestoreResult{SafetyRef: safety.Ref, Kept: []string{}}
	cur, err := treeOf(ctx, root, safety.Sha)
	if err != nil {
		return nil, err
	}
	plan, err := planRestore(ctx, root, cur, target)
	if err != nil {
		return nil, fmt.Errorf("restore stopped before any change: %w", err)
	}
	if len(plan.conflicts) > 0 {
		m.gitNote(id, "restore", fmt.Sprintf("Restore to %s stopped before any change: %d paths hold files that no checkpoint has (ignored files, or symlinks). Move them, then restore again.", ref, len(plan.conflicts)), false)
		return nil, &orchGitFail{code: 409, msg: "restore stopped before any change: these paths hold files that no checkpoint has", output: strings.Join(plan.conflicts, "\n")}
	}
	fail := func(step string, err error) (*orchRestoreResult, error) {
		m.gitNote(id, "restore", fmt.Sprintf("Restore to %s failed while it %s: %v. The work tree is partly restored; safety checkpoint: %s", ref, step, err, safety.Ref), false)
		return nil, &orchGitFail{code: 500, msg: "restore failed part way; safety checkpoint " + safety.Ref, output: err.Error()}
	}
	for _, p := range plan.dels {
		full := filepath.Join(root, filepath.FromSlash(p))
		if err := os.Remove(full); err != nil && !os.IsNotExist(err) {
			return fail("deleted files", err)
		}
		removeEmptyParents(root, full)
		res.Deleted++
	}
	for _, d := range plan.emptyDirs {
		if err := removeEmptyTree(filepath.Join(root, filepath.FromSlash(d))); err != nil && !os.IsNotExist(err) {
			return fail("removed directories", err)
		}
	}
	if len(plan.adds)+len(plan.mods) > 0 {
		tmp, done, err := tempIndex(ctx, root, false)
		if err != nil {
			return fail("prepared the index", err)
		}
		defer done()
		env := []string{"GIT_INDEX_FILE=" + tmp}
		if _, _, err := (gitRun{ctx: ctx, dir: root, env: env}).run("read-tree", target); err != nil {
			return fail("read the target tree", err)
		}
		for _, set := range []struct {
			paths []string
			args  []string
		}{{plan.adds, []string{"checkout-index", "-z", "--stdin"}}, {plan.mods, []string{"checkout-index", "-f", "-z", "--stdin"}}} {
			if len(set.paths) == 0 {
				continue
			}
			in := []byte(strings.Join(set.paths, "\x00") + "\x00")
			if _, _, err := (gitRun{ctx: ctx, dir: root, env: env, stdin: in, timeout: orchGitLongTimeout}).run(set.args...); err != nil {
				return fail("wrote files", err)
			}
		}
	}
	cc := caseCache{}
	var missing []string
	for _, p := range append(append([]string{}, plan.adds...), plan.mods...) {
		if _, err := os.Lstat(filepath.Join(root, filepath.FromSlash(p))); err != nil || cc.realCase(root, p) != p {
			missing = append(missing, p)
		}
	}
	if len(missing) > 0 {
		return fail("checked the result", fmt.Errorf("not written: %s", strings.Join(missing, ", ")))
	}
	res.Written = len(plan.adds) + len(plan.mods)
	res.Kept = append(res.Kept, plan.kept...)
	note := fmt.Sprintf("Restored the work tree to %s (%d written, %d deleted). Safety checkpoint: %s", ref, res.Written, res.Deleted, safety.Ref)
	if len(plan.kept) > 0 {
		note += fmt.Sprintf(". Kept %d ignored paths: %s", len(plan.kept), clipString(strings.Join(plan.kept, ", "), 400))
	}
	m.gitNote(id, "restore", note, true)
	return res, nil
}

// gitNote appends one "other" item to the thread so the chat shows a git action.
func (m *OrchManager) gitNote(id, command, text string, ok bool) {
	status := "completed"
	if !ok {
		status = "failed"
	}
	_ = m.store.write(func(t *orchTx) error {
		var runID string
		_ = t.tx.QueryRow(`SELECT id FROM runs WHERE thread_id=? ORDER BY idx DESC LIMIT 1`, id).Scan(&runID)
		it := &OItem{ID: orchID("og_"), ThreadID: id, RunID: runID, Kind: "other", Status: status, Command: command, Text: text, StartedAt: t.now, EndedAt: t.now}
		return (&orchBase{m: m, id: id}).emitItem(t, it)
	})
}
