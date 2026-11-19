package main

import (
	"bufio"
	"crypto/sha1"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// searchIndex owns search.db: one writer connection (the indexer) and a read pool.
type searchIndex struct {
	w, r     *sql.DB
	path     string
	fts5     bool
	mathFns  bool
	throttle float64 // sleep this many times the work time (3 = 25% duty)

	mu           sync.Mutex
	indexing     bool
	lastRunAt    int64
	firstPassMs  int64
	firstPassAt  int64
	bytesIndexed int64
	totalFiles   int
	states       map[string]*searchFileState
	repoOfCwd    map[string]string
	subMetas     map[string]string // session dir → scanned marker
	metaTool     map[string]string // agent-*.jsonl → toolUseId
}

var searchIx *searchIndex

func searchDBPath() string { return filepath.Join(stateDir(), "search.db") }

func openSearchIndex(path string) (*searchIndex, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	dsn := path + "?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)"
	w, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	w.SetMaxOpenConns(1)
	// Read connections keep a small page cache and close when idle; search is bursty.
	r, err := sql.Open("sqlite", dsn+"&_pragma=query_only(1)&_pragma=cache_size(-512)")
	if err != nil {
		w.Close()
		return nil, err
	}
	r.SetMaxOpenConns(4)
	r.SetMaxIdleConns(1)
	r.SetConnMaxIdleTime(2 * time.Minute)
	ix := &searchIndex{w: w, r: r, path: path, throttle: 3, states: map[string]*searchFileState{}, repoOfCwd: map[string]string{},
		subMetas: map[string]string{}, metaTool: map[string]string{}}
	if _, err := w.Exec(`CREATE VIRTUAL TABLE temp.fts5probe USING fts5(a); DROP TABLE temp.fts5probe;`); err == nil {
		ix.fts5 = true
	} else {
		log.Printf("search: FTS5 unavailable (%v); using LIKE fallback", err)
	}
	if _, err := w.Exec(`SELECT exp(0.5)`); err == nil {
		ix.mathFns = true
	}
	if err := ix.migrate(); err != nil {
		w.Close()
		r.Close()
		return nil, err
	}
	return ix, nil
}

// searchIndexGen bumps when parsing changes what old transcripts index; a lower stored gen rebuilds the index.
const searchIndexGen = 2 // 2: user slash and ! commands

func (ix *searchIndex) migrate() error {
	var gen int
	_ = ix.w.QueryRow(`PRAGMA user_version`).Scan(&gen)
	if gen < searchIndexGen {
		if _, err := ix.w.Exec(`DROP TABLE IF EXISTS docs_fts; DROP TABLE IF EXISTS docs_tri; DROP TABLE IF EXISTS docs;
			DROP TABLE IF EXISTS spans; DROP TABLE IF EXISTS files; PRAGMA user_version = ` + strconv.Itoa(searchIndexGen)); err != nil {
			return err
		}
	}
	_, err := ix.w.Exec(`
		CREATE TABLE IF NOT EXISTS docs (
			id INTEGER PRIMARY KEY, session TEXT, agent TEXT, cwd TEXT, repo TEXT, ts INTEGER,
			kind TEXT, tool TEXT, turn INTEGER, span TEXT, path TEXT, text TEXT);
		CREATE INDEX IF NOT EXISTS docs_session_ts ON docs(session, ts);
		CREATE INDEX IF NOT EXISTS docs_ts ON docs(ts);
		CREATE TABLE IF NOT EXISTS spans (
			id TEXT PRIMARY KEY, session TEXT, agent TEXT, cwd TEXT, repo TEXT, ts INTEGER, dur INTEGER,
			tool TEXT, fam TEXT, cmd TEXT, cmdKey TEXT, err INTEGER, errSig TEXT, errText TEXT, child TEXT, ref TEXT);
		CREATE INDEX IF NOT EXISTS spans_ts ON spans(ts);
		CREATE INDEX IF NOT EXISTS spans_ref ON spans(ref);
		CREATE INDEX IF NOT EXISTS spans_repo ON spans(repo);
		CREATE TABLE IF NOT EXISTS files (
			path TEXT PRIMARY KEY, size INTEGER, mtime INTEGER, offset INTEGER, session TEXT,
			agent TEXT, cwd TEXT, turn INTEGER);
	`)
	if err == nil {
		_, _ = ix.w.Exec(`ALTER TABLE files ADD COLUMN head TEXT`) // fails harmlessly when present
	}
	if err != nil || !ix.fts5 {
		return err
	}
	_, err = ix.w.Exec(`
		CREATE VIRTUAL TABLE IF NOT EXISTS docs_fts USING fts5(text, path, content='docs', content_rowid='id', tokenize='porter unicode61');
		CREATE VIRTUAL TABLE IF NOT EXISTS docs_tri USING fts5(text, path, content='docs', content_rowid='id', tokenize='trigram');
	`)
	return err
}

// ── per-file parse state ──────────────────────────────────────────────────

type searchFileState struct {
	path, agent, session  string
	cwd, repo             string
	turn                  int
	offset, size, mtime   int64
	sub                   bool // Claude subagent transcript
	subName               string
	lastPrompt, lastReply string
	lastPromptTs          int64
	lastReplyTs           int64
	head                  string          // "<n>:<sha1 of first n bytes>" at index time
	skipRef               map[string]bool // replayed tool ids whose span already exists
	closedRef             map[string]bool // refs whose result was already indexed
}

type searchDoc struct {
	Session, Agent, Cwd, Repo string
	Ts                        int64
	Kind, Tool                string
	Turn                      int
	Span, Path, Text          string
}

type searchSpan struct {
	ID, Session, Agent, Cwd, Repo string
	Ts, Dur                       int64
	Tool, Fam, Cmd, CmdKey        string
	Err                           bool
	ErrSig, ErrText, Child, Ref   string
}

// spanResult closes an open span by ref and indexes its output.
type spanResult struct {
	Ref    string
	Ts     int64
	Dur    int64 // explicit duration when the record carries one
	IsErr  bool
	Code   int // exit code when known, -1 unknown
	Text   string
	Turn   int
	Detect bool // run exit-code / error-pattern detection on the text
}

type searchOp struct {
	doc   *searchDoc
	span  *searchSpan
	docs  []searchDoc // docs that follow a span insert (skipped when the span is a replay)
	res   *spanResult
	child [2]string // span ref → child id
}

// ── indexer loop ──────────────────────────────────────────────────────────

func startSearchIndexer() {
	ix, err := openSearchIndex(searchDBPath())
	if err != nil {
		log.Printf("search: open failed: %v", err)
		return
	}
	searchIx = ix
	go ix.run()
}

func (ix *searchIndex) run() {
	first := true
	for {
		start := time.Now()
		ix.pass()
		ix.drivenPass()
		if first {
			ix.mu.Lock()
			ix.firstPassMs = time.Since(start).Milliseconds()
			ix.firstPassAt = time.Now().UnixMilli()
			ix.mu.Unlock()
			log.Printf("search: first pass done in %s", time.Since(start).Round(time.Second))
			first = false
		}
		publishV3("search.status", "status", ix.status())
		time.Sleep(20 * time.Second)
	}
}

type searchFile struct {
	path  string
	agent string
	size  int64
	mtime int64
}

func listTranscripts() []searchFile {
	var out []searchFile
	add := func(p, agent string) {
		st, err := os.Stat(p)
		if err != nil || st.IsDir() {
			return
		}
		out = append(out, searchFile{path: p, agent: agent, size: st.Size(), mtime: st.ModTime().UnixMilli()})
	}
	cl := filepath.Join(homeDir(), ".claude", "projects")
	for _, pat := range []string{"*/*.jsonl", "*/*/subagents/*.jsonl"} {
		ms, _ := filepath.Glob(filepath.Join(cl, pat))
		for _, m := range ms {
			add(m, "claude")
		}
	}
	cx := filepath.Join(homeDir(), ".codex", "sessions")
	_ = filepath.WalkDir(cx, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasPrefix(d.Name(), "rollout-") && strings.HasSuffix(d.Name(), ".jsonl") {
			add(p, "codex")
		}
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].mtime > out[j].mtime })
	return out
}

