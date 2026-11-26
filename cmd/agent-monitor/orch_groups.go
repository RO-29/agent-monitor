package main

import (
	"database/sql"
	"encoding/json"
	"time"
)

const orchReadyToSettled = 24 * time.Hour

func activeStatus(s string) bool { return s == "starting" || s == "running" || s == "waiting" }

// subtreeTx returns every descendant of id.
func subtreeTx(q interface {
	Query(string, ...any) (*sql.Rows, error)
}, id string) ([]OThread, error) {
	return scanJSONRows[OThread](q.Query(`WITH RECURSIVE d(id) AS (
		SELECT id FROM threads WHERE parent_id=?
		UNION SELECT t.id FROM threads t JOIN d ON t.parent_id=d.id)
		SELECT json FROM threads WHERE id IN (SELECT id FROM d)`, id))
}

// ancestorsTx returns the parent chain of id, nearest first.
func ancestorsTx(q interface {
	QueryRow(string, ...any) *sql.Row
}, id string) []string {
	var out []string
	seen := map[string]bool{id: true}
	for {
		var parent sql.NullString
		if err := q.QueryRow(`SELECT parent_id FROM threads WHERE id=?`, id).Scan(&parent); err != nil || !parent.Valid || parent.String == "" || seen[parent.String] {
			return out
		}
		id = parent.String
		seen[id] = true
		out = append(out, id)
	}
}

// deriveGroup sets th.Group (and ReadyAt, expired SnoozedUntil) from the thread and its subtree.
func deriveGroup(t *orchTx, th *OThread) error {
	desc, err := subtreeTx(t.tx, th.ID)
	if err != nil {
		return err
	}
	ids := []any{th.ID}
	working := th.Status == "starting" || th.Status == "running" || th.Status == "waiting"
	for _, c := range desc {
		ids = append(ids, c.ID)
		if activeStatus(c.Status) {
			working = true
		}
	}
	q := `SELECT COUNT(*) FROM requests WHERE json_extract(json,'$.state')='open' AND thread_id IN (?` + repeatArgs(len(ids)-1) + `)`
	var open int
	if err := t.tx.QueryRow(q, ids...).Scan(&open); err != nil {
		return err
	}
	need := open > 0 || th.Status == "error"
	done := !need && !working && th.Status != "closed"
	if !done {
		th.ReadyAt = 0
	} else if th.ReadyAt == 0 {
		th.ReadyAt = t.now
	}
	if th.SnoozedUntil != 0 && th.SnoozedUntil <= t.now {
		th.SnoozedUntil = 0
	}
	switch {
	case th.Status == "closed", th.SnoozedUntil > t.now, th.SettledAt > 0:
		th.Group = "settled"
	case need:
		th.Group = "need"
	case working:
		th.Group = "working"
	case t.now-th.ReadyAt > orchReadyToSettled.Milliseconds():
		th.Group = "settled"
	default:
		th.Group = "ready"
	}
	return nil
}

func repeatArgs(n int) string {
	b := make([]byte, 0, 2*n)
	for i := 0; i < n; i++ {
		b = append(b, ",?"...)
	}
	return string(b)
}

// refreshGroups recomputes the group of touched threads and their ancestors before commit.
// Earlier thread.updated events of this tx get the final thread; a changed thread without one gets a new event.
func (t *orchTx) refreshGroups() error {
	if len(t.touched) == 0 {
		return nil
	}
	ids := map[string]bool{}
	var order []string
	add := func(id string) {
		if !ids[id] {
			ids[id] = true
			order = append(order, id)
		}
	}
	for id := range t.touched {
		add(id)
		for _, a := range ancestorsTx(t.tx, id) {
			add(a)
		}
	}
	for _, id := range order {
		th, err := t.thread(id)
		if err != nil {
			continue
		}
		before := *th
		if err := deriveGroup(t, th); err != nil {
			return err
		}
		changed := before.Group != th.Group || before.ReadyAt != th.ReadyAt || before.SnoozedUntil != th.SnoozedUntil
		if changed {
			if err := t.storeThread(th); err != nil {
				return err
			}
		}
		data, _ := json.Marshal(map[string]any{"thread": th})
		patched := false
		for i := range t.events {
			if t.events[i].ThreadID == id && t.events[i].Type == "thread.updated" {
				t.events[i].Data = data
				patched = true
			}
		}
		if !patched && before.Group != th.Group {
			t.events = append(t.events, OEvent{ThreadID: id, At: t.now, Type: "thread.updated", Data: data})
		}
	}
	t.touched = nil
	return nil
}

// SweepGroups recomputes every thread's group; it moves expired snoozes and stale ready threads.
func (s *OrchStore) SweepGroups() error {
	return s.write(func(t *orchTx) error {
		rows, err := t.tx.Query(`SELECT id FROM threads`)
		if err != nil {
			return err
		}
		var ids []string
		for rows.Next() {
			var id string
			if rows.Scan(&id) == nil {
				ids = append(ids, id)
			}
		}
		rows.Close()
		for _, id := range ids {
			t.touch(id)
		}
		return nil
	})
}

// RootOf returns the root thread id of id's lineage.
func (s *OrchStore) RootOf(id string) string {
	root := id
	_ = s.read(func(tx *sql.Tx) error {
		if a := ancestorsTx(tx, id); len(a) > 0 {
			root = a[len(a)-1]
		}
		return nil
	})
	return root
}

// Subtree returns every descendant of id.
func (s *OrchStore) Subtree(id string) ([]OThread, error) {
	var out []OThread
	err := s.read(func(tx *sql.Tx) error {
		var err error
		out, err = subtreeTx(tx, id)
		return err
	})
	return out, err
}

// Children returns the direct children of id, oldest first.
func (s *OrchStore) Children(id string) ([]OThread, error) {
	var out []OThread
	err := s.read(func(tx *sql.Tx) error {
		var err error
		out, err = scanJSONRows[OThread](tx.Query(`SELECT json FROM threads WHERE parent_id=? ORDER BY created_at`, id))
		return err
	})
	return out, err
}
