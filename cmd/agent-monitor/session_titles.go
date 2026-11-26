package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strings"
)

// Titles the user set in agent-monitor for watched sessions; they win over the agent's own title.

const titleMax = 120

func cleanTitle(s string) string {
	t := strings.TrimSpace(firstLine(strings.TrimSpace(s)))
	if r := []rune(t); len(r) > titleMax {
		t = string(r[:titleMax])
	}
	return t
}

// LoadTitles reads the saved titles and applies them to the sessions in the store.
func (s *Store) LoadTitles(path string) {
	m := map[string]string{}
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &m)
	}
	s.mu.Lock()
	s.titlePath, s.titles = path, m
	for id, t := range m {
		if v := s.sessions[id]; v != nil {
			s.agentTitles[id] = v.Title
			v.Title = t
		}
	}
	s.mu.Unlock()
}

// SetTitle saves a user title for a session; an empty title goes back to the agent's own title.
func (s *Store) SetTitle(id, title string) (*Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	prev := s.sessions[id]
	if prev == nil {
		return nil, errors.New("no such session")
	}
	if s.titles == nil {
		s.titles = map[string]string{}
	}
	if _, renamed := s.titles[id]; !renamed {
		s.agentTitles[id] = prev.Title
	}
	if title == "" {
		delete(s.titles, id)
	} else {
		s.titles[id] = title
	}
	if s.titlePath != "" {
		if err := writeFileAtomic(s.titlePath, s.titles); err != nil {
			return nil, err
		}
	}
	next := *prev
	next.Title = title
	if title == "" {
		next.Title = s.agentTitles[id]
		delete(s.agentTitles, id)
	}
	s.sessions[id] = &next
	s.broadcastLocked(ServerEvent{Kind: "upsert", Session: &next})
	return &next, nil
}

// userTitle returns the user title of the newest renamed session in a list, or "".
func (s *Store) userTitle(ids []string) string {
	if s == nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(ids) - 1; i >= 0; i-- {
		if t := s.titles[ids[i]]; t != "" {
			return t
		}
	}
	return ""
}

func handleSessionTitle(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID    string `json:"id"`
		Title string `json:"title"`
	}
	if r.Method != http.MethodPost || json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&in) != nil || in.ID == "" {
		http.Error(w, "POST {id, title}", http.StatusBadRequest)
		return
	}
	sess, err := store.SetTitle(in.ID, cleanTitle(in.Title))
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "title": sess.Title})
}
