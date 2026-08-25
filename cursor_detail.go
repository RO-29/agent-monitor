package main

import (
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"os/exec"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// cursorChatAggregate carries the seed-time aggregate for Cursor chats.
// Currently just a message count — token totals etc. aren't recoverable from
// the protobuf blobs without the schema.
type cursorChatAggregate struct {
	MessageCount int
}

// scanCursorChat extracts whatever readable content we can from a Cursor IDE
// chat's SQLite store.db. Cursor uses a content-addressed blob store: meta
// holds a `latestRootBlobId`, and the blobs table is a Merkle DAG where each
// blob is a protobuf message. The schema isn't documented, but a generic
// protobuf scan recovers the user-visible strings (titles, message text,
// tool names, code blocks) by walking the tree from root and collecting any
// length-delimited fields whose bytes parse as readable UTF-8.
//
// transcriptPath is the chat directory: ~/.cursor/chats/<wsh>/<chatID>/.
func scanCursorChat(transcriptPath string, detailOut *SessionDetail, maxMessages int) (cursorChatAggregate, error) {
	storeDB := transcriptPath + "/store.db"
	agg := cursorChatAggregate{}
	if !pathExists(storeDB) {
		return agg, nil
	}

	// Read the meta blob to get the root pointer + chat name.
	rootHex, name, mode, createdAt := cursorReadMetaFields(storeDB)
	_, _, _ = name, mode, createdAt // already surfaced in cursor.go's seed path

	blobs := cursorLoadAllBlobs(storeDB)
	if len(blobs) == 0 {
		return agg, nil
	}

	// Walk from root, breadth-first, recording every text fragment we find.
	type frag struct {
		blobID string
		text   string
	}
	var frags []frag
	visited := map[string]bool{}
	queue := []string{rootHex}
	if rootHex == "" {
		// No root pointer — fall back to scanning every blob.
		for id := range blobs {
			queue = append(queue, id)
		}
	}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		if visited[id] {
			continue
		}
		visited[id] = true
		raw, ok := blobs[id]
		if !ok {
			continue
		}
		hashes, texts := decodeProtobufFields(raw)
		for _, t := range texts {
			frags = append(frags, frag{blobID: id, text: t})
		}
		for _, h := range hashes {
			if len(h) == 32 {
				queue = append(queue, hex.EncodeToString(h))
			}
		}
	}

	// Cursor's DAG doesn't carry timestamps inside the blobs we can decode
	// without the schema, so we display fragments in the order we visited
	// them (BFS from root) — that approximates a chronological message log
	// for typical chat structures.
	//
	// Heuristic role assignment: alternate user/assistant per fragment so the
	// UI shows a conversation-like view. A real schema would let us be exact;
	// this is the best approximation without one. Drop fragments < 30 chars
	// — those are nearly always tool names, IDs, short config strings, or
	// protobuf labels rather than real message text.
	baseTs := time.Now().UnixMilli()
	role := "user"
	count := 0
	for _, f := range frags {
		t := f.text
		if len(t) < 30 {
			continue
		}
		if cursorLooksLikeID(t) {
			continue
		}
		if !strings.ContainsAny(t, " \n\t.,:;?!") {
			continue
		}
		count++
		if detailOut != nil && (maxMessages == 0 || count <= maxMessages) {
			detailOut.Messages = append(detailOut.Messages, DetailMsg{
				Ts:   baseTs + int64(count)*1000,
				Role: role,
				Text: clipString(t, 6000),
			})
			role = map[string]string{"user": "assistant", "assistant": "user"}[role]
		}
	}
	agg.MessageCount = count
	if detailOut != nil {
		sort.Slice(detailOut.Messages, func(i, j int) bool {
			return detailOut.Messages[i].Ts < detailOut.Messages[j].Ts
		})
	}
	return agg, nil
}

// cursorReadMetaFields parses the meta-row JSON: returns latestRootBlobId,
// name, mode, createdAt. Returns empty values on failure.
func cursorReadMetaFields(storeDB string) (rootHex, name, mode string, createdAt int64) {
	out, err := exec.Command("/usr/bin/sqlite3", storeDB,
		"SELECT value FROM meta WHERE key='0' LIMIT 1;").Output()
	if err != nil {
		return
	}
	hexStr := strings.TrimSpace(string(out))
	raw, err := hex.DecodeString(hexStr)
	if err != nil {
		return
	}
	var m struct {
		LatestRootBlobId string `json:"latestRootBlobId"`
		Name             string `json:"name"`
		Mode             string `json:"mode"`
		CreatedAt        int64  `json:"createdAt"`
	}
	if err := json.Unmarshal(raw, &m); err == nil {
		return m.LatestRootBlobId, m.Name, m.Mode, m.CreatedAt
	}
	return
}

