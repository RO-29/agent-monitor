package main

import (
	"bufio"
	"context"
	"encoding/json"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	orchFilesTTL   = 30 * time.Second
	orchFilesMax   = 50
	orchFilesScan  = 50000
	orchWalkDepth  = 8
	orchSkillsDesc = 300
	orchFilesCwds  = 16 // cached cwds; the least recently used one goes first
)

// orchWalkBudget bounds the directory walk of a folder outside git.
var orchWalkBudget = 3 * time.Second

type orchFileHit struct {
	Path string `json:"path"`
	Kind string `json:"kind"` // file | dir
}

type orchFileList struct {
	at, used time.Time
	items    []orchFileHit
}

var orchFileCache = struct {
	sync.Mutex
	m map[string]orchFileList
}{m: map[string]orchFileList{}}

// listFiles returns the files under cwd (git ls-files plus untracked non-ignored, or a bounded walk) and their dirs.
func listFiles(cwd string) []orchFileHit {
	orchFileCache.Lock()
	if c, ok := orchFileCache.m[cwd]; ok && time.Since(c.at) < orchFilesTTL {
		c.used = time.Now()
		orchFileCache.m[cwd] = c
		orchFileCache.Unlock()
		return c.items
	}
	orchFileCache.Unlock()
	var files []string
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if gitTop(cwd) != "" {
		out, _, err := gitRun{ctx: ctx, dir: cwd, cap: 32 << 20}.run("ls-files", "-z", "-co", "--exclude-standard")
		if err == nil {
			for _, p := range strings.Split(out, "\x00") {
				if p != "" && len(files) < orchFilesScan {
					files = append(files, p)
				}
			}
		}
	}
	if files == nil {
		wctx, wcancel := context.WithTimeout(context.Background(), orchWalkBudget)
		files = walkFiles(wctx, cwd)
		wcancel()
	}
	seen := map[string]bool{}
	items := make([]orchFileHit, 0, len(files)*2)
	for _, f := range files {
		for d := filepath.Dir(f); d != "." && d != "/" && !seen[d]; d = filepath.Dir(d) {
			seen[d] = true
			items = append(items, orchFileHit{Path: d, Kind: "dir"})
		}
		items = append(items, orchFileHit{Path: f, Kind: "file"})
	}
	orchFileCache.Lock()
	now := time.Now()
	orchFileCache.m[cwd] = orchFileList{at: now, used: now, items: items}
	for len(orchFileCache.m) > orchFilesCwds {
		oldest := ""
		for k, v := range orchFileCache.m {
			if oldest == "" || v.used.Before(orchFileCache.m[oldest].used) {
				oldest = k
			}
		}
		delete(orchFileCache.m, oldest)
	}
	orchFileCache.Unlock()
	return items
}

