package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	orchUploadMax      = 25 << 20
	orchAttachmentsMax = 10
	orchUploadMaxAge   = 30 * 24 * time.Hour
	// orchModelImageMax is the largest image sent inline as an image block; larger ones go as a path.
	orchModelImageMax = 10 << 20
	orchUploadTmp     = ".upload-"
)

// orchUploadQuota caps the bytes kept under uploads/ (a var for tests).
var (
	orchUploadQuota int64 = 500 << 20
	orchUploadMu    sync.Mutex
	// orchUploadSlots caps uploads in flight; a caller waits up to orchUploadWait for one, then gets 429.
	orchUploadSlots = make(chan struct{}, 3)
	orchUploadWait  = 60 * time.Second
)

// uploadMaxAge is the age past which an upload is deleted; 0 means keep until the quota.
func uploadMaxAge() time.Duration {
	switch h := getSettings().UploadMaxAgeHours; {
	case h < 0:
		return 0
	case h == 0:
		return orchUploadMaxAge
	default:
		return time.Duration(h) * time.Hour
	}
}

// UploadStats is what the uploads folder holds after a sweep.
type UploadStats struct {
	Files       int   `json:"files"`
	Bytes       int64 `json:"bytes"`
	Deleted     int   `json:"deleted"`
	Oldest      int64 `json:"oldest,omitempty"`
	MaxAgeHours int   `json:"maxAgeHours"`
	QuotaBytes  int64 `json:"quotaBytes"`
}

// pruneUploads deletes uploads older than the set age and returns the bytes left.
func (m *OrchManager) pruneUploads() int64 { return m.sweepUploads(uploadMaxAge()).Bytes }

// sweepUploads deletes uploads older than maxAge (0 keeps them) and stray temp files, and counts the rest.
func (m *OrchManager) sweepUploads(maxAge time.Duration) UploadStats {
	st := UploadStats{MaxAgeHours: getSettings().UploadMaxAgeHours, QuotaBytes: orchUploadQuota}
	now := time.Now()
	var names []string
	kept := map[string]bool{}
	_ = filepath.WalkDir(m.uploadsDir(), func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		if strings.HasPrefix(d.Name(), orchUploadTmp) {
			if info.ModTime().Before(now.Add(-time.Hour)) {
				_ = os.Remove(p) // left by a crash mid-upload
			}
			return nil
		}
		if strings.HasSuffix(p, ".name") {
			names = append(names, p) // removed with its file, or below when that file is gone
			return nil
		}
		if maxAge > 0 && info.ModTime().Before(now.Add(-maxAge)) {
			if os.Remove(p) == nil {
				st.Deleted++
			}
			return nil
		}
		kept[strings.TrimSuffix(p, filepath.Ext(p))] = true
		st.Files++
		st.Bytes += info.Size()
		if t := info.ModTime().UnixMilli(); st.Oldest == 0 || t < st.Oldest {
			st.Oldest = t
		}
		return nil
	})
	// A .name file whose upload is gone (deleted above, a crash, a manual delete) goes too.
	for _, n := range names {
		if !kept[strings.TrimSuffix(n, ".name")] {
			_ = os.Remove(n)
		}
	}
	return st
}

var (
	orchUploadIDRe = regexp.MustCompile(`^up_[0-9a-f]{16}$`)
	orchThreadIDRe = regexp.MustCompile(`^ot_[0-9a-f]{16}$`)
	orchUploadExt  = map[string]string{"image/png": ".png", "image/jpeg": ".jpg", "image/webp": ".webp", "image/gif": ".gif", "image/heic": ".heic",
		"application/pdf": ".pdf", "text/plain": ".txt", "text/markdown": ".md", "text/csv": ".csv", "application/json": ".json"}
	orchExtMime = map[string]string{}
	// orchModelImage are the image types every driver can hand to its model as an image.
	orchModelImage = map[string]bool{"image/png": true, "image/jpeg": true, "image/webp": true, "image/gif": true}
)

