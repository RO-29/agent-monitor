package main

import (
	"context"
	"database/sql"
	"time"
)

// orchDriver is one live driven thread, whatever agent runs it.
type orchDriver interface {
	send(text string, imgs []orchImage) (string, error)
	interrupt() error
	close()
	// shutdown stops the child process and leaves the thread state alone.
	shutdown()
	// live is true while the child process runs a turn of an open thread.
	live() bool
	// procAlive is true while the child process exists, between turns too.
	procAlive() bool
}

// orchBase holds the store bookkeeping that every agent adapter shares.
type orchBase struct {
	m  *OrchManager
	id string
}

func (b *orchBase) emitItem(t *orchTx, it *OItem) error {
	if it.Kind == "error" {
		it.Text = reANSI.ReplaceAllString(it.Text, "")
	}
	if err := t.putItem(it); err != nil {
		return err
	}
	t.emit(b.id, "item.started", map[string]any{"item": it})
	t.emit(b.id, "item.completed", map[string]any{"item": it})
	return nil
}

func (b *orchBase) putThreadTx(t *orchTx, th *OThread) {
	th.UpdatedAt = t.now
	_ = t.putThread(th)
	t.emit(b.id, "thread.updated", map[string]any{"thread": th})
}

// beginRun records a new run with its user item and marks the thread running.
func (b *orchBase) beginRun(runID, text string, imgs []orchImage) error {
	now := time.Now().UnixMilli()
	err := b.m.store.write(func(t *orchTx) error {
		th, err := t.thread(b.id)
		if err != nil {
			return err
		}
		var n int
		_ = t.tx.QueryRow(`SELECT COUNT(*) FROM runs WHERE thread_id=?`, b.id).Scan(&n)
		run := &ORun{ID: runID, ThreadID: b.id, Index: n, Prompt: text, Status: "running", StartedAt: now}
		item := &OItem{ID: runID + ".user", ThreadID: b.id, RunID: runID, Kind: "user", Status: "completed", Text: text, Attachments: imageAttachments(imgs), StartedAt: now, EndedAt: now}
		if err := t.putRun(run); err != nil {
			return err
		}
		t.emit(b.id, "run.started", map[string]any{"run": run})
		if err := b.emitItem(t, item); err != nil {
			return err
		}
		if th.Status != "starting" {
			th.Status = "running"
		}
		if th.Title == "" {
			th.Title = orchTitle(text)
		}
		th.Error = ""
		th.SettledAt, th.ReportedAt = 0, 0
		b.putThreadTx(t, th)
		return nil
	})
	if err == nil {
		b.m.checkpointRun(b.id, runID, "start")
	}
	return err
}

// addUserItem records a steer message inside a running run.
func (b *orchBase) addUserItem(runID, text string, imgs []orchImage) {
	now := time.Now().UnixMilli()
	item := &OItem{ID: orchID(runID + ".u"), ThreadID: b.id, RunID: runID, Kind: "user", Status: "completed", Text: text, Attachments: imageAttachments(imgs), StartedAt: now, EndedAt: now}
	_ = b.m.store.write(func(t *orchTx) error { return b.emitItem(t, item) })
}

// writeRunFailure ends a run that never got going; threadErr also puts the thread in error.
func (b *orchBase) writeRunFailure(runID, msg string, threadErr bool) {
	defer b.m.afterRunEnd(b.id)
	b.m.checkpointRun(b.id, runID, "end")
	_ = b.m.store.write(func(t *orchTx) error {
		now := t.now
		if run, err := t.run(runID); err == nil {
			run.Status, run.EndedAt = "failed", now
			_ = t.putRun(run)
			t.emit(b.id, "run.ended", map[string]any{"run": run})
		}
		_ = b.emitItem(t, &OItem{ID: runID + ".err", ThreadID: b.id, RunID: runID, Kind: "error", Status: "failed", Text: msg, StartedAt: now, EndedAt: now})
		th, err := t.thread(b.id)
		if err != nil || newerRunTx(t, b.id, runID) {
			return nil
		}
		if th.Status != "closed" {
			th.Status = "idle"
			if threadErr {
				th.Status, th.Error = "error", reANSI.ReplaceAllString(msg, "")
			}
		}
		b.putThreadTx(t, th)
		return nil
	})
}

// writeRunEnd closes a run with its final status and returns the thread to idle.
func (b *orchBase) writeRunEnd(runID, status, errText string, usage *OUsage) {
	defer b.m.afterRunEnd(b.id)
	b.m.store.flushDeltas("")
	b.m.checkpointRun(b.id, runID, "end")
	_ = b.m.store.write(func(t *orchTx) error {
		if errText != "" {
			_ = b.emitItem(t, &OItem{ID: runID + ".err", ThreadID: b.id, RunID: runID, Kind: "error", Status: "failed", Text: errText, StartedAt: t.now, EndedAt: t.now})
		}
		if usage != nil {
			if run, err := t.run(runID); err == nil {
				run.Usage = usage
				_ = t.putRun(run)
			}
		}
		endRunTx(t, b.id, runID, status)
		th, err := t.thread(b.id)
		if err != nil || th.Status == "closed" || newerRunTx(t, b.id, runID) {
			return nil
		}
		th.Status = "idle"
		b.putThreadTx(t, th)
		return nil
	})
}

