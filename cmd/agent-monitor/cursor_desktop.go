package main

import (
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Cursor desktop keeps its agent chats in globalStorage/state.vscdb (cursorDiskKV). Three layouts exist:
// A: composerData.fullConversationHeadersOnly + bubbleId:<chat>:<bubble>
// B: composerConvHeaders:<chat>:<page> + bubbleId records
// C: composerData.conversationState (base64 protobuf ids) + agentKv:blob:<id> JSON messages (the CLI format)

const cursorDeskPrefix = "cursordb:"

func cursorDesktopDir() string {
	return filepath.Join(homeDir(), "Library", "Application Support", "Cursor", "User")
}

func openCursorDesktop() (*sql.DB, error) {
	p := filepath.Join(cursorDesktopDir(), "globalStorage", "state.vscdb")
	if _, err := os.Stat(p); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", "file:"+p+"?mode=ro&_pragma=busy_timeout(3000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

var cursorWorkspaces = struct {
	mu sync.Mutex
	m  map[string]string
}{m: map[string]string{}}

// cursorWorkspaceFolder reads workspaceStorage/<id>/workspace.json for the folder path.
func cursorWorkspaceFolder(id string) string {
	if id == "" {
		return ""
	}
	cursorWorkspaces.mu.Lock()
	defer cursorWorkspaces.mu.Unlock()
	if f, ok := cursorWorkspaces.m[id]; ok {
		return f
	}
	var w struct {
		Folder string `json:"folder"`
	}
	raw, _ := os.ReadFile(filepath.Join(cursorDesktopDir(), "workspaceStorage", id, "workspace.json"))
	_ = json.Unmarshal(raw, &w)
	f := ""
	if u, err := url.Parse(w.Folder); err == nil && u.Scheme == "file" {
		f = u.Path
	}
	cursorWorkspaces.m[id] = f
	return f
}

// startCursorDesktopWatcher checks the database every 15 s and files changed chats as cursor sessions.
func startCursorDesktopWatcher(s *Store) {
	go func() {
		var stamp string
		since := int64(0) // top-level chats are few (43 of 259 here); subagent runs are skipped
		for ; ; time.Sleep(15 * time.Second) {
			st := ""
			for _, f := range []string{"state.vscdb", "state.vscdb-wal"} {
				if fi, err := os.Stat(filepath.Join(cursorDesktopDir(), "globalStorage", f)); err == nil {
					st += fmt.Sprintf("%d/%d;", fi.Size(), fi.ModTime().UnixNano())
				}
			}
			if st == "" || st == stamp {
				continue
			}
			stamp = st
			if n, err := scanCursorDesktop(s, since); err != nil {
				log.Printf("cursor desktop: %v", err)
			} else if n > 0 {
				since = max(since, n)
			}
		}
	}()
}

// scanCursorDesktop upserts top-level chats updated after since; it returns the newest update time seen.
func scanCursorDesktop(s *Store, since int64) (int64, error) {
	db, err := openCursorDesktop()
	if err != nil {
		return 0, nil
	}
	defer db.Close()
	rows, err := db.Query(`SELECT composerId, COALESCE(workspaceId,''), COALESCE(createdAt,0), COALESCE(lastUpdatedAt,0), COALESCE(value,'')
		FROM composerHeaders WHERE COALESCE(isSubagent,0)=0 AND COALESCE(isArchived,0)=0 AND lastUpdatedAt > ?`, since)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	newest := int64(0)
	now := time.Now().UnixMilli()
	for rows.Next() {
		var id, ws, val string
		var created, updated int64
		if rows.Scan(&id, &ws, &created, &updated, &val) != nil {
			continue
		}
		var h struct {
			Name string `json:"name"`
		}
		_ = json.Unmarshal([]byte(val), &h)
		state := ClassifyAge(now - updated)
		if state == StateRunning && now-updated > 60_000 {
			state = StateIdle
		}
		in := UpsertInput{
			Tool: ToolCursor, SessionID: id, State: state, HasState: true,
			EventKind: "cursor-desktop", TranscriptPath: cursorDeskPrefix + id, HasTranscript: true,
			StartedAtOverride: created, LastActivityAtOverride: updated,
		}
		if cwd := cursorWorkspaceFolder(ws); cwd != "" {
			in.Cwd, in.HasCwd = cwd, true
		}
		if h.Name != "" {
			in.Title, in.HasTitle = h.Name, true
		}
		s.Upsert(in)
		newest = max(newest, updated)
	}
	return newest, rows.Err()
}

var cursorDeskCache = struct {
	mu sync.Mutex
	m  map[string]cursorParsed
}{m: map[string]cursorParsed{}}

// cursorDesktopItems reads one desktop chat; the parse is cached until the chat's lastUpdatedAt changes.
func cursorDesktopItems(threadID, composerID string) ([]*OItem, []*ORun, error) {
	db, err := openCursorDesktop()
	if err != nil {
		return nil, nil, err
	}
	defer db.Close()
	var raw []byte
	if err := db.QueryRow(`SELECT value FROM cursorDiskKV WHERE key=?`, "composerData:"+composerID).Scan(&raw); err != nil {
		return nil, nil, fmt.Errorf("cursor chat %s: %w", composerID, err)
	}
	var cd struct {
		Headers   []cursorBubbleHead `json:"fullConversationHeadersOnly"`
		State     string             `json:"conversationState"`
		CreatedAt int64              `json:"createdAt"`
		UpdatedAt int64              `json:"lastUpdatedAt"`
		Status    string             `json:"status"`
	}
	if err := json.Unmarshal(raw, &cd); err != nil {
		return nil, nil, err
	}
	stamp := fmt.Sprintf("%d/%s", cd.UpdatedAt, cd.Status)
	cursorDeskCache.mu.Lock()
	if c, ok := cursorDeskCache.m[composerID]; ok && c.stamp == stamp {
		cursorDeskCache.mu.Unlock()
		return c.items, c.runs, nil
	}
	cursorDeskCache.mu.Unlock()

	heads := cd.Headers
	if len(heads) == 0 {
		heads = cursorConvHeaders(db, composerID)
	}
	var items []*OItem
	var runs []*ORun
	if len(heads) > 0 {
		items, runs = cursorBubbleItems(db, threadID, composerID, heads, cd.CreatedAt)
	} else if order := cursorStateOrder(cd.State); len(order) > 0 {
		blobs := map[string][]byte{}
		for _, id := range order {
			var b []byte
			if db.QueryRow(`SELECT value FROM cursorDiskKV WHERE key=?`, "agentKv:blob:"+id).Scan(&b) == nil {
				blobs[id] = b
			}
		}
		items, runs = cursorBuild(threadID, order, blobs, cd.CreatedAt)
	}
	if cd.Status == "generating" && len(runs) > 0 {
		runs[len(runs)-1].Status, runs[len(runs)-1].EndedAt = "running", 0
	}
	cursorDeskCache.mu.Lock()
	if len(cursorDeskCache.m) > 16 {
		cursorDeskCache.m = map[string]cursorParsed{}
	}
	cursorDeskCache.m[composerID] = cursorParsed{stamp: stamp, items: items, runs: runs}
	cursorDeskCache.mu.Unlock()
	return items, runs, nil
}

type cursorBubbleHead struct {
	BubbleID  string `json:"bubbleId"`
	Type      int    `json:"type"`
	CreatedAt string `json:"createdAt"`
}

// cursorConvHeaders reads the paged composerConvHeaders:<chat>:<page> records of the newest layout.
func cursorConvHeaders(db *sql.DB, composerID string) []cursorBubbleHead {
	p := "composerConvHeaders:" + composerID + ":"
	rows, err := db.Query(`SELECT value FROM cursorDiskKV WHERE key >= ? AND key < ? ORDER BY key`, p, p+"~")
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []cursorBubbleHead
	for rows.Next() {
		var v []byte
		var page []cursorBubbleHead
		if rows.Scan(&v) == nil && json.Unmarshal(v, &page) == nil {
			out = append(out, page...)
		}
	}
	return out
}

// cursorStateOrder decodes conversationState ("~" + base64 protobuf) to the message blob ids.
func cursorStateOrder(state string) []string {
	s := strings.TrimPrefix(strings.TrimSpace(state), "~")
	if s == "" {
		return nil
	}
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		if b, err = base64.RawStdEncoding.DecodeString(strings.TrimRight(s, "=")); err != nil {
			return nil
		}
	}
	return cursorOrder(b)
}

type cursorBubble struct {
	Type     int             `json:"type"`
	Text     string          `json:"text"`
	Thinking json.RawMessage `json:"thinking"`
	Tool     *struct {
		Name    string          `json:"name"`
		RawArgs string          `json:"rawArgs"`
		Params  json.RawMessage `json:"params"`
		Status  string          `json:"status"`
		Result  json.RawMessage `json:"result"`
	} `json:"toolFormerData"`
}

// cursorBubbleItems turns layout A and B bubbles into items, in header order.
func cursorBubbleItems(db *sql.DB, threadID, composerID string, heads []cursorBubbleHead, createdAt int64) ([]*OItem, []*ORun) {
	var items []*OItem
	var runs []*ORun
	var run *ORun
	ts := createdAt
	for i, h := range heads {
		if t, err := time.Parse(time.RFC3339Nano, h.CreatedAt); err == nil {
			ts = t.UnixMilli()
		} else if i > 0 {
			ts++ // no time recorded: keep the header order
		}
		var raw []byte
		if db.QueryRow(`SELECT value FROM cursorDiskKV WHERE key=?`, "bubbleId:"+composerID+":"+h.BubbleID).Scan(&raw) != nil {
			continue
		}
		var b cursorBubble
		if json.Unmarshal(raw, &b) != nil {
			continue
		}
		short := h.BubbleID
		if len(short) > 13 {
			short = short[:13]
		}
		add := func(n string, it *OItem) {
			it.ID, it.ThreadID, it.StartedAt = threadID+"@"+short+n, threadID, ts
			if run != nil {
				it.RunID = run.ID
			}
			items = append(items, it)
		}
		if b.Type == 1 {
			text := strings.TrimSpace(b.Text)
			if text == "" {
				continue
			}
			if run != nil && run.Status == "running" {
				run.Status, run.EndedAt = "completed", ts
			}
			run = &ORun{ID: threadID + "@" + short, ThreadID: threadID, Index: len(runs), Prompt: text, Status: "running", StartedAt: ts}
			runs = append(runs, run)
			add(".0", &OItem{Kind: "user", Status: "completed", Text: text, EndedAt: ts})
			continue
		}
		if t := cursorThinking(b.Thinking); t != "" {
			add(".r", &OItem{Kind: "reasoning", Status: "completed", Text: t})
		}
		if b.Tool != nil && b.Tool.Name != "" {
			args := json.RawMessage(b.Tool.RawArgs)
			if len(args) == 0 || !json.Valid(args) {
				args = b.Tool.Params
			}
			it := cursorToolItem(cursorPart{ToolName: b.Tool.Name, Args: args})
			if b.Tool.Status == "completed" || b.Tool.Status == "error" || b.Tool.Status == "cancelled" {
				it.Status, it.EndedAt = "completed", ts
				if b.Tool.Status != "completed" {
					it.Status = "failed"
				}
			}
			if len(b.Tool.Result) > 0 {
				it.Output = clipString(cursorResultText(b.Tool.Result), 8192)
			}
			add(".t", it)
		}
		if t := strings.TrimSpace(b.Text); t != "" {
			add(".a", &OItem{Kind: "agent", Status: "completed", Text: t})
		}
	}
	if run != nil && run.Status == "running" && len(items) > 0 && items[len(items)-1].Kind == "agent" {
		run.Status, run.EndedAt = "completed", ts
	}
	return items, runs
}

func cursorThinking(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if t := strings.TrimSpace(s); strings.HasPrefix(t, "{") {
			return cursorThinking(json.RawMessage(t)) // a string that holds the object
		}
		return strings.TrimSpace(s)
	}
	var o struct {
		Text string `json:"text"`
	}
	_ = json.Unmarshal(raw, &o)
	return strings.TrimSpace(o.Text)
}
