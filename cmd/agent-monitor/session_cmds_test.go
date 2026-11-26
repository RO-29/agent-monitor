package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func cmdClaudeFixture(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "claude-cmds.jsonl")
	jsonl(t, path,
		cUser(1, "Go"),
		cUser(2, "<bash-input>ls -la</bash-input>"),
		cUser(3, "<bash-stdout>\x1b[1mtotal 0\x1b[0m\nfile &amp; more</bash-stdout><bash-stderr></bash-stderr>"),
		cUser(4, "<bash-input>cat nope</bash-input>"),
		cUser(5, "<bash-stdout></bash-stdout><bash-stderr>cat: nope: No such file</bash-stderr>"),
		claudeRec("user", 6, map[string]any{"role": "user", "content": "<bash-input>side</bash-input>"}, map[string]any{"isSidechain": true}),
		cUser(7, "<bash-input>sleep 999</bash-input>"),
		bgMoved(8, "/tmp/claude-1/x/s/tasks/bk1x2y3z4.output"),
		cUser(9, "<task-notification>\n<task-id>bk1x2y3z4</task-id>\n<status>completed</status>\n<summary>Background command \"sleep 999\" completed (exit code 2)</summary>\n</task-notification>"),
		cUser(10, "<bash-input>"+strings.Repeat("x", 10)+"</bash-input><bash-stdout>"+strings.Repeat("é", 2000)+"</bash-stdout><bash-stderr></bash-stderr>"),
	)
	return path
}

