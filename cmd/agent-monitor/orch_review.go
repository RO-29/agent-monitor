package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	orchReviewMarker   = "Review the changes another agent made"
	orchReviewPatchCap = 60 << 10
)

type orchReviewIn struct {
	Agent string `json:"agent"`
	Model string `json:"model"`
	Focus string `json:"focus"`
	Run   *int   `json:"run"`
}

// diffHint names a git command that shows the same diff, for the reviewer to read in full.
func diffHint(th *OThread, root string, d *orchDiff) string {
	ref := func(spec string) string {
		switch spec {
		case "", "worktree":
			return ""
		case "head":
			return "HEAD"
		case "base":
			if _, err := treeOf(context.Background(), root, baseRef(th.ID)); err == nil {
				return baseRef(th.ID)
			}
			return "HEAD"
		}
		if strings.HasPrefix(spec, "refs/") {
			return spec
		}
		return cpPrefix(th.ID) + spec
	}
	from, to := ref(d.From), ref(d.To)
	if to == "" {
		return "git diff " + from + " (tracked files; untracked files are listed above)"
	}
	return "git diff " + from + " " + to
}

func reviewPrompt(th *OThread, task, focus, hint string, d *orchDiff) string {
	var b strings.Builder
	b.WriteString(orchReviewMarker + " in this repository. Do not edit any file: read files and run read-only commands only.\n\n")
	fmt.Fprintf(&b, "Thread: %s\n", th.Title)
	if task != "" {
		fmt.Fprintf(&b, "Task given to that agent:\n%s\n", clipString(task, 2000))
	}
	if focus != "" {
		fmt.Fprintf(&b, "\nReview focus: %s\n", clipString(focus, 2000))
	}
	fmt.Fprintf(&b, "\nChanged files (%d):\n", len(d.Files))
	for _, f := range d.Files {
		fmt.Fprintf(&b, "- %s (%s, +%d -%d)\n", f.Path, f.Status, f.Add, f.Del)
	}
	b.WriteString("\nDiff:\n```diff\n")
	n, cut := 0, d.Truncated
	for _, f := range d.Files {
		if n+len(f.Patch) > orchReviewPatchCap {
			cut = true
			break
		}
		b.WriteString(f.Patch)
		if !strings.HasSuffix(f.Patch, "\n") {
			b.WriteString("\n")
		}
		n += len(f.Patch)
	}
	b.WriteString("```\n")
	if cut {
		b.WriteString("The diff above is cut. Read the rest with: " + hint + "\n")
	} else {
		b.WriteString("The same diff: " + hint + "\n")
	}
	b.WriteString(`
Look for bugs, regressions, security problems, missing tests and unclear code. Finish with a short summary, then exactly one fenced JSON block in this form:

` + "```json" + `
{"summary": "one paragraph", "findings": [{"severity": "high", "file": "path/to/file.go", "line": 12, "title": "short title", "body": "what is wrong and how to fix it"}]}
` + "```" + `

severity is one of critical, high, medium, low, info. Use "findings": [] when you find no problems.
`)
	return b.String()
}

