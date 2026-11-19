package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"strings"
)

func compactJSON(b []byte) string {
	var buf bytes.Buffer
	if json.Compact(&buf, b) != nil {
		return string(b)
	}
	return buf.String()
}

// unwire removes what we added; adopted entries and edited entries stay.
func (c *wireCtx) unwire(agent string) []wireResult {
	var out []wireResult
	var keep, gone []ledgerEntry
	// Newest first, so a container we created is pruned after every entry inside it is gone.
	for i := len(c.led.Entries) - 1; i >= 0; i-- {
		e := c.led.Entries[i]
		if agent != "" && e.Agent != agent {
			keep = append(keep, e)
			continue
		}
		if e.Action != "added" || e.Removed {
			continue // adopted entries are never ours to remove, and removed ones are already gone
		}
		res := wireResult{Agent: e.Agent, Kind: e.Kind, File: e.File, Entries: []string{e.ID}}
		if err := c.unwireOne(e); err != nil {
			res.Action, res.Detail = "manual: remove "+e.ID, err.Error()
			keep = append(keep, e)
		} else {
			res.Action = "removed"
			gone = append(gone, e)
		}
		out = append(out, res)
	}
	for i, j := 0, len(keep)-1; i < j; i, j = i+1, j-1 {
		keep[i], keep[j] = keep[j], keep[i]
	}
	c.led.Entries, c.dirty = keep, true
	c.finishFresh(gone)
	return out
}

func (c *wireCtx) unwireOne(e ledgerEntry) error {
	path := c.abs(e.File)
	switch e.Via {
	case "cli":
		cli := c.cliAt(e.Agent)
		if cli == "" {
			return errors.New("run: " + e.Agent + " mcp remove " + wireName)
		}
		if raw, err := os.ReadFile(path); err == nil {
			c.backupOnce(path, raw, modeOf(path))
		}
		args := []string{"mcp", "remove", wireName}
		if e.Agent == "claude" {
			args = append(args, "--scope", "user")
		}
		return wireRun(cli, args...)
	case "append":
		return c.modify(path, func(old []byte, ex bool) ([]byte, error) {
			o := string(old)
			if !ex || e.Written == "" || strings.Count(o, e.Written) != 1 {
				return nil, errors.New("the block was changed; delete the " + wireName + " table by hand")
			}
			o = strings.Replace(o, e.Written, "", 1)
			return []byte(o), nil
		})
	case "splice":
		return c.modify(path, func(old []byte, ex bool) ([]byte, error) {
			if !ex {
				return nil, errors.New("file is gone")
			}
			root, err := parseJSONSpans(old)
			if err != nil {
				return nil, err
			}
			var res []byte
			var container []string
			if e.Kind == "hook" {
				arr := root.at(e.Path)
				if arr == nil || arr.kind != '[' {
					return nil, errors.New("event list not found")
				}
				idx := -1
				for i, v := range arr.vals {
					if compactJSON(old[v.start:v.end]) == compactJSON([]byte(e.Written)) {
						idx = i
					}
				}
				if idx < 0 {
					return nil, errors.New("the hook entry was changed; remove it by hand")
				}
				res, container = removeItem(old, arr, idx), e.Path
			} else {
				container = e.Path[:len(e.Path)-1]
				m := root.at(container)
				if m == nil || m.kind != '{' {
					return nil, errors.New("entry not found")
				}
				idx := -1
				for i, k := range m.keys {
					if k == e.Path[len(e.Path)-1] && compactJSON(old[m.vals[i].start:m.vals[i].end]) == compactJSON([]byte(e.Written)) {
						idx = i
					}
				}
				if idx < 0 {
					return nil, errors.New("the entry was changed; remove it by hand")
				}
				res = removeItem(old, m, idx)
			}
			res, err = pruneAfterRemove(res, container, e.Created)
			if err != nil || !json.Valid(res) {
				return nil, errors.New("removal left invalid JSON")
			}
			return res, nil
		})
	}
	return nil
}

// finishFresh deletes a file we created, or empties one that held nothing, once nothing of the user is left.
func (c *wireCtx) finishFresh(removed []ledgerEntry) {
	done := map[string]bool{}
	for _, e := range removed {
		if e.Fresh == "" || done[e.File] {
			continue
		}
		done[e.File] = true
		path := c.abs(e.File)
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		left := false
		if e.Via == "append" {
			left = strings.TrimSpace(string(raw)) != ""
		} else if root, perr := parseJSONSpans(raw); perr == nil {
			left = compactJSON(raw) != compactJSON([]byte(e.Tmpl)) && len(root.vals) > 0
		} else {
			left = true
		}
		if left {
			continue
		}
		if e.Fresh == "absent" {
			_ = os.Remove(path)
		} else {
			_ = os.WriteFile(path, []byte(e.Orig), modeOf(path))
		}
	}
}
