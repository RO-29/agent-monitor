package main

import (
	"database/sql"
	"encoding/json"
	"log"
	"strconv"
	"strings"
)

// Driven items are indexed as docs whose session is the orchestrator thread id (ot_...).
// Progress is the orch event seq; a gap (events pruned) or a fresh index rescans every item.

const drivenIndexBatch = 5000

func (ix *searchIndex) drivenTables() error {
	_, err := ix.w.Exec(`CREATE TABLE IF NOT EXISTS orch_docs(item_id TEXT PRIMARY KEY, doc_ids TEXT);
		CREATE TABLE IF NOT EXISTS search_meta(k TEXT PRIMARY KEY, v INTEGER);`)
	return err
}

// drivenDocs maps one finished orchestrator item to search docs.
func drivenDocs(it *OItem, th *OThread, repo string) []searchDoc {
	base := searchDoc{Session: th.ID, Agent: th.Agent, Cwd: th.Cwd, Repo: repo, Ts: it.StartedAt, Span: it.ID}
	doc := func(kind, tool, text, path string) searchDoc {
		d := base
		d.Kind, d.Tool, d.Text, d.Path = kind, tool, text, path
		return d
	}
	var out []searchDoc
	switch it.Kind {
	case "user":
		out = append(out, doc("prompt", "", clipString(it.Text, 8192), ""))
	case "agent":
		out = append(out, doc("reply", "", clipString(it.Text, 8192), ""))
	case "command":
		out = append(out, doc("command", "Bash", clipString(firstOf(it.Command, it.Text), 2048), ""))
		if it.By == "user" {
			markUserDocs(out[len(out)-1:])
		}
		if strings.TrimSpace(it.Output) != "" {
			kind := "output"
			if it.Status == "failed" || (it.ExitCode != nil && *it.ExitCode != 0) {
				kind = "error"
			}
			out = append(out, doc(kind, "Bash", clipString(it.Output, 4096), ""))
		}
	case "fileChange":
		paths := make([]string, 0, len(it.Files))
		for _, f := range it.Files {
			paths = append(paths, f.Path)
		}
		if len(paths) > 0 {
			out = append(out, doc("file", "Edit", "", strings.Join(paths, " ")))
		}
	case "error":
		out = append(out, doc("error", "", clipString(it.Text, 4096), ""))
	case "mcp", "web", "other":
		if t := strings.TrimSpace(it.Command + " " + it.Text); t != "" {
			out = append(out, doc("tool", it.Command, clipString(t+"\n"+it.Output, 1024), ""))
		}
	}
	return out
}