// StartReview creates a read-only child that reviews the thread's diff and returns it.
func (m *OrchManager) StartReview(id string, in orchReviewIn) (*OThread, error) {
	th, err := m.store.Thread(id)
	if err != nil {
		return nil, err
	}
	if th.ReviewOf != "" {
		return nil, orchBadRequest("a review thread cannot be reviewed")
	}
	if th.Review != nil && th.Review.State == "running" {
		if c, err := m.store.Thread(th.Review.ChildID); err == nil && activeStatus(c.Status) {
			return nil, orchConflict("a review is already running: " + c.ID)
		}
	}
	if in.Agent == "" {
		in.Agent = "codex"
	}
	if !m.validAgent(in.Agent) {
		return nil, m.agentError()
	}
	if in.Run != nil && *in.Run < 0 {
		return nil, orchBadRequest("run must be a run index")
	}
	if err := m.spendGate(""); err != nil {
		return nil, err
	}
	d, err := m.Diff(id, "", "", "", in.Run)
	if err != nil {
		return nil, err
	}
	if len(d.Files) == 0 {
		return nil, orchConflict("nothing to review: the diff is empty")
	}
	task := ""
	if runs := m.runsOf(id); len(runs) > 0 {
		task = runs[0].Prompt
	}
	prompt := reviewPrompt(th, task, in.Focus, diffHint(th, gitTop(th.Cwd), d), d)

	child, err := m.newThread(orchCreate{Agent: in.Agent, Cwd: th.Cwd, Model: in.Model, Sandbox: "read-only"}, "")
	if err != nil {
		return nil, err
	}
	child.ParentID, child.Depth, child.Workspace = th.ID, th.Depth+1, "read-only"
	child.Title = orchTitle("Review: " + firstOf(th.Title, th.ID))
	child.Status, child.NotifyParent, child.ReviewOf = "starting", boolPtr(false), th.ID
	if err := m.insertThread(child); err != nil {
		return nil, err
	}
	now := time.Now().UnixMilli()
	rv := &OReview{State: "running", ChildID: child.ID, Agent: in.Agent, Run: in.Run, Focus: in.Focus, Findings: []OFinding{}, StartedAt: now}
	m.writeReview(th.ID, rv, "started", fmt.Sprintf("Review by %s started (%d files)", in.Agent, len(d.Files)))
	if _, err := m.startThread(child.ID, prompt); err != nil {
		rv.State, rv.Error, rv.EndedAt = "failed", err.Error(), time.Now().UnixMilli()
		m.writeReview(th.ID, rv, "failed", "Review could not start: "+err.Error())
		return nil, err
	}
	return m.store.Thread(child.ID)
}

func (m *OrchManager) runsOf(id string) []ORun {
	d, err := m.store.ThreadDetail(id)
	if err != nil {
		return nil
	}
	return d.Runs
}

// writeReview stores the review on the thread and upserts its "review" item.
func (m *OrchManager) writeReview(id string, rv *OReview, status, text string) {
	b := &orchBase{m: m, id: id}
	_ = m.store.write(func(t *orchTx) error {
		th, err := t.thread(id)
		if err != nil {
			return err
		}
		th.Review = rv
		b.putThreadTx(t, th)
		itemID := rv.ChildID + ".review"
		it, err := t.item(itemID)
		if err != nil {
			var runID string
			_ = t.tx.QueryRow(`SELECT id FROM runs WHERE thread_id=? ORDER BY idx DESC LIMIT 1`, id).Scan(&runID)
			it = &OItem{ID: itemID, ThreadID: id, RunID: runID, Kind: "review", Command: "review", ChildID: rv.ChildID, StartedAt: t.now}
		}
		it.Status, it.Text = status, text
		if status != "started" {
			it.EndedAt = t.now
		}
		if err := t.putItem(it); err != nil {
			return err
		}
		if status == "started" {
			t.emit(id, "item.started", map[string]any{"item": it})
		} else {
			t.emit(id, "item.completed", map[string]any{"item": it})
		}
		return nil
	})
}

// afterReviewRun parses a review child's final answer into its parent's review.
func (m *OrchManager) afterReviewRun(childID string) {
	child, err := m.store.Thread(childID)
	if err != nil || child.ReviewOf == "" || activeStatus(child.Status) {
		return
	}
	parent, err := m.store.Thread(child.ReviewOf)
	if err != nil || parent.Review == nil || parent.Review.ChildID != childID {
		return
	}
	res := m.taskResults([]OThread{*child})[0]
	rv := *parent.Review
	rv.EndedAt = time.Now().UnixMilli()
	if res.Status != "completed" {
		rv.State, rv.Error = "failed", firstOf(res.Result, "review "+res.Status)
		if res.Status == "closed" || res.Status == "interrupted" {
			rv.Error = "review " + res.Status
		}
		m.writeReview(parent.ID, &rv, "failed", "Review "+res.Status)
		return
	}
	summary, findings := parseReview(res.Result)
	rv.State, rv.Summary, rv.Findings, rv.Error = "done", summary, findings, ""
	m.writeReview(parent.ID, &rv, "completed", reviewLine(findings, summary))
	m.notifyReview(parent, &rv)
}

