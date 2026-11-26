package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// OTemplate is a saved New thread form; prompts may use {{vars}}.
type OTemplate struct {
	ID             string          `json:"id"`
	Name           string          `json:"name"`
	Description    string          `json:"description,omitempty"`
	Agent          string          `json:"agent"`
	Model          string          `json:"model,omitempty"`
	PermissionMode string          `json:"permissionMode,omitempty"`
	Sandbox        string          `json:"sandbox,omitempty"`
	Workspace      string          `json:"workspace"`
	Where          string          `json:"where,omitempty"` // "terminal" or "headless"; empty is headless
	Prompt         string          `json:"prompt"`
	Children       []orchChildSpec `json:"children,omitempty"`
	Review         bool            `json:"review,omitempty"`
	ReviewAgent    string          `json:"reviewAgent,omitempty"`
	Builtin        bool            `json:"builtin,omitempty"`
	Vars           []string        `json:"vars"`
	CreatedAt      int64           `json:"createdAt,omitempty"`
	UpdatedAt      int64           `json:"updatedAt,omitempty"`
}

var (
	orchVarRe      = regexp.MustCompile(`\{\{\s*([A-Za-z_][A-Za-z0-9_]*)\s*\}\}`)
	orchTemplateMu sync.Mutex
)

var orchBuiltinTemplates = []OTemplate{
	{ID: "builtin-fix-and-review", Name: "Fix and review", Builtin: true,
		Description: "Claude fixes the task in a new worktree; Codex reviews the diff when the run ends.",
		Agent:       "claude", PermissionMode: "acceptEdits", Workspace: "worktree", Review: true, ReviewAgent: "codex",
		Prompt: "Fix this in the repository at {{cwd}} (branch {{branch}}):\n\n{{task}}\n\n" +
			"Keep the change small, run the tests that cover it, and say what you changed and how you checked it."},
	{ID: "builtin-read-only-investigation", Name: "Read-only investigation", Builtin: true,
		Description: "Claude in plan mode, read-only: it reads and reports, and changes no file.",
		Agent:       "claude", PermissionMode: "plan", Workspace: "read-only",
		Prompt: "Investigate this in {{cwd}} without changing any file:\n\n{{question}}\n\n" +
			"Report what you found with file paths and line numbers, and what you would change."},
}

func templateVars(t *OTemplate) []string {
	seen := map[string]bool{}
	out := []string{}
	add := func(s string) {
		for _, m := range orchVarRe.FindAllStringSubmatch(s, -1) {
			if !seen[m[1]] {
				seen[m[1]] = true
				out = append(out, m[1])
			}
		}
	}
	add(t.Prompt)
	for _, c := range t.Children {
		add(c.Title)
		add(c.Prompt)
	}
	return out
}

func (m *OrchManager) templatesPath() string { return filepath.Join(m.stateDir, "templates.json") }

func (m *OrchManager) userTemplates() ([]OTemplate, error) {
	return readJSONList[OTemplate](m, m.templatesPath())
}

// orchCorruptFile reports a state file that did not parse; it was moved aside, never overwritten.
type orchCorruptFile struct{ path, aside string }

func (e *orchCorruptFile) Error() string {
	return e.path + " did not parse and was moved to " + e.aside + "; nothing was changed, try again to start from an empty list"
}

// readJSONList reads a JSON list; a missing file is empty, any other read error is returned, and a
// file that does not parse is moved to <name>.corrupt-<time> and reported.
func readJSONList[T any](m *OrchManager, path string) ([]T, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return []T{}, nil
	}
	if err != nil {
		return nil, err
	}
	var out []T
	if err := json.Unmarshal(b, &out); err != nil {
		aside := path + ".corrupt-" + time.Now().Format("20060102-150405.000")
		if rerr := os.Rename(path, aside); rerr != nil {
			if errors.Is(rerr, os.ErrNotExist) {
				return []T{}, nil
			}
			return nil, fmt.Errorf("%s did not parse (%v) and could not be moved aside: %w", path, err, rerr)
		}
		log.Printf("orch: %s did not parse (%v); moved to %s", path, err, aside)
		m.notify(notifyEvent{Kind: "error", Title: filepath.Base(path) + " did not parse", Body: "Moved to " + aside})
		return nil, &orchCorruptFile{path: path, aside: aside}
	}
	if out == nil {
		out = []T{}
	}
	return out, nil
}