const orchUploadTypes = "png, jpeg, webp, gif or heic images, pdf, and text files: txt, md, csv, json, html, xml, yaml, logs and source code"

// orchTextExt are text files kept as text/plain: markup and source code, never served as a page or run.
var orchTextExt = map[string]bool{}

func init() {
	for _, e := range strings.Fields(`.html .htm .xml .yaml .yml .toml .ini .cfg .conf .log .diff .patch .sql .graphql .proto
		.go .py .rb .js .mjs .cjs .ts .tsx .jsx .vue .svelte .css .scss .java .kt .kts .scala .swift .m .mm .c .h .cc .cpp .hpp .cs .rs .php .pl .lua .r
		.sh .bash .zsh .fish .ps1 .tf .hcl .gradle .groovy .dart .ex .exs .erl .hs .clj .zig .nim .jl .ipynb`) {
		orchTextExt[e] = true
	}
}

func init() {
	for m, e := range orchUploadExt {
		orchExtMime[e] = m
	}
}

// OAttachment is an uploaded image as a user item shows it.
type OAttachment struct {
	ID   string `json:"id"`
	Mime string `json:"mime"`
	Name string `json:"name,omitempty"`
	Size int64  `json:"size"`
	URL  string `json:"url"`
}

// orchImage is a resolved upload a driver sends to its agent.
type orchImage struct {
	OAttachment
	Path string
}

func (m *OrchManager) uploadsDir() string { return filepath.Join(m.stateDir, "uploads") }

// sniffUpload returns the accepted mime type of b by its content. hints (declared types, file names) only
// pick which text type a text file is; a text body that claims to be an image or a pdf is refused.
func sniffUpload(b []byte, hints ...string) string {
	return sniffUploadText(b, bytes.IndexByte(b, 0) < 0 && utf8.Valid(trimPartialRune(b)), hints...)
}

// sniffUploadText is sniffUpload for a head of the file; text says the whole file is NUL-free UTF-8.
func sniffUploadText(head []byte, text bool, hints ...string) string {
	if isHEIC(head) {
		return "image/heic"
	}
	out, textHint, refuse := "text/plain", false, false
	for _, h := range hints {
		h = strings.ToLower(strings.TrimSpace(h))
		h, _, _ = strings.Cut(h, ";")
		if orchTextExt[filepath.Ext(h)] {
			h = "text/plain"
		} else if m, ok := orchExtMime[filepath.Ext(h)]; ok {
			h = m
		} else if filepath.Ext(h) == ".markdown" || h == "text/x-markdown" {
			h = "text/markdown"
		}
		switch {
		case strings.HasPrefix(h, "image/") || h == "application/pdf":
			refuse = true
		case h == "text/plain":
			textHint = true
		case h == "text/markdown" || h == "text/csv" || h == "application/json":
			out, textHint = h, true
		}
	}
	mt, _, _ := strings.Cut(http.DetectContentType(head), ";")
	switch mt {
	case "image/png", "image/jpeg", "image/webp", "image/gif", "application/pdf":
		return mt
	case "text/plain":
	case "text/html", "text/xml":
		if !textHint { // markup is kept only as a declared text file, and it is served as plain text
			return ""
		}
	default:
		return ""
	}
	if !text || refuse {
		return ""
	}
	return out
}

// trimPartialRune drops a rune cut at the end of a head read.
func trimPartialRune(b []byte) []byte {
	for i := len(b) - 1; i >= 0 && i >= len(b)-3; i-- {
		if utf8.RuneStart(b[i]) {
			if !utf8.FullRune(b[i:]) {
				return b[:i]
			}
			break
		}
	}
	return b
}

// uploadScan sees an upload stream: its head for sniffing, its size, and whether it is NUL-free UTF-8.
type uploadScan struct {
	head  []byte
	size  int64
	bad   bool
	cbuf  [4]byte
	carry []byte // a rune cut at the end of the last write
}

