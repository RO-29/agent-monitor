package main

import (
	"database/sql"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sort"
	"time"

	_ "modernc.org/sqlite"
)

// SessionDB is a local SQLite mirror of the in-memory session store. The store
// is otherwise volatile — a daemon restart or the decay GC drops history. We
// snapshot every few minutes (and on shutdown) so session history survives for
// far longer than the live store keeps it, and reload it on startup.
type SessionDB struct {
	db *sql.DB
}

func sessionDBPath() string { return filepath.Join(stateDir(), "sessions.db") }

func OpenSessionDB() (*SessionDB, error) {
	dir := stateDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	// _busy_timeout so a concurrent read never errors mid-snapshot; WAL for
	// crash-safe incremental writes.
	db, err := sql.Open("sqlite", sessionDBPath()+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // SQLite is single-writer; serialize to avoid SQLITE_BUSY.
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS sessions (
			id               TEXT PRIMARY KEY,
			tool             TEXT,
			session_id       TEXT,
			cwd              TEXT,
			state            TEXT,
			title            TEXT,
			started_at       INTEGER,
			last_activity_at INTEGER,
			message_count    INTEGER,
			updated_at       INTEGER,
			data             TEXT NOT NULL
		);
		CREATE INDEX IF NOT EXISTS idx_sessions_last ON sessions(last_activity_at);
	`); err != nil {
		return nil, err
	}
	return &SessionDB{db: db}, nil
}

// Save upserts every session by id. Called on a ticker and at shutdown. The
// full record is stored as a JSON blob (so the schema never has to track every
// Session field) plus a few promoted columns for querying/sorting.
func (d *SessionDB) Save(sessions []*Session) (int, error) {
	tx, err := d.db.Begin()
	if err != nil {
		return 0, err
	}
	stmt, err := tx.Prepare(`
		INSERT INTO sessions (id, tool, session_id, cwd, state, title, started_at, last_activity_at, message_count, updated_at, data)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			tool=excluded.tool, session_id=excluded.session_id, cwd=excluded.cwd,
			state=excluded.state, title=excluded.title, started_at=excluded.started_at,
			last_activity_at=excluded.last_activity_at, message_count=excluded.message_count,
			updated_at=excluded.updated_at, data=excluded.data`)
	if err != nil {
		_ = tx.Rollback()
		return 0, err
	}
	defer stmt.Close()
	now := time.Now().UnixMilli()
	n := 0
	for _, s := range sessions {
		blob, err := json.Marshal(s)
		if err != nil {
			continue
		}
		if _, err := stmt.Exec(s.ID, string(s.Tool), s.SessionID, s.Cwd, string(s.State),
			s.Title, s.StartedAt, s.LastActivityAt, s.MessageCount, now, string(blob)); err != nil {
			_ = tx.Rollback()
			return n, err
		}
		n++
	}
	return n, tx.Commit()
}

// LoadAll returns every persisted session, newest activity first.
func (d *SessionDB) LoadAll() ([]*Session, error) {
	rows, err := d.db.Query(`SELECT data FROM sessions ORDER BY last_activity_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Session
	for rows.Next() {
		var blob string
		if err := rows.Scan(&blob); err != nil {
			continue
		}
		var s Session
		if json.Unmarshal([]byte(blob), &s) == nil && s.ID != "" {
			out = append(out, &s)
		}
	}
	return out, rows.Err()
}

func (d *SessionDB) Count() int {
	var n int
	_ = d.db.QueryRow(`SELECT COUNT(*) FROM sessions`).Scan(&n)
	return n
}

func (d *SessionDB) Close() error { return d.db.Close() }

// importSnapshotIfEmpty seeds the DB from the newest sessions-snapshot-*.json
// (written by hand before the sqlite migration) the first time the DB is empty,
// so the migration restart doesn't lose the sessions that were only in memory.
func (d *SessionDB) importSnapshotIfEmpty() {
	if d.Count() > 0 {
		return
	}
	dir := stateDir()
	entries, _ := filepath.Glob(filepath.Join(dir, "sessions-snapshot-*.json"))
	if len(entries) == 0 {
		return
	}
	sort.Strings(entries)
	newest := entries[len(entries)-1]
	raw, err := os.ReadFile(newest)
	if err != nil {
		return
	}
	var wrap struct {
		Sessions []*Session `json:"sessions"`
	}
	if json.Unmarshal(raw, &wrap) != nil || len(wrap.Sessions) == 0 {
		return
	}
	if n, err := d.Save(wrap.Sessions); err == nil {
		log.Printf("sessions.db: imported %d session(s) from %s", n, filepath.Base(newest))
	}
}

// StartSessionPersistence loads persisted history into the store, then snapshots
// the store to SQLite every `every` and returns a flush func for shutdown.
func StartSessionPersistence(s *Store, db *SessionDB, every time.Duration) (flush func()) {
	db.importSnapshotIfEmpty()
	if hist, err := db.LoadAll(); err == nil && len(hist) > 0 {
		added := s.LoadHistorical(hist)
		log.Printf("sessions.db: restored %d historical session(s) (%d already live)", added, len(hist)-added)
	}
	snapshot := func() {
		if n, err := db.Save(s.All()); err != nil {
			log.Printf("sessions.db: snapshot failed: %v", err)
		} else {
			log.Printf("sessions.db: snapshotted %d session(s)", n)
		}
	}
	go func() {
		t := time.NewTicker(every)
		defer t.Stop()
		for range t.C {
			snapshot()
		}
	}()
	return snapshot
}
