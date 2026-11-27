package main

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
)

var orchMgr *OrchManager

// registerOrchRoutes wires /api/orch/*; it needs the approval store, so it runs after registerApprovalRoutes.
func registerOrchRoutes(mux *http.ServeMux) {
	store, err := openOrchStore(stateDir(), nil)
	if err != nil {
		log.Printf("orch: disabled: %v", err)
		return
	}
	orchMgr = newOrchManager(store, approvalStore)
	drivenSessionFn = orchMgr.DrivenLive
	go func() {
		time.Sleep(5 * time.Second) // the HTTP server and its MCP routes must answer first
		if ids := orchMgr.ContinueInterrupted(); len(ids) > 0 {
			log.Printf("orch: continued %d turns the restart ended", len(ids))
		}
	}()
	go func() {
		for {
			if n, err := store.Prune(time.Now()); err != nil {
				log.Printf("orch: prune: %v", err)
			} else if n > 0 {
				log.Printf("orch: pruned %d events", n)
			}
			time.Sleep(time.Hour)
		}
	}()
	go func() {
		for ; ; time.Sleep(time.Hour) {
			orchUploadMu.Lock()
			orchMgr.pruneUploads()
			orchUploadMu.Unlock()
		}
	}()
	go func() {
		for range time.Tick(30 * time.Second) {
			orchMgr.spendSweep()
		}
	}()
	go func() {
		for range time.Tick(time.Minute) {
			if err := store.SweepGroups(); err != nil {
				log.Printf("orch: groups: %v", err)
			}
		}
	}()
	go func() {
		for {
			if n, err := orchMgr.PruneCheckpoints(time.Now()); err != nil {
				log.Printf("orch: prune checkpoints: %v", err)
			} else if n > 0 {
				log.Printf("orch: pruned checkpoints of %d threads", n)
			}
			time.Sleep(24 * time.Hour)
		}
	}()
	go orchMgr.scheduleLoop()
	startQueue(orchMgr)
	orchMgr.routes(mux)
}

// decodeOptional decodes a JSON body when one is present; it writes the 400 itself.
func decodeOptional(w http.ResponseWriter, r *http.Request, v any) bool {
	err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(v)
	if err != nil && !errors.Is(err, io.EOF) {
		orchWriteErr(w, orchBadRequest("bad json"))
		return false
	}
	return true
}