// walkFiles lists a non-git folder, skipping hidden entries and node_modules; it stops when ctx ends.
func walkFiles(ctx context.Context, cwd string) []string {
	out := []string{}
	_ = filepath.WalkDir(cwd, func(p string, d fs.DirEntry, err error) error {
		if ctx.Err() != nil {
			return filepath.SkipAll
		}
		if err != nil || p == cwd {
			return nil
		}
		rel, _ := filepath.Rel(cwd, p)
		name := d.Name()
		if d.IsDir() {
			if strings.HasPrefix(name, ".") || name == "node_modules" || strings.Count(rel, string(filepath.Separator)) >= orchWalkDepth {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasPrefix(name, ".") {
			return nil
		}
		out = append(out, rel)
		if len(out) >= orchFilesScan {
			return filepath.SkipAll
		}
		return nil
	})
	return out
}

// fuzzyScore matches q as a subsequence of path (case-insensitive); higher is better, -1 means no match.
func fuzzyScore(path, q string) int {
	if q == "" {
		return 0
	}
	lp, lq := strings.ToLower(path), strings.ToLower(q)
	base := strings.ToLower(filepath.Base(path))
	score, pi, run := 0, 0, 0
	for _, c := range lq {
		idx := strings.IndexRune(lp[pi:], c)
		if idx < 0 {
			return -1
		}
		if idx == 0 && pi > 0 {
			run++
			score += 2 * run
		} else {
			run = 0
		}
		at := pi + idx
		if at == 0 || strings.ContainsRune("/._-", rune(lp[at-1])) {
			score += 3
		}
		score++
		pi = at + 1
	}
	switch {
	case base == lq:
		score += 100
	case strings.HasPrefix(base, lq):
		score += 50
	case strings.Contains(base, lq):
		score += 30
	case strings.Contains(lp, lq):
		score += 15
	}
	return score
}

func searchFiles(items []orchFileHit, q string) []orchFileHit {
	type hit struct {
		orchFileHit
		score int
	}
	q = strings.TrimSpace(strings.TrimPrefix(q, "@"))
	hits := []hit{}
	for _, it := range items {
		if s := fuzzyScore(it.Path, q); s >= 0 {
			hits = append(hits, hit{it, s})
		}
	}
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].score != hits[j].score {
			return hits[i].score > hits[j].score
		}
		di, dj := strings.Count(hits[i].Path, "/"), strings.Count(hits[j].Path, "/")
		if di != dj {
			return di < dj
		}
		if len(hits[i].Path) != len(hits[j].Path) {
			return len(hits[i].Path) < len(hits[j].Path)
		}
		return hits[i].Path < hits[j].Path
	})
	out := make([]orchFileHit, 0, min(len(hits), orchFilesMax))
	for i := 0; i < len(hits) && i < orchFilesMax; i++ {
		out = append(out, hits[i].orchFileHit)
	}
	return out
}

// ── skills ──

type orchSkill struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Source      string `json:"source"` // user | project | plugin | codex-prompt
	Plugin      string `json:"plugin,omitempty"`
	Agent       string `json:"agent"` // claude | codex
	Insert      string `json:"insert"`
	Path        string `json:"path"`
}

// frontmatter reads the leading --- block of a Markdown file as flat key: value pairs.
func frontmatter(path string) map[string]string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	if !sc.Scan() || strings.TrimSpace(sc.Text()) != "---" {
		return map[string]string{}
	}
	out := map[string]string{}
	key, block := "", false
	for n := 0; sc.Scan() && n < 200; n++ {
		line := sc.Text()
		if strings.TrimSpace(line) == "---" {
			break
		}
		if block && (strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") || strings.TrimSpace(line) == "") {
			out[key] = strings.TrimSpace(out[key] + " " + strings.TrimSpace(line))
			continue
		}
		block = false
		if strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") {
			continue
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key, v = strings.TrimSpace(k), strings.TrimSpace(v)
		switch v {
		case ">", "|", ">-", "|-", ">+", "|+":
			block, v = true, ""
		default:
			if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
				if v[0] == '"' {
					var s string
					if json.Unmarshal([]byte(v), &s) == nil {
						v = s
					} else {
						v = v[1 : len(v)-1]
					}
				} else {
					v = strings.ReplaceAll(v[1:len(v)-1], "''", "'")
				}
			}
		}
		out[key] = v
	}
	return out
}

// skillDirs reads <dir>/<name>/SKILL.md entries.
func skillDirs(dir, source, plugin string) []orchSkill {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []orchSkill
	for _, e := range ents {
		p := filepath.Join(dir, e.Name(), "SKILL.md")
		if !pathExists(p) {
			continue
		}
		fm := frontmatter(p)
		name := firstOf(fm["name"], e.Name())
		insert := "/" + name
		if plugin != "" {
			insert = "/" + plugin + ":" + name
		}
		out = append(out, orchSkill{Name: name, Description: clipString(fm["description"], orchSkillsDesc), Source: source,
			Plugin: plugin, Agent: "claude", Insert: insert, Path: p})
	}
	return out
}