func reviewLine(findings []OFinding, summary string) string {
	if len(findings) == 0 {
		return "Review done: no findings. " + clipString(firstLine(summary), 300)
	}
	counts := map[string]int{}
	for _, f := range findings {
		counts[f.Severity]++
	}
	var parts []string
	for _, s := range orchSeverities {
		if counts[s] > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", counts[s], s))
		}
	}
	return fmt.Sprintf("Review done: %d findings (%s)", len(findings), strings.Join(parts, ", "))
}

func (m *OrchManager) notifyReview(parent *OThread, rv *OReview) {
	if m.notifier == nil {
		return
	}
	m.notifier(notifyEvent{Kind: "finished", Title: "Review of " + firstOf(parent.Title, parent.ID) + " finished",
		Body: reviewLine(rv.Findings, rv.Summary), ThreadID: parent.ID})
}

// maybeAutoReview starts the template review once, after a root run of a thread that asked for one.
func (m *OrchManager) maybeAutoReview(id string) {
	th, err := m.store.Thread(id)
	if err != nil || th.AutoReview == "" || th.Review != nil || th.Status != "idle" {
		return
	}
	runs := m.runsOf(id)
	if len(runs) == 0 || runs[len(runs)-1].Status != "completed" {
		return
	}
	if kids, err := m.store.Subtree(id); err == nil {
		for _, k := range kids {
			if activeStatus(k.Status) {
				return
			}
		}
	}
	if _, err := m.StartReview(id, orchReviewIn{Agent: th.AutoReview}); err != nil {
		log.Printf("orch: auto review %s: %v", id, err)
		m.gitNote(id, "review", "Automatic review not started: "+err.Error(), false)
		_, _ = m.UpdateMeta(id, func(t *OThread, now int64) {
			t.Review = &OReview{State: "failed", Agent: th.AutoReview, Findings: []OFinding{}, Error: err.Error(), StartedAt: now, EndedAt: now}
		})
	}
}

// ── parsing ──

var (
	orchSeverities = []string{"critical", "high", "medium", "low", "info"}
	fenceRe        = regexp.MustCompile("(?s)```([A-Za-z0-9_-]*)[^\\n]*\\n(.*?)\\n?```")
	trailingComma  = regexp.MustCompile(`,(\s*[}\]])`)
	headingRe      = regexp.MustCompile(`^#{2,4}\s+(.*)$`)
	fileLineRe     = regexp.MustCompile("`?([A-Za-z0-9_./-]+\\.[A-Za-z0-9]+):(\\d+)`?")
	sevWordRe      = regexp.MustCompile(`(?i)\b(critical|blocker|high|major|medium|moderate|warning|low|minor|nit|info)\b`)
)

func normSeverity(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "critical", "blocker", "p0", "severe":
		return "critical"
	case "high", "major", "error", "p1", "important":
		return "high"
	case "medium", "moderate", "warning", "warn", "p2":
		return "medium"
	case "low", "minor", "p3":
		return "low"
	case "info", "nit", "note", "suggestion", "style", "p4":
		return "info"
	}
	return "medium"
}

func anyString(m map[string]any, keys ...string) string {
	for _, k := range keys {
		switch v := m[k].(type) {
		case string:
			if v != "" {
				return v
			}
		case float64:
			return strconv.FormatFloat(v, 'f', -1, 64)
		}
	}
	return ""
}

