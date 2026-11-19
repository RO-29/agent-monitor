package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// orchGitFail is a git command failure that carries the command output to the client.
type orchGitFail struct {
	code   int
	msg    string
	output string
}

func (e *orchGitFail) Error() string { return e.msg + ": " + e.output }

func gitFail(code int, msg string, err error) error {
	out := err.Error()
	var ge *gitError
	if errors.As(err, &ge) {
		out = strings.TrimSpace(ge.out)
	}
	return &orchGitFail{code: code, msg: msg, output: clipString(out, 16<<10)}
}

func gitWriteErr(w http.ResponseWriter, err error) {
	var gf *orchGitFail
	if errors.As(err, &gf) {
		writeJSONStatus(w, gf.code, map[string]any{"error": gf.msg, "output": gf.output})
		return
	}
	orchWriteErr(w, err)
}

// gitThread loads a thread for a mutating git route: it must exist, be idle and sit in a repo.
func (m *OrchManager) gitThread(id string, needIdle bool) (*OThread, string, error) {
	th, err := m.store.Thread(id)
	if err != nil {
		return nil, "", err
	}
	if needIdle && activeStatus(th.Status) {
		return nil, "", orchConflict("a run is active")
	}
	root := gitTop(th.Cwd)
	if root == "" {
		return nil, "", orchConflict("not a git repository")
	}
	return th, root, nil
}

// Commit builds the commit in a private index from HEAD and runs the user's hooks.
// With paths only those are committed; afterwards the real index is updated for the committed paths only.
func (m *OrchManager) Commit(id, message string, paths []string) (string, error) {
	if strings.TrimSpace(message) == "" {
		return "", orchBadRequest("message is required")
	}
	_, root, err := m.gitThread(id, true)
	if err != nil {
		return "", err
	}
	for _, p := range paths {
		if p == "" || strings.HasPrefix(p, "-") || !filepath.IsLocal(p) {
			return "", orchBadRequest("bad path " + p)
		}
	}
	unlock := m.git.lockRepo(root)
	defer unlock()
	if busy := m.repoBusy(root); busy != "" {
		return "", busyErr(busy)
	}
	ctx, cancel := context.WithTimeout(context.Background(), orchGitLongTimeout)
	defer cancel()
	idx, done, err := tempIndex(ctx, root, true)
	if err != nil {
		return "", err
	}
	defer done()
	env := []string{"GIT_INDEX_FILE=" + idx, "GIT_LITERAL_PATHSPECS=1"}
	r := gitRun{ctx: ctx, dir: root, env: env, timeout: orchGitLongTimeout}
	head := headSha(ctx, root)
	headTree := emptyTree(ctx, root)
	if head != "" {
		if headTree, err = treeOf(ctx, root, head); err != nil {
			return "", err
		}
		if _, _, err := r.run("read-tree", "-m", head); err != nil {
			if _, _, err := r.run("read-tree", head); err != nil {
				return "", gitFail(422, "git read-tree failed", err)
			}
		}
	} else if _, _, err := r.run("read-tree", "--empty"); err != nil {
		return "", gitFail(422, "git read-tree failed", err)
	}
	addArgs := []string{"add", "-A"}
	if len(paths) > 0 {
		addArgs = append([]string{"add", "-A", "--"}, paths...)
	}
	if _, _, err := r.run(addArgs...); err != nil {
		m.gitNote(id, "git commit", "git add failed: "+clipString(err.Error(), 500), false)
		return "", gitFail(422, "git add failed", err)
	}
	tree, _, err := r.run("write-tree")
	if err != nil {
		return "", gitFail(422, "git write-tree failed", err)
	}
	if strings.TrimSpace(tree) == headTree {
		return "", &orchGitFail{code: 409, msg: "nothing to commit", output: "the named paths have no changes"}
	}
	if _, _, err := r.run("commit", "-q", "-m", message); err != nil {
		m.gitNote(id, "git commit", "Commit failed: "+clipString(err.Error(), 500), false)
		return "", gitFail(422, "git commit failed", err)
	}
	sha := headSha(ctx, root)
	resetArgs := []string{"reset", "-q", sha}
	if len(paths) > 0 {
		resetArgs = append(append(resetArgs, "--"), paths...)
	}
	subject := strings.TrimSpace(firstLine(message))
	if _, _, err := (gitRun{ctx: ctx, dir: root, env: []string{"GIT_LITERAL_PATHSPECS=1"}}).run(resetArgs...); err != nil {
		m.gitNote(id, "git commit", fmt.Sprintf("Committed %s %s, but the index was not updated: %s", shortSha(sha), clipString(subject, 120), clipString(err.Error(), 300)), false)
		return sha, nil
	}
	m.gitNote(id, "git commit", fmt.Sprintf("Committed %s %s", shortSha(sha), clipString(subject, 120)), true)
	return sha, nil
}

