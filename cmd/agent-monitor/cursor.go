package main

import (
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const cursorScanAge = 365 * 24 * time.Hour

type cursorChatMeta struct {
	AgentID   string `json:"agentId"`
	Name      string `json:"name"`
	Mode      string `json:"mode"`
	CreatedAt int64  `json:"createdAt"`
}

var (
	cursorWorkspaceMu sync.RWMutex
	// MD5(absPath) -> absPath. Cursor stores chats in
	// ~/.cursor/chats/<md5(cwd)>/<chat-uuid>/store.db
	cursorWorkspaceMap = map[string]string{}
	cursorMetaCache    sync.Map // chat dir path -> *cursorChatMeta
	cursorLastMtime    sync.Map // chatID -> last seen mtime; skip re-upsert if unchanged
)

func startCursorAdapter(s *Store) {
	root := filepath.Join(homeDir(), ".cursor", "chats")
	if !pathExists(root) {
		return
	}
	rebuildCursorWorkspaceMap()
	tick := func() { cursorScan(s, root) }
	tick()
	go func() {
		t := time.NewTicker(10 * time.Second)
		defer t.Stop()
		for range t.C {
			rebuildCursorWorkspaceMap()
			tick()
		}
	}()
}

// rebuildCursorWorkspaceMap walks ~/.cursor/projects/ and builds an MD5 →
// path map. Cursor encodes paths as dirnames like "Users-me-web-app"
// (i.e. "/Users/me/web-app" with "/" replaced by "-"); MD5 of the
// decoded path matches the workspace hash used as the chats subdirectory.
func rebuildCursorWorkspaceMap() {
	projectsDir := filepath.Join(homeDir(), ".cursor", "projects")
	entries, err := os.ReadDir(projectsDir)
	if err != nil {
		return
	}
	next := map[string]string{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		// Cursor doesn't prefix the dir name with "-"; e.g. "Users-me-web-
		// app-api-core" decodes to "/Users/me/web-app/api-core". The encoding
		// is lossy on hyphens-in-names so we use the shared disk-walking
		// decoder.
		decoded := decodePathFromDashEncoded(e.Name(), false)
		sum := md5.Sum([]byte(decoded))
		next[hex.EncodeToString(sum[:])] = decoded
	}
	cursorWorkspaceMu.Lock()
	cursorWorkspaceMap = next
	cursorWorkspaceMu.Unlock()
}

func cursorLookupWorkspace(hash string) string {
	cursorWorkspaceMu.RLock()
	defer cursorWorkspaceMu.RUnlock()
	return cursorWorkspaceMap[hash]
}

func cursorScan(s *Store, root string) {
	cutoff := time.Now().Add(-cursorScanAge).UnixMilli()
	workspaces, err := os.ReadDir(root)
	if err != nil {
		return
	}
	// The first scan parses every chat; four at a time keeps start-up short.
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	defer wg.Wait()
	for _, ws := range workspaces {
		if !ws.IsDir() {
			continue
		}
		hash := ws.Name()
		cwd := cursorLookupWorkspace(hash)
		if cwd == "" {
			cwd = "cursor:" + hash[:8]
		}
		wsDir := filepath.Join(root, hash)
		chats, err := os.ReadDir(wsDir)
		if err != nil {
			continue
		}
		for _, chat := range chats {
			if !chat.IsDir() {
				continue
			}
			wg.Add(1)
			sem <- struct{}{}
			go func(id, cwd string) {
				defer func() { <-sem; wg.Done() }()
				cursorRecordChat(s, wsDir, id, cwd, cutoff)
			}(chat.Name(), cwd)
		}
	}
}

func cursorRecordChat(s *Store, wsDir, chatID, cwd string, cutoff int64) {
	chatDir := filepath.Join(wsDir, chatID)
	storeDB := filepath.Join(chatDir, "store.db")
	wal := filepath.Join(chatDir, "store.db-wal")

	st, err := os.Stat(chatDir)
	if err != nil {
		return
	}
	mtime := st.ModTime().UnixMilli()
	if ws, err := os.Stat(wal); err == nil {
		if w := ws.ModTime().UnixMilli(); w > mtime {
			mtime = w
		}
	}
	if mtime < cutoff {
		return
	}
	// Skip upserts when nothing changed since the last scan — otherwise the
	// 10s tick floods the timeline with "wal-tick" events.
	if prev, ok := cursorLastMtime.Load(chatID); ok {
		if pm, _ := prev.(int64); pm == mtime {
			return
		}
	}
	cursorLastMtime.Store(chatID, mtime)

	meta := cursorReadMeta(chatDir, storeDB)
	title := ""
	createdAt := int64(0)
	mode := ""
	if meta != nil {
		title = meta.Name
		mode = meta.Mode
		createdAt = meta.CreatedAt
	}
	if title == "" || title == "New Agent" {
		if p := cursorChatFirstPrompt(chatID, chatDir); p != "" {
			title = p
		}
	}

	if strings.HasPrefix(cwd, "cursor:") {
		if c := cursorChatCwd(chatDir); c != "" {
			cwd = c
		}
	}
	ageMs := time.Now().UnixMilli() - mtime
	state := ClassifyAge(ageMs)

	in := UpsertInput{
		Tool: ToolCursor, SessionID: chatID,
		Cwd: cwd, HasCwd: true,
		State: state, HasState: true,
		EventKind: "wal-tick", EventText: "",
		LastActivityAtOverride: mtime,
		TranscriptPath:         chatDir, HasTranscript: true,
	}
	if title != "" {
		in.Title, in.HasTitle = title, true
	}
	if mode != "" {
		in.Mode, in.HasMode = mode, true
	}
	if createdAt > 0 {
		in.StartedAtOverride = createdAt
	}
	// The parsed chat gives the message count, and an ended turn (its last item an answer) reads as idle.
	if items, _, err := cursorItemsFor(sid(ToolCursor, chatID), chatDir); err == nil {
		in.MessageCountSet, in.HasMessageCount = len(items), true
		if state == StateRunning && len(items) > 0 && items[len(items)-1].Kind == "agent" {
			in.State = StateIdle
		}
	}
	s.Upsert(in)
}

// cursorReadMeta caches per-chat metadata. The store.db file is small but it's
// a SQLite DB — reading it on every poll across all chats would be wasteful.
// Cache by chatDir; metadata is essentially immutable (name only changes when
// the user renames, which is rare enough that cache invalidation can wait).
func cursorReadMeta(chatDir, storeDB string) *cursorChatMeta {
	if v, ok := cursorMetaCache.Load(chatDir); ok {
		return v.(*cursorChatMeta)
	}
	if !pathExists(storeDB) {
		return nil
	}
	raw := cursorMetaJSON(storeDB)
	if raw == nil {
		return nil
	}
	var m cursorChatMeta
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil
	}
	cursorMetaCache.Store(chatDir, &m)
	return &m
}

var cursorPromptCache sync.Map

// cursorChatFirstPrompt names a chat that Cursor left as "New Agent": the first line of its first user message.
func cursorChatFirstPrompt(chatID, chatDir string) string {
	if v, ok := cursorPromptCache.Load(chatDir); ok {
		return v.(string)
	}
	items, _, err := cursorItemsFor(chatID, chatDir)
	if err != nil {
		return ""
	}
	for _, it := range items {
		if it.Kind == "user" {
			if t := firstLine(it.Text); t != "" {
				t = clipString(t, 80)
				cursorPromptCache.Store(chatDir, t)
				return t
			}
		}
	}
	return ""
}