func anyLine(v any) int {
	switch x := v.(type) {
	case float64:
		return int(x)
	case string:
		digits := strings.TrimLeft(x, "L")
		end := 0
		for end < len(digits) && digits[end] >= '0' && digits[end] <= '9' {
			end++
		}
		n, _ := strconv.Atoi(digits[:end])
		return n
	case map[string]any:
		return anyLine(firstNonNil(x["start"], x["line"]))
	case []any:
		if len(x) > 0 {
			return anyLine(x[0])
		}
	}
	return 0
}

func firstNonNil(vs ...any) any {
	for _, v := range vs {
		if v != nil {
			return v
		}
	}
	return nil
}

func findingFrom(m map[string]any) (OFinding, bool) {
	f := OFinding{
		Severity: normSeverity(anyString(m, "severity", "level", "priority")),
		File:     anyString(m, "file", "path", "filename"),
		Line:     anyLine(firstNonNil(m["line"], m["lines"], m["start_line"], m["startLine"])),
		Title:    anyString(m, "title", "summary", "message", "issue", "name"),
		Body:     anyString(m, "body", "description", "detail", "details", "explanation", "suggestion", "fix"),
	}
	if f.File != "" && f.Line == 0 {
		if file, line, ok := strings.Cut(f.File, ":"); ok {
			if n := anyLine(line); n > 0 {
				f.File, f.Line = file, n
			}
		}
	}
	if f.Title == "" {
		f.Title = clipString(firstLine(f.Body), 200)
	}
	return f, f.Title != ""
}

// decodeFindings reads a findings object ({summary, findings}) or a bare findings array.
func decodeFindings(raw string) (string, []OFinding, bool) {
	var v any
	dec := json.NewDecoder(strings.NewReader(raw))
	if err := dec.Decode(&v); err != nil {
		fixed := trailingComma.ReplaceAllString(raw, "$1")
		if err := json.NewDecoder(strings.NewReader(fixed)).Decode(&v); err != nil {
			return "", nil, false
		}
	}
	var list []any
	summary := ""
	switch x := v.(type) {
	case map[string]any:
		summary = anyString(x, "summary", "overview", "verdict")
		found := false
		for _, k := range []string{"findings", "issues", "comments", "problems"} {
			if l, ok := x[k].([]any); ok {
				list, found = l, true
				break
			}
		}
		if !found {
			return "", nil, false
		}
	case []any:
		list = x
	default:
		return "", nil, false
	}
	out := []OFinding{}
	for _, it := range list {
		if m, ok := it.(map[string]any); ok {
			if f, ok := findingFrom(m); ok {
				out = append(out, f)
			}
		}
	}
	return summary, out, true
}

// parseReview finds the findings in a reviewer's answer: a fenced JSON block first, a bare JSON object next, headings last.
func parseReview(text string) (string, []OFinding) {
	fences := fenceRe.FindAllStringSubmatchIndex(text, -1)
	for i := len(fences) - 1; i >= 0; i-- {
		f := fences[i]
		lang := strings.ToLower(text[f[2]:f[3]])
		body := strings.TrimSpace(text[f[4]:f[5]])
		if lang != "json" && lang != "" && lang != "jsonc" {
			continue
		}
		if !strings.HasPrefix(body, "{") && !strings.HasPrefix(body, "[") {
			continue
		}
		if summary, findings, ok := decodeFindings(body); ok {
			return firstOf(summary, proseBefore(text, f[0])), findings
		}
	}
	if i := strings.LastIndex(text, `"findings"`); i >= 0 {
		if j := strings.LastIndex(text[:i], "{"); j >= 0 {
			if summary, findings, ok := decodeFindings(text[j:]); ok {
				return firstOf(summary, proseBefore(text, j)), findings
			}
		}
	}
	return headingFindings(text)
}

func proseBefore(text string, end int) string {
	return clipString(strings.TrimSpace(text[:end]), 2000)
}