func shortSha(s string) string {
	if len(s) > 9 {
		return s[:9]
	}
	return s
}

func currentBranch(ctx context.Context, root string) (string, error) {
	b, err := git(ctx, root, "symbolic-ref", "--short", "-q", "HEAD")
	if err != nil || b == "" {
		return "", orchConflict("HEAD is detached")
	}
	return b, nil
}

// protectedBranch reports whether a push of b is refused: main, master, the remote's default branch,
// init.defaultBranch, and trunk or develop when the remote's default branch is unknown.
func protectedBranch(ctx context.Context, root, b string) bool {
	if b == "main" || b == "master" {
		return true
	}
	if init, _ := git(ctx, root, "config", "--get", "init.defaultBranch"); init != "" && b == init {
		return true
	}
	remote, err := git(ctx, root, "symbolic-ref", "--short", "-q", "refs/remotes/origin/HEAD")
	if err != nil || remote == "" {
		return b == "trunk" || b == "develop"
	}
	return b == strings.TrimPrefix(remote, "origin/")
}

// Push pushes the current branch to the same name on origin with an explicit refspec, never with force.
// It writes upstream config only when setUpstream is true.
func (m *OrchManager) Push(id string, setUpstream bool) (string, error) {
	_, root, err := m.gitThread(id, false)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), orchGitLongTimeout)
	defer cancel()
	branch, err := currentBranch(ctx, root)
	if err != nil {
		return "", err
	}
	if protectedBranch(ctx, root, branch) {
		return "", &orchHTTPError{http.StatusForbidden, "refusing to push the default branch " + branch}
	}
	args := []string{"push"}
	if setUpstream {
		args = append(args, "--set-upstream")
	}
	args = append(args, "origin", "refs/heads/"+branch+":refs/heads/"+branch)
	out, _, err := gitRun{ctx: ctx, dir: root, timeout: orchGitLongTimeout}.run(args...)
	if err != nil {
		m.gitNote(id, "git push", "Push failed: "+clipString(err.Error(), 500), false)
		return "", gitFail(422, "git push failed", err)
	}
	m.gitNote(id, "git push", "Pushed "+branch+" to origin", true)
	return strings.TrimSpace(out), nil
}

type orchPRIn struct {
	Title string `json:"title"`
	Body  string `json:"body"`
	Draft *bool  `json:"draft"`
	Base  string `json:"base"`
}

