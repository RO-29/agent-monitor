package main

import (
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
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
			cursorRecordChat(s, wsDir, chat.Name(), cwd, cutoff)
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
	// Walk the blob DAG once to count "messages" we would surface in the
	// detail view — the UI shows this as the cards' msg count.
	if agg, err := scanCursorChat(chatDir, nil, 0); err == nil {
		in.MessageCountSet, in.HasMessageCount = agg.MessageCount, true
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
	// Use the system sqlite3 CLI to avoid a CGO dependency. The TEXT column
	// stores hex-encoded JSON (Cursor encodes its meta blob this way).
	out, err := exec.Command("/usr/bin/sqlite3", storeDB,
		"SELECT value FROM meta WHERE key='0' LIMIT 1;").Output()
	if err != nil {
		return nil
	}
	hexStr := strings.TrimSpace(string(out))
	if hexStr == "" {
		return nil
	}
	raw, err := hex.DecodeString(hexStr)
	if err != nil {
		return nil
	}
	var m cursorChatMeta
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil
	}
	cursorMetaCache.Store(chatDir, &m)
	return &m
}