// headingFindings treats each ## heading as one finding; a "Summary" heading or the lead text is the summary.
func headingFindings(text string) (string, []OFinding) {
	findings := []OFinding{}
	var lead, summary []string
	var cur *OFinding
	var body []string
	inSummary := false
	flush := func() {
		if cur != nil {
			cur.Body = strings.TrimSpace(strings.Join(body, "\n"))
			if cur.File == "" {
				if m := fileLineRe.FindStringSubmatch(cur.Body); m != nil {
					cur.File, cur.Line = m[1], anyLine(m[2])
				}
			}
			findings = append(findings, *cur)
		}
		cur, body = nil, nil
	}
	for _, line := range strings.Split(text, "\n") {
		if m := headingRe.FindStringSubmatch(strings.TrimSpace(line)); m != nil {
			flush()
			title := strings.TrimSpace(m[1])
			low := strings.ToLower(strings.Trim(title, "*: "))
			if low == "summary" || low == "overview" || low == "verdict" {
				inSummary = true
				continue
			}
			if low == "findings" || low == "issues" {
				inSummary = false
				continue
			}
			inSummary = false
			f := OFinding{Severity: "medium", Title: title}
			if s := sevWordRe.FindString(title); s != "" {
				f.Severity = normSeverity(s)
				f.Title = strings.TrimSpace(strings.Trim(strings.Replace(title, s, "", 1), " -:[]()*"))
				f.Title = strings.TrimSpace(strings.Trim(f.Title, " -:[]()*"))
			}
			if fm := fileLineRe.FindStringSubmatch(title); fm != nil {
				f.File, f.Line = fm[1], anyLine(fm[2])
			}
			if f.Title == "" {
				f.Title = title
			}
			cur = &f
			continue
		}
		switch {
		case cur != nil:
			body = append(body, line)
		case inSummary:
			summary = append(summary, line)
		default:
			lead = append(lead, line)
		}
	}
	flush()
	s := strings.TrimSpace(strings.Join(summary, "\n"))
	if s == "" {
		s = strings.TrimSpace(strings.Join(lead, "\n"))
	}
	return clipString(s, 2000), findings
}

// ── merge ──

type orchMergeIn struct {
	Into     string `json:"into"`
	Strategy string `json:"strategy"`
	Message  string `json:"message"`
}

type orchMergeResult struct {
	Sha      string   `json:"sha"`
	Into     string   `json:"into"`
	Branch   string   `json:"branch"`
	Strategy string   `json:"strategy"`
	Applied  string   `json:"applied"` // checkout (fast-forwarded a clean checkout) | ref (branch not checked out)
	Thread   *OThread `json:"thread"`
}

// orchMergeConflict is a 409 that lists the conflicted files.
type orchMergeConflict struct {
	files []string
}

func (e *orchMergeConflict) Error() string {
	return "merge conflicts in " + strings.Join(e.files, ", ")
}

// gitVersion returns the installed git's major and minor version.
func gitVersion(ctx context.Context) (int, int, string) {
	out, _ := git(ctx, os.TempDir(), "version")
	v := strings.TrimPrefix(out, "git version ")
	word, _, _ := strings.Cut(v, " ")
	f := strings.SplitN(word, ".", 3)
	if len(f) < 2 {
		return 0, 0, out
	}
	maj, _ := strconv.Atoi(f[0])
	mi, _ := strconv.Atoi(f[1])
	return maj, mi, v
}

// noHooksEnv turns off every git hook for one call, the reference-transaction hook included.
var noHooksEnv = []string{"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=core.hooksPath", "GIT_CONFIG_VALUE_0=" + os.DevNull}

