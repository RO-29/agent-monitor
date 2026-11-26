package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const opencodeScanAge = 365 * 24 * time.Hour

type opencodeSession struct {
	ID        string `json:"id"`
	Directory string `json:"directory"`
	Title     string `json:"title"`
	Time      struct {
		Created int64 `json:"created"`
		Updated int64 `json:"updated"`
	} `json:"time"`
}

var (
	opencodeWatchedMu sync.Mutex
	opencodeWatched   = map[string]bool{}
)

func startOpencodeAdapter(s *Store) {
	root := filepath.Join(homeDir(), ".local", "share", "opencode", "storage")
	sessionRoot := filepath.Join(root, "session")
	messageRoot := filepath.Join(root, "message")
	partRoot := filepath.Join(root, "part")
	if !pathExists(sessionRoot) {
		return
	}

	scan := func() {
		cutoff := time.Now().Add(-opencodeScanAge).UnixMilli()
		projects, err := os.ReadDir(sessionRoot)
		if err != nil {
			return
		}
		for _, proj := range projects {
			if !proj.IsDir() {
				continue
			}
			projPath := filepath.Join(sessionRoot, proj.Name())
			files, err := os.ReadDir(projPath)
			if err != nil {
				continue
			}
			for _, f := range files {
				name := f.Name()
				if !strings.HasPrefix(name, "ses_") || !strings.HasSuffix(name, ".json") {
					continue
				}
				fp := filepath.Join(projPath, name)
				st, err := os.Stat(fp)
				if err != nil || st.ModTime().UnixMilli() < cutoff {
					continue
				}
				opencodeSeed(s, fp)
			}
		}
	}

	scan()
	// Polling replaces the original fs.watch + recursive watch. For ~4h scan
	// windows the cost is negligible and we avoid an fsnotify dependency.
	go func() {
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		for range t.C {
			scan()
			opencodeScanMessages(s, messageRoot)
			opencodeScanParts(s, messageRoot, partRoot)
		}
	}()
}

var (
	opencodeSeededMu sync.Mutex
	opencodeSeeded   = map[string]bool{} // session-file path -> seeded
)

func opencodeSeed(s *Store, fp string) {
	// Skip re-seeding the same session on every periodic scan — that's what
	// generated the 80 "seed" events the user saw in the timeline.
	opencodeSeededMu.Lock()
	if opencodeSeeded[fp] {
		opencodeSeededMu.Unlock()
		return
	}
	opencodeSeeded[fp] = true
	opencodeSeededMu.Unlock()

	data, err := os.ReadFile(fp)
	if err != nil {
		return
	}
	var rec opencodeSession
	if err := json.Unmarshal(data, &rec); err != nil || rec.ID == "" {
		return
	}
	ageMs := time.Now().UnixMilli() - rec.Time.Updated
	state := ClassifyAge(ageMs)
	msgDir, partRoot := opencodeStoragePaths(rec.ID)
	in := UpsertInput{
		Tool: ToolOpencode, SessionID: rec.ID,
		Cwd: rec.Directory, HasCwd: true,
		State: state, HasState: true,
		EventKind: "seed", EventText: rec.Title,
		LastActivityAtOverride: rec.Time.Updated,
		StartedAtOverride:      rec.Time.Created,
		TranscriptPath:         msgDir, HasTranscript: true,
	}
	if rec.Title != "" {
		in.Title, in.HasTitle = rec.Title, true
	}
	// Pull the rich aggregate (token totals, tool histogram, files, message
	// count, first/last message) from the message + part files on disk.
	if agg, err := scanOpencodeStorage(rec.ID, msgDir, partRoot, nil, 0); err == nil {
		t := agg.Tokens
		in.Tokens = &t
		in.ToolUsageDelta = agg.ToolUsage
		if agg.FirstMessage != "" {
			in.FirstMessage, in.HasFirst = agg.FirstMessage, true
		}
		if agg.LastMessage != "" {
			in.Message, in.HasMessage = agg.LastMessage, true
		}
		if agg.Model != "" {
			in.Model, in.HasModel = agg.Model, true
		}
		in.FilesTouchedSet, in.HasFiles = len(agg.Files), true
		in.MessageCountSet, in.HasMessageCount = agg.MessageCount, true
	}
	s.Upsert(in)
}

// opencodeScanMessages bumps a session whenever a new msg_*.json appears.
func opencodeScanMessages(s *Store, messageRoot string) {
	if !pathExists(messageRoot) {
		return
	}
	dirs, err := os.ReadDir(messageRoot)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-30 * time.Second)
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		sessionID := d.Name()
		dir := filepath.Join(messageRoot, sessionID)
		files, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, f := range files {
			if !strings.HasPrefix(f.Name(), "msg_") || !strings.HasSuffix(f.Name(), ".json") {
				continue
			}
			fp := filepath.Join(dir, f.Name())
			st, err := os.Stat(fp)
			if err != nil || st.ModTime().Before(cutoff) {
				continue
			}
			key := "msg:" + fp
			opencodeWatchedMu.Lock()
			seen := opencodeWatched[key]
			if !seen {
				opencodeWatched[key] = true
			}
			opencodeWatchedMu.Unlock()
			if seen {
				continue
			}
			data, err := os.ReadFile(fp)
			if err != nil {
				continue
			}
			var rec struct {
				Role string `json:"role"`
			}
			if err := json.Unmarshal(data, &rec); err != nil {
				continue
			}
			kind := "assistant_message"
			if rec.Role == "user" {
				kind = "user_message"
			} else if rec.Role != "assistant" {
				continue
			}
			s.Upsert(UpsertInput{
				Tool: ToolOpencode, SessionID: sessionID,
				State: StateRunning, HasState: true,
				EventKind: kind, EventText: "",
			})
		}
	}
}

// opencodeScanParts: when a new part lands, the parent message id tells us
// which session is active. We map message id back to session by checking which
// message subdir contains the parent message file.
func opencodeScanParts(s *Store, messageRoot, partRoot string) {
	if !pathExists(partRoot) {
		return
	}
	cutoff := time.Now().Add(-30 * time.Second)
	msgDirs, err := os.ReadDir(partRoot)
	if err != nil {
		return
	}
	for _, md := range msgDirs {
		if !md.IsDir() {
			continue
		}
		messageID := md.Name()
		partDir := filepath.Join(partRoot, messageID)
		files, err := os.ReadDir(partDir)
		if err != nil {
			continue
		}
		for _, f := range files {
			fp := filepath.Join(partDir, f.Name())
			st, err := os.Stat(fp)
			if err != nil || st.ModTime().Before(cutoff) {
				continue
			}
			key := "part:" + fp
			opencodeWatchedMu.Lock()
			seen := opencodeWatched[key]
			if !seen {
				opencodeWatched[key] = true
			}
			opencodeWatchedMu.Unlock()
			if seen {
				continue
			}
			sessionID := opencodeFindSessionForMessage(messageRoot, messageID)
			if sessionID == "" {
				continue
			}
			s.Upsert(UpsertInput{
				Tool: ToolOpencode, SessionID: sessionID,
				State: StateRunning, HasState: true,
				EventKind: "part", EventText: "",
			})
		}
	}
}

func opencodeFindSessionForMessage(messageRoot, messageID string) string {
	dirs, err := os.ReadDir(messageRoot)
	if err != nil {
		return ""
	}
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		if pathExists(filepath.Join(messageRoot, d.Name(), messageID+".json")) {
			return d.Name()
		}
	}
	return ""
}
