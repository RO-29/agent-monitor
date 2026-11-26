package main

import (
	"path/filepath"
	"regexp"
	"strings"
)

// Files that are not images, pdf or text (zip, Office, video, audio, archives, anything else) are kept as they are.
// The agent opens them by path; the daemon serves them inert, as a download or as playable media.

var binaryMimeExt = map[string]string{
	"application/zip": ".zip", "application/gzip": ".gz", "application/x-7z-compressed": ".7z", "application/vnd.rar": ".rar",
	"application/x-tar": ".tar", "application/vnd.sqlite3": ".sqlite",
	"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet":         ".xlsx",
	"application/vnd.openxmlformats-officedocument.wordprocessingml.document":   ".docx",
	"application/vnd.openxmlformats-officedocument.presentationml.presentation": ".pptx",
	"application/vnd.ms-excel": ".xls", "application/msword": ".doc", "application/vnd.ms-powerpoint": ".ppt",
	"video/mp4": ".mp4", "video/quicktime": ".mov", "video/webm": ".webm", "video/x-matroska": ".mkv", "video/x-msvideo": ".avi",
	"audio/mp4": ".m4a", "audio/mpeg": ".mp3", "audio/wav": ".wav", "audio/ogg": ".ogg", "audio/flac": ".flac",
	"image/bmp": ".bmp", "image/tiff": ".tiff",
}

// zipKinds and oleKinds name a zip or an old Office file by its extension; the content alone cannot tell.
var zipKinds = map[string]string{
	".xlsx": "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
	".docx": "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
	".pptx": "application/vnd.openxmlformats-officedocument.presentationml.presentation",
}
var oleKinds = map[string]string{".xls": "application/vnd.ms-excel", ".doc": "application/msword", ".ppt": "application/vnd.ms-powerpoint"}

var reSafeExt = regexp.MustCompile(`^\.[a-z0-9]{1,10}$`)

// sniffBinary names a non-text file by its first bytes, with the extension only to tell zip and OLE kinds apart.
func sniffBinary(head []byte, ext string) string {
	ext = strings.ToLower(ext)
	has := func(off int, magic string) bool {
		return len(head) >= off+len(magic) && string(head[off:off+len(magic)]) == magic
	}
	switch {
	case has(0, "PK\x03\x04") || has(0, "PK\x05\x06"):
		if m, ok := zipKinds[ext]; ok {
			return m
		}
		return "application/zip"
	case has(0, "\xd0\xcf\x11\xe0\xa1\xb1\x1a\xe1"):
		if m, ok := oleKinds[ext]; ok {
			return m
		}
		return "application/octet-stream"
	case has(0, "\xff\xfe") || has(0, "\xfe\xff"):
		return "application/octet-stream" // UTF-16 text: its BOM looks like an MPEG frame header
	case has(4, "ftyp"):
		switch {
		case has(8, "qt  "):
			return "video/quicktime"
		case has(8, "M4A ") || has(8, "M4B "):
			return "audio/mp4"
		case has(8, "isom") || has(8, "iso2") || has(8, "mp41") || has(8, "mp42") || has(8, "avc1") || has(8, "M4V ") || has(8, "3gp") || has(8, "dash"):
			return "video/mp4"
		}
		return "application/octet-stream"
	case has(0, "\x1a\x45\xdf\xa3"):
		if ext == ".mkv" {
			return "video/x-matroska"
		}
		return "video/webm"
	case has(0, "RIFF") && has(8, "AVI "):
		return "video/x-msvideo"
	case has(0, "RIFF") && has(8, "WAVE"):
		return "audio/wav"
	case has(0, "ID3") || len(head) > 1 && head[0] == 0xff && head[1]&0xe0 == 0xe0:
		return "audio/mpeg"
	case has(0, "OggS"):
		return "audio/ogg"
	case has(0, "fLaC"):
		return "audio/flac"
	case has(0, "\x1f\x8b"):
		return "application/gzip"
	case has(0, "7z\xbc\xaf\x27\x1c"):
		return "application/x-7z-compressed"
	case has(0, "Rar!\x1a\x07"):
		return "application/vnd.rar"
	case has(257, "ustar"):
		return "application/x-tar"
	case has(0, "SQLite format 3\x00"):
		return "application/vnd.sqlite3"
	case has(0, "BM") && len(head) > 14:
		return "image/bmp"
	case has(0, "II*\x00") || has(0, "MM\x00*"):
		return "image/tiff"
	}
	return "application/octet-stream"
}

// binaryExt is the stored extension: the type's own, else the file's (so tools that read the extension work),
// never one that names an image, pdf or text type the content is not.
func binaryExt(mime, name string) string {
	if e, ok := binaryMimeExt[mime]; ok {
		return e
	}
	e := strings.ToLower(filepath.Ext(name))
	if !reSafeExt.MatchString(e) || binaryExtClaimsOther(e) {
		return ".bin"
	}
	return e
}

// binaryExtClaimsOther is true for extensions the image, pdf and text paths own.
func binaryExtClaimsOther(e string) bool {
	if orchTextExt[e] || e == ".svg" || e == ".markdown" {
		return true
	}
	for _, x := range orchUploadExt {
		if x == e {
			return true
		}
	}
	return e == ".jpeg" || e == ".heif"
}

// binaryReadBack is the mime of a stored binary upload, or "" when its extension is not one binaryExt writes.
func binaryReadBack(head []byte, ext string) string {
	if ext != ".bin" && (!reSafeExt.MatchString(ext) || binaryExtClaimsOther(ext)) {
		return ""
	}
	m := sniffBinary(head, ext)
	if want, ok := binaryMimeExt[m]; ok && want != ext {
		return "application/octet-stream" // e.g. a zip stored under its own name .jar
	}
	return m
}

// playable media is served inline so the chat can show it; every other binary downloads.
func playableMime(m string) bool {
	return strings.HasPrefix(m, "video/") || strings.HasPrefix(m, "audio/") || m == "image/bmp"
}