// writeJSONFile replaces path atomically: a synced temporary file is renamed over it, then the directory is synced.
func writeJSONFile(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		d.Close()
	}
	return nil
}

// Templates returns the built-ins, then the user's templates by name.
func (m *OrchManager) Templates() ([]OTemplate, error) {
	out := []OTemplate{}
	for _, t := range orchBuiltinTemplates {
		t.Vars = templateVars(&t)
		out = append(out, t)
	}
	user, err := m.userTemplates()
	if err != nil {
		return nil, err
	}
	sort.SliceStable(user, func(i, j int) bool { return strings.ToLower(user[i].Name) < strings.ToLower(user[j].Name) })
	for _, t := range user {
		t.Vars = templateVars(&t)
		out = append(out, t)
	}
	return out, nil
}

func (m *OrchManager) Template(id string) (*OTemplate, error) {
	all, err := m.Templates()
	if err != nil {
		return nil, err
	}
	for _, t := range all {
		if t.ID == id {
			return &t, nil
		}
	}
	return nil, &orchHTTPError{http.StatusNotFound, "template not found"}
}

// checkTemplate validates and normalizes a template body.
func (m *OrchManager) checkTemplate(t *OTemplate) error {
	t.Name = strings.TrimSpace(t.Name)
	if t.Name == "" || len([]rune(t.Name)) > 80 {
		return orchBadRequest("name must be 1 to 80 characters")
	}
	if t.Agent == "" {
		t.Agent = "codex"
	}
	if !m.validAgent(t.Agent) {
		return m.agentError()
	}
	if strings.TrimSpace(t.Prompt) == "" {
		return orchBadRequest("prompt is required")
	}
	switch t.Workspace {
	case "":
		t.Workspace = "here"
	case "here", "worktree", "read-only":
	default:
		return orchBadRequest("workspace must be here, worktree or read-only")
	}
	switch t.Where {
	case "", "headless":
	case "terminal":
		if t.Agent != "claude" && t.Agent != "codex" {
			return orchBadRequest("only Claude and Codex start in a terminal")
		}
	default:
		return orchBadRequest("where must be terminal or headless")
	}
	switch t.PermissionMode {
	case "", "manual", "acceptEdits", "plan", "auto":
	default:
		return orchBadRequest("permissionMode must be manual, acceptEdits, plan or auto")
	}
	switch t.Sandbox {
	case "", "read-only", "workspace-write":
	default:
		return orchBadRequest("sandbox must be read-only or workspace-write")
	}
	if len(t.Children) > orchMaxRunning {
		return orchBadRequest("too many children")
	}
	for i := range t.Children {
		c := &t.Children[i]
		if c.Agent == "" {
			c.Agent = t.Agent
		}
		if !m.validAgent(c.Agent) {
			return m.agentError()
		}
		if strings.TrimSpace(c.Prompt) == "" {
			return orchBadRequest("child prompt is required")
		}
		switch c.Workspace {
		case "", "shared", "worktree", "read-only":
		default:
			return orchBadRequest("child workspace must be shared, worktree or read-only")
		}
	}
	if t.ReviewAgent != "" && !m.validAgent(t.ReviewAgent) {
		return orchBadRequest("reviewAgent is not a known agent")
	}
	t.Builtin, t.Vars = false, nil
	return nil
}

func isBuiltinTemplate(id string) bool {
	for _, t := range orchBuiltinTemplates {
		if t.ID == id {
			return true
		}
	}
	return false
}

