package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
)

func (c *wireCtx) samePath(a, b string) bool {
	norm := func(p string) string {
		if p == "~" || strings.HasPrefix(p, "~/") {
			p = filepath.Join(c.home, p[1:])
		}
		if r, err := filepath.EvalSymlinks(p); err == nil {
			return r
		}
		return filepath.Clean(p)
	}
	return norm(a) == norm(b)
}

func (c *wireCtx) sameCmd(cmd string, args []string, wantCmd string, wantArgs []string) bool {
	if len(args) == 0 && len(wantArgs) == 0 {
		return c.samePath(cmd, wantCmd)
	}
	return c.samePath(cmd, wantCmd) && reflect.DeepEqual(args, wantArgs)
}

func (c *wireCtx) res(agent, kind, file string) wireResult {
	return wireResult{Agent: agent, Kind: kind, File: c.rel(file), Dry: c.dry}
}

func (c *wireCtx) restartFlag(r *wireResult) {
	if !c.dry && wireSessionsOlder != nil && (r.Action == "added" || r.Action == "needs trust in Codex") {
		r.RestartNeeded = wireSessionsOlder(r.Agent, c.now.UnixMilli())
	}
}

// wireSessionsOlder tells whether a live session of the agent started before the write.
var wireSessionsOlder func(agent string, at int64) bool

var mcpArgs = []string{"mcp-perm-server"}

func snippetJSON(path []string, v any) string {
	var x any = v
	for i := len(path) - 1; i >= 0; i-- {
		x = obj{{path[i], x}}
	}
	return string(jsonText(x))
}

// mcpJSON describes one agent's MCP entry in a JSON file.
type mcpJSON struct {
	agent      string
	file       string // under home
	path       []string
	unverified bool
	build      func(cmd string, args []string) any
	read       func(b []byte, n *jnode) (cmd string, args []string, ok bool)
}

func readCommandArgs(b []byte, n *jnode) (string, []string, bool) {
	cmd, ok := n.get("command").str(b)
	if !ok {
		return "", nil, false
	}
	args, ok := n.get("args").strList(b)
	return cmd, args, ok
}

func readCommandArray(b []byte, n *jnode) (string, []string, bool) {
	l, ok := n.get("command").strList(b)
	if !ok || len(l) == 0 {
		return "", nil, false
	}
	return l[0], l[1:], true
}

func buildCommandArgs(cmd string, args []string) any {
	return obj{{"command", cmd}, {"args", args}}
}

func (c *wireCtx) mcpJSONStep(s mcpJSON) wireResult {
	path := filepath.Join(c.home, s.file)
	res := c.res(s.agent, "mcp", path)
	res.Unverified = s.unverified
	cmd, args := c.bin(), mcpArgs
	full := append(append([]string{}, s.path...), wireName)
	snippet := "add to " + c.rel(path) + ": " + snippetJSON(full, s.build(cmd, args))
	var raw []byte
	exists := false
	if b, err := os.ReadFile(path); err == nil {
		raw, exists = b, true
	}
	prev := c.led.find(s.agent, res.File, wireName)
	if c.force[s.agent] && prev != nil && prev.Removed {
		c.dropLedger(s.agent, res.File, wireName)
		prev = nil
	}
	if exists && !emptyDoc(raw) {
		root, err := parseJSONSpans(raw)
		if err != nil || root.kind != '{' {
			res.Action, res.Detail = "manual", snippet
			return res
		}
		if cont := root.at(s.path); cont != nil || len(s.path) == 0 {
			if cont == nil {
				cont = root
			}
			if cont.kind != '{' {
				res.Action, res.Detail = "manual", snippet
				return res
			}
			if ent := cont.get(wireName); ent != nil {
				ec, ea, ok := "", []string(nil), ent.kind == '{'
				if ok {
					ec, ea, ok = s.read(raw, ent)
				}
				if !ok || !c.sameCmd(ec, ea, cmd, args) {
					return skipped(res, "user-edited")
				}
				return c.adopt(res, prev, wireName, "mcp")
			}
		}
	}
	if prev != nil {
		return c.tombstone(res, prev)
	}
	res.Action = "added"
	if !c.allow("mcp") {
		return c.held(res)
	}
	var written string
	var created []string
	err := c.modify(path, func(old []byte, ex bool) ([]byte, error) {
		base := old
		if !ex || emptyDoc(old) {
			base = []byte("{\n}\n")
		}
		leaf := s.build(cmd, args)
		out, cr, err := putAtPath(base, full, leaf)
		created, written = cr, string(jsonText(leaf))
		return out, err
	})
	if err != nil {
		return c.failed(res, err, snippet)
	}
	if !c.dry {
		e := ledgerEntry{Agent: s.agent, Kind: "mcp", File: res.File, ID: wireName, Action: "added", Via: "splice",
			Path: full, Created: created, Written: written}
		if !exists {
			e.Fresh, e.Tmpl = "absent", "{\n}\n"
		} else if emptyDoc(raw) {
			e.Fresh, e.Orig, e.Tmpl = "empty", string(raw), "{\n}\n"
		}
		c.setLedger(e)
	}
	c.restartFlag(&res)
	return res
}

