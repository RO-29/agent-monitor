package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const userCmdClaude = `{"type":"user","cwd":"/tmp/demo","timestamp":"2026-11-10T10:00:00Z","message":{"role":"user","content":"<bash-input>make deploy-timerange</bash-input>"}}
{"type":"user","cwd":"/tmp/demo","timestamp":"2026-11-10T10:00:02Z","message":{"role":"user","content":"<bash-stdout>deployed 3 shards</bash-stdout><bash-stderr></bash-stderr>"}}
{"type":"user","cwd":"/tmp/demo","timestamp":"2026-11-10T10:00:05Z","message":{"role":"user","content":"look at the shards"}}
{"type":"assistant","cwd":"/tmp/demo","timestamp":"2026-11-10T10:00:06Z","message":{"role":"assistant","content":[{"type":"text","text":"Looking."}]}}
{"type":"user","cwd":"/tmp/demo","timestamp":"2026-11-10T10:00:10Z","message":{"role":"user","content":"<command-name>/model</command-name>\n<command-message>model</command-message>\n<command-args>opus</command-args>"}}
{"type":"user","cwd":"/tmp/demo","timestamp":"2026-11-10T10:00:11Z","message":{"role":"user","content":"<local-command-stdout>Set model to opus</local-command-stdout>"}}
`

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func userSpans(tr *SessionTrace) []Span {
	var out []Span
	for _, s := range tr.Spans {
		if s.By == "user" {
			out = append(out, s)
		}
	}
	return out
}

func TestClaudeTraceShowsUserCommands(t *testing.T) {
	sid := "21111111-2222-3333-4444-555555555555"
	path := filepath.Join(t.TempDir(), "-tmp-demo", sid+".jsonl")
	writeFile(t, path, userCmdClaude)
	sess := &Session{ID: "claude:" + sid, Tool: ToolClaude, SessionID: sid, TranscriptPath: path}
	tr := buildClaudeTrace(sess)
	u := userSpans(tr)
	if len(u) != 2 {
		t.Fatalf("user spans %+v", u)
	}
	bang, slash := u[0], u[1]
	if bang.Name != "!" || bang.Res != "make deploy-timerange" || bang.Dur != 2000 || bang.Fam != "bash" || bang.Parent == "" {
		t.Fatalf("bang %+v", bang)
	}
	if io := spanIOFor(sess, tr, bang.ID); io.Result != "deployed 3 shards" {
		t.Fatalf("bang io %+v", io)
	}
	if slash.Name != "/model" || slash.Res != "/model opus" || spanIOFor(sess, tr, slash.ID).Result != "Set model to opus" {
		t.Fatalf("slash %+v %+v", slash, spanIOFor(sess, tr, slash.ID))
	}
	starts, _ := turnBounds(tr)
	if len(starts) != 2 {
		t.Fatalf("turns %d: a leading ! command opens one turn, the prompt the next", len(starts))
	}
	for _, s := range tr.Spans {
		if s.ID == slash.Parent && s.Ts > slash.Ts {
			t.Fatal("slash command parent turn starts after it")
		}
	}
}

func TestCodexTraceShowsUserShell(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout-x.jsonl")
	writeFile(t, path, strings.Join([]string{
		`{"timestamp":"2026-11-10T10:00:00Z","type":"session_meta","payload":{"id":"c11","cwd":"/w"}}`,
		`{"timestamp":"2026-11-10T10:00:03Z","type":"event_msg","payload":{"type":"item_completed","item":{"type":"CommandExecution","id":"us0","command":["/bin/zsh","-lc","make deploy-timerange"],"cwd":"file:///w","source":"user_shell","exit_code":2,"aggregated_output":"no target","duration":{"secs":1,"nanos":0}}}}`,
	}, "\n")+"\n")
	sess := &Session{ID: "codex:c11", Tool: ToolCodex, SessionID: "c11", TranscriptPath: path}
	tr := buildCodexTrace(sess)
	u := userSpans(tr)
	if len(u) != 1 || u[0].Name != "!" || u[0].Res != "make deploy-timerange" || !u[0].Err || u[0].Dur != 1000 || spanIOFor(sess, tr, u[0].ID).Result != "no target" {
		t.Fatalf("user spans %+v", u)
	}
	if starts, _ := turnBounds(tr); len(starts) != 1 || starts[0] > u[0].Ts {
		t.Fatalf("turn starts %v, span at %d", starts, u[0].Ts)
	}
}

func TestSearchFindsUserCommands(t *testing.T) {
	ix, dir := newTestIndex(t)
	path := filepath.Join(dir, "projects", "-tmp-demo", "31111111-2222-3333-4444-555555555555.jsonl")
	writeFile(t, path, userCmdClaude)
	if err := ix.indexFile(path, "claude"); err != nil {
		t.Fatal(err)
	}
	cpath := filepath.Join(dir, "rollout-2026-11-10T10-00-00-01a1df10-633b-7031-a610-56ef0beef44e.jsonl")
	writeFile(t, cpath, strings.Join([]string{
		`{"timestamp":"2026-11-10T10:00:00Z","type":"session_meta","payload":{"id":"01a1df10-633b-7031-a610-56ef0beef44e","cwd":"/tmp/codexrepo"}}`,
		`{"timestamp":"2026-11-10T10:00:03Z","type":"event_msg","payload":{"type":"item_completed","item":{"type":"CommandExecution","id":"us9","command":["/bin/zsh","-lc","git push"],"cwd":"file:///tmp/codexrepo","source":"user_shell","exit_code":0,"aggregated_output":"ok"}}}`,
	}, "\n")+"\n")
	if err := ix.indexFile(cpath, "codex"); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, ix, `SELECT COUNT(*) FROM docs WHERE kind='command' AND tool='user'`); n != 3 {
		t.Fatalf("user command docs %d, want 3 (!, /model, codex !)", n)
	}
	res, err := ix.search(parseSearchQuery("deploy-timerange", nil), "", "", 20, 0, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, h := range res.Hits {
		if h.Kind == "command" && h.Tool == "user" {
			found = true
		}
	}
	if !found {
		t.Fatalf("no user command hit for deploy-timerange: %+v", res.Hits)
	}
	res, err = ix.search(parseSearchQuery("tool:user", nil), "", "", 20, 0, time.Now())
	if err != nil || res.Facets["tool"]["user"] == 0 && len(res.Hits) == 0 {
		t.Fatalf("tool:user %+v %v", res, err)
	}
}
