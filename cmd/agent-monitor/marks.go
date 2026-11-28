package main

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// threadMark is the user's pin, done and snooze for a watched (terminal) thread or session.
type threadMark struct {
	Pinned       bool  `json:"pinned,omitempty"`
	SettledAt    int64 `json:"settledAt,omitempty"`
	SnoozedUntil int64 `json:"snoozedUntil,omitempty"`
}

var marks = struct {
	sync.Mutex
	m      map[string]threadMark
	loaded bool
}{m: map[string]threadMark{}}

func marksPath() string { return filepath.Join(stateDir(), "thread-marks.json") }

func markOf(id string) threadMark {
	marks.Lock()
	defer marks.Unlock()
	if !marks.loaded {
		marks.loaded = true
		if b, err := os.ReadFile(marksPath()); err == nil {
			_ = json.Unmarshal(b, &marks.m)
		}
	}
	return marks.m[id]
}

// hides: Done or Snooze still holds; activity after Done clears it.
func (m threadMark) hides(lastActivity, now int64) bool {
	return m.SnoozedUntil > now || (m.SettledAt > 0 && m.SettledAt >= lastActivity)
}

// snoozesEnded lists ids whose snooze ended in (from, to].
func snoozesEnded(from, to int64) []string {
	markOf("")
	marks.Lock()
	defer marks.Unlock()
	var out []string
	for id, m := range marks.m {
		if m.SnoozedUntil > from && m.SnoozedUntil <= to {
			out = append(out, id)
		}
	}
	return out
}

type markReq struct {
	IDs          []string `json:"ids"`
	Pinned       *bool    `json:"pinned,omitempty"`
	Settled      *bool    `json:"settled,omitempty"`
	SnoozedUntil *int64   `json:"snoozedUntil,omitempty"`
}

func applyMarks(req markReq, now int64) {
	markOf("") // loads the file
	marks.Lock()
	defer marks.Unlock()
	for _, id := range req.IDs {
		if id == "" {
			continue
		}
		m := marks.m[id]
		if req.Pinned != nil {
			m.Pinned = *req.Pinned
		}
		if req.Settled != nil {
			m.SettledAt, m.SnoozedUntil = 0, 0
			if *req.Settled {
				m.SettledAt = now
			}
		}
		if req.SnoozedUntil != nil {
			m.SnoozedUntil = *req.SnoozedUntil
		}
		if m == (threadMark{}) {
			delete(marks.m, id)
		} else {
			marks.m[id] = m
		}
	}
	if err := writeFileAtomic(marksPath(), marks.m); err != nil {
		log.Printf("marks: save: %v", err)
	}
}

// POST /api/v3/mark {ids, pinned?, settled?, snoozedUntil?}: ids are the thread id and its session ids.
func handleMark(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var req markReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.IDs) == 0 || len(req.IDs) > 200 {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	applyMarks(req, time.Now().UnixMilli())
	if store != nil {
		for _, id := range req.IDs {
			store.Touch(id)
		}
	}
	w.WriteHeader(http.StatusNoContent)
}