// pluginSkills scans the skills of installed Claude plugins that settings.json does not disable.
func pluginSkills(cfgDir string) []orchSkill {
	b, err := os.ReadFile(filepath.Join(cfgDir, "plugins", "installed_plugins.json"))
	if err != nil {
		return nil
	}
	var inst struct {
		Plugins map[string][]struct {
			InstallPath string `json:"installPath"`
		} `json:"plugins"`
	}
	if json.Unmarshal(b, &inst) != nil {
		return nil
	}
	var settings struct {
		EnabledPlugins map[string]bool `json:"enabledPlugins"`
	}
	if sb, err := os.ReadFile(filepath.Join(cfgDir, "settings.json")); err == nil {
		_ = json.Unmarshal(sb, &settings)
	}
	keys := make([]string, 0, len(inst.Plugins))
	for k := range inst.Plugins {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out []orchSkill
	for _, key := range keys {
		if on, ok := settings.EnabledPlugins[key]; ok && !on {
			continue
		}
		name, _, _ := strings.Cut(key, "@")
		for _, v := range inst.Plugins[key] {
			out = append(out, skillDirs(filepath.Join(v.InstallPath, "skills"), "plugin", name)...)
			break
		}
	}
	return out
}

func codexHome() string {
	if v := os.Getenv("CODEX_HOME"); v != "" {
		return v
	}
	return filepath.Join(homeDir(), ".codex")
}

func codexPrompts(dir string) []orchSkill {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []orchSkill
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		p := filepath.Join(dir, e.Name())
		name := strings.TrimSuffix(e.Name(), ".md")
		out = append(out, orchSkill{Name: name, Description: clipString(frontmatter(p)["description"], orchSkillsDesc),
			Source: "codex-prompt", Agent: "codex", Insert: "/prompts:" + name, Path: p})
	}
	return out
}

// Skills lists what the composer's $ picker offers for an agent ("" = claude and codex).
func (m *OrchManager) Skills(agent, cwd string) []orchSkill {
	out := []orchSkill{}
	if agent == "" || agent == "claude" {
		cfg := claudeConfigDir()
		out = append(out, skillDirs(filepath.Join(cfg, "skills"), "user", "")...)
		if cwd != "" {
			seen := map[string]bool{}
			for _, d := range []string{cwd, gitTop(cwd)} {
				if d == "" || seen[d] {
					continue
				}
				seen[d] = true
				out = append(out, skillDirs(filepath.Join(d, ".claude", "skills"), "project", "")...)
			}
		}
		out = append(out, pluginSkills(cfg)...)
	}
	if agent == "" || agent == "codex" {
		out = append(out, codexPrompts(filepath.Join(codexHome(), "prompts"))...)
	}
	return out
}

func (m *OrchManager) composerRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/orch/files", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		cwd := q.Get("cwd")
		if !filepath.IsAbs(cwd) {
			orchWriteErr(w, orchBadRequest("cwd must be an absolute path"))
			return
		}
		cwd = filepath.Clean(cwd)
		if !knownCwd(cwd) {
			orchWriteErr(w, &orchHTTPError{http.StatusForbidden, "cwd is not the folder of a watched session or a driven thread"})
			return
		}
		if st, err := os.Stat(cwd); err != nil || !st.IsDir() {
			orchWriteErr(w, orchBadRequest("cwd does not exist"))
			return
		}
		writeJSON(w, searchFiles(listFiles(cwd), q.Get("q")))
	})
	mux.HandleFunc("GET /api/orch/skills", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		cwd := q.Get("cwd")
		if cwd != "" && !filepath.IsAbs(cwd) {
			orchWriteErr(w, orchBadRequest("cwd must be an absolute path"))
			return
		}
		if cwd != "" && !knownCwd(cwd) {
			cwd = ""
		}
		agent := q.Get("agent")
		if agent != "" && agent != "claude" && agent != "codex" {
			writeJSON(w, []orchSkill{})
			return
		}
		writeJSON(w, m.Skills(agent, cwd))
	})
}