// SaveTemplate creates (id "") or replaces a user template.
func (m *OrchManager) SaveTemplate(id string, t OTemplate) (*OTemplate, error) {
	if isBuiltinTemplate(id) {
		return nil, &orchHTTPError{http.StatusForbidden, "built-in templates cannot be changed: copy it first"}
	}
	if err := m.checkTemplate(&t); err != nil {
		return nil, err
	}
	orchTemplateMu.Lock()
	defer orchTemplateMu.Unlock()
	ts, err := m.userTemplates()
	if err != nil {
		return nil, err
	}
	now := time.Now().UnixMilli()
	if id == "" {
		t.ID, t.CreatedAt, t.UpdatedAt = orchID("tpl_"), now, now
		ts = append(ts, t)
	} else {
		i := indexTemplate(ts, id)
		if i < 0 {
			return nil, &orchHTTPError{http.StatusNotFound, "template not found"}
		}
		t.ID, t.CreatedAt, t.UpdatedAt = id, ts[i].CreatedAt, now
		ts[i] = t
	}
	if err := writeJSONFile(m.templatesPath(), ts); err != nil {
		return nil, err
	}
	t.Vars = templateVars(&t)
	return &t, nil
}

func indexTemplate(ts []OTemplate, id string) int {
	for i := range ts {
		if ts[i].ID == id {
			return i
		}
	}
	return -1
}

func (m *OrchManager) DeleteTemplate(id string) error {
	if isBuiltinTemplate(id) {
		return &orchHTTPError{http.StatusForbidden, "built-in templates cannot be deleted"}
	}
	used, err := m.schedulesUsing(id)
	if err != nil {
		return err
	}
	if len(used) > 0 {
		return &orchHTTPError{http.StatusConflict, "template is used by schedule " + strings.Join(used, ", ")}
	}
	orchTemplateMu.Lock()
	defer orchTemplateMu.Unlock()
	ts, err := m.userTemplates()
	if err != nil {
		return err
	}
	i := indexTemplate(ts, id)
	if i < 0 {
		return &orchHTTPError{http.StatusNotFound, "template not found"}
	}
	return writeJSONFile(m.templatesPath(), append(ts[:i], ts[i+1:]...))
}

// orchMissingVars is a 400 that names the variables a run did not give.
type orchMissingVars struct{ names []string }

func (e *orchMissingVars) Error() string { return "missing variables: " + strings.Join(e.names, ", ") }

// renderTemplate fills {{vars}}; cwd, branch and date are automatic and the run's vars may override them.
func renderTemplate(s string, vars map[string]string, missing map[string]bool) string {
	return orchVarRe.ReplaceAllStringFunc(s, func(tok string) string {
		name := orchVarRe.FindStringSubmatch(tok)[1]
		v, ok := vars[name]
		if !ok {
			missing[name] = true
		}
		return v
	})
}

type orchTemplateRun struct {
	Cwd  string            `json:"cwd"`
	Vars map[string]string `json:"vars"`
}

// templateCreate turns a template and its run values into a create request.
func (m *OrchManager) templateCreate(t *OTemplate, run orchTemplateRun) (orchCreate, error) {
	cwd := run.Cwd
	if strings.HasPrefix(cwd, "~/") {
		cwd = filepath.Join(homeDir(), cwd[2:])
	}
	if !filepath.IsAbs(cwd) {
		return orchCreate{}, orchBadRequest("cwd must be an absolute path")
	}
	if st, err := os.Stat(cwd); err != nil || !st.IsDir() {
		return orchCreate{}, orchBadRequest("cwd does not exist")
	}
	_, branch := gitInfo(cwd)
	vars := map[string]string{"cwd": cwd, "branch": branch, "date": time.Now().Format("2006-01-02")}
	for k, v := range run.Vars {
		vars[k] = v
	}
	missing := map[string]bool{}
	in := orchCreate{Agent: t.Agent, Cwd: cwd, Prompt: renderTemplate(t.Prompt, vars, missing), Model: t.Model,
		Sandbox: t.Sandbox, PermissionMode: t.PermissionMode, Workspace: t.Workspace, TemplateID: t.ID, Where: t.Where}
	for _, c := range t.Children {
		c.Title = renderTemplate(c.Title, vars, missing)
		c.Prompt = renderTemplate(c.Prompt, vars, missing)
		in.Children = append(in.Children, c)
	}
	if len(missing) > 0 {
		names := make([]string, 0, len(missing))
		for k := range missing {
			names = append(names, k)
		}
		sort.Strings(names)
		return orchCreate{}, &orchMissingVars{names: names}
	}
	if t.Review {
		in.AutoReview = firstOf(t.ReviewAgent, "codex")
	}
	return in, nil
}

