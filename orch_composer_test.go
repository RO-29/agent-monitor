package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// onePixelPNG is a valid 1x1 red PNG.
var onePixelPNG, _ = base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8DwHwAFBQIAX8jx0gAAAABJRU5ErkJggg==")

func uploadFile(t *testing.T, url, name string, data []byte, threadID string) (int, map[string]any) {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	if threadID != "" {
		_ = mw.WriteField("threadId", threadID)
	}
	fw, _ := mw.CreateFormFile("file", name)
	fw.Write(data)
	mw.Close()
	resp, err := http.Post(url+"/api/orch/uploads", mw.FormDataContentType(), &body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func getJSONURL(t *testing.T, url string, v any) int {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	_ = json.NewDecoder(resp.Body).Decode(v)
	return resp.StatusCode
}

func TestOrchUploads(t *testing.T) {
	e := newOrchEnv(t)
	srv := e.server()
	code, out := uploadFile(t, srv.URL, "notes.txt", onePixelPNG, "")
	if code != 201 || out["mime"] != "image/png" || !strings.HasPrefix(out["id"].(string), "up_") {
		t.Fatalf("png named .txt: %d %v", code, out)
	}
	id := out["id"].(string)
	if p := out["path"].(string); !strings.HasPrefix(p, filepath.Join(e.dir, "uploads", "new")+"/") || !strings.HasSuffix(p, ".png") {
		t.Fatalf("path %s", p)
	}
	if code, out := uploadFile(t, srv.URL, "fake.png", []byte("hello, not an image"), ""); code != 415 {
		t.Fatalf("text named .png: %d %v", code, out)
	}
	if code, _ := uploadFile(t, srv.URL, "svg.png", []byte(`<svg xmlns="http://www.w3.org/2000/svg"></svg>`), ""); code != 415 {
		t.Fatalf("svg accepted: %d", code)
	}
	big := append(append([]byte{}, onePixelPNG...), make([]byte, orchUploadMax)...)
	if code, out := uploadFile(t, srv.URL, "big.png", big, ""); code != 413 {
		t.Fatalf("big: %d %v", code, out)
	}
	if code, _ := uploadFile(t, srv.URL, "a.png", onePixelPNG, "ot_0000000000000000"); code != 404 {
		t.Fatalf("unknown thread scope: %d", code)
	}
	if code, _ := uploadFile(t, srv.URL, "a.png", onePixelPNG, "../x"); code != 400 {
		t.Fatalf("bad scope: %d", code)
	}
	resp, err := http.Get(srv.URL + "/api/orch/uploads/" + id)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "image/png" || !bytes.Equal(b, onePixelPNG) || resp.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("get upload %d %v", resp.StatusCode, resp.Header)
	}
	for _, bad := range []string{"up_0000000000000000", "..%2F..%2Fetc", "up_*"} {
		if r, _ := http.Get(srv.URL + "/api/orch/uploads/" + bad); r.StatusCode != 404 {
			t.Fatalf("%s: %d", bad, r.StatusCode)
		}
	}
	if _, err := e.m.SendWith("ot_x", "hi", []string{"up_0000000000000000"}); err == nil {
		t.Fatal("unknown attachment accepted")
	}
}

func TestOrchClaudeImageAttachment(t *testing.T) {
	e := newOrchEnv(t)
	img, err := e.m.SaveUpload("new", "pixel.png", bytes.NewReader(onePixelPNG))
	if err != nil {
		t.Fatal(err)
	}
	th, err := e.m.CreateThread(orchCreate{Agent: "claude", Cwd: e.cwd, Prompt: "what colour", Attachments: []string{img.ID}})
	if err != nil {
		t.Fatal(err)
	}
	e.waitRunEnd()
	d := e.detail(th.ID)
	u := itemOf(d, "user")
	if u == nil || len(u.Attachments) != 1 || u.Attachments[0].ID != img.ID || u.Attachments[0].Mime != "image/png" || u.Attachments[0].Name != "pixel.png" {
		t.Fatalf("user item %+v", u)
	}
	var sent map[string]any
	for _, l := range strings.Split(e.fakeLog(), "\n") {
		if strings.HasPrefix(l, `in {"message"`) || (strings.HasPrefix(l, "in ") && strings.Contains(l, `"type":"user"`)) {
			_ = json.Unmarshal([]byte(strings.TrimPrefix(l, "in ")), &sent)
		}
	}
	msg, _ := sent["message"].(map[string]any)
	blocks, _ := msg["content"].([]any)
	if len(blocks) != 2 {
		t.Fatalf("content %v", msg)
	}
	b0 := blocks[0].(map[string]any)
	src := b0["source"].(map[string]any)
	if b0["type"] != "image" || src["type"] != "base64" || src["media_type"] != "image/png" || src["data"] != base64.StdEncoding.EncodeToString(onePixelPNG) {
		t.Fatalf("image block %v", b0)
	}
	if b1 := blocks[1].(map[string]any); b1["type"] != "text" || b1["text"] != "what colour" {
		t.Fatalf("text block %v", b1)
	}
}

func TestOrchCodexImageAttachment(t *testing.T) {
	e := newOrchEnv(t)
	th := e.create("hello")
	e.waitRunEnd()
	e.idle(th.ID)
	img, err := e.m.SaveUpload(th.ID, "p.png", bytes.NewReader(onePixelPNG))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(img.Path, filepath.Join(e.dir, "uploads", th.ID)+"/") {
		t.Fatalf("path %s", img.Path)
	}
	if _, err := e.m.SendWith(th.ID, "look", []string{img.ID}); err != nil {
		t.Fatal(err)
	}
	e.waitRunEnd()
	var input []any
	for _, l := range strings.Split(e.fakeLog(), "\n") {
		if rest, ok := strings.CutPrefix(l, "turn/start "); ok {
			_, js, _ := strings.Cut(rest, " ")
			var p map[string]any
			_ = json.Unmarshal([]byte(js), &p)
			input, _ = p["input"].([]any)
		}
	}
	if len(input) != 2 || input[0].(map[string]any)["text"] != "look" ||
		input[1].(map[string]any)["type"] != "localImage" || input[1].(map[string]any)["path"] != img.Path {
		t.Fatalf("turn input %v", input)
	}
}

func TestOrchACPImageAttachment(t *testing.T) {
	e := newOrchEnv(t)
	e.acpSetup(e.m)
	th := e.createACP("first", "")
	e.waitRunEnd()
	e.idle(th.ID)
	img, _ := e.m.SaveUpload("new", "p.png", bytes.NewReader(onePixelPNG))
	_, err := e.m.SendWith(th.ID, "see", []string{img.ID})
	var he *orchHTTPError
	if err == nil || !asHTTP(err, &he) || he.code != 400 {
		t.Fatalf("no image support: %v", err)
	}
	if st := e.thread(th.ID).Status; st != "idle" {
		t.Fatalf("status after refusal %s", st)
	}

	e2 := newOrchEnv(t)
	e2.acpSetup(e2.m, "ORCH_FAKE_ACP_IMAGE=1")
	th2, err := e2.m.CreateThread(orchCreate{Agent: "fake", Cwd: e2.cwd, Prompt: "see", Attachments: []string{mustUpload(t, e2.m).ID}})
	if err != nil {
		t.Fatal(err)
	}
	e2.waitRunEnd()
	if got := agentText(e2.detail(th2.ID)); got != "echo see" {
		t.Fatalf("agent %q", got)
	}
	if !strings.Contains(e2.fakeLog(), `{"data":"`+base64.StdEncoding.EncodeToString(onePixelPNG)+`","mimeType":"image/png","type":"image"}`) {
		t.Fatalf("no image block in %s", e2.fakeLog())
	}
}

func mustUpload(t *testing.T, m *OrchManager) *orchImage {
	t.Helper()
	img, err := m.SaveUpload("new", "p.png", bytes.NewReader(onePixelPNG))
	if err != nil {
		t.Fatal(err)
	}
	return img
}

func asHTTP(err error, he **orchHTTPError) bool {
	e, ok := err.(*orchHTTPError)
	*he = e
	return ok
}

func TestOrchFilesEndpoint(t *testing.T) {
	e := newOrchEnv(t)
	srv := e.server()
	repo := gitRepo(t)
	for p, body := range map[string]string{"src/main.go": "x", "src/util/strings.go": "x", "README.md": "x", "build/out.bin": "x", "notes.txt": "x"} {
		os.MkdirAll(filepath.Join(repo, filepath.Dir(p)), 0o755)
		os.WriteFile(filepath.Join(repo, p), []byte(body), 0o644)
	}
	os.WriteFile(filepath.Join(repo, ".gitignore"), []byte("build/\n"), 0o644)
	cmd := exec.Command("git", "-C", repo, "add", "src", "README.md", ".gitignore")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatal(string(out))
	}
	allowCwds(t, repo)
	var hits []orchFileHit
	if code := getJSONURL(t, srv.URL+"/api/orch/files?cwd="+repo+"&q=strgo", &hits); code != 200 || len(hits) == 0 || hits[0].Path != "src/util/strings.go" {
		t.Fatalf("q=strgo: %d %v", code, hits)
	}
	getJSONURL(t, srv.URL+"/api/orch/files?cwd="+repo+"&q=", &hits)
	got := map[string]string{}
	for _, h := range hits {
		got[h.Path] = h.Kind
	}
	if got["notes.txt"] != "file" || got["src"] != "dir" || got["src/util"] != "dir" || got["src/main.go"] != "file" {
		t.Fatalf("listing %v", got)
	}
	if _, ok := got["build/out.bin"]; ok {
		t.Fatalf("ignored file listed: %v", got)
	}
	getJSONURL(t, srv.URL+"/api/orch/files?cwd="+repo+"&q=util", &hits)
	if len(hits) == 0 || hits[0].Path != "src/util" || hits[0].Kind != "dir" {
		t.Fatalf("q=util %v", hits)
	}
	var errOut map[string]any
	if code := getJSONURL(t, srv.URL+"/api/orch/files?cwd=relative", &errOut); code != 400 {
		t.Fatalf("relative cwd %d", code)
	}
	plain := t.TempDir()
	os.MkdirAll(filepath.Join(plain, "node_modules", "x"), 0o755)
	os.WriteFile(filepath.Join(plain, "node_modules", "x", "i.js"), nil, 0o644)
	os.WriteFile(filepath.Join(plain, "app.js"), nil, 0o644)
	allowCwds(t, plain)
	getJSONURL(t, srv.URL+"/api/orch/files?cwd="+plain, &hits)
	if len(hits) != 1 || hits[0].Path != "app.js" {
		t.Fatalf("plain folder %v", hits)
	}
}

