package main

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strings"
	"testing"
)

func uploadTyped(t *testing.T, url, name, ctype string, data []byte) (int, map[string]any) {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	h := textproto.MIMEHeader{}
	h.Set("Content-Disposition", `form-data; name="file"; filename="`+name+`"`)
	h.Set("Content-Type", ctype)
	fw, _ := mw.CreatePart(h)
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

var (
	tinyPDF  = []byte("%PDF-1.4\n1 0 obj<<>>endobj\ntrailer<<>>\n%%EOF\n")
	tinyHEIC = append([]byte{0, 0, 0, 0x18}, []byte("ftypheic\x00\x00\x00\x00mif1heic")...)
)

func TestOrchUploadFileTypes(t *testing.T) {
	e := newOrchEnv(t)
	srv := e.server()
	for _, c := range []struct {
		name, ctype string
		data        []byte
		mime, ext   string
	}{
		{"doc.pdf", "application/pdf", tinyPDF, "application/pdf", ".pdf"},
		{"photo.heic", "image/heic", tinyHEIC, "image/heic", ".heic"},
		{"notes.txt", "text/plain", []byte("hello\n"), "text/plain", ".txt"},
		{"README.md", "application/octet-stream", []byte("# Title\n\ntext\n"), "text/markdown", ".md"},
		{"rows.csv", "text/csv", []byte("a,b\n1,2\n"), "text/csv", ".csv"},
		{"cfg.json", "application/json", []byte(`{"a":1}`), "application/json", ".json"},
		{"bad.json", "application/json", []byte(`{"a":`), "text/plain", ".txt"},
		{"shot.png", "image/png", onePixelPNG, "image/png", ".png"},
	} {
		code, out := uploadTyped(t, srv.URL, c.name, c.ctype, c.data)
		if code != 201 || out["mime"] != c.mime || out["name"] != c.name || int(out["size"].(float64)) != len(c.data) || !strings.HasPrefix(out["url"].(string), "/api/orch/uploads/up_") {
			t.Fatalf("%s: %d %v", c.name, code, out)
		}
		if p, _ := out["path"].(string); !strings.HasSuffix(p, c.ext) {
			t.Fatalf("%s stored as %s", c.name, p)
		}
		up, err := e.m.Upload(out["id"].(string))
		if err != nil || up.Mime != c.mime {
			t.Fatalf("%s read back: %v %+v", c.name, err, up)
		}
	}
	for _, c := range []struct {
		name, ctype string
		data        []byte
	}{
		{"x.svg", "image/svg+xml", []byte(`<svg xmlns="http://www.w3.org/2000/svg"></svg>`)},
		{"fake.pdf", "application/pdf", []byte("just text")},
		{"fake.png", "application/octet-stream", []byte("just text")},
		{"bin.txt", "text/plain", []byte{0x7f, 'E', 'L', 'F', 2, 1, 1, 0, 0, 0}},
		{"a.zip", "application/zip", []byte("PK\x03\x04rest")},
	} {
		if code, out := uploadTyped(t, srv.URL, c.name, c.ctype, c.data); code != 415 {
			t.Fatalf("%s accepted: %d %v", c.name, code, out)
		}
	}
	// HTML is kept as plain text: stored as .txt and served as text/plain, never as a page.
	if code, out := uploadTyped(t, srv.URL, "page.html", "text/html", []byte("<!doctype html><html><script>alert(1)</script></html>")); code != 201 || out["mime"] != "text/plain" || !strings.HasSuffix(out["path"].(string), ".txt") {
		t.Fatalf("page.html: %d %v", code, out)
	}
	big := append(append([]byte{}, tinyPDF...), make([]byte, 25<<20)...)
	if code, out := uploadTyped(t, srv.URL, "big.pdf", "application/pdf", big); code != 413 {
		t.Fatalf("over 25 MB: %d %v", code, out)
	}
	nearly := append(append([]byte{}, tinyPDF...), bytes.Repeat([]byte(" "), 11<<20)...)
	if code, out := uploadTyped(t, srv.URL, "eleven.pdf", "application/pdf", nearly); code != 201 {
		t.Fatalf("11 MB pdf: %d %v", code, out)
	}

	// Text files are served as plain text, never as markup.
	_, out := uploadTyped(t, srv.URL, "cfg.json", "application/json", []byte(`{"a":1}`))
	resp, err := http.Get(srv.URL + out["url"].(string))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.Header.Get("Content-Type") != "text/plain; charset=utf-8" || resp.Header.Get("Content-Security-Policy") == "" {
		t.Fatalf("json served as %v", resp.Header)
	}
}

func TestDriverInputsCarryFilePaths(t *testing.T) {
	e := newOrchEnv(t)
	pdf, err := e.m.SaveUploadTyped("new", "spec.pdf", "application/pdf", bytes.NewReader(tinyPDF))
	if err != nil {
		t.Fatal(err)
	}
	png, err := e.m.SaveUpload("new", "pixel.png", bytes.NewReader(onePixelPNG))
	if err != nil {
		t.Fatal(err)
	}
	ups := []orchImage{*pdf, *png}
	c, err := claudeUserContent("read this", ups)
	if err != nil {
		t.Fatal(err)
	}
	blocks := c.([]any)
	if len(blocks) != 2 || blocks[0].(map[string]any)["type"] != "image" || blocks[1].(map[string]any)["text"] != withFilePaths("read this", []orchImage{*pdf}) {
		t.Fatalf("claude content %v", blocks)
	}
	in := codexInput("read this", ups)
	if len(in) != 2 || in[0].(map[string]any)["text"] != withFilePaths("read this", []orchImage{*pdf}) || in[1].(map[string]any)["path"] != png.Path {
		t.Fatalf("codex input %v", in)
	}
	if got := codexInput("", []orchImage{*pdf}); len(got) != 1 || got[0].(map[string]any)["text"] != withFilePaths("", []orchImage{*pdf}) {
		t.Fatalf("file only %v", got)
	}
}

func TestCLISendAttachments(t *testing.T) {
	e := newOrchEnv(t)
	prev := orchMgr
	orchMgr = e.m
	t.Cleanup(func() { orchMgr = prev })
	pdf, err := e.m.SaveUploadTyped("new", "spec.pdf", "application/pdf", bytes.NewReader(tinyPDF))
	if err != nil {
		t.Fatal(err)
	}
	png, err := e.m.SaveUpload("new", "pixel.png", bytes.NewReader(onePixelPNG))
	if err != nil {
		t.Fatal(err)
	}
	id, f := setupCLITest(t, qop("enqueue", "one"))
	f.screens = []string{scrIdle}
	f.sess = &Session{State: StateAwaitingInput, RecentEvents: []RecentEvent{{Kind: "Stop"}}}
	body, _ := json.Marshal(map[string]any{"text": "look at these\n", "attachments": []string{pdf.ID, png.ID}})
	code, out := cliPost(id, "send", string(body))
	want := withFilePaths("look at these\n", []orchImage{*pdf, *png}) + "⏎"
	if code != 200 || len(f.typed) != 1 || f.typed[0] != want {
		t.Fatalf("claude attach: %d %v %q", code, out, f.typed)
	}
	f.screens, f.n = []string{scrIdle}, 0
	body, _ = json.Marshal(map[string]any{"attachments": []string{pdf.ID}})
	if code, _ := cliPost(id, "send", string(body)); code != 200 || f.typed[1] != withFilePaths("", []orchImage{*pdf})+"⏎" {
		t.Fatalf("attachment without text: %d %q", code, f.typed)
	}
	f.screens, f.n = []string{scrIdle}, 0
	if code, out := cliPost(id, "send", `{"text":"x","attachments":["up_0000000000000000"]}`); code != 400 || len(f.typed) != 2 {
		t.Fatalf("unknown id: %d %v", code, out)
	}
	if code, _ := cliPost(id, "send", `{"text":"x","attachments":["../../etc/passwd"]}`); code != 400 {
		t.Fatalf("bad id: %d", code)
	}

	// Codex gets the same plain note.
	f.screens, f.n = []string{scrIdle}, 0
	f.sess = &Session{Tool: ToolCodex, State: StateAwaitingInput, RecentEvents: []RecentEvent{{Kind: "Stop"}}}
	body, _ = json.Marshal(map[string]any{"text": "hi", "attachments": []string{pdf.ID}})
	if code, out := cliPost("codex:0199aaaa-bbbb-7ccc-8ddd-eeeeffff0000", "send", string(body)); code != 200 || f.typed[len(f.typed)-1] != withFilePaths("hi", []orchImage{*pdf})+"⏎" {
		t.Fatalf("codex attach: %d %v %q", code, out, f.typed)
	}
}

func TestTerminalStartPromptCarriesAttachments(t *testing.T) {
	e := newOrchEnv(t)
	prev := orchMgr
	orchMgr = e.m
	t.Cleanup(func() { orchMgr = prev })
	stubTerminalStart(t)
	typed := make(chan string, 2)
	typeWhenReadyFn = func(_ string, p string) error { typed <- p; return nil }
	pdf, err := e.m.SaveUploadTyped("new", "spec.pdf", "application/pdf", bytes.NewReader(tinyPDF))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.m.launch(orchCreate{Agent: "claude", Cwd: e.cwd, Prompt: "summarise", Where: "terminal", Attachments: []string{pdf.ID}}); err != nil {
		t.Fatal(err)
	}
	if p := <-typed; p != withFilePaths("summarise", []orchImage{*pdf}) {
		t.Fatalf("typed %q", p)
	}
	if _, err := e.m.launch(orchCreate{Agent: "codex", Cwd: e.cwd, Prompt: "x", Where: "terminal", Attachments: []string{"up_0000000000000000"}}); err == nil {
		t.Fatal("unknown attachment accepted")
	}
}

func TestOrchTerminalRouteAttachments(t *testing.T) {
	e := newOrchEnv(t)
	prev := orchMgr
	orchMgr = e.m
	t.Cleanup(func() { orchMgr = prev })
	srv := withHostDaemon(t)
	stubTerminalStart(t)
	typed := make(chan string, 4)
	typeWhenReadyFn = func(_ string, p string) error { typed <- p; return nil }
	pdf, err := e.m.SaveUploadTyped("new", "spec.pdf", "application/pdf", bytes.NewReader(tinyPDF))
	if err != nil {
		t.Fatal(err)
	}
	cwd := t.TempDir()
	body := func(agent, id string) string {
		return `{"agent":"` + agent + `","cwd":"` + cwd + `","prompt":"read it","attachments":["` + id + `"]}`
	}
	if code, out := postJSON(t, srv.URL+"/api/orch/terminal", body("claude", pdf.ID)); code != 200 {
		t.Fatalf("claude: %d %v", code, out)
	}
	if p := <-typed; p != withFilePaths("read it", []orchImage{*pdf}) {
		t.Fatalf("claude prompt %q", p)
	}
	if code, out := postJSON(t, srv.URL+"/api/orch/terminal", body("codex", pdf.ID)); code != 200 {
		t.Fatalf("codex: %d %v", code, out)
	}
	if p := <-typed; p != withFilePaths("read it", []orchImage{*pdf}) {
		t.Fatalf("codex prompt %q", p)
	}
	if code, _ := postJSON(t, srv.URL+"/api/orch/terminal", body("claude", "up_0000000000000000")); code != 400 {
		t.Fatalf("unknown id: %d", code)
	}
}

func TestFileNoteFormat(t *testing.T) {
	got := withFilePaths("look", []orchImage{{OAttachment: OAttachment{Name: "shot (1).png", Mime: "image/png", Size: 294037}, Path: "/h/uploads/new/up_359be6f26d9aeef3.png"}})
	want := "look\n\n" + fileNoteHead + "\n- /h/uploads/new/up_359be6f26d9aeef3.png (shot 1.png, image/png, 287 KB)"
	if got != want || strings.Contains(got, "@") {
		t.Fatalf("note:\n%s", got)
	}
}