// checkoutsOf returns the worktrees that have branch checked out; a worktree in a rebase or bisect
// of branch is an error, because moving the branch under it would break that operation.
func checkoutsOf(ctx context.Context, dir, branch string) ([]string, error) {
	out, err := git(ctx, dir, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, err
	}
	ref := "refs/heads/" + branch
	var paths []string
	for _, block := range strings.Split(out, "\n\n") {
		path, has := "", false
		for _, line := range strings.Split(block, "\n") {
			if p, ok := strings.CutPrefix(line, "worktree "); ok {
				path = p
			}
			if b, ok := strings.CutPrefix(line, "branch "); ok && b == ref {
				has = true
			}
		}
		if path == "" || !pathExists(path) {
			continue
		}
		if has {
			paths = append(paths, path)
		}
		gd, err := git(ctx, path, "rev-parse", "--absolute-git-dir")
		if err != nil {
			continue
		}
		for _, f := range []string{"rebase-merge/head-name", "rebase-apply/head-name"} {
			if b, err := os.ReadFile(filepath.Join(gd, f)); err == nil && strings.TrimSpace(string(b)) == ref {
				return nil, orchConflict(branch + " is being rebased in " + path + "; finish or abort the rebase first")
			}
		}
		if b, err := os.ReadFile(filepath.Join(gd, "BISECT_START")); err == nil {
			if v := strings.TrimSpace(string(b)); v == branch || v == ref {
				return nil, orchConflict(branch + " is being bisected in " + path + "; run git bisect reset first")
			}
		}
	}
	return paths, nil
}

// addedInTheWay lists paths that the commit range adds where something already exists in the checkout.
// The checkout is clean, so such a file is ignored, and a fast-forward would overwrite it.
func addedInTheWay(ctx context.Context, checkout, from, to string) ([]string, error) {
	added, err := gitList(ctx, checkout, nil, nil, "diff", "--no-color", "--no-ext-diff", "--name-only", "-z", "--no-renames", "--diff-filter=A", from, to)
	if err != nil {
		return nil, err
	}
	var hits []string
	for _, p := range added {
		parts := strings.Split(p, "/")
		for i := range parts {
			q := strings.Join(parts[:i+1], "/")
			fi, err := os.Lstat(filepath.Join(checkout, filepath.FromSlash(q)))
			if err != nil {
				break
			}
			if fi.IsDir() && i < len(parts)-1 {
				continue
			}
			hits = append(hits, q)
			break
		}
	}
	return hits, nil
}