func (m *OrchManager) CreatePR(id string, in orchPRIn) (string, error) {
	if strings.TrimSpace(in.Title) == "" {
		return "", orchBadRequest("title is required")
	}
	if strings.HasPrefix(in.Base, "-") {
		return "", orchBadRequest("bad base")
	}
	_, root, err := m.gitThread(id, false)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), orchGitLongTimeout)
	defer cancel()
	branch, err := currentBranch(ctx, root)
	if err != nil {
		return "", err
	}
	if _, err := git(ctx, root, "rev-parse", "--verify", "-q", "refs/remotes/origin/"+branch); err != nil && upstreamOf(ctx, root) == "" {
		return "", orchConflict("the branch is not on origin yet; push first")
	}
	args := []string{"pr", "create", "--head", branch, "--title", in.Title, "--body", in.Body}
	if in.Draft == nil || *in.Draft {
		args = append(args, "--draft")
	}
	if in.Base != "" {
		args = append(args, "--base", in.Base)
	}
	out, err := m.git.launch(ctx, root, m.git.gh, args...)
	if err != nil {
		m.gitNote(id, "gh pr create", "PR failed: "+clipString(strings.TrimSpace(string(out)), 500), false)
		return "", &orchGitFail{code: 422, msg: "gh pr create failed", output: clipString(strings.TrimSpace(string(out))+"\n"+err.Error(), 16<<10)}
	}
	url := ""
	for _, l := range strings.Split(string(out), "\n") {
		if l = strings.TrimSpace(l); strings.HasPrefix(l, "https://") || strings.HasPrefix(l, "http://") {
			url = l
		}
	}
	m.gitNote(id, "gh pr create", "Opened "+firstOf(url, "a pull request"), true)
	return url, nil
}

// localCaller is true only for a loopback peer that no proxy forwarded.
func localCaller(r *http.Request) bool {
	if !isLoopbackRemote(r.RemoteAddr) {
		return false
	}
	for _, h := range []string{"X-Forwarded-For", "X-Real-Ip", "Forwarded", "Tailscale-User-Login"} {
		if r.Header.Get(h) != "" {
			return false
		}
	}
	return true
}

func (m *OrchManager) Open(id, target string) error {
	th, err := m.store.Thread(id)
	if err != nil {
		return err
	}
	cwd := th.Cwd
	if st, err := os.Stat(cwd); err != nil || !st.IsDir() {
		return orchConflict("cwd does not exist")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var name string
	var args []string
	switch target {
	case "editor":
		name, args = "open", []string{"-t", cwd}
		for _, ed := range []string{"cursor", "code"} {
			if p, err := m.git.look(ed); err == nil {
				name, args = p, []string{cwd}
				break
			}
		}
	case "terminal":
		name, args = "open", []string{"-a", "Terminal", cwd}
		if sess := m.recentTmux(ctx); sess != "" {
			name, args = "tmux", []string{"new-window", "-t", sess + ":", "-c", cwd}
		}
	case "finder":
		name, args = "open", []string{cwd}
	default:
		return orchBadRequest("target must be editor, terminal or finder")
	}
	if out, err := m.git.launch(ctx, cwd, name, args...); err != nil {
		return &orchGitFail{code: 422, msg: "open failed", output: strings.TrimSpace(string(out) + "\n" + err.Error())}
	}
	return nil
}

// recentTmux returns the most recently attached tmux session, or "".
func (m *OrchManager) recentTmux(ctx context.Context) string {
	out, err := m.git.launch(ctx, "", "tmux", "list-sessions", "-F", "#{session_last_attached}\t#{session_name}")
	if err != nil {
		return ""
	}
	type sess struct {
		at   int64
		name string
	}
	var all []sess
	for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		at, name, ok := strings.Cut(l, "\t")
		if !ok || name == "" {
			continue
		}
		n, _ := strconv.ParseInt(at, 10, 64)
		all = append(all, sess{n, name})
	}
	if len(all) == 0 {
		return ""
	}
	sort.Slice(all, func(i, j int) bool { return all[i].at > all[j].at })
	return all[0].name
}