// newerRunTx is true when another run of the thread is still running (a send landed as this run ended).
func newerRunTx(t *orchTx, threadID, runID string) bool {
	var n int
	_ = t.tx.QueryRow(`SELECT COUNT(*) FROM runs WHERE thread_id=? AND idx > COALESCE((SELECT idx FROM runs WHERE id=?), -1)
		AND json_extract(json,'$.status')='running'`, threadID, runID).Scan(&n)
	return n > 0
}

// writeClosed marks the thread closed and interrupts its open run.
func (b *orchBase) writeClosed(runID string) {
	defer b.m.afterRunEnd(b.id)
	b.m.store.flushDeltas("")
	b.m.checkpointRun(b.id, runID, "end")
	_ = b.m.store.write(func(t *orchTx) error {
		if runID != "" {
			endRunTx(t, b.id, runID, "interrupted")
		}
		th, err := t.thread(b.id)
		if err != nil {
			return err
		}
		th.Status = "closed"
		b.putThreadTx(t, th)
		return nil
	})
}

// writeExit records an unexpected child exit: the open run fails and the thread shows the stderr tail.
func (b *orchBase) writeExit(runID, summary string, errored bool) {
	defer b.m.afterRunEnd(b.id)
	b.m.store.flushDeltas("")
	b.m.checkpointRun(b.id, runID, "end")
	_ = b.m.store.write(func(t *orchTx) error {
		if runID != "" {
			endRunTx(t, b.id, runID, "failed")
		}
		th, err := t.thread(b.id)
		if err != nil || th.Status == "closed" {
			return nil
		}
		if runID != "" || errored {
			th.Status, th.Error = "error", reANSI.ReplaceAllString(summary, "")
		} else {
			th.Status = "idle"
		}
		b.putThreadTx(t, th)
		return nil
	})
}

// awaitApproval runs a driven approval through rules, shows it as a request while it waits,
// and closes the request when it is answered. running reports whether a run is still active.
func (b *orchBase) awaitApproval(ctx context.Context, a *Approval, runID, itemID, kind string, running func() bool) (approvalDecision, string) {
	a.DrivenID = b.id
	a.RootID = b.m.store.RootOf(b.id)
	if th, err := b.m.store.Thread(b.id); err != nil || threadReadOnly(th) {
		a.noAutoAllow = true
	}
	var req *ORequest
	dec, outcome := b.m.approvals.awaitDriven(ctx, a, b.m.holdFor(), func(snap *Approval) {
		req = &ORequest{ID: orchID("rq_"), ThreadID: b.id, RunID: runID, ItemID: itemID, Kind: kind, ApprovalID: snap.ID, State: "open"}
		_ = b.m.store.write(func(t *orchTx) error {
			if err := t.putRequest(req); err != nil {
				return err
			}
			t.emit(b.id, "request.opened", map[string]any{"request": req})
			if th, err := t.thread(b.id); err == nil && th.Status != "closed" {
				th.Status = "waiting"
				b.putThreadTx(t, th)
			}
			return nil
		})
		b.m.notifyNeedsYou(b.id, snap)
	})
	if req == nil {
		return dec, outcome
	}
	_ = b.m.store.write(func(t *orchTx) error {
		req.State = "closed"
		if err := t.putRequest(req); err != nil {
			return err
		}
		t.emit(b.id, "request.closed", map[string]any{"request": req})
		var open int
		_ = t.tx.QueryRow(`SELECT COUNT(*) FROM requests WHERE thread_id=? AND json_extract(json,'$.state')='open'`, b.id).Scan(&open)
		th, err := t.thread(b.id)
		if err == nil && th.Status == "waiting" && open == 0 {
			th.Status = "idle"
			if running() {
				th.Status = "running"
			}
			b.putThreadTx(t, th)
		}
		return nil
	})
	return dec, outcome
}

func (b *orchBase) itemSnapshot(id string) (*OItem, error) {
	if id == "" {
		return nil, errOrchNotFound
	}
	var it *OItem
	err := b.m.store.read(func(tx *sql.Tx) error {
		var err error
		it, err = getJSON[OItem](tx, `SELECT json FROM items WHERE id=?`, id)
		return err
	})
	return it, err
}

// threadReadOnly is true for a read-only workspace; allow rules never answer for it.
func threadReadOnly(th *OThread) bool {
	return th.Workspace == "read-only"
}

func firstOf(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