// RunTemplate starts a headless thread from a template, whatever its Where says.
func (m *OrchManager) RunTemplate(id string, run orchTemplateRun) (*OThread, error) {
	t, err := m.Template(id)
	if err != nil {
		return nil, err
	}
	in, err := m.templateCreate(t, run)
	if err != nil {
		return nil, err
	}
	return m.CreateThread(in)
}

// LaunchTemplate starts a template where it says: a headless thread or a terminal session.
func (m *OrchManager) LaunchTemplate(id string, run orchTemplateRun) (*orchLaunched, error) {
	t, err := m.Template(id)
	if err != nil {
		return nil, err
	}
	in, err := m.templateCreate(t, run)
	if err != nil {
		return nil, err
	}
	return m.launch(in)
}

func templateWriteErr(w http.ResponseWriter, err error) {
	var mv *orchMissingVars
	if errors.As(err, &mv) {
		writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": mv.Error(), "missing": mv.names})
		return
	}
	gitWriteErr(w, gitFailOr(err))
}

func (m *OrchManager) templateRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/orch/templates", func(w http.ResponseWriter, r *http.Request) {
		all, err := m.Templates()
		if err != nil {
			orchWriteErr(w, err)
			return
		}
		writeJSON(w, all)
	})
	mux.HandleFunc("GET /api/orch/templates/{id}", func(w http.ResponseWriter, r *http.Request) {
		t, err := m.Template(r.PathValue("id"))
		if err != nil {
			orchWriteErr(w, err)
			return
		}
		writeJSON(w, t)
	})
	mux.HandleFunc("POST /api/orch/templates", func(w http.ResponseWriter, r *http.Request) {
		var in OTemplate
		if !decodeOptional(w, r, &in) {
			return
		}
		t, err := m.SaveTemplate("", in)
		if err != nil {
			orchWriteErr(w, err)
			return
		}
		writeJSONStatus(w, http.StatusCreated, map[string]any{"template": t})
	})
	mux.HandleFunc("PUT /api/orch/templates/{id}", func(w http.ResponseWriter, r *http.Request) {
		var in OTemplate
		if !decodeOptional(w, r, &in) {
			return
		}
		t, err := m.SaveTemplate(r.PathValue("id"), in)
		if err != nil {
			orchWriteErr(w, err)
			return
		}
		writeJSON(w, map[string]any{"template": t})
	})
	mux.HandleFunc("DELETE /api/orch/templates/{id}", func(w http.ResponseWriter, r *http.Request) {
		if err := m.DeleteTemplate(r.PathValue("id")); err != nil {
			orchWriteErr(w, err)
			return
		}
		writeJSON(w, map[string]any{"ok": true})
	})
	mux.HandleFunc("POST /api/orch/templates/{id}/copy", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Name string `json:"name"`
		}
		if !decodeOptional(w, r, &in) {
			return
		}
		src, err := m.Template(r.PathValue("id"))
		if err != nil {
			orchWriteErr(w, err)
			return
		}
		cp := *src
		cp.Name = firstOf(strings.TrimSpace(in.Name), clipString(src.Name, 72)+" (copy)")
		cp.Children = append([]orchChildSpec(nil), src.Children...)
		t, err := m.SaveTemplate("", cp)
		if err != nil {
			orchWriteErr(w, err)
			return
		}
		writeJSONStatus(w, http.StatusCreated, map[string]any{"template": t})
	})
	mux.HandleFunc("POST /api/orch/templates/{id}/run", func(w http.ResponseWriter, r *http.Request) {
		var in orchTemplateRun
		if !decodeOptional(w, r, &in) {
			return
		}
		l, err := m.LaunchTemplate(r.PathValue("id"), in)
		if err != nil {
			templateWriteErr(w, err)
			return
		}
		if l.Terminal != nil {
			writeJSONStatus(w, http.StatusCreated, map[string]any{"terminal": l.Terminal})
			return
		}
		writeJSONStatus(w, http.StatusCreated, map[string]any{"thread": l.Thread})
	})
}
