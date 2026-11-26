package main

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// CURSOR_DESKTOP=1 go test -run TestCursorDesktopReal -v reads the real Cursor desktop database.
func TestCursorDesktopReal(t *testing.T) {
	if os.Getenv("CURSOR_DESKTOP") == "" {
		t.Skip("set CURSOR_DESKTOP=1")
	}
	st := NewStore()
	t0 := time.Now()
	newest, err := scanCursorDesktop(st, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("scan: %d sessions, newest %d, %v", len(st.All()), newest, time.Since(t0))
	for _, id := range []string{"d1063518-28fd-4dd9-9369-77f3d5284cdd", "957bfc26-81cf-493b-bc31-51d2016ed7a5", "6cf80e51-293c-4c0b-afc9-2171fe17c6ef"} {
		t0 := time.Now()
		items, runs, err := cursorDesktopItems("cursor:"+id, id)
		if err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		t.Logf("%s: %d items, %d runs, %v", id[:8], len(items), len(runs), time.Since(t0))
		for _, it := range items[:min(6, len(items))] {
			t.Logf("   %-9s %-9s %.60q out=%d", it.Kind, it.Status, firstOf(it.Command, it.Text), len(it.Output))
		}
		if s := st.Get("cursor:" + id); s != nil {
			t.Logf("   session: %q cwd=%s state=%s", s.Title, s.Cwd, s.State)
		}
	}
}

func TestCursorStateOrder(t *testing.T) {
	if got := cursorStateOrder("~CiA" + "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="); len(got) != 1 {
		t.Fatalf("order %v", got)
	}
	if cursorStateOrder("") != nil || cursorStateOrder("~not base64!") != nil {
		t.Fatal("bad state must give no ids")
	}
}

func TestCursorFirstPrompt(t *testing.T) {
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "s.vscdb"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, q := range []string{
		`CREATE TABLE cursorDiskKV (key TEXT PRIMARY KEY, value BLOB)`,
		`INSERT INTO cursorDiskKV VALUES ('composerData:c1', '{"fullConversationHeadersOnly":[{"bubbleId":"b0","type":2},{"bubbleId":"b1","type":1}]}')`,
		`INSERT INTO cursorDiskKV VALUES ('bubbleId:c1:b0', '{"type":2,"text":"assistant first"}')`,
		`INSERT INTO cursorDiskKV VALUES ('bubbleId:c1:b1', '{"type":1,"text":"  Fix the login page\nwith details"}')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if got := cursorFirstPrompt(db, "c1"); got != "Fix the login page" {
		t.Fatalf("first prompt = %q", got)
	}
	if got := cursorFirstPrompt(db, "missing"); got != "" {
		t.Fatalf("missing chat = %q", got)
	}
}