func TestOrchSkillsEndpoint(t *testing.T) {
	e := newOrchEnv(t)
	srv := e.server()
	cfg, codex, proj := t.TempDir(), t.TempDir(), t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	t.Setenv("CODEX_HOME", codex)
	write := func(p, s string) {
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(s), 0o644)
	}
	write(filepath.Join(cfg, "skills", "deploy", "SKILL.md"), "---\nname: deploy\ndescription: \"Ship it: now\"\n---\nbody")
	write(filepath.Join(cfg, "skills", "folded", "SKILL.md"), "---\ndescription: >\n  first line\n  second line\nother: x\n---\n")
	write(filepath.Join(proj, ".claude", "skills", "local", "SKILL.md"), "---\nname: local\ndescription: project skill\n---\n")
	plug := filepath.Join(cfg, "plugins", "cache", "p1")
	off := filepath.Join(cfg, "plugins", "cache", "p2")
	write(filepath.Join(plug, "skills", "tdd", "SKILL.md"), "---\nname: tdd\ndescription: test first\n---\n")
	write(filepath.Join(off, "skills", "hidden", "SKILL.md"), "---\nname: hidden\n---\n")
	b, _ := json.Marshal(map[string]any{"version": 2, "plugins": map[string]any{
		"mp-skills@mp": []any{map[string]any{"installPath": plug}}, "off@mp": []any{map[string]any{"installPath": off}}}})
	write(filepath.Join(cfg, "plugins", "installed_plugins.json"), string(b))
	write(filepath.Join(cfg, "settings.json"), `{"enabledPlugins":{"off@mp":false}}`)
	write(filepath.Join(codex, "prompts", "plan.md"), "---\ndescription: make a plan\n---\nUse it")

	allowCwds(t, proj)
	var skills []orchSkill
	getJSONURL(t, srv.URL+"/api/orch/skills?agent=claude&cwd="+proj, &skills)
	got := map[string]orchSkill{}
	for _, s := range skills {
		got[s.Name] = s
	}
	if s := got["deploy"]; s.Description != "Ship it: now" || s.Source != "user" || s.Insert != "/deploy" {
		t.Fatalf("deploy %+v", s)
	}
	if s := got["folded"]; s.Description != "first line second line" {
		t.Fatalf("folded %+v", s)
	}
	if s := got["local"]; s.Source != "project" || s.Insert != "/local" {
		t.Fatalf("local %+v", s)
	}
	if s := got["tdd"]; s.Source != "plugin" || s.Plugin != "mp-skills" || s.Insert != "/mp-skills:tdd" {
		t.Fatalf("tdd %+v", s)
	}
	if _, ok := got["hidden"]; ok || len(skills) != 4 {
		t.Fatalf("skills %+v", skills)
	}
	getJSONURL(t, srv.URL+"/api/orch/skills?agent=codex", &skills)
	if len(skills) != 1 || skills[0].Insert != "/prompts:plan" || skills[0].Description != "make a plan" || skills[0].Source != "codex-prompt" {
		t.Fatalf("codex %+v", skills)
	}
	getJSONURL(t, srv.URL+"/api/orch/skills?agent=opencode", &skills)
	if len(skills) != 0 {
		t.Fatalf("acp %+v", skills)
	}
}