// RemoveWorktree takes a forced checkpoint, then runs git worktree remove; the branch is kept.
// Ignored files are deleted, so they need force too.
func (m *OrchManager) RemoveWorktree(id string, force bool) (*OThread, error) {
	th, err := m.store.Thread(id)
	if err != nil {
		return nil, err
	}
	if th.Worktree == nil || th.Worktree.Removed {
		return nil, orchBadRequest("thread has no worktree")
	}
	if activeStatus(th.Status) {
		return nil, orchConflict("a run is active")
	}
	wt := th.Worktree.Path
	ctx, cancel := context.WithTimeout(context.Background(), orchGitLongTimeout)
	defer cancel()
	cpNote := ""
	if pathExists(wt) {
		common, err := git(ctx, wt, "rev-parse", "--path-format=absolute", "--git-common-dir")
		if err != nil {
			return nil, gitFail(422, "not a git worktree", err)
		}
		unlock := m.git.lockRepo(wt)
		defer unlock()
		if top := gitTop(wt); top != "" {
			if busy := m.repoBusy(top); busy != "" {
				return nil, busyErr(busy)
			}
		}
		status, err := git(ctx, wt, "status", "--porcelain", "--ignored")
		if err != nil {
			return nil, gitFail(422, "git status failed", err)
		}
		var changed, ignored []string
		for _, l := range strings.Split(status, "\n") {
			if p, ok := strings.CutPrefix(l, "!! "); ok {
				ignored = append(ignored, p)
			} else if l != "" {
				changed = append(changed, l)
			}
		}
		if !force && len(changed) > 0 {
			return nil, &orchGitFail{code: 409, msg: "worktree has uncommitted changes", output: strings.Join(changed, "\n")}
		}
		if !force && len(ignored) > 0 {
			return nil, &orchGitFail{code: 409, msg: "worktree has ignored files that removal deletes and no checkpoint holds; remove with force to delete them", output: strings.Join(ignored, "\n")}
		}
		idx := 0
		if d, err := m.store.ThreadDetail(id); err == nil && len(d.Runs) > 0 {
			idx = d.Runs[len(d.Runs)-1].Index
		}
		if top := gitTop(wt); top != "" {
			cp, _, err := m.checkpointLocked(th, top, idx, "remove", true)
			if err != nil {
				return nil, fmt.Errorf("checkpoint before removal: %w", err)
			}
			cpNote = "; checkpoint " + cp.Ref
		}
		if len(ignored) > 0 {
			cpNote += fmt.Sprintf("; %d ignored paths deleted", len(ignored))
		}
		args := []string{"worktree", "remove"}
		if force {
			args = append(args, "--force")
		}
		if _, err := git(ctx, common, append(args, wt)...); err != nil {
			m.gitNote(id, "git worktree remove", "Remove failed: "+clipString(err.Error(), 500), false)
			return nil, gitFail(422, "git worktree remove failed", err)
		}
	} else if top := gitTop(filepath.Dir(wt)); top != "" {
		_, _ = git(ctx, top, "worktree", "prune")
	}
	out, err := m.UpdateMeta(id, func(t *OThread, now int64) {
		if t.Worktree != nil {
			t.Worktree.Removed = true
		}
	})
	if err != nil {
		return nil, err
	}
	m.gitNote(id, "git worktree remove", "Removed worktree "+wt+"; branch "+th.Worktree.Branch+" kept"+cpNote, true)
	return out, nil
}

