package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeSearchDaemon serves the daemon routes the search tools read.
func fakeSearchDaemon(t *testing.T, seen *[]string) *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/search", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("q")
		*seen = append(*seen, q)
		if strings.HasPrefix(q, "session:") {
			writeJSON(w, searchResult{Total: 2, Hits: []searchHit{
				{ID: 1, Session: "codex:zz", Agent: "codex", Kind: "prompt", Turn: 4, Ts: 1000, Snippet: "fix the tunnel"},
				{ID: 2, Session: "codex:zz", Agent: "codex", Kind: "reply", Turn: 4, Ts: 2000, Snippet: "done &amp; tested"},
			}})
			return
		}
		writeJSON(w, searchResult{Total: 5, Hits: []searchHit{
			{ID: 1, Session: "claude:aa", Agent: "claude", Repo: "dot-mac", Cwd: "/u/dot-mac", Kind: "command", Turn: 7, Ts: 1794667224250, Snippet: "ssh -L &#39;<mark>tunnel</mark>&#39; &lt;x&gt;", ThreadID: "claude:root"},
			{ID: 2, Session: "ot_d1", Agent: "codex", Repo: "am", Kind: "reply", Span: "it-9", Ts: 1794667000000, Snippet: "the <mark>tunnel</mark> is up", ThreadID: "ot_d1", DrivenID: "ot_d1"},
			{ID: 3, Session: "claude:aa", Agent: "claude", Repo: "dot-mac", Kind: "reply", Turn: 8, Ts: 1794667300000, Snippet: "b"},
			{ID: 4, Session: "claude:aa", Agent: "claude", Repo: "dot-mac", Kind: "reply", Turn: 9, Ts: 1794667400000, Snippet: "c"},
			{ID: 5, Session: "claude:aa", Agent: "claude", Repo: "dot-mac", Kind: "reply", Turn: 9, Ts: 1794667500000, Snippet: "d"},
		}})
	})
	mux.HandleFunc("/api/v3/threads", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("all") != "1" || r.URL.Query().Get("driven") != "1" {
			t.Errorf("threads query %q", r.URL.RawQuery)
		}
		writeJSON(w, map[string]any{"threads": []map[string]any{
			{"id": "claude:root", "title": "raw", "display": "Fix the stg tunnel"},
			{"id": "ot_d1", "title": "Driven tunnel check"},
		}})
	})
	mux.HandleFunc("/api/session/{id}/items", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("id") != "claude:aa" {
			writeJSONStatus(w, 404, map[string]string{"error": "no transcript for session"})
			return
		}
		page := func(from, to int, more bool, earlier int64) WatchedItems {
			wi := WatchedItems{ID: "claude:aa", Agent: ToolClaude, More: more}
			if more {
				wi.Earlier = &earlier
			}
			for i := from; i <= to; i++ {
				rid := fmt.Sprintf("claude:aa@%d", i)
				wi.Runs = append(wi.Runs, &ORun{ID: rid, Index: i, StartedAt: int64(i) * 1000})
				one := 1
				wi.Items = append(wi.Items,
					&OItem{ID: rid + ".0", RunID: rid, Kind: "user", Text: fmt.Sprintf("prompt %d", i), StartedAt: int64(i) * 1000},
					&OItem{ID: rid + ".1", RunID: rid, Kind: "command", Command: "make test", ExitCode: &one, Output: strings.Repeat("x", 5000), StartedAt: int64(i)*1000 + 1},
					&OItem{ID: rid + ".2", RunID: rid, Kind: "agent", Text: fmt.Sprintf("answer %d %s", i, strings.Repeat("y", 1500)), StartedAt: int64(i)*1000 + 2})
			}
			return wi
		}
		switch r.URL.Query().Get("before") {
		case "":
			writeJSON(w, page(41, 80, true, 500))
		case "500":
			writeJSON(w, page(1, 40, false, 0))
		default:
			t.Errorf("unexpected before=%s", r.URL.Query().Get("before"))
		}
	})
	mux.HandleFunc("/api/orch/threads/{id}", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, OThreadDetail{Runs: []ORun{{ID: "r1", Index: 1, StartedAt: 10}}, Items: []OItem{
			{ID: "u", RunID: "r1", Kind: "user", Text: "check the tunnel"},
			{ID: "f", RunID: "r1", Kind: "fileChange", Files: []OFile{{Path: "a.go"}}},
		}})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("search tools must only GET, got %s %s", r.Method, r.URL.Path)
		}
		http.NotFound(w, r)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func callText(t *testing.T, resp map[string]any) (string, map[string]any, bool) {
	t.Helper()
	res, _ := resp["result"].(map[string]any)
	if res == nil {
		t.Fatalf("no result: %v", resp)
	}
	content, _ := res["content"].([]any)
	text := content[0].(map[string]any)["text"].(string)
	sc, _ := res["structuredContent"].(map[string]any)
	isErr, _ := res["isError"].(bool)
	return text, sc, isErr
}

func TestMCPSearchToolsListedForEverySession(t *testing.T) {
	for _, o := range []mcpOrch{{}, {thread: "ot_x"}} {
		resp := mcpSession(t, "http://127.0.0.1:9", o)("tools/list", nil)
		tools, _ := resp["result"].(map[string]any)["tools"].([]any)
		found := 0
		for _, tl := range tools {
			m := tl.(map[string]any)
			if n := m["name"]; n == "search_sessions" || n == "session_context" {
				found++
				if !strings.Contains(m["description"].(string), "local agent history") {
					t.Errorf("%s description must say it searches the local agent history", n)
				}
			}
		}
		if found != 2 {
			t.Fatalf("orch %q: want both search tools, got %d", o.thread, found)
		}
	}
}

