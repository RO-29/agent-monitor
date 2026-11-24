package main

import (
	"errors"
	"io/fs"
	"net/http"
	"os"
	"sort"
)

// watchedSessionFiles lists every file the session's completed edits touched, over the whole transcript.
func watchedSessionFiles(ref watchedRef) ([]string, error) {
	if _, err := watchedItemsFor(ref, -1, -1); err != nil { // brings the turn index up to date
		return nil, err
	}
	s := watchedSessionFor(ref)
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := os.Open(ref.Path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	p := s.newParser(0)
	if err := p.advance(f, s.ix.off); err != nil {
		return nil, err
	}
	p.refreshBg()
	out := &WatchedItems{}
	p.collect(out, -1, 0)
	return editedPaths(out.Items), nil
}

func editedPaths(items []*OItem) []string {
	seen := map[string]bool{}
	paths := []string{}
	for _, it := range items {
		if it.Kind != "fileChange" || it.Status == "declined" || it.Status == "failed" {
			continue
		}
		for _, f := range it.Files {
			if f.Path != "" && !seen[f.Path] {
				seen[f.Path] = true
				paths = append(paths, f.Path)
			}
		}
	}
	sort.Strings(paths)
	return paths
}

func serveWatchedFiles(w http.ResponseWriter, id string) {
	ref := resolveWatched(id)
	if !ref.Resolved || ref.Path == "" {
		writeJSONStatus(w, http.StatusNotFound, map[string]any{"error": "no transcript for session"})
		return
	}
	var paths []string
	var err error
	switch {
	case ref.Tool == ToolClaude || ref.Tool == ToolCodex:
		paths, err = watchedSessionFiles(ref)
	case ref.Tool == ToolCursor && isCursorTranscript(ref.Path):
		var items []*OItem
		items, _, err = cursorTranscriptItems(ref.ID, ref.Path)
		paths = editedPaths(items)
	default:
		writeJSONStatus(w, http.StatusNotImplemented, map[string]any{"error": "edited files are not read for " + string(ref.Tool) + " yet"})
		return
	}
	if errors.Is(err, fs.ErrNotExist) {
		paths, err = []string{}, nil
	}
	if err != nil {
		writeJSONStatus(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, map[string]any{"paths": paths})
}