func (m *OrchManager) gitRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/orch/threads/{id}/git", func(w http.ResponseWriter, r *http.Request) {
		info, err := m.GitInfo(r.PathValue("id"))
		if err != nil {
			gitWriteErr(w, gitFailOr(err))
			return
		}
		writeJSON(w, info)
	})
	mux.HandleFunc("GET /api/orch/git", func(w http.ResponseWriter, r *http.Request) {
		cwd := r.URL.Query().Get("cwd")
		if !filepath.IsAbs(cwd) {
			orchWriteErr(w, orchBadRequest("cwd must be an absolute path"))
			return
		}
		if !knownCwd(cwd) {
			orchWriteErr(w, &orchHTTPError{http.StatusForbidden, "cwd is not the folder of a watched session or a driven thread"})
			return
		}
		info, _, err := gitStatus(cwd)
		if err != nil {
			gitWriteErr(w, gitFailOr(err))
			return
		}
		writeJSON(w, info)
	})
	mux.HandleFunc("GET /api/orch/threads/{id}/diff", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		var run *int
		if v := q.Get("run"); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 0 {
				orchWriteErr(w, orchBadRequest("run must be a run index"))
				return
			}
			run = &n
		}
		d, err := m.Diff(r.PathValue("id"), q.Get("from"), q.Get("to"), q.Get("path"), run)
		if err != nil {
			gitWriteErr(w, gitFailOr(err))
			return
		}
		writeJSON(w, d)
	})
	mux.HandleFunc("POST /api/orch/threads/{id}/restore", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Ref string `json:"ref"`
		}
		if !decodeOptional(w, r, &in) {
			return
		}
		res, err := m.Restore(r.PathValue("id"), in.Ref)
		if err != nil {
			gitWriteErr(w, gitFailOr(err))
			return
		}
		writeJSON(w, map[string]any{"ok": true, "safetyRef": res.SafetyRef, "written": res.Written, "deleted": res.Deleted, "kept": res.Kept})
	})
	mux.HandleFunc("POST /api/orch/threads/{id}/commit", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Message string   `json:"message"`
			Paths   []string `json:"paths"`
		}
		if !decodeOptional(w, r, &in) {
			return
		}
		sha, err := m.Commit(r.PathValue("id"), in.Message, in.Paths)
		if err != nil {
			gitWriteErr(w, gitFailOr(err))
			return
		}
		writeJSON(w, map[string]any{"ok": true, "sha": sha})
	})
	mux.HandleFunc("POST /api/orch/threads/{id}/push", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			SetUpstream bool `json:"setUpstream"`
		}
		if !decodeOptional(w, r, &in) {
			return
		}
		out, err := m.Push(r.PathValue("id"), in.SetUpstream)
		if err != nil {
			gitWriteErr(w, gitFailOr(err))
			return
		}
		writeJSON(w, map[string]any{"ok": true, "output": out})
	})
	mux.HandleFunc("POST /api/orch/threads/{id}/pr", func(w http.ResponseWriter, r *http.Request) {
		var in orchPRIn
		if !decodeOptional(w, r, &in) {
			return
		}
		url, err := m.CreatePR(r.PathValue("id"), in)
		if err != nil {
			gitWriteErr(w, gitFailOr(err))
			return
		}
		writeJSON(w, map[string]any{"ok": true, "url": url})
	})
	mux.HandleFunc("POST /api/orch/threads/{id}/open", func(w http.ResponseWriter, r *http.Request) {
		if !localCaller(r) {
			writeJSONStatus(w, http.StatusForbidden, map[string]any{"error": "open works only from this Mac"})
			return
		}
		var in struct {
			Target string `json:"target"`
		}
		if !decodeOptional(w, r, &in) {
			return
		}
		if err := m.Open(r.PathValue("id"), in.Target); err != nil {
			gitWriteErr(w, err)
			return
		}
		writeJSON(w, map[string]any{"ok": true})
	})
	mux.HandleFunc("POST /api/orch/threads/{id}/worktree/remove", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Force bool `json:"force"`
		}
		if !decodeOptional(w, r, &in) {
			return
		}
		th, err := m.RemoveWorktree(r.PathValue("id"), in.Force)
		if err != nil {
			gitWriteErr(w, gitFailOr(err))
			return
		}
		writeJSON(w, map[string]any{"ok": true, "thread": th})
	})
}

// gitFailOr turns a bare git error into a 422 with its output; other errors pass through.
func gitFailOr(err error) error {
	var ge *gitError
	var he *orchHTTPError
	var gf *orchGitFail
	if errors.As(err, &he) || errors.As(err, &gf) || !errors.As(err, &ge) {
		return err
	}
	return gitFail(422, "git failed", err)
}