// Merge uses git merge-tree and a compare-and-swap ref update; it runs no hooks and never pushes.
// A clean checked-out target is fast-forwarded with read-tree.
func (m *OrchManager) Merge(id string, in orchMergeIn) (*orchMergeResult, error) {
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
	switch in.Strategy {
	case "":
		in.Strategy = "squash"
	case "squash", "merge":
	default:
		return nil, orchBadRequest("strategy must be merge or squash")
	}
	wt, branch := th.Worktree.Path, th.Worktree.Branch
	if !pathExists(wt) {
		return nil, orchConflict("worktree " + wt + " is gone")
	}
	ctx, cancel := context.WithTimeout(context.Background(), orchGitLongTimeout)
	defer cancel()
	if maj, mi, v := gitVersion(ctx); maj < 2 || (maj == 2 && mi < 38) {
		return nil, &orchHTTPError{http.StatusUnprocessableEntity, "merge needs git 2.38 or later (git merge-tree --write-tree); this git is " + firstOf(v, "unknown")}
	}
	unlock := m.git.lockRepo(wt)
	defer unlock()
	if st, err := git(ctx, wt, "status", "--porcelain"); err != nil {
		return nil, gitFail(422, "git status failed", err)
	} else if st != "" {
		return nil, &orchGitFail{code: 409, msg: "worktree has uncommitted changes: commit them first", output: st}
	}
	into := strings.TrimSpace(in.Into)
	if into == "" {
		into = th.Worktree.OriginBranch
	}
	if into == "" && th.Worktree.Origin != "" {
		into, _ = git(ctx, th.Worktree.Origin, "symbolic-ref", "--short", "-q", "HEAD")
	}
	if into == "" {
		into = defaultBranch(ctx, wt)
	}
	if into == "" || strings.HasPrefix(into, "-") {
		return nil, orchBadRequest("into must name a branch")
	}
	if _, err := git(ctx, wt, "check-ref-format", "--branch", into); err != nil {
		return nil, orchBadRequest("bad branch name " + into)
	}
	if into == branch {
		return nil, orchBadRequest("into is the thread's own branch")
	}
	intoSha, err := git(ctx, wt, "rev-parse", "--verify", "-q", "refs/heads/"+into+"^{commit}")
	if err != nil || intoSha == "" {
		return nil, &orchHTTPError{http.StatusNotFound, "unknown branch " + into}
	}
	srcSha, err := git(ctx, wt, "rev-parse", "--verify", "-q", "refs/heads/"+branch+"^{commit}")
	if err != nil || srcSha == "" {
		return nil, orchConflict("branch " + branch + " is gone")
	}
	if _, err := git(ctx, wt, "merge-base", "--is-ancestor", srcSha, intoSha); err == nil {
		return nil, orchConflict("nothing to merge: " + into + " already contains " + branch)
	}
	targets, err := checkoutsOf(ctx, wt, into)
	if err != nil {
		return nil, err
	}
	if len(targets) > 1 {
		return nil, orchConflict(into + " is checked out in several worktrees: " + strings.Join(targets, ", "))
	}
	target := ""
	if len(targets) == 1 {
		target = targets[0]
	}
	roots := []string{gitTop(wt)}
	if target != "" {
		roots = append(roots, gitTop(target))
	}
	if busy := m.repoBusy(roots...); busy != "" {
		return nil, busyErr(busy)
	}
	if target != "" {
		if st, err := git(ctx, target, "status", "--porcelain"); err != nil {
			return nil, gitFail(422, "git status failed", err)
		} else if st != "" {
			return nil, &orchGitFail{code: 409, msg: "the checkout of " + into + " at " + target + " has uncommitted changes", output: st}
		}
	}
	msg := strings.TrimSpace(in.Message)
	if msg == "" {
		if in.Strategy == "merge" {
			msg = fmt.Sprintf("Merge branch '%s' into %s", branch, into)
		} else {
			msg = firstOf(th.Title, "Squash merge of "+branch)
		}
	}
	long := gitRun{ctx: ctx, dir: wt, env: noHooksEnv, timeout: orchGitLongTimeout}
	out, _, err := long.run("merge-tree", "--write-tree", "-z", "--name-only", "--no-messages", intoSha, srcSha)
	parts := strings.Split(out, "\x00")
	var ge *gitError
	if err != nil && errors.As(err, &ge) && ge.code == 1 && !ge.timeout {
		var files []string
		for _, f := range parts[1:] {
			if f == "" {
				break
			}
			if !slices.Contains(files, f) {
				files = append(files, f)
			}
		}
		m.gitNote(id, "git merge", fmt.Sprintf("Merge of %s into %s stopped: conflicts in %s; %s is unchanged", branch, into, strings.Join(files, ", "), into), false)
		return nil, &orchMergeConflict{files: files}
	}
	if err != nil {
		m.gitNote(id, "git merge", "Merge failed: "+clipString(err.Error(), 500), false)
		return nil, gitFail(422, "git merge-tree failed", err)
	}
	tree := strings.TrimSpace(parts[0])
	if in.Strategy == "squash" {
		if t, _ := treeOf(ctx, wt, intoSha); t == tree {
			return nil, orchConflict("nothing to merge: the changes are already in " + into)
		}
	}
	ctArgs := []string{"commit-tree", tree, "-p", intoSha}
	if in.Strategy == "merge" {
		ctArgs = append(ctArgs, "-p", srcSha)
	}
	cout, _, err := long.run(append(ctArgs, "-m", msg)...)
	if err != nil {
		m.gitNote(id, "git merge", "Merge commit failed: "+clipString(err.Error(), 500), false)
		return nil, gitFail(422, "git commit-tree failed", err)
	}
	newSha := strings.TrimSpace(cout)
	applied := "ref"
	refArgs := []string{"update-ref", "-m", "agent-monitor merge", "refs/heads/" + into, newSha, intoSha}
	if target != "" {
		tr := gitRun{ctx: ctx, dir: target, env: noHooksEnv, timeout: orchGitLongTimeout}
		if st, _ := git(ctx, target, "status", "--porcelain"); st != "" {
			return nil, &orchGitFail{code: 409, msg: "the checkout of " + into + " changed during the merge", output: st}
		}
		if h := headSha(ctx, target); h != intoSha {
			return nil, orchConflict(into + " moved during the merge; nothing was applied")
		}
		hits, err := addedInTheWay(ctx, target, intoSha, newSha)
		if err != nil {
			return nil, gitFail(422, "git diff failed", err)
		}
		if len(hits) > 0 {
			m.gitNote(id, "git merge", fmt.Sprintf("Merge of %s into %s stopped: the checkout at %s has ignored files where the merge adds files: %s", branch, into, target, clipString(strings.Join(hits, ", "), 400)), false)
			return nil, &orchGitFail{code: 409, msg: "the checkout of " + into + " at " + target + " has ignored files that the merge would overwrite; nothing was applied", output: strings.Join(hits, "\n")}
		}
		if _, _, err := tr.run("read-tree", "-m", "-u", intoSha, newSha); err != nil {
			return nil, gitFail(409, "could not update the checkout of "+into+"; nothing was applied", err)
		}
		if _, _, err := tr.run(refArgs...); err != nil {
			_, _, _ = tr.run("read-tree", "-m", "-u", newSha, intoSha)
			return nil, gitFail(409, into+" moved during the merge; nothing was applied", err)
		}
		applied = "checkout"
	} else if _, _, err := long.run(refArgs...); err != nil {
		return nil, gitFail(409, into+" moved during the merge; nothing was applied", err)
	}
	m.gitNote(id, "git merge", fmt.Sprintf("Merged %s into %s (%s) as %s", branch, into, in.Strategy, shortSha(newSha)), true)
	res, err := m.UpdateMeta(id, func(t *OThread, now int64) {
		t.Merge = &OMerge{Into: into, Strategy: in.Strategy, Sha: newSha, At: now}
	})
	if err != nil {
		return nil, err
	}
	return &orchMergeResult{Sha: newSha, Into: into, Branch: branch, Strategy: in.Strategy, Applied: applied, Thread: res}, nil
}