func TestMCPSearchSessions(t *testing.T) {
	var seen []string
	srv := fakeSearchDaemon(t, &seen)
	call := mcpSession(t, srv.URL, mcpOrch{})
	text, sc, isErr := callText(t, call("tools/call", map[string]any{"name": "search_sessions", "arguments": map[string]any{
		"query": "tunnel", "agent": "claude", "repo": "my repo", "since": "7d", "limit": 1}}))
	if isErr {
		t.Fatalf("error: %s", text)
	}
	if len(seen) != 1 || seen[0] != `tunnel agent:claude repo:"my repo" since:7d` {
		t.Fatalf("query sent %q", seen)
	}
	sessions := sc["sessions"].([]any)
	if len(sessions) != 1 {
		t.Fatalf("limit 1 gave %d sessions", len(sessions))
	}
	s := sessions[0].(map[string]any)
	if s["id"] != "claude:aa" || s["title"] != "Fix the stg tunnel" || s["hits"].(float64) != 4 || s["repo"] != "dot-mac" || s["cwd"] != "/u/dot-mac" {
		t.Fatalf("session %v", s)
	}
	snips := s["snippets"].([]any)
	if len(snips) != 3 {
		t.Fatalf("want top 3 snippets, got %d", len(snips))
	}
	if first := snips[0].(map[string]any); first["text"] != "ssh -L 'tunnel' <x>" || first["turn"].(float64) != 7 {
		t.Fatalf("snippet must be plain text: %v", first)
	}
	if u := s["url"].(string); !strings.HasPrefix(u, srv.URL+"/w/claude:root?") || !strings.Contains(u, "hit=7") || !strings.Contains(u, "session=claude%3Aaa") {
		t.Fatalf("url %s", u)
	}
	if strings.Contains(text, "<mark>") || !strings.Contains(text, "Fix the stg tunnel") {
		t.Fatalf("text %s", text)
	}

	_, sc, _ = callText(t, call("tools/call", map[string]any{"name": "search_sessions", "arguments": map[string]any{"query": "tunnel"}}))
	sessions = sc["sessions"].([]any)
	if len(sessions) != 2 {
		t.Fatalf("default limit: %d sessions", len(sessions))
	}
	d := sessions[1].(map[string]any)
	if d["id"] != "ot_d1" || d["title"] != "Driven tunnel check" || d["url"] != srv.URL+"/w/ot_d1?hit=it-9&ts=1794667000000" {
		t.Fatalf("driven session %v", d)
	}

	if text, _, isErr := callText(t, call("tools/call", map[string]any{"name": "search_sessions", "arguments": map[string]any{}})); !isErr || !strings.Contains(text, "query is required") {
		t.Fatalf("missing query: %s", text)
	}
}

func TestMCPSessionContext(t *testing.T) {
	var seen []string
	srv := fakeSearchDaemon(t, &seen)
	call := mcpSession(t, srv.URL, mcpOrch{})

	text, _, isErr := callText(t, call("tools/call", map[string]any{"name": "session_context", "arguments": map[string]any{"sessionId": "claude:aa", "turn": 3, "around": 1}}))
	if isErr {
		t.Fatalf("error: %s", text)
	}
	for _, want := range []string{"turns 2 to 4 of 80", "## Turn 2", "## Turn 3 (hit)", "## Turn 4", "User: prompt 3", "$ make test (exit 1)", "Agent: answer 3"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in\n%s", want, text)
		}
	}
	if strings.Contains(text, "## Turn 5") || strings.Contains(text, "## Turn 1 ") {
		t.Errorf("turns outside the window:\n%s", text)
	}

	text, _, _ = callText(t, call("tools/call", map[string]any{"name": "session_context", "arguments": map[string]any{"sessionId": "claude:aa", "around": 5}}))
	if !strings.Contains(text, "## Turn 80 (hit)") || len(text) > sessionContextMax || !strings.Contains(text, "turns left out") || strings.Contains(text, "## Turn 75") {
		t.Fatalf("default turn must be the last one, capped at 8 KB (len %d):\n%s", len(text), text)
	}

	text, _, _ = callText(t, call("tools/call", map[string]any{"name": "session_context", "arguments": map[string]any{"sessionId": "ot_d1"}}))
	if !strings.Contains(text, "User: check the tunnel") || !strings.Contains(text, "Edited: a.go") {
		t.Fatalf("driven context:\n%s", text)
	}

	text, _, isErr = callText(t, call("tools/call", map[string]any{"name": "session_context", "arguments": map[string]any{"sessionId": "codex:zz", "turn": 4}}))
	if isErr || !strings.Contains(text, "from the search index") || !strings.Contains(text, "reply: done & tested") {
		t.Fatalf("index fallback:\n%s", text)
	}

	if text, _, isErr := callText(t, call("tools/call", map[string]any{"name": "session_context", "arguments": map[string]any{}})); !isErr || !strings.Contains(text, "sessionId is required") {
		t.Fatalf("missing id: %s", text)
	}
}

func TestPlainSnippet(t *testing.T) {
	if got := plainSnippet(`a <mark>b</mark> &lt;c&gt; &#39;d&#39; &amp;`); got != `a b <c> 'd' &` {
		t.Fatalf("%q", got)
	}
	if p := workspacePath(searchHit{Session: "codex:x", Turn: 2, Ts: 5}); p != "/w/codex:x?hit=2&session=codex%3Ax&ts=5" {
		t.Fatalf("%s", p)
	}
	if len(clipContext(strings.Repeat("é", sessionContextMax))) > sessionContextMax {
		t.Fatal("clipContext over the cap")
	}
}