// indexDriven adds the items finished since the last pass; it returns how many items it indexed.
func (ix *searchIndex) indexDriven(store *OrchStore) (int, error) {
	if store == nil {
		return 0, nil
	}
	if err := ix.drivenTables(); err != nil {
		return 0, err
	}
	var seq int64
	_ = ix.w.QueryRow(`SELECT v FROM search_meta WHERE k='orch_seq'`).Scan(&seq)
	var items []OItem
	var threads map[string]*OThread
	var head int64
	err := store.read(func(tx *sql.Tx) error {
		head = currentSeq(tx)
		ths, err := scanJSONRows[OThread](tx.Query(`SELECT json FROM threads`))
		if err != nil {
			return err
		}
		threads = map[string]*OThread{}
		for i := range ths {
			threads[ths[i].ID] = &ths[i]
		}
		var oldest int64
		_ = tx.QueryRow(`SELECT COALESCE(MIN(seq),0) FROM events`).Scan(&oldest)
		if seq == 0 || seq > head || (oldest > 0 && oldest > seq+1) {
			items, err = scanJSONRows[OItem](tx.Query(`SELECT json FROM items WHERE json_extract(json,'$.status')<>'started'`))
			return err
		}
		rows, err := tx.Query(`SELECT seq, data FROM events WHERE seq>? AND type='item.completed' ORDER BY seq LIMIT ?`, seq, drivenIndexBatch)
		if err != nil {
			return err
		}
		ids := map[string]bool{}
		var last int64
		for rows.Next() {
			var data string
			if rows.Scan(&last, &data) != nil {
				continue
			}
			var ev struct {
				Item struct {
					ID string `json:"id"`
				} `json:"item"`
			}
			if json.Unmarshal([]byte(data), &ev) == nil && ev.Item.ID != "" {
				ids[ev.Item.ID] = true
			}
		}
		rows.Close()
		if last > 0 && len(ids) >= drivenIndexBatch {
			head = last
		}
		for id := range ids {
			if it, err := getJSON[OItem](tx, `SELECT json FROM items WHERE id=?`, id); err == nil {
				items = append(items, *it)
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	n := 0
	for i := range items {
		th := threads[items[i].ThreadID]
		if th == nil || items[i].Status == "started" {
			continue
		}
		if err := ix.putDrivenItem(&items[i], th); err != nil {
			return n, err
		}
		n++
	}
	_, err = ix.w.Exec(`INSERT INTO search_meta(k, v) VALUES('orch_seq', ?) ON CONFLICT(k) DO UPDATE SET v=excluded.v`, head)
	return n, err
}

// putDrivenItem replaces the docs of one item in a single transaction.
func (ix *searchIndex) putDrivenItem(it *OItem, th *OThread) error {
	docs := drivenDocs(it, th, ix.repoFor(th.Cwd))
	tx, err := ix.w.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var old string
	if tx.QueryRow(`SELECT doc_ids FROM orch_docs WHERE item_id=?`, it.ID).Scan(&old) == nil && old != "" {
		for _, s := range strings.Split(old, ",") {
			id, _ := strconv.ParseInt(s, 10, 64)
			var kind, text, path string
			if tx.QueryRow(`SELECT kind, text, path FROM docs WHERE id=?`, id).Scan(&kind, &text, &path) != nil {
				continue
			}
			if ix.fts5 {
				_, _ = tx.Exec(`INSERT INTO docs_fts(docs_fts, rowid, text, path) VALUES('delete', ?, ?, ?)`, id, text, path)
				_, _ = tx.Exec(`INSERT INTO docs_tri(docs_tri, rowid, text, path) VALUES('delete', ?, ?, ?)`, id, triText(kind, text), path)
			}
			if _, err := tx.Exec(`DELETE FROM docs WHERE id=?`, id); err != nil {
				return err
			}
		}
	}
	var ids []string
	for _, d := range docs {
		if strings.TrimSpace(d.Text) == "" && d.Path == "" {
			continue
		}
		res, err := tx.Exec(`INSERT INTO docs(session, agent, cwd, repo, ts, kind, tool, turn, span, path, text) VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
			d.Session, d.Agent, d.Cwd, d.Repo, d.Ts, d.Kind, d.Tool, d.Turn, d.Span, d.Path, d.Text)
		if err != nil {
			return err
		}
		id, _ := res.LastInsertId()
		ids = append(ids, strconv.FormatInt(id, 10))
		if ix.fts5 {
			if _, err := tx.Exec(`INSERT INTO docs_fts(rowid, text, path) VALUES(?,?,?)`, id, d.Text, d.Path); err != nil {
				return err
			}
			if _, err := tx.Exec(`INSERT INTO docs_tri(rowid, text, path) VALUES(?,?,?)`, id, triText(d.Kind, d.Text), d.Path); err != nil {
				return err
			}
		}
	}
	if _, err := tx.Exec(`INSERT INTO orch_docs(item_id, doc_ids) VALUES(?,?) ON CONFLICT(item_id) DO UPDATE SET doc_ids=excluded.doc_ids`, it.ID, strings.Join(ids, ",")); err != nil {
		return err
	}
	return tx.Commit()
}

// drivenPass runs after each transcript pass.
func (ix *searchIndex) drivenPass() {
	if orchMgr == nil {
		return
	}
	if _, err := ix.indexDriven(orchMgr.store); err != nil {
		log.Printf("search: driven items: %v", err)
	}
}

// drivenHitThread links a hit to its driven thread: a driven doc, or a watched session a driven thread owns.
func drivenHitThread(session string) string {
	if strings.HasPrefix(session, "ot_") {
		return session
	}
	return orchDrivenFor([]string{session})
}