func (ix *searchIndex) pass() {
	ix.mu.Lock()
	ix.indexing = true
	ix.mu.Unlock()
	defer func() {
		ix.mu.Lock()
		ix.indexing = false
		ix.lastRunAt = time.Now().UnixMilli()
		ix.mu.Unlock()
	}()
	files := listTranscripts()
	known := ix.loadFileRows()
	ix.mu.Lock()
	ix.totalFiles = len(files)
	ix.mu.Unlock()
	for _, f := range files {
		if k, ok := known[f.path]; ok && k[0] == f.size && k[1] == f.mtime {
			continue
		}
		if err := ix.indexFile(f.path, f.agent); err != nil {
			log.Printf("search: index %s: %v", filepath.Base(f.path), err)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func (ix *searchIndex) loadFileRows() map[string][2]int64 {
	out := map[string][2]int64{}
	rows, err := ix.w.Query(`SELECT path, size, mtime FROM files`)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var p string
		var s, m int64
		if rows.Scan(&p, &s, &m) == nil {
			out[p] = [2]int64{s, m}
		}
	}
	return out
}

func (ix *searchIndex) fileState(path, agent string) *searchFileState {
	if st := ix.states[path]; st != nil {
		return st
	}
	st := &searchFileState{path: path, agent: agent, skipRef: map[string]bool{}, closedRef: map[string]bool{}}
	var sess, cwd, head sql.NullString
	var turn, off sql.NullInt64
	if ix.w.QueryRow(`SELECT session, cwd, turn, offset, head FROM files WHERE path=?`, path).Scan(&sess, &cwd, &turn, &off, &head) == nil {
		st.session, st.cwd, st.turn, st.offset, st.head = sess.String, cwd.String, int(turn.Int64), off.Int64, head.String
		st.repo = ix.repoFor(st.cwd)
	}
	if agent == "claude" {
		dir := filepath.Dir(path)
		if filepath.Base(dir) == "subagents" {
			st.sub = true
			st.subName = strings.TrimSuffix(filepath.Base(path), ".jsonl")
			st.session = "claude:" + filepath.Base(filepath.Dir(dir))
		} else {
			st.session = "claude:" + strings.TrimSuffix(filepath.Base(path), ".jsonl")
		}
	} else if st.session == "" {
		st.session = "codex:" + codexIDFromPath(path)
	}
	ix.states[path] = st
	return st
}

func codexIDFromPath(p string) string {
	b := strings.TrimSuffix(filepath.Base(p), ".jsonl")
	if len(b) >= 36 {
		return b[len(b)-36:]
	}
	return b
}

// repoFor is the basename of the git root above cwd, else of cwd.
func (ix *searchIndex) repoFor(cwd string) string {
	if cwd == "" {
		return ""
	}
	if r, ok := ix.repoOfCwd[cwd]; ok {
		return r
	}
	r := filepath.Base(cwd)
	for d := cwd; d != "/" && d != "." && d != ""; d = filepath.Dir(d) {
		if d == homeDir() {
			break
		}
		if pathExists(filepath.Join(d, ".git")) {
			r = filepath.Base(d)
			break
		}
	}
	ix.repoOfCwd[cwd] = r
	return r
}

const (
	searchFlushOps   = 400
	searchFlushBytes = 2 << 20
	searchFlushTime  = 150 * time.Millisecond
)

// indexFile parses the complete lines appended since the stored offset.
func (ix *searchIndex) indexFile(path, agent string) error {
	st := ix.fileState(path, agent)
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	cur := fileHead(f, info.Size())
	rewritten := st.offset > 0 && (info.Size() < st.offset || headChanged(st.head, f, info.Size()))
	if rewritten {
		if err := ix.purgeSession(st.session); err != nil {
			return err
		}
	}
	st.head = cur
	st.size, st.mtime = info.Size(), info.ModTime().UnixMilli()
	if st.sub {
		ix.linkSubagent(st)
	}
	if _, err := f.Seek(st.offset, io.SeekStart); err != nil {
		return err
	}
	rd := bufio.NewReaderSize(f, 512*1024)
	var ops []searchOp
	off := st.offset
	pending := int64(0)
	work := time.Now()
	flush := func() error {
		if err := ix.apply(st, ops, off); err != nil {
			return err
		}
		ix.mu.Lock()
		ix.bytesIndexed += pending
		ix.mu.Unlock()
		ops, pending = ops[:0], 0
		ix.sleepFor(time.Since(work))
		work = time.Now()
		return nil
	}
	for {
		line, err := rd.ReadBytes('\n')
		if err != nil {
			break
		}
		off += int64(len(line))
		pending += int64(len(line))
		if agent == "claude" {
			ops = ix.parseClaudeLine(st, line, ops)
		} else {
			ops = ix.parseCodexLine(st, line, ops)
		}
		if len(ops) >= searchFlushOps || pending >= searchFlushBytes || time.Since(work) > searchFlushTime {
			if err := flush(); err != nil {
				return err
			}
		}
	}
	return flush()
}

// sleepFor keeps the indexer near 1/(1+throttle) of one core.
func (ix *searchIndex) sleepFor(work time.Duration) {
	if ix.throttle <= 0 {
		return
	}
	d := time.Duration(float64(work) * ix.throttle)
	if d > 2*time.Second {
		d = 2 * time.Second
	}
	time.Sleep(d)
}

func (ix *searchIndex) purgeSession(session string) error {
	tx, err := ix.w.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if ix.fts5 {
		rows, err := tx.Query(`SELECT id, kind, text, path FROM docs WHERE session=?`, session)
		if err != nil {
			return err
		}
		type row struct {
			id               int64
			kind, text, path string
		}
		var all []row
		for rows.Next() {
			var r row
			if rows.Scan(&r.id, &r.kind, &r.text, &r.path) == nil {
				all = append(all, r)
			}
		}
		rows.Close()
		for _, r := range all {
			if _, err := tx.Exec(`INSERT INTO docs_fts(docs_fts, rowid, text, path) VALUES('delete', ?, ?, ?)`, r.id, r.text, r.path); err != nil {
				return err
			}
			if _, err := tx.Exec(`INSERT INTO docs_tri(docs_tri, rowid, text, path) VALUES('delete', ?, ?, ?)`, r.id, triText(r.kind, r.text), r.path); err != nil {
				return err
			}
		}
	}
	for _, q := range []string{`DELETE FROM docs WHERE session=?`, `DELETE FROM spans WHERE session=?`, `UPDATE files SET offset=0, turn=0, size=0, mtime=0 WHERE session=?`} {
		if _, err := tx.Exec(q, session); err != nil {
			return err
		}
	}
	for _, s := range ix.states {
		if s.session == session {
			s.offset, s.turn, s.size, s.mtime = 0, 0, 0, 0
			s.lastPrompt, s.lastReply, s.lastPromptTs, s.lastReplyTs = "", "", 0, 0
			s.skipRef, s.closedRef = map[string]bool{}, map[string]bool{}
		}
	}
	return tx.Commit()
}

// triText clips long outputs for the trigram index, which is ~3x the text size.
func triText(kind, text string) string {
	n := 2048
	if kind == "output" || kind == "error" {
		n = 1024
	}
	return clipString(text, n)
}

// apply writes one batch and the new file offset in a single transaction.
func (ix *searchIndex) apply(st *searchFileState, ops []searchOp, off int64) error {
	tx, err := ix.w.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	insDoc, err := tx.Prepare(`INSERT INTO docs(session, agent, cwd, repo, ts, kind, tool, turn, span, path, text) VALUES(?,?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		return err
	}
	defer insDoc.Close()
	var insFts, insTri *sql.Stmt
	if ix.fts5 {
		if insFts, err = tx.Prepare(`INSERT INTO docs_fts(rowid, text, path) VALUES(?,?,?)`); err != nil {
			return err
		}
		defer insFts.Close()
		if insTri, err = tx.Prepare(`INSERT INTO docs_tri(rowid, text, path) VALUES(?,?,?)`); err != nil {
			return err
		}
		defer insTri.Close()
	}
	insSpan, err := tx.Prepare(`INSERT OR IGNORE INTO spans(id, session, agent, cwd, repo, ts, dur, tool, fam, cmd, cmdKey, err, errSig, errText, child, ref) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		return err
	}
	defer insSpan.Close()
	writeDoc := func(d *searchDoc) error {
		if strings.TrimSpace(d.Text) == "" && d.Path == "" {
			return nil
		}
		res, err := insDoc.Exec(d.Session, d.Agent, d.Cwd, d.Repo, d.Ts, d.Kind, d.Tool, d.Turn, d.Span, d.Path, d.Text)
		if err != nil {
			return err
		}
		if !ix.fts5 {
			return nil
		}
		id, _ := res.LastInsertId()
		if _, err := insFts.Exec(id, d.Text, d.Path); err != nil {
			return err
		}
		_, err = insTri.Exec(id, triText(d.Kind, d.Text), d.Path)
		return err
	}
	for i := range ops {
		op := &ops[i]
		switch {
		case op.span != nil:
			s := op.span
			res, err := insSpan.Exec(s.ID, s.Session, s.Agent, s.Cwd, s.Repo, s.Ts, s.Dur, s.Tool, s.Fam, s.Cmd, s.CmdKey, b2i(s.Err), s.ErrSig, s.ErrText, s.Child, s.Ref)
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n == 0 {
				st.skipRef[s.Ref] = true
				continue
			}
			for j := range op.docs {
				if err := writeDoc(&op.docs[j]); err != nil {
					return err
				}
			}
		case op.res != nil:
			if err := ix.applyResult(tx, st, op.res, writeDoc); err != nil {
				return err
			}
		case op.child[0] != "":
			if _, err := tx.Exec(`UPDATE spans SET child=? WHERE ref=? AND (child IS NULL OR child='')`, op.child[1], op.child[0]); err != nil {
				return err
			}
		case op.doc != nil:
			if err := writeDoc(op.doc); err != nil {
				return err
			}
		}
	}
	if _, err := tx.Exec(`INSERT INTO files(path, size, mtime, offset, session, agent, cwd, turn, head) VALUES(?,?,?,?,?,?,?,?,?)
		ON CONFLICT(path) DO UPDATE SET size=excluded.size, mtime=excluded.mtime, offset=excluded.offset,
		session=excluded.session, agent=excluded.agent, cwd=excluded.cwd, turn=excluded.turn, head=excluded.head`,
		st.path, sizeAt(st, off), st.mtime, off, st.session, st.agent, st.cwd, st.turn, st.head); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	st.offset = off
	return nil
}

// sizeAt records the real size only once the whole file is consumed, so a crash mid-file re-scans.
func sizeAt(st *searchFileState, off int64) int64 {
	if off >= st.size {
		return st.size
	}
	return -1
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

func (ix *searchIndex) applyResult(tx *sql.Tx, st *searchFileState, r *spanResult, writeDoc func(*searchDoc) error) error {
	if st.skipRef[r.Ref] || st.closedRef[r.Ref] {
		return nil
	}
	var id, tool, fam string
	var ts int64
	err := tx.QueryRow(`SELECT id, tool, fam, ts FROM spans WHERE ref=? AND session=?`, r.Ref, st.session).Scan(&id, &tool, &fam, &ts)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	st.closedRef[r.Ref] = true
	isErr := r.IsErr
	if r.Detect && !isErr {
		isErr = resultLooksFailed(fam, r.Code, r.Text)
	}
	dur := r.Dur
	if dur <= 0 && r.Ts > ts {
		dur = r.Ts - ts
	}
	sig, etext := "", ""
	if isErr {
		sig, etext = errorSignature(r.Text)
		if sig == "" && r.Code > 0 {
			sig = "exit code N (no output)"
		}
	}
	if _, err := tx.Exec(`UPDATE spans SET dur=?, err=?, errSig=?, errText=? WHERE id=?`, dur, b2i(isErr), sig, etext, id); err != nil {
		return err
	}
	kind := "output"
	if isErr {
		kind = "error"
	}
	return writeDoc(&searchDoc{Session: st.session, Agent: st.agent, Cwd: st.cwd, Repo: st.repo, Ts: r.Ts, Kind: kind, Tool: tool,
		Turn: r.Turn, Span: id, Text: clipString(r.Text, 4096)})
}

// linkSubagent reads agent-*.meta.json to tie a subagent file to its Task span.
func (ix *searchIndex) linkSubagent(st *searchFileState) {
	if _, ok := ix.metaTool[st.path]; ok {
		return
	}
	raw, err := os.ReadFile(strings.TrimSuffix(st.path, ".jsonl") + ".meta.json")
	var meta struct {
		ToolUseID string `json:"toolUseId"`
	}
	if err == nil {
		_ = json.Unmarshal(raw, &meta)
	}
	ix.metaTool[st.path] = meta.ToolUseID
	if meta.ToolUseID != "" {
		_, _ = ix.w.Exec(`UPDATE spans SET child=? WHERE ref=? AND (child IS NULL OR child='')`, "subagent:"+st.subName, meta.ToolUseID)
	}
}

// ── result classification ─────────────────────────────────────────────────

var (
	reExitCode    = regexp.MustCompile(`(?i)(?:exit code:?|exited with code)\s*(-?\d+)`)
	reErrLine     = regexp.MustCompile(`(?i)FAIL|Error|error:|✕|panic|Traceback|exit code`)
	reSigQuoted   = regexp.MustCompile("\"[^\"]*\"|'[^']*'|`[^`]*`")
	reSigPath     = regexp.MustCompile(`[\w.~@-]*(?:/[\w.@+-]+)+/?`)
	reSigHex      = regexp.MustCompile(`\b0x[0-9a-fA-F]+\b|\b[0-9a-f]{7,}\b`)
	reSigNum      = regexp.MustCompile(`\d+(?:\.\d+)?`)
	reSigSpace    = regexp.MustCompile(`\s+`)
	reSigTimeLike = regexp.MustCompile(`\b\d{4}-\d{2}-\d{2}[T ]\d{2}:\d{2}:\d{2}(?:\.\d+)?Z?\b`)
)

// resultLooksFailed applies the exit-code and error-pattern rules to a tool result.
func resultLooksFailed(fam string, code int, text string) bool {
	if code > 0 {
		return true
	}
	if code == 0 {
		return false
	}
	head := text[:min(len(text), 300)]
	if m := reExitCode.FindStringSubmatch(head); m != nil {
		n, _ := strconv.Atoi(m[1])
		return n != 0
	}
	if fam == "bash" {
		fl := strings.ToLower(firstLine(head))
		return strings.HasPrefix(fl, "error:") || strings.Contains(fl, " error: ")
	}
	return false
}

// errorSignature returns the normalized first failing line and the raw failing lines.
func errorSignature(text string) (sig, sample string) {
	lines := strings.Split(text, "\n")
	idx := -1
	for i, ln := range lines {
		if i > 200 {
			break
		}
		if reErrLine.MatchString(ln) && !isExitCodeOnly(ln) {
			idx = i
			break
		}
	}
	if idx < 0 {
		for i, ln := range lines {
			if i > 200 {
				break
			}
			if reErrLine.MatchString(ln) {
				idx = i
				break
			}
		}
	}
	if idx < 0 {
		idx = 0
		for i, ln := range lines {
			if strings.TrimSpace(ln) != "" {
				idx = i
				break
			}
		}
	}
	if idx >= len(lines) {
		return "", ""
	}
	end := min(idx+3, len(lines))
	sample = clipString(strings.TrimSpace(strings.Join(lines[idx:end], "\n")), 400)
	return normalizeErrSig(lines[idx]), sample
}

func isExitCodeOnly(ln string) bool {
	t := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(ln), "Error:"))
	return reExitCode.MatchString(t) && len(reExitCode.ReplaceAllString(t, "")) < 3
}

func normalizeErrSig(ln string) string {
	s := strings.TrimSpace(ln)
	s = reSigTimeLike.ReplaceAllString(s, "<ts>")
	s = reSigQuoted.ReplaceAllString(s, "<q>")
	s = reSigPath.ReplaceAllString(s, "<path>")
	s = reSigHex.ReplaceAllString(s, "<hex>")
	s = reSigNum.ReplaceAllString(s, "N")
	s = reSigSpace.ReplaceAllString(s, " ")
	return clipString(s, 160)
}

// ── command keys ──────────────────────────────────────────────────────────

var (
	reCmdEnv    = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=\S*$`)
	reCmdHash   = regexp.MustCompile(`\b[0-9a-f]{7,}\b`)
	reCmdDigits = regexp.MustCompile(`\d+`)
	cmdSubTools = map[string]bool{"git": true, "go": true, "npm": true, "pnpm": true, "yarn": true, "kubectl": true, "gcloud": true, "docker": true, "cargo": true, "gh": true, "make": true, "terraform": true, "helm": true, "bun": true, "npx": true, "uv": true, "pip": true, "brew": true, "codex": true, "claude": true, "tmux": true, "wrangler": true, "bq": true, "gsutil": true, "atlas": true, "firebase": true, "aws": true, "node": false}
	cmdWrappers = map[string]bool{"sudo": true, "time": true, "env": true, "nohup": true, "command": true, "exec": true, "timeout": true}
)

// commandKey groups commands: program + up to two subcommands, args collapsed.
func commandKey(cmd string) string {
	c := strings.TrimSpace(firstLine(cmd))
	for {
		i, w := strings.Index(c, "&&"), 2
		if j := strings.Index(c, ";"); j >= 0 && (i < 0 || j < i) {
			i, w = j, 1
		}
		if i < 0 || !strings.HasPrefix(c, "cd ") && !strings.HasPrefix(c, "export ") && !strings.HasPrefix(c, ". ") && !strings.HasPrefix(c, "source ") {
			break
		}
		c = strings.TrimSpace(c[i+w:])
	}
	if i := strings.IndexAny(c, "|;&><"); i > 0 {
		c = c[:i]
	}
	toks := strings.Fields(c)
	for len(toks) > 0 && (reCmdEnv.MatchString(toks[0]) || cmdWrappers[toks[0]]) {
		if toks[0] == "timeout" && len(toks) > 1 {
			toks = toks[1:]
		}
		toks = toks[1:]
	}
	if len(toks) == 0 {
		return ""
	}
	prog := filepath.Base(strings.Trim(toks[0], "\"'"))
	out := []string{prog}
	maxTok := 2
	if cmdSubTools[prog] {
		maxTok = 3
	}
	for _, t := range toks[1:] {
		if len(out) >= maxTok {
			break
		}
		if strings.HasPrefix(t, "-") || strings.ContainsAny(t, "\"'$=`") {
			break
		}
		switch {
		case strings.Contains(t, "/") || strings.HasPrefix(t, "~"):
			t = "<path>"
		case reCmdHash.MatchString(t):
			t = reCmdHash.ReplaceAllString(t, "<hash>")
		case reCmdDigits.MatchString(t):
			t = reCmdDigits.ReplaceAllString(t, "N")
		}
		out = append(out, t)
		if t == "<path>" {
			break
		}
	}
	return strings.Join(out, " ")
}

