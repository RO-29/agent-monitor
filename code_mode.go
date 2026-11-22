package main

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
)

// Claude Code's Code Mode runs a script with the REPL tool. The script's own tool calls are written later as
// isVirtual records (ids "repl_…"), right after the repl-eval event that ends the script.

type replEvent struct {
	eval   string
	status string // "settled" when the script finished without an error
	at     int64  // ms
	ms     int64  // script run time
	result string
}

var (
	reReplEvent = regexp.MustCompile(`<event\b([^>]*)>([\s\S]*?)</event>`)
	reReplAttr  = regexp.MustCompile(`(\w+)="([^"]*)"`)
	reReplRes   = regexp.MustCompile(`<result>([\s\S]*?)</result>`)
)

// parseReplEvents returns the repl-eval events in an event delivery (an attachment or a Poll result).
func parseReplEvents(text string) []replEvent {
	if !strings.Contains(text, `kind="repl-eval"`) {
		return nil
	}
	var out []replEvent
	for _, m := range reReplEvent.FindAllStringSubmatch(text, -1) {
		a := map[string]string{}
		for _, kv := range reReplAttr.FindAllStringSubmatch(m[1], -1) {
			a[kv[1]] = kv[2]
		}
		if a["kind"] != "repl-eval" || a["eval"] == "" {
			continue
		}
		ms, _ := strconv.ParseInt(a["ms"], 10, 64)
		ev := replEvent{eval: a["eval"], status: a["status"], at: parseTimestamp(a["at"]), ms: ms}
		if r := reReplRes.FindStringSubmatch(m[2]); r != nil {
			ev.result = strings.TrimSpace(r[1])
		} else {
			ev.result = strings.TrimSpace(m[2])
		}
		out = append(out, ev)
	}
	return out
}

// replDispatch reads a REPL tool result: the eval id, and whether the script still runs (async).
func replDispatch(raw json.RawMessage) (eval string, async bool) {
	var r struct {
		EvalID          any  `json:"evalId"`
		AsyncDispatched bool `json:"asyncDispatched"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &r) != nil {
		return "", false
	}
	switch v := r.EvalID.(type) {
	case string:
		eval = v
	case float64:
		eval = strconv.FormatFloat(v, 'f', -1, 64)
	}
	return eval, r.AsyncDispatched
}

// replLinks maps eval ids to script items, and holds the script whose inner calls come next.
type replLinks struct {
	byEval map[string]*wItem
	cur    *wItem
}

// dispatched records a REPL result; true means the script runs on and its event completes the item.
func (l *replLinks) dispatched(wi *wItem, raw json.RawMessage) bool {
	eval, async := replDispatch(raw)
	if eval != "" {
		if l.byEval == nil {
			l.byEval = map[string]*wItem{}
		}
		l.byEval[eval] = wi
	}
	if !async {
		l.cur = wi
	}
	return async
}