func (s *uploadScan) Write(p []byte) (int, error) {
	n := len(p)
	if len(s.head) < 512 {
		s.head = append(s.head, p[:min(len(p), 512-len(s.head))]...)
	}
	s.size += int64(n)
	if s.bad {
		return n, nil
	}
	if bytes.IndexByte(p, 0) >= 0 {
		s.bad = true
		return n, nil
	}
	if k := len(s.carry); k > 0 {
		r := append(s.cbuf[:k:4], p[:min(len(p), 4-k)]...)
		if !utf8.FullRune(r) {
			s.carry = r
			return n, nil
		}
		c, size := utf8.DecodeRune(r)
		if c == utf8.RuneError && size == 1 {
			s.bad = true
			return n, nil
		}
		p = p[size-k:]
		s.carry = nil
	}
	body := trimPartialRune(p)
	if !utf8.Valid(body) {
		s.bad = true
		return n, nil
	}
	if rest := p[len(body):]; len(rest) > 0 {
		s.carry = append(s.cbuf[:0], rest...)
	}
	return n, nil
}

// jsonFileValid is json.Valid over a file, without loading it whole.
func jsonFileValid(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	dec := json.NewDecoder(bufio.NewReader(f))
	dec.UseNumber()
	for depth := 0; ; {
		tok, err := dec.Token()
		if err != nil {
			return false
		}
		if d, ok := tok.(json.Delim); ok {
			if d == '{' || d == '[' {
				depth++
			} else {
				depth--
			}
		}
		if depth == 0 {
			break
		}
	}
	_, err = dec.Token()
	return err == io.EOF
}

// isHEIC reads the ISO BMFF ftyp box of a HEIC or HEIF still image.
func isHEIC(b []byte) bool {
	if len(b) < 12 || string(b[4:8]) != "ftyp" {
		return false
	}
	switch string(b[8:12]) {
	case "heic", "heix", "heim", "heis", "hevc", "hevx", "mif1", "msf1":
		return true
	}
	return false
}

// sniffImage is sniffUpload for callers that only take images the model can read.
func sniffImage(b []byte) string {
	if mt := sniffUpload(b, ""); orchModelImage[mt] {
		return mt
	}
	return ""
}

// SaveUpload stores one image for scope (a thread id or "new").
func (m *OrchManager) SaveUpload(scope, name string, r io.Reader) (*orchImage, error) {
	return m.SaveUploadTyped(scope, name, "", r)
}

// SaveUploadTyped is SaveUpload with the client's declared type, a hint for text files only.
func (m *OrchManager) SaveUploadTyped(scope, name, declared string, r io.Reader) (*orchImage, error) {
	return m.SaveUploadCtx(context.Background(), scope, name, declared, r)
}