func (m *OrchManager) reviewRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/orch/threads/{id}/review", func(w http.ResponseWriter, r *http.Request) {
		var in orchReviewIn
		if !decodeOptional(w, r, &in) {
			return
		}
		child, err := m.StartReview(r.PathValue("id"), in)
		if err != nil {
			gitWriteErr(w, gitFailOr(err))
			return
		}
		parent, _ := m.store.Thread(r.PathValue("id"))
		writeJSONStatus(w, http.StatusCreated, map[string]any{"threadId": child.ID, "thread": child, "parent": parent})
	})
	mux.HandleFunc("POST /api/orch/threads/{id}/merge", func(w http.ResponseWriter, r *http.Request) {
		var in orchMergeIn
		if !decodeOptional(w, r, &in) {
			return
		}
		res, err := m.Merge(r.PathValue("id"), in)
		var mc *orchMergeConflict
		if errors.As(err, &mc) {
			writeJSONStatus(w, http.StatusConflict, map[string]any{"error": "merge conflicts", "files": mc.files})
			return
		}
		if err != nil {
			gitWriteErr(w, gitFailOr(err))
			return
		}
		writeJSON(w, map[string]any{"ok": true, "sha": res.Sha, "into": res.Into, "branch": res.Branch, "strategy": res.Strategy, "applied": res.Applied, "thread": res.Thread})
	})
}