func (c *wireCtx) failed(res wireResult, err error, snippet string) wireResult {
	if errors.Is(err, errChanged) {
		return skipped(res, "changed during write")
	}
	res.Action, res.Detail = "manual", snippet+" ("+err.Error()+")"
	return res
}

func (c *wireCtx) held(res wireResult) wireResult {
	if !c.dry {
		res.Action = "skipped: waiting for your confirmation"
	}
	return res
}

func (c *wireCtx) adopt(res wireResult, prev *ledgerEntry, id, kind string) wireResult {
	if prev != nil && !prev.Removed {
		res.Action = "unchanged"
		return res
	}
	res.Action = "adopted"
	if !c.dry {
		act := "adopted"
		if prev != nil && prev.Action == "added" && !prev.Removed {
			act = "added"
		}
		e := ledgerEntry{Agent: res.Agent, Kind: kind, File: res.File, ID: id, Action: act, Via: "adopt"}
		if prev != nil {
			e = *prev
			e.Removed, e.Action = false, act
		}
		c.setLedger(e)
	}
	return res
}

func (c *wireCtx) tombstone(res wireResult, prev *ledgerEntry) wireResult {
	res.Action = "removed by user"
	if !prev.Removed && !c.dry {
		prev.Removed = true
		c.dirty = true
	}
	return res
}

// hookJSON describes the hook entries one agent file takes.
type hookJSON struct {
	agent  string
	file   string
	events []string
	script string
	elem   func(ev, cmd string) any
	fresh  string // contents for a new file
	codex  bool
}

var scriptTokenRe = regexp.MustCompile(`\S+`)

func expandHome(p string) string {
	for _, pre := range []string{"$HOME/", "${HOME}/", "~/"} {
		if strings.HasPrefix(p, pre) {
			return filepath.Join(homeDir(), p[len(pre):])
		}
	}
	return p
}

// hookCmdIs matches the resolved script name under any agent-monitor checkout, never the exact command text.
func hookCmdIs(cmd, script, own string) bool {
	for _, t := range scriptTokenRe.FindAllString(cmd, -1) {
		t = expandHome(strings.Trim(t, `"'`))
		if r, err := filepath.EvalSymlinks(t); err == nil {
			t = r
		}
		if t == own || filepath.Base(t) == script {
			return true
		}
	}
	return false
}

func nodeHasHook(b []byte, n *jnode, script, own string) bool {
	switch n.kind {
	case '{':
		for i, k := range n.keys {
			if k == "command" {
				if s, ok := n.vals[i].str(b); ok && hookCmdIs(s, script, own) {
					return true
				}
			} else if nodeHasHook(b, n.vals[i], script, own) {
				return true
			}
		}
	case '[':
		for _, v := range n.vals {
			if nodeHasHook(b, v, script, own) {
				return true
			}
		}
	}
	return false
}

