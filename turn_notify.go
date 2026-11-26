package main

import (
	"strings"
	"sync"
	"time"
)

// Notices for watched sessions, which have no driven-thread events: a turn that ended, asked or was interrupted,
// and terminal prompts (including the ones an agent shows on /exit).
const turnNotifyWindow = 30 * time.Minute

type turnWatchState struct {
	act    int64  // the session's LastActivityAt at the last read
	cursor int64  // the watched-items cursor
	run    string // the last run announced (or seen at start)
}

var turnWatch = struct {
	mu      sync.Mutex
	seen    map[string]*turnWatchState
	prompts map[string]string // key -> prompt fingerprint: one session key covers many prompts
	primed  bool
}{seen: map[string]*turnWatchState{}}

var turnNotifyEmit = func(ev notifyEvent) {
	if notifier != nil {
		notifier.Emit(ev)
	}
}

var turnNotifyResolve = func(key string) {
	if notifier != nil {
		notifier.Resolve(key)
	}
}

func startTurnNotifier() {
	go func() {
		t := time.NewTicker(3 * time.Second)
		defer t.Stop()
		for range t.C {
			if notifier == nil || store == nil {
				continue
			}
			turnNotifyTick(store.All(), time.Now())
			promptNotifyTick(needsYou())
			notifier.remindTick()
		}
	}()
}

func turnNotifyTick(all []*Session, now time.Time) {
	turnWatch.mu.Lock()
	defer turnWatch.mu.Unlock()
	for _, s := range all {
		if s.TranscriptPath == "" || now.UnixMilli()-s.LastActivityAt > turnNotifyWindow.Milliseconds() {
			continue
		}
		st := turnWatch.seen[s.ID]
		if st != nil && st.act == s.LastActivityAt {
			continue
		}
		ref := watchedRef{ID: s.ID, Tool: s.Tool, RawID: s.SessionID, Path: s.TranscriptPath, Cwd: s.Cwd, Model: s.Model, Resolved: true}
		cur := int64(-1)
		if st != nil {
			cur = st.cursor
		}
		out, err := watchedItemsFor(ref, cur, -1)
		if err != nil {
			continue
		}
		run := lastEndedRun(out.Runs)
		if st == nil || out.Reset {
			// First sight: remember where the session is, announce nothing old.
			st = &turnWatchState{}
			turnWatch.seen[s.ID] = st
			if run != nil {
				st.run = run.ID
			}
		} else if run != nil && run.ID != st.run {
			st.run = run.ID
			if ev, ok := turnNotice(s.ID, firstOf(s.Title, firstLine(s.FirstMessage), "Agent"), run, out.Items); ok {
				turnNotifyEmit(ev)
			}
		}
		st.act, st.cursor = s.LastActivityAt, out.Cursor
	}
}

func lastEndedRun(runs []*ORun) *ORun {
	var last *ORun
	for _, r := range runs {
		if (r.Status == "completed" || r.Status == "interrupted") && r.EndedAt > 0 && (last == nil || r.EndedAt >= last.EndedAt) {
			last = r
		}
	}
	return last
}

// turnNotice: an interrupted turn, a turn whose last answer ends on a question, or a finished turn.
func turnNotice(id, title string, run *ORun, items []*OItem) (notifyEvent, bool) {
	if run.Status == "interrupted" {
		return notifyEvent{Kind: "error", ThreadID: id, Title: title + " was interrupted", Body: clipString(firstLine(strings.TrimSpace(run.Prompt)), 300)}, true
	}
	answer := ""
	for _, it := range items {
		if it.RunID == run.ID && it.Kind == "agent" && strings.TrimSpace(it.Text) != "" {
			answer = strings.TrimSpace(it.Text)
		}
	}
	if q := endingQuestion(answer); q != "" {
		return notifyEvent{Kind: "needs-you", ThreadID: id, Title: title + " asks you", Body: clipString(q, 300)}, true
	}
	return notifyEvent{Kind: "finished", ThreadID: id, Title: title + " finished", Body: clipString(firstLine(answer), 300)}, true
}

// endingQuestion returns the answer's last line when it asks something, else "".
func endingQuestion(answer string) string {
	ln := strings.TrimSpace(lastLine(strings.TrimRight(answer, " \n*_`")))
	ln = strings.Trim(ln, "*_` ")
	if strings.HasSuffix(ln, "?") && len(ln) > 3 {
		return ln
	}
	return ""
}

// promptNotifyTick announces each new terminal prompt or question once, and settles the notice when it closes.
func promptNotifyTick(items []NeedsYouItem) {
	turnWatch.mu.Lock()
	defer turnWatch.mu.Unlock()
	now := map[string]string{}
	for _, it := range items {
		if it.Kind != "prompt" && it.Kind != "question" {
			continue
		}
		fp := it.PromptKey
		old, had := turnWatch.prompts[it.Key]
		if fp == "" && had {
			fp = old // a screen read failed this tick: not a new prompt
		}
		now[it.Key] = fp
		if !turnWatch.primed || had && (old == fp || old == "") {
			continue
		}
		if had {
			turnNotifyResolve(it.Key) // prompt A closed and prompt B opened between two ticks
		}
		title := firstOf(it.Title, "Agent")
		body, head := it.Message, title+" needs you"
		if it.Kind == "question" {
			head = title + ": a question for you"
			if q := it.Question; q != nil && len(q.Questions) > 0 {
				body = q.Questions[min(q.Step, len(q.Questions)-1)].Question
			} else if q != nil && q.Tool == "ExitPlanMode" {
				head = title + ": a plan to review"
			}
		}
		turnNotifyEmit(notifyEvent{Kind: "needs-you", ThreadID: it.SessionID, Title: head, Body: clipString(body, 300), Keys: []string{it.Key}})
	}
	for k := range turnWatch.prompts {
		if _, ok := now[k]; !ok {
			turnNotifyResolve(k)
		}
	}
	turnWatch.prompts, turnWatch.primed = now, true
}