// ── status ────────────────────────────────────────────────────────────────

type searchStatus struct {
	FTS5         bool  `json:"fts5"`
	IndexedFiles int   `json:"indexedFiles"`
	TotalFiles   int   `json:"totalFiles"`
	Docs         int64 `json:"docs"`
	Spans        int64 `json:"spans"`
	Indexing     bool  `json:"indexing"`
	LastRunAt    int64 `json:"lastRunAt"`
	FirstPassMs  int64 `json:"firstPassMs"`
	BytesIndexed int64 `json:"bytesIndexed"`
	DBBytes      int64 `json:"dbBytes"`
}

func (ix *searchIndex) status() searchStatus {
	ix.mu.Lock()
	s := searchStatus{FTS5: ix.fts5, TotalFiles: ix.totalFiles, Indexing: ix.indexing, LastRunAt: ix.lastRunAt,
		FirstPassMs: ix.firstPassMs, BytesIndexed: ix.bytesIndexed}
	ix.mu.Unlock()
	var n int
	_ = ix.r.QueryRow(`SELECT COUNT(*) FROM files WHERE size >= 0`).Scan(&n)
	s.IndexedFiles = n
	_ = ix.r.QueryRow(`SELECT COALESCE(MAX(id),0) FROM docs`).Scan(&s.Docs)
	_ = ix.r.QueryRow(`SELECT COUNT(*) FROM spans`).Scan(&s.Spans)
	for _, suf := range []string{"", "-wal"} {
		if st, err := os.Stat(ix.path + suf); err == nil {
			s.DBBytes += st.Size()
		}
	}
	return s
}

const headBytes = 1024

// fileHead fingerprints the first bytes of a transcript so a same-path rewrite is detected.
func fileHead(f *os.File, size int64) string {
	n := min(size, headBytes)
	buf := make([]byte, n)
	if _, err := f.ReadAt(buf, 0); err != nil && n > 0 {
		return ""
	}
	sum := sha1.Sum(buf)
	return strconv.FormatInt(n, 10) + ":" + hex.EncodeToString(sum[:])
}

// headChanged compares the stored fingerprint against the same byte count of the current file.
func headChanged(stored string, f *os.File, size int64) bool {
	ns, h, ok := strings.Cut(stored, ":")
	n, err := strconv.ParseInt(ns, 10, 64)
	if !ok || err != nil || h == "" || size < n {
		return false
	}
	buf := make([]byte, n)
	if _, err := f.ReadAt(buf, 0); err != nil && n > 0 {
		return false
	}
	sum := sha1.Sum(buf)
	return hex.EncodeToString(sum[:]) != h
}