func cmdGet(t *testing.T, path string) []CmdItem {
	t.Helper()
	rec := httptest.NewRecorder()
	if !watchedSessionRoute(rec, httptest.NewRequest("GET", path, nil), strings.TrimPrefix(strings.SplitN(path, "?", 2)[0], "/api/session/")) {
		t.Fatalf("route not handled: %s", path)
	}
	var out struct {
		Items []CmdItem `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	return out.Items
}

func TestTranscriptCmdsClaude(t *testing.T) {
	path := cmdClaudeFixture(t, t.TempDir())
	items := transcriptCmds(path, ToolClaude)
	if len(items) != 4 {
		t.Fatalf("want 4 commands (sidechain skipped), got %+v", items)
	}
	if it := items[0]; it.Cmd != "ls -la" || it.Output != "total 0\nfile & more" || it.ExitCode != nil || it.At != parseTimestamp(tsAt(2)) || it.Bg {
		t.Fatalf("ls %+v", it)
	}
	if it := items[1]; it.Cmd != "cat nope" || it.Output != "cat: nope: No such file" {
		t.Fatalf("cat %+v", it)
	}
	if it := items[2]; it.Cmd != "sleep 999" || !it.Bg || it.ExitCode == nil || *it.ExitCode != 2 || !strings.Contains(it.Output, "moved to the background") {
		t.Fatalf("bg %+v", it)
	}
	if it := items[3]; len(it.Output) > cmdOutputMax || len(it.Output) < cmdOutputMax-1 || !strings.HasPrefix(it.Output, "éé") || strings.ContainsRune(it.Output, '�') {
		t.Fatalf("output cap %d", len(it.Output))
	}

	// Appended lines are read incrementally; the pending command gets its output from the next line.
	appendRecs(t, path, cUser(11, "<bash-input>pwd</bash-input>"))
	if items = transcriptCmds(path, ToolClaude); len(items) != 5 || items[4].Cmd != "pwd" || items[4].Output != "" {
		t.Fatalf("append input %+v", items)
	}
	appendRecs(t, path, cUser(12, "<bash-stdout>/w</bash-stdout><bash-stderr></bash-stderr>"))
	if items = transcriptCmds(path, ToolClaude); len(items) != 5 || items[4].Output != "/w" {
		t.Fatalf("append output %+v", items)
	}

	// A rewritten (smaller) file is read again from the start.
	jsonl(t, path, cUser(1, "<bash-input>whoami</bash-input>"))
	if items = transcriptCmds(path, ToolClaude); len(items) != 1 || items[0].Cmd != "whoami" {
		t.Fatalf("rewrite %+v", items)
	}
}

func TestTranscriptCmdsCodex(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout-cmds.jsonl")
	ms := func(sec int) int64 { return parseTimestamp(tsAt(sec)) }
	jsonl(t, path,
		map[string]any{"type": "session_meta", "timestamp": tsAt(0), "payload": map[string]any{"id": "c9"}},
		codexEv(1, map[string]any{"type": "user_message", "message": "Fix it"}),
		codexDone(3, map[string]any{"type": "CommandExecution", "id": "us1", "command": []any{"/bin/zsh", "-lc", "git status"}, "cwd": "file:///w",
			"source": "user_shell", "status": "completed", "aggregated_output": "clean\n", "exit_code": 0}, ms(3)),
		codexDone(4, map[string]any{"type": "CommandExecution", "id": "a1", "command": []any{"/bin/zsh", "-lc", "agent cmd"},
			"status": "completed", "aggregated_output": "x", "exit_code": 0}, ms(4)),
		codexDone(5, map[string]any{"type": "CommandExecution", "id": "us2", "command": "make test", "cwd": "file:///w",
			"source": "user_shell", "status": "completed", "aggregated_output": "FAIL\n", "exit_code": 127}, ms(5)),
	)
	items := transcriptCmds(path, ToolCodex)
	if len(items) != 2 {
		t.Fatalf("want 2 user commands, got %+v", items)
	}
	if it := items[0]; it.Cmd != "git status" || it.Output != "clean" || it.ExitCode == nil || *it.ExitCode != 0 {
		t.Fatalf("git %+v", it)
	}
	if it := items[1]; it.Cmd != "make test" || it.ExitCode == nil || *it.ExitCode != 127 || it.At == 0 {
		t.Fatalf("make %+v", it)
	}
}

func TestSessionCommandsRoute(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	prev := store
	t.Cleanup(func() { store = prev })
	store = nil
	id := "abcdef12-3456-4789-8abc-def0123456bb"
	dir := filepath.Join(cfg, "projects", "-w")
	_ = os.MkdirAll(dir, 0o755)
	src := cmdClaudeFixture(t, t.TempDir())
	b, _ := os.ReadFile(src)
	if err := os.WriteFile(filepath.Join(dir, id+".jsonl"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	items := cmdGet(t, "/api/session/claude:"+id+"/commands")
	if len(items) != 4 || items[0].Cmd != strings.Repeat("x", 10) || items[3].Cmd != "ls -la" {
		t.Fatalf("newest first: %+v", items)
	}
	if items = cmdGet(t, "/api/session/claude:"+id+"/commands?limit=2"); len(items) != 2 || items[1].Cmd != "sleep 999" {
		t.Fatalf("limit: %+v", items)
	}
	rec := httptest.NewRecorder()
	watchedSessionRoute(rec, httptest.NewRequest("GET", "/x", nil), "claude:nope-nope/commands")
	if rec.Code != 404 {
		t.Fatalf("unknown session: %d", rec.Code)
	}
}

func TestRecentCommandsDedup(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.jsonl")
	jsonl(t, a, cUser(1, "<bash-input>git status</bash-input>"), cUser(2, "<bash-stdout>old</bash-stdout>"),
		cUser(3, "<bash-input>make</bash-input>"), cUser(4, "<bash-stdout>ok</bash-stdout>"))
	b := filepath.Join(dir, "rollout-b.jsonl")
	jsonl(t, b, codexDone(10, map[string]any{"type": "CommandExecution", "id": "u", "command": "git status",
		"source": "user_shell", "status": "completed", "aggregated_output": "new", "exit_code": 0}, parseTimestamp(tsAt(10))))
	old := filepath.Join(dir, "old.jsonl")
	jsonl(t, old, cUser(1, "<bash-input>ancient</bash-input>"))

	prev := store
	t.Cleanup(func() { store = prev })
	store = NewStore()
	now := time.Now().UnixMilli()
	store.sessions["claude:a"] = &Session{ID: "claude:a", Tool: ToolClaude, TranscriptPath: a, Cwd: "/wa", LastActivityAt: now}
	store.sessions["codex:b"] = &Session{ID: "codex:b", Tool: ToolCodex, TranscriptPath: b, Cwd: "/wb", LastActivityAt: now - 1000}
	store.sessions["claude:old"] = &Session{ID: "claude:old", Tool: ToolClaude, TranscriptPath: old, LastActivityAt: now - int64(30*24*time.Hour/time.Millisecond)}
	store.sessions["cursor:c"] = &Session{ID: "cursor:c", Tool: ToolCursor, TranscriptPath: a, LastActivityAt: now}

	mux := http.NewServeMux()
	registerCommandRoutes(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/api/commands/recent?limit=50", nil))
	var out struct {
		Items []CmdItem `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if len(out.Items) != 2 {
		t.Fatalf("want git status + make, got %+v", out.Items)
	}
	g := out.Items[0]
	if g.Cmd != "git status" || g.SessionID != "codex:b" || g.Cwd != "/wb" || g.Output != "new" {
		t.Fatalf("newest git status first: %+v", g)
	}
	if m := out.Items[1]; m.Cmd != "make" || m.SessionID != "claude:a" || m.Cwd != "/wa" {
		t.Fatalf("make %+v", m)
	}
	if got := recentCmds(1); len(got) != 1 {
		t.Fatalf("limit 1: %+v", got)
	}
}