// SaveUploadCtx is SaveUploadTyped that waits for a free slot while ctx lives.
func (m *OrchManager) SaveUploadCtx(ctx context.Context, scope, name, declared string, r io.Reader) (*orchImage, error) {
	if scope == "" {
		scope = "new"
	}
	if scope != "new" {
		if !orchThreadIDRe.MatchString(scope) {
			return nil, orchBadRequest("bad threadId")
		}
		if _, err := m.store.Thread(scope); err != nil {
			return nil, err
		}
	}
	wait := time.NewTimer(orchUploadWait)
	select {
	case orchUploadSlots <- struct{}{}:
		wait.Stop()
		defer func() { <-orchUploadSlots }()
	case <-wait.C:
		return nil, &orchHTTPError{http.StatusTooManyRequests, "too many uploads at once; try again"}
	case <-ctx.Done():
		wait.Stop()
		return nil, &orchHTTPError{http.StatusTooManyRequests, "too many uploads at once; try again"}
	}
	if err := os.MkdirAll(m.uploadsDir(), 0o700); err != nil {
		return nil, err
	}
	tmp, err := os.CreateTemp(m.uploadsDir(), orchUploadTmp+"*")
	if err != nil {
		return nil, err
	}
	tmpPath := tmp.Name()
	defer func() {
		if tmpPath != "" {
			_ = os.Remove(tmpPath)
		}
	}()
	scan := &uploadScan{}
	_, err = io.Copy(io.MultiWriter(tmp, scan), io.LimitReader(r, orchUploadMax+1))
	if cerr := tmp.Close(); err == nil && cerr != nil {
		return nil, cerr
	}
	if err != nil {
		var mbe *http.MaxBytesError
		var pe *fs.PathError
		if errors.As(err, &mbe) || errors.As(err, &pe) {
			return nil, err
		}
		return nil, orchBadRequest("upload read failed")
	}
	size := scan.size
	if size > orchUploadMax {
		return nil, &orchHTTPError{http.StatusRequestEntityTooLarge, "file is larger than 25 MB"}
	}
	if size == 0 {
		return nil, orchBadRequest("empty file")
	}
	mime := sniffUploadText(scan.head, !scan.bad, declared, name)
	if mime == "application/json" && !jsonFileValid(tmpPath) {
		mime = "text/plain"
	}
	if mime == "" {
		return nil, &orchHTTPError{http.StatusUnsupportedMediaType, "only " + orchUploadTypes + " files are accepted"}
	}
	orchUploadMu.Lock()
	defer orchUploadMu.Unlock()
	if m.pruneUploads()+size > orchUploadQuota {
		return nil, &orchHTTPError{http.StatusInsufficientStorage, fmt.Sprintf("upload storage is full (%d MB)", orchUploadQuota>>20)}
	}
	dir := filepath.Join(m.uploadsDir(), scope)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	id := orchID("up_")
	path := filepath.Join(dir, id+orchUploadExt[mime])
	if err := os.Rename(tmpPath, path); err != nil {
		return nil, err
	}
	tmpPath = ""
	name = filepath.Base(strings.TrimSpace(name))
	if name == "." || name == "/" {
		name = ""
	}
	_ = os.WriteFile(filepath.Join(dir, id+".name"), []byte(clipString(name, 200)), 0o600)
	return &orchImage{OAttachment: OAttachment{ID: id, Mime: mime, Name: clipString(name, 200), Size: size, URL: "/api/orch/uploads/" + id}, Path: path}, nil
}

// Upload finds an upload by id in any scope.
func (m *OrchManager) Upload(id string) (*orchImage, error) {
	if !orchUploadIDRe.MatchString(id) {
		return nil, &orchHTTPError{http.StatusNotFound, "unknown upload " + id}
	}
	matches, _ := filepath.Glob(filepath.Join(m.uploadsDir(), "*", id+".*"))
	for _, p := range matches {
		if strings.HasSuffix(p, ".name") {
			continue
		}
		f, err := os.Open(p)
		if err != nil {
			continue
		}
		head := make([]byte, 512)
		n, _ := io.ReadFull(f, head)
		st, _ := f.Stat()
		f.Close()
		mime := sniffUpload(head[:n], filepath.Ext(p))
		if mime == "" || st == nil || orchUploadExt[mime] != filepath.Ext(p) {
			continue
		}
		name, _ := os.ReadFile(strings.TrimSuffix(p, filepath.Ext(p)) + ".name")
		return &orchImage{OAttachment: OAttachment{ID: id, Mime: mime, Name: string(name), Size: st.Size(), URL: "/api/orch/uploads/" + id}, Path: p}, nil
	}
	return nil, &orchHTTPError{http.StatusNotFound, "unknown upload " + id}
}

// resolveUploads maps attachment ids to images; unknown ids answer 400.
func (m *OrchManager) resolveUploads(ids []string) ([]orchImage, error) {
	if len(ids) > orchAttachmentsMax {
		return nil, orchBadRequest(fmt.Sprintf("at most %d attachments", orchAttachmentsMax))
	}
	out := make([]orchImage, 0, len(ids))
	for _, id := range ids {
		img, err := m.Upload(id)
		if err != nil {
			return nil, orchBadRequest("unknown attachment " + id)
		}
		// The age setting counts from the last use, so a queued or re-sent file stays.
		now := time.Now()
		_ = os.Chtimes(img.Path, now, now)
		out = append(out, *img)
	}
	return out, nil
}