func orchWriteErr(w http.ResponseWriter, err error) {
	var he *orchHTTPError
	var se *orchSpendError
	switch {
	case errors.As(err, &se):
		writeJSONStatus(w, http.StatusPaymentRequired, map[string]any{"error": se.Error(), "spent": round2(se.Spent), "limit": se.Limit, "scope": se.Scope})
	case errors.Is(err, errOrchNotFound):
		writeJSONStatus(w, http.StatusNotFound, map[string]any{"error": "thread not found"})
	case errors.As(err, &he):
		writeJSONStatus(w, he.code, map[string]any{"error": he.msg})
	default:
		writeJSONStatus(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
	}
}

func (m *OrchManager) routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/orch/threads", func(w http.ResponseWriter, r *http.Request) {
		threads, seq, err := m.store.Threads()
		if err != nil {
			orchWriteErr(w, err)
			return
		}
		writeJSON(w, map[string]any{"threads": threads, "seq": seq})
	})
	mux.HandleFunc("GET /api/orch/costs", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"costs": m.ThreadCosts()})
	})
	mux.HandleFunc("POST /api/orch/threads", func(w http.ResponseWriter, r *http.Request) {
		var in orchCreate
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&in); err != nil {
			orchWriteErr(w, orchBadRequest("bad json"))
			return
		}
		// A thread that runs with no approvals is a shell: admin only.
		if denyNoApprovals(w, r, in.ApprovalPolicy, in.PermissionMode, in.Sandbox) {
			return
		}
		th, err := m.CreateThread(in)
		if err != nil {
			orchWriteErr(w, err)
			return
		}
		writeJSON(w, map[string]any{"thread": th})
	})
	mux.HandleFunc("GET /api/orch/threads/{id}", func(w http.ResponseWriter, r *http.Request) {
		d, err := m.store.ThreadDetail(r.PathValue("id"))
		if err != nil {
			orchWriteErr(w, err)
			return
		}
		writeJSON(w, d)
	})
	mux.HandleFunc("POST /api/orch/threads/{id}/{action}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		var err error
		out := map[string]any{"ok": true}
		switch r.PathValue("action") {
		case "messages":
			var in struct {
				Text        string   `json:"text"`
				Attachments []string `json:"attachments"`
			}
			if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&in); err != nil {
				orchWriteErr(w, orchBadRequest("bad json"))
				return
			}
			if denyBang(w, r, in.Text) {
				return
			}
			var mode string
			mode, err = m.SendWith(id, in.Text, in.Attachments)
			out["mode"] = mode
		case "interrupt":
			// No body keeps the phase 2 behaviour; a body cascades unless cascade is false.
			in := struct {
				Cascade *bool `json:"cascade"`
			}{}
			body, rerr := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
			if rerr != nil || (len(strings.TrimSpace(string(body))) > 0 && json.Unmarshal(body, &in) != nil) {
				orchWriteErr(w, orchBadRequest("bad json"))
				return
			}
			cascade := len(strings.TrimSpace(string(body))) > 0 && (in.Cascade == nil || *in.Cascade)
			err = m.InterruptTree(id, cascade)
		case "close":
			in := struct {
				Cascade *bool `json:"cascade"`
			}{}
			if !decodeOptional(w, r, &in) {
				return
			}
			err = m.CloseTree(id, in.Cascade == nil || *in.Cascade)
		case "delegate":
			var in orchChildSpec
			if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&in); err != nil {
				orchWriteErr(w, orchBadRequest("bad json"))
				return
			}
			child, err := m.Delegate(id, in)
			if err != nil {
				orchWriteErr(w, err)
				return
			}
			writeJSONStatus(w, http.StatusCreated, map[string]any{"threadId": child.ID, "thread": child})
			return
		case "settle":
			var in struct {
				Settled bool `json:"settled"`
			}
			if !decodeOptional(w, r, &in) {
				return
			}
			_, err = m.UpdateMeta(id, func(th *OThread, now int64) {
				th.SettledAt = 0
				if in.Settled {
					th.SettledAt = now
				}
			})
		case "snooze":
			var in struct {
				Until int64 `json:"until"`
			}
			if !decodeOptional(w, r, &in) {
				return
			}
			if in.Until < 0 {
				orchWriteErr(w, orchBadRequest("until must be epoch ms or 0"))
				return
			}
			_, err = m.UpdateMeta(id, func(th *OThread, now int64) { th.SnoozedUntil = in.Until })
		case "pin":
			var in struct {
				Pinned bool `json:"pinned"`
			}
			if !decodeOptional(w, r, &in) {
				return
			}
			_, err = m.UpdateMeta(id, func(th *OThread, now int64) { th.Pinned = in.Pinned })
		case "rename":
			var in struct {
				Title string `json:"title"`
			}
			if !decodeOptional(w, r, &in) {
				return
			}
			title := cleanTitle(in.Title)
			if title == "" {
				orchWriteErr(w, orchBadRequest("title is required"))
				return
			}
			_, err = m.UpdateMeta(id, func(th *OThread, now int64) { th.Title = title })
		default:
			http.NotFound(w, r)
			return
		}
		if err != nil {
			orchWriteErr(w, err)
			return
		}
		if th, err := m.store.Thread(id); err == nil {
			out["thread"] = th
		}
		writeJSON(w, out)
	})
	mux.HandleFunc("GET /api/orch/threads/{id}/tasks", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		var ids []string
		for _, v := range strings.Split(q.Get("ids"), ",") {
			if v = strings.TrimSpace(v); v != "" {
				ids = append(ids, v)
			}
		}
		var wait time.Duration
		if v := q.Get("wait"); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 0 {
				orchWriteErr(w, orchBadRequest("wait must be seconds"))
				return
			}
			wait = min(time.Duration(n)*time.Second, orchWaitMax)
		}
		res, timedOut, err := m.Tasks(r.Context(), r.PathValue("id"), ids, wait)
		if err != nil {
			orchWriteErr(w, err)
			return
		}
		writeJSON(w, map[string]any{"tasks": res, "timedOut": timedOut})
	})
	m.uploadRoutes(mux)
	m.composerRoutes(mux)
	m.reviewRoutes(mux)
	m.templateRoutes(mux)
	m.scheduleRoutes(mux)
	m.continueRoute(mux)
	m.agentRoutes(mux)
	m.gitRoutes(mux)
	m.fixRoutes(mux)
	m.hunkRoutes(mux)
	m.spendRoutes(mux)
	m.queueRoutes(mux)
	mux.HandleFunc("GET /api/orch/stream", func(w http.ResponseWriter, r *http.Request) {
		serveOrchStream(m.store, w, r)
	})
}

// orchDrivenFor returns the driven thread id when a driven thread holds one of the sessions (see orchHandover).
func orchDrivenFor(sessions []string) string {
	if orchMgr == nil {
		return ""
	}
	for _, sid := range sessions {
		// Native ids are unique per agent, so any watched agent (opencode, cursor too) can match.
		if _, native, ok := strings.Cut(sid, ":"); ok {
			if id := orchMgr.DrivenID(native); id != "" && !orchMgr.handover(id).Terminal {
				return id
			}
		}
	}
	return ""
}

// withoutDriven drops watched threads that a driven thread already shows.
func withoutDriven(in []ThreadV3) []ThreadV3 {
	out := in[:0:0]
	for _, t := range in {
		if t.DrivenID == "" || t.Kind == "driven" {
			out = append(out, t)
		}
	}
	return out
}

// denyBang answers 403 when a caller below admin sends a "!" line, which runs a shell command with no approval.
func denyBang(w http.ResponseWriter, r *http.Request, text string) bool {
	if !belowAdmin(r) || !strings.HasPrefix(strings.TrimSpace(text), "!") {
		return false
	}
	writeJSONStatus(w, http.StatusForbidden, map[string]any{"error": "a ! shell command needs admin access"})
	return true
}

// denyNoApprovals answers 403 when a caller below admin asks for an agent that runs with no approvals; that agent is a shell.
func denyNoApprovals(w http.ResponseWriter, r *http.Request, policy, mode, sandbox string) bool {
	if !belowAdmin(r) || (policy != "never" && mode != "auto" && mode != "bypassPermissions" && sandbox != "danger-full-access") {
		return false
	}
	writeJSONStatus(w, http.StatusForbidden, map[string]any{"error": "a thread with no approvals needs admin access"})
	return true
}