// cursorLoadAllBlobs returns a map of blob-id (hex string) -> raw bytes.
// We force `hex(data)` in SQL because the data column is BLOB — emitting raw
// bytes through the CLI mangles newlines and breaks line-based parsing.
func cursorLoadAllBlobs(storeDB string) map[string][]byte {
	out, err := exec.Command("/usr/bin/sqlite3", "-separator", "|",
		storeDB, "SELECT id, hex(data) FROM blobs;").Output()
	if err != nil {
		return nil
	}
	res := map[string][]byte{}
	for _, line := range strings.Split(string(out), "\n") {
		if line == "" {
			continue
		}
		i := strings.IndexByte(line, '|')
		if i < 0 {
			continue
		}
		id := line[:i]
		raw, err := hex.DecodeString(line[i+1:])
		if err != nil {
			continue
		}
		res[id] = raw
	}
	return res
}

// decodeProtobufFields walks the protobuf wire format generically. Returns
// length-delimited bytes that look like 32-byte hashes (Merkle children) and
// strings that look like UTF-8 text (message content). Other field types
// (varint, fixed) are read past but not surfaced.
func decodeProtobufFields(buf []byte) (hashes [][]byte, texts []string) {
	for i := 0; i < len(buf); {
		tag, n := binary.Uvarint(buf[i:])
		if n <= 0 {
			return
		}
		i += n
		wireType := tag & 0x7
		switch wireType {
		case 0: // varint
			_, n := binary.Uvarint(buf[i:])
			if n <= 0 {
				return
			}
			i += n
		case 1: // fixed64
			i += 8
		case 2: // length-delimited
			length, n := binary.Uvarint(buf[i:])
			if n <= 0 {
				return
			}
			i += n
			if i+int(length) > len(buf) {
				return
			}
			data := buf[i : i+int(length)]
			i += int(length)
			// Classify by content. The order matters:
			//   1) 32-byte segments are SHA-256 child hashes
			//   2) Try to recurse as nested protobuf — if it parses cleanly
			//      we trust the structure. If recursion finds text fragments,
			//      use those; otherwise treat the whole thing as text.
			//   3) Fall through to "plain text" only when there's no nested
			//      structure detected — this avoids surfacing protobuf tag
			//      bytes as text noise.
			if len(data) == 32 {
				hashes = append(hashes, data)
				continue
			}
			h2, t2 := decodeProtobufFields(data)
			if len(h2) > 0 || len(t2) > 0 {
				hashes = append(hashes, h2...)
				texts = append(texts, t2...)
				continue
			}
			if utf8.Valid(data) && cursorIsReadableText(data) {
				if t := cleanCursorText(data); t != "" {
					texts = append(texts, t)
				}
			}
		case 5: // fixed32
			i += 4
		default:
			return
		}
	}
	return
}

// cursorIsReadableText decides whether a byte slice is "user-visible text"
// vs. random binary. Requires at least 4 chars and a high ratio of printable
// to total characters.
func cursorIsReadableText(data []byte) bool {
	if len(data) < 4 {
		return false
	}
	printable := 0
	for _, b := range data {
		if b >= 0x20 && b < 0x7f {
			printable++
		} else if b == '\n' || b == '\t' || b == '\r' {
			printable++
		}
	}
	return float64(printable)/float64(len(data)) > 0.92
}

// cleanCursorText strips control bytes from the boundaries of an extracted
// text fragment. The Merkle blobs concatenate small protobuf fields, so a
// "message" sometimes starts/ends with stray tag bytes (\x12, \x1a, …).
// Returns "" if nothing meaningful is left.
func cleanCursorText(data []byte) string {
	// Trim leading bytes that aren't printable (allow \n\t\r in the middle).
	start := 0
	for start < len(data) {
		b := data[start]
		if b >= 0x20 && b < 0x7f {
			break
		}
		start++
	}
	end := len(data)
	for end > start {
		b := data[end-1]
		if b >= 0x20 && b < 0x7f || b == '\n' || b == '\t' || b == '\r' {
			break
		}
		end--
	}
	t := strings.TrimSpace(string(data[start:end]))
	if len(t) < 4 {
		return ""
	}
	return t
}

// cursorLooksLikeID filters out fragments that are obvious internal IDs and
// not message content. UUIDs, hex hashes, sole digits, etc.
func cursorLooksLikeID(s string) bool {
	t := strings.TrimSpace(s)
	if len(t) >= 32 && len(t) <= 40 {
		hexish := true
		for _, r := range t {
			if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F') || r == '-') {
				hexish = false
				break
			}
		}
		if hexish {
			return true
		}
	}
	// UUID v4 pattern
	if len(t) == 36 && strings.Count(t, "-") == 4 {
		return true
	}
	return false
}