func imageAttachments(imgs []orchImage) []OAttachment {
	if len(imgs) == 0 {
		return nil
	}
	out := make([]OAttachment, len(imgs))
	for i, im := range imgs {
		out[i] = im.OAttachment
	}
	return out
}

// imageBase64 encodes a file straight from disk, so only the encoded copy is held.
func imageBase64(im orchImage) (string, error) {
	f, err := os.Open(im.Path)
	if err != nil {
		return "", fmt.Errorf("attachment %s: %w", im.ID, err)
	}
	defer f.Close()
	var sb strings.Builder
	if st, err := f.Stat(); err == nil {
		sb.Grow(base64.StdEncoding.EncodedLen(int(st.Size())))
	}
	enc := base64.NewEncoder(base64.StdEncoding, &sb)
	if _, err := io.Copy(enc, f); err != nil {
		return "", fmt.Errorf("attachment %s: %w", im.ID, err)
	}
	_ = enc.Close()
	return sb.String(), nil
}

// splitUploads separates images the model reads from files it opens by path (pdf, text, heic, big images).
func splitUploads(ups []orchImage) (pics, files []orchImage) {
	for _, u := range ups {
		if orchModelImage[u.Mime] && u.Size <= orchModelImageMax {
			pics = append(pics, u)
		} else {
			files = append(files, u)
		}
	}
	return pics, files
}

// withFilePaths appends one absolute path per file on new lines after the text; at prefixes "@" (Claude Code input).
// fileNoteHead starts the note that lists attached files; the web and iPhone chat show it as attachment chips.
const fileNoteHead = "Attached files (open them with your file tools):"

// withFilePaths appends a plain note with each file's absolute path; Claude Code and Codex open them with their file tools.
func withFilePaths(text string, files []orchImage) string {
	if len(files) == 0 {
		return text
	}
	lines := []string{fileNoteHead}
	for _, f := range files {
		lines = append(lines, fmt.Sprintf("- %s (%s, %s, %s)", f.Path, strings.NewReplacer("(", "", ")", "", "\n", " ").Replace(f.Name), f.Mime, fmtFileSize(f.Size)))
	}
	if strings.TrimSpace(text) == "" {
		return strings.Join(lines, "\n")
	}
	return strings.TrimRight(text, " \t\r\n") + "\n\n" + strings.Join(lines, "\n")
}

func fmtFileSize(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%d KB", n>>10)
	}
	return fmt.Sprintf("%d B", n)
}