func (c *wireCtx) hookStep(s hookJSON) wireResult {
	path := filepath.Join(c.home, s.file)
	res := c.res(s.agent, "hook", path)
	script := filepath.Join(c.installDir, "bin", s.script)
	if st, err := os.Stat(script); err != nil || st.IsDir() {
		return skipped(res, c.rel(script)+" not found")
	}
	var raw []byte
	exists := false
	if b, err := os.ReadFile(path); err == nil {
		raw, exists = b, true
	}
	base := raw
	fresh := ""
	if !exists || emptyDoc(raw) {
		base, fresh = []byte(s.fresh), "absent"
		if exists {
			fresh = "empty"
		}
	}
	snippet := "add to " + c.rel(path) + " under hooks: " + string(jsonText(s.elem(s.events[0], script))) + " for " + strings.Join(s.events, ", ")
	if root, err := parseJSONSpans(base); err != nil || root.kind != '{' {
		res.Action, res.Detail = "manual", snippet
		return res
	} else if h := root.get("hooks"); h != nil && h.kind != '{' {
		res.Action, res.Detail = "manual", snippet
		return res
	}
	type plan struct {
		ev, act string
		prev    *ledgerEntry
	}
	var plans []plan
	root, _ := parseJSONSpans(base)
	for _, ev := range s.events {
		prev := c.led.find(s.agent, res.File, ev)
		if c.force[s.agent] && prev != nil && prev.Removed {
			c.dropLedger(s.agent, res.File, ev)
			prev = nil
		}
		arr := root.at([]string{"hooks", ev})
		if arr != nil && arr.kind != '[' {
			res.Action, res.Detail = "manual", snippet
			return res
		}
		switch {
		case arr != nil && nodeHasHook(base, arr, s.script, script):
			plans = append(plans, plan{ev, "adopt", prev})
		case prev != nil:
			plans = append(plans, plan{ev, "tombstone", prev})
		default:
			plans = append(plans, plan{ev, "add", nil})
		}
	}
	adds := 0
	for _, p := range plans {
		if p.act == "add" {
			adds++
		}
	}
	held := adds > 0 && !c.allow("hook")
	created := map[string][]string{}
	if adds > 0 && !held {
		err := c.modify(path, func(old []byte, ex bool) ([]byte, error) {
			b := old
			if !ex || emptyDoc(old) {
				b = []byte(s.fresh)
			}
			for _, p := range plans {
				if p.act != "add" {
					continue
				}
				r, err := parseJSONSpans(b)
				if err != nil {
					return nil, err
				}
				elem := s.elem(p.ev, script)
				if arr := r.at([]string{"hooks", p.ev}); arr != nil {
					b = insertElem(b, arr, elem)
					created[p.ev] = nil
				} else {
					var cr []string
					if b, cr, err = putAtPath(b, []string{"hooks", p.ev}, []any{elem}); err != nil {
						return nil, err
					}
					created[p.ev] = cr
				}
			}
			if !json.Valid(b) {
				return nil, errors.New("result is not valid JSON")
			}
			return b, nil
		})
		if err != nil {
			return c.failed(res, err, snippet)
		}
	}
	added, adopted, removed, marked := 0, 0, 0, false
	for _, p := range plans {
		switch p.act {
		case "add":
			if held {
				res.Entries = append(res.Entries, p.ev+": waiting for your confirmation")
				continue
			}
			added++
			res.Entries = append(res.Entries, p.ev+": added")
			if !c.dry {
				e := ledgerEntry{Agent: s.agent, Kind: "hook", File: res.File, ID: p.ev, Action: "added", Via: "splice",
					Path: []string{"hooks", p.ev}, Created: created[p.ev], Written: string(jsonText(s.elem(p.ev, script)))}
				if fresh != "" && !marked {
					e.Fresh, e.Orig, e.Tmpl, marked = fresh, string(raw), s.fresh, true
				}
				c.setLedger(e)
			}
		case "adopt":
			if p.prev != nil && !p.prev.Removed {
				continue
			}
			adopted++
			res.Entries = append(res.Entries, p.ev+": adopted")
			c.adoptHook(res, p.prev, p.ev)
		case "tombstone":
			if !p.prev.Removed {
				removed++
				res.Entries = append(res.Entries, p.ev+": removed by user")
				if !c.dry {
					p.prev.Removed = true
					c.dirty = true
				}
			}
		}
	}
	switch {
	case held:
		res.Action = "added"
		return c.held(res)
	case added > 0:
		res.Action = "added"
		if s.codex && !c.dry {
			res.Action, res.Detail = "needs trust in Codex", "Codex records a hash for each new hook entry when you trust it"
		} else if s.codex {
			res.Detail = "needs trust in Codex once added"
		}
	case removed > 0:
		res.Action = "removed by user"
	case adopted > 0:
		res.Action = "adopted"
	default:
		res.Action = "unchanged"
	}
	c.restartFlag(&res)
	return res
}

func (c *wireCtx) adoptHook(res wireResult, prev *ledgerEntry, ev string) {
	if c.dry {
		return
	}
	act := "adopted"
	e := ledgerEntry{Agent: res.Agent, Kind: "hook", File: res.File, ID: ev, Action: act, Via: "adopt"}
	if prev != nil {
		if prev.Removed {
			e.Written = prev.Written
		} else {
			e = *prev
		}
	}
	c.setLedger(e)
}
