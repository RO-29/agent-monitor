package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type wireResult struct {
	Agent         string   `json:"agent"`
	Kind          string   `json:"kind"` // mcp | hook | extension
	File          string   `json:"file"`
	Action        string   `json:"action"`
	Entries       []string `json:"entries,omitempty"`
	Detail        string   `json:"detail,omitempty"`
	Dry           bool     `json:"dry,omitempty"`
	RestartNeeded bool     `json:"restartNeeded,omitempty"`
	Unverified    bool     `json:"unverified,omitempty"`
}

type ledgerEntry struct {
	Agent   string   `json:"agent"`
	Kind    string   `json:"kind"`
	File    string   `json:"file"`
	ID      string   `json:"id"`
	Action  string   `json:"action"` // added | adopted
	Removed bool     `json:"removed,omitempty"`
	Via     string   `json:"via,omitempty"` // splice | cli | append
	Path    []string `json:"path,omitempty"`
	Created []string `json:"created,omitempty"`
	Written string   `json:"written,omitempty"` // exact text we added
	Fresh   string   `json:"fresh,omitempty"`   // absent | empty: the file did not exist or held nothing
	Orig    string   `json:"orig,omitempty"`
	Tmpl    string   `json:"tmpl,omitempty"` // what the new file held before our entry
	At      int64    `json:"at"`
}

type wireLedger struct {
	AutoWire map[string]string `json:"autoWire,omitempty"`
	Entries  []ledgerEntry     `json:"entries"`
}

func ledgerPath() string { return filepath.Join(stateDir(), "wiring.json") }

func loadLedger() *wireLedger {
	l := &wireLedger{}
	if raw, err := os.ReadFile(ledgerPath()); err == nil {
		_ = json.Unmarshal(raw, l)
	}
	if l.AutoWire == nil {
		l.AutoWire = map[string]string{}
	}
	return l
}

func (l *wireLedger) save() error {
	raw, _ := json.MarshalIndent(l, "", "  ")
	if err := os.MkdirAll(filepath.Dir(ledgerPath()), 0o755); err != nil {
		return err
	}
	tmp := ledgerPath() + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, ledgerPath())
}

func (l *wireLedger) find(agent, file, id string) *ledgerEntry {
	for i := range l.Entries {
		e := &l.Entries[i]
		if e.Agent == agent && e.File == file && e.ID == id {
			return e
		}
	}
	return nil
}

func (l *wireLedger) mode(kind string) string {
	if m := l.AutoWire[kind]; m == "on" || m == "off" {
		return m
	}
	return "ask"
}

var wireKinds = []string{"mcp", "hook", "extension"}

// wireRun starts a CLI; tests replace it so no real agent CLI ever runs.
var wireRun = func(bin string, args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, args...).CombinedOutput()
	if err != nil {
		return errors.New(strings.TrimSpace(string(out)) + ": " + err.Error())
	}
	return nil
}

// wireBeforeRename lets a test change the file between the read and the rename.
var wireBeforeRename = func() {}

type wireCtx struct {
	home       string
	installDir string
	dry        bool
	allow      func(kind string) bool
	led        *wireLedger
	dirty      bool
	now        time.Time
	force      map[string]bool
}

func (c *wireCtx) bin() string { return filepath.Join(c.installDir, "agent-monitor") }

func (c *wireCtx) rel(p string) string {
	if r, err := filepath.Rel(c.home, p); err == nil && !strings.HasPrefix(r, "..") {
		return "~/" + r
	}
	return p
}

func (c *wireCtx) abs(file string) string {
	if strings.HasPrefix(file, "~/") {
		return filepath.Join(c.home, file[2:])
	}
	return file
}

func (c *wireCtx) setLedger(e ledgerEntry) {
	e.At = c.now.Unix()
	if old := c.led.find(e.Agent, e.File, e.ID); old != nil {
		*old = e
	} else {
		c.led.Entries = append(c.led.Entries, e)
	}
	c.dirty = true
}

func (c *wireCtx) dropLedger(agent, file, id string) {
	for i, e := range c.led.Entries {
		if e.Agent == agent && e.File == file && e.ID == id {
			c.led.Entries = append(c.led.Entries[:i], c.led.Entries[i+1:]...)
			c.dirty = true
			return
		}
	}
}

func (c *wireCtx) backupOnce(path string, old []byte, mode os.FileMode) {
	bak := path + ".bak-am-" + c.now.UTC().Format("2006-01-02")
	if _, err := os.Stat(bak); err == nil {
		return
	}
	if os.WriteFile(bak, old, mode) == nil {
		_ = os.Chmod(bak, mode)
	}
	all, _ := filepath.Glob(path + ".bak-am-*")
	sort.Strings(all)
	for len(all) > 5 {
		_ = os.Remove(all[0])
		all = all[1:]
	}
}

func legacyBackups(path string) []string {
	all, _ := filepath.Glob(path + ".bak-*")
	out := []string{}
	for _, p := range all {
		if !strings.Contains(p, ".bak-am-") {
			out = append(out, p)
		}
	}
	return out
}

var errChanged = errors.New("changed during write")

// modify reads path, lets mut build the new bytes, and replaces the file atomically; it retries once.
func (c *wireCtx) modify(path string, mut func(old []byte, exists bool) ([]byte, error)) error {
	real := path
	if r, err := filepath.EvalSymlinks(path); err == nil {
		real = r
	}
	for attempt := 0; attempt < 2; attempt++ {
		st1, statErr := os.Stat(real)
		exists := statErr == nil
		var old []byte
		mode := os.FileMode(0o600)
		if exists {
			var err error
			if old, err = os.ReadFile(real); err != nil {
				return err
			}
			mode = st1.Mode().Perm()
		}
		out, err := mut(old, exists)
		if err != nil {
			return err
		}
		if c.dry || (exists && bytes.Equal(out, old)) {
			return nil
		}
		dir := filepath.Dir(real)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		tmp, err := os.CreateTemp(dir, ".am-wire-*")
		if err != nil {
			return err
		}
		_, werr := tmp.Write(out)
		if werr == nil {
			werr = tmp.Sync()
		}
		if cerr := tmp.Close(); werr == nil {
			werr = cerr
		}
		if werr == nil {
			werr = os.Chmod(tmp.Name(), mode)
		}
		if werr != nil {
			_ = os.Remove(tmp.Name())
			return werr
		}
		wireBeforeRename()
		st2, err2 := os.Stat(real)
		if (err2 == nil) != exists || (exists && (st2.Size() != st1.Size() || !st2.ModTime().Equal(st1.ModTime()))) {
			_ = os.Remove(tmp.Name())
			continue
		}
		if exists {
			c.backupOnce(real, old, mode)
		}
		if err := os.Rename(tmp.Name(), real); err != nil {
			_ = os.Remove(tmp.Name())
			return err
		}
		return nil
	}
	return errChanged
}

func skipped(res wireResult, why string) wireResult {
	res.Action = "skipped: " + why
	return res
}

func modeOf(path string) os.FileMode {
	if st, err := os.Stat(path); err == nil {
		return st.Mode().Perm()
	}
	return 0o600
}