// claudeContent builds the stream-json user content: plain text, or image blocks then the text; other files go as paths.
func claudeUserContent(text string, ups []orchImage) (any, error) {
	imgs, files := splitUploads(ups)
	text = withFilePaths(text, files)
	if len(imgs) == 0 {
		return text, nil
	}
	blocks := make([]any, 0, len(imgs)+1)
	for _, im := range imgs {
		data, err := imageBase64(im)
		if err != nil {
			return nil, err
		}
		blocks = append(blocks, map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": im.Mime, "data": data}})
	}
	if text != "" {
		blocks = append(blocks, map[string]any{"type": "text", "text": text})
	}
	return blocks, nil
}

// codexInput builds turn/start and turn/steer input: the text item, then one localImage per upload.
func codexInput(text string, ups []orchImage) []any {
	imgs, files := splitUploads(ups)
	text = withFilePaths(text, files)
	in := []any{}
	if text != "" || len(imgs) == 0 {
		in = append(in, map[string]any{"type": "text", "text": text, "text_elements": []any{}})
	}
	for _, im := range imgs {
		in = append(in, map[string]any{"type": "localImage", "path": im.Path})
	}
	return in
}

// acpPrompt builds session/prompt content blocks: images first, then the text.
func acpPrompt(text string, ups []orchImage) ([]any, error) {
	imgs, files := splitUploads(ups)
	text = withFilePaths(text, files)
	out := []any{}
	for _, im := range imgs {
		data, err := imageBase64(im)
		if err != nil {
			return nil, err
		}
		out = append(out, map[string]any{"type": "image", "mimeType": im.Mime, "data": data})
	}
	if text != "" || len(imgs) == 0 {
		out = append(out, map[string]any{"type": "text", "text": text})
	}
	return out, nil
}

func (m *OrchManager) uploadRoutes(mux *http.ServeMux) {
	// GET counts the uploads; POST .../prune deletes what the age setting says now, or all with {"all":true}.
	mux.HandleFunc("GET /api/orch/uploads/stats", func(w http.ResponseWriter, r *http.Request) {
		orchUploadMu.Lock()
		defer orchUploadMu.Unlock()
		writeJSON(w, m.sweepUploads(0))
	})
	mux.HandleFunc("POST /api/orch/uploads/prune", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			All bool `json:"all"`
		}
		_ = json.NewDecoder(io.LimitReader(r.Body, 1024)).Decode(&body)
		age := uploadMaxAge()
		if body.All {
			age = 2 * time.Minute // a message being sent right now keeps its file
		}
		orchUploadMu.Lock()
		defer orchUploadMu.Unlock()
		writeJSON(w, m.sweepUploads(age))
	})
	mux.HandleFunc("POST /api/orch/uploads", func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, orchUploadMax+(1<<20))
		mr, err := r.MultipartReader()
		if err != nil {
			orchWriteErr(w, orchBadRequest("multipart/form-data body expected"))
			return
		}
		scope := r.URL.Query().Get("threadId")
		for {
			part, err := mr.NextPart()
			if err != nil {
				var mbe *http.MaxBytesError
				if errors.As(err, &mbe) {
					orchWriteErr(w, &orchHTTPError{http.StatusRequestEntityTooLarge, "file is larger than 25 MB"})
					return
				}
				orchWriteErr(w, orchBadRequest("multipart body has no file part"))
				return
			}
			if part.FormName() == "threadId" {
				b, _ := io.ReadAll(io.LimitReader(part, 64))
				scope = strings.TrimSpace(string(b))
				continue
			}
			if part.FileName() == "" && part.FormName() != "file" {
				continue
			}
			img, err := m.SaveUploadCtx(r.Context(), scope, part.FileName(), part.Header.Get("Content-Type"), part)
			if err != nil {
				var mbe *http.MaxBytesError
				if errors.As(err, &mbe) {
					err = &orchHTTPError{http.StatusRequestEntityTooLarge, "file is larger than 25 MB"}
				}
				orchWriteErr(w, err)
				return
			}
			out := map[string]any{"id": img.ID, "mime": img.Mime, "name": img.Name, "size": img.Size, "url": img.URL}
			if localCaller(r) {
				out["path"] = img.Path
			}
			writeJSONStatus(w, http.StatusCreated, out)
			return
		}
	})
	mux.HandleFunc("GET /api/orch/uploads/{id}", func(w http.ResponseWriter, r *http.Request) {
		img, err := m.Upload(r.PathValue("id"))
		if err != nil {
			orchWriteErr(w, err)
			return
		}
		f, err := os.Open(img.Path)
		if err != nil {
			orchWriteErr(w, &orchHTTPError{http.StatusNotFound, "unknown upload"})
			return
		}
		defer f.Close()
		ct := img.Mime
		if strings.HasPrefix(ct, "text/") || ct == "application/json" {
			ct = "text/plain; charset=utf-8" // shown as text, never run
		}
		w.Header().Set("Content-Type", ct)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if ct != "application/pdf" {
			w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox") // the browser PDF viewer refuses a sandbox
		}
		if ct == "image/heic" {
			w.Header().Set("Content-Disposition", "attachment")
		}
		w.Header().Set("Cache-Control", "private, max-age=86400")
		_, _ = io.Copy(w, f)
	})
}
