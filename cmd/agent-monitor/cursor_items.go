package main

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Cursor CLI chats: meta names the newest root blob, whose protobuf field 1 lists message blob ids in order;
// each message blob is JSON in the Vercel AI SDK shape (role + content parts).

type cursorMsg struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type cursorPart struct {
	Type       string          `json:"type"`
	Text       string          `json:"text"`
	ToolCallID string          `json:"toolCallId"`
	ToolName   string          `json:"toolName"`
	Args       json.RawMessage `json:"args"`
	Result     json.RawMessage `json:"result"`
}

var (
	reCursorQuery = regexp.MustCompile(`(?s)<user_query>\s*(.*?)\s*</user_query>`)
	reCursorTime  = regexp.MustCompile(`<timestamp>([^<]+)</timestamp>`)
	reCursorThink = regexp.MustCompile(`(?s)^\s*<think>(.*?)</think>\s*`)
	reCursorWS    = regexp.MustCompile(`Workspace Path: ([^\\"]+)`) // raw JSON: stop at \n or the closing quote
)

var cursorItemCache = struct {
	mu sync.Mutex
	m  map[string]cursorParsed
}{m: map[string]cursorParsed{}}

type cursorParsed struct {
	stamp string
	items []*OItem
	runs  []*ORun
	cwd   string // "Workspace Path" from Cursor's context message
}

// cursorItemsFor reads a Cursor chat dir into items and runs; the parse is cached until store.db or its WAL changes.
func cursorItemsFor(threadID, chatDir string) ([]*OItem, []*ORun, error) {
	storeDB := chatDir + "/store.db"
	stamp := ""
	for _, p := range []string{storeDB, storeDB + "-wal"} {
		if st, err := os.Stat(p); err == nil {
			stamp += fmt.Sprintf("%d/%d;", st.Size(), st.ModTime().UnixNano())
		}
	}
	if stamp == "" {
		return nil, nil, os.ErrNotExist
	}
	cursorItemCache.mu.Lock()
	if c, ok := cursorItemCache.m[chatDir]; ok && c.stamp == stamp {
		cursorItemCache.mu.Unlock()
		return c.items, c.runs, nil
	}
	cursorItemCache.mu.Unlock()

	rootHex, _, _, createdAt := cursorReadMetaFields(storeDB)
	blobs := cursorLoadAllBlobs(storeDB)
	root, ok := blobs[rootHex]
	if !ok {
		return nil, nil, fmt.Errorf("cursor chat has no root blob")
	}
	order := cursorOrder(root)
	items, runs := cursorBuild(threadID, order, blobs, createdAt)

	cursorItemCache.mu.Lock()
	if len(cursorItemCache.m) > 32 {
		cursorItemCache.m = map[string]cursorParsed{}
	}
	cursorItemCache.m[chatDir] = cursorParsed{stamp: stamp, items: items, runs: runs, cwd: cursorWorkspace(order, blobs)}
	cursorItemCache.mu.Unlock()
	return items, runs, nil
}

// cursorWorkspace finds the "Workspace Path:" line Cursor puts in its first context message.
func cursorWorkspace(order []string, blobs map[string][]byte) string {
	for _, id := range order[:min(len(order), 4)] {
		if m := reCursorWS.FindSubmatch(blobs[id]); m != nil {
			return strings.TrimSpace(string(m[1]))
		}
	}
	return ""
}

// cursorChatCwd is the chat's workspace folder from its messages ("" when unknown).
func cursorChatCwd(chatDir string) string {
	if _, _, err := cursorItemsFor("", chatDir); err != nil {
		return ""
	}
	cursorItemCache.mu.Lock()
	defer cursorItemCache.mu.Unlock()
	return cursorItemCache.m[chatDir].cwd
}

// cursorOrder returns the 32-byte ids in the root blob's field 1, in order.
func cursorOrder(buf []byte) []string {
	var out []string
	for i := 0; i < len(buf); {
		tag, n := binary.Uvarint(buf[i:])
		if n <= 0 {
			break
		}
		i += n
		switch tag & 7 {
		case 0:
			_, n := binary.Uvarint(buf[i:])
			if n <= 0 {
				return out
			}
			i += n
		case 1:
			i += 8
		case 5:
			i += 4
		case 2:
			l, n := binary.Uvarint(buf[i:])
			if n <= 0 || i+n+int(l) > len(buf) {
				return out
			}
			i += n
			if tag>>3 == 1 && l == 32 {
				out = append(out, hex.EncodeToString(buf[i:i+32]))
			}
			i += int(l)
		default:
			return out
		}
	}
	return out
}

func cursorBuild(threadID string, order []string, blobs map[string][]byte, createdAt int64) ([]*OItem, []*ORun) {
	var items []*OItem
	var runs []*ORun
	var run *ORun
	tools := map[string]*OItem{}
	ts := createdAt
	add := func(id string, it *OItem) {
		it.ID, it.ThreadID, it.StartedAt = threadID+"@"+id, threadID, ts
		if run != nil {
			it.RunID = run.ID
		}
		items = append(items, it)
	}
	for _, bid := range order {
		raw := blobs[strings.ToUpper(bid)]
		if raw == nil {
			raw = blobs[bid]
		}
		var m cursorMsg
		if raw == nil || json.Unmarshal(raw, &m) != nil || m.Role == "" {
			continue
		}
		parts := cursorParts(m.Content)
		short := bid[:12]
		switch m.Role {
		case "user":
			var text string
			for _, p := range parts {
				if p.Type == "text" {
					text += p.Text
				}
			}
			q := reCursorQuery.FindStringSubmatch(text)
			if q == nil {
				continue // context Cursor adds (user_info, rules), not something the user typed
			}
			if t := reCursorTime.FindStringSubmatch(text); t != nil {
				if at := parseCursorTime(t[1]); at > 0 {
					ts = at
				}
			}
			if run != nil && run.Status == "running" {
				run.Status, run.EndedAt = "completed", ts
			}
			run = &ORun{ID: threadID + "@" + short, ThreadID: threadID, Index: len(runs), Prompt: q[1], Status: "running", StartedAt: ts}
			runs = append(runs, run)
			add(short+".0", &OItem{Kind: "user", Status: "completed", Text: q[1], EndedAt: ts})
		case "assistant":
			for i, p := range parts {
				id := fmt.Sprintf("%s.%d", short, i)
				switch p.Type {
				case "text":
					text := p.Text
					if th := reCursorThink.FindStringSubmatch(text); th != nil {
						if t := strings.TrimSpace(th[1]); t != "" {
							add(id+"r", &OItem{Kind: "reasoning", Status: "completed", Text: t})
						}
						text = text[len(th[0]):]
					}
					if strings.TrimSpace(text) != "" {
						add(id, &OItem{Kind: "agent", Status: "completed", Text: strings.TrimSpace(text)})
					}
				case "reasoning":
					if t := strings.TrimSpace(p.Text); t != "" {
						add(id, &OItem{Kind: "reasoning", Status: "completed", Text: t})
					}
				case "tool-call":
					it := cursorToolItem(p)
					add(id, it)
					if p.ToolCallID != "" {
						tools[p.ToolCallID] = it
					}
				}
			}
		case "tool":
			for _, p := range parts {
				if it := tools[p.ToolCallID]; it != nil && p.Type == "tool-result" {
					it.Output, it.Status, it.EndedAt = clipString(cursorResultText(p.Result), 8192), "completed", ts
				}
			}
		}
	}
	if run != nil && run.Status == "running" && len(items) > 0 && items[len(items)-1].Kind == "agent" {
		run.Status, run.EndedAt = "completed", ts
	}
	return items, runs
}

func cursorParts(c json.RawMessage) []cursorPart {
	var s string
	if json.Unmarshal(c, &s) == nil {
		return []cursorPart{{Type: "text", Text: s}}
	}
	var ps []cursorPart
	_ = json.Unmarshal(c, &ps)
	return ps
}

// cursorToolItem shows a shell call as a command and other tools by name with their main argument.
func cursorToolItem(p cursorPart) *OItem {
	var args map[string]any
	_ = json.Unmarshal(p.Args, &args)
	if cmd, _ := args["command"].(string); cmd != "" {
		it := &OItem{Kind: "command", Status: "started", Text: p.ToolName, Command: cmd}
		if cwd, _ := args["cwd"].(string); cwd != "" {
			it.Cwd = cwd
		}
		return it
	}
	detail := ""
	for _, k := range []string{"target_file", "targetFile", "path", "file_path", "filePath", "target_directory", "targetDirectory",
		"query", "pattern", "globPattern", "searchTerm", "url"} {
		if v, _ := args[k].(string); v != "" {
			detail = v
			break
		}
	}
	text := p.ToolName
	if detail != "" {
		text += " " + detail
	}
	kind := "other"
	if strings.Contains(strings.ToLower(p.ToolName), "edit") || strings.Contains(strings.ToLower(p.ToolName), "write") {
		kind = "fileChange"
	}
	return &OItem{Kind: kind, Status: "started", Text: text}
}

func cursorResultText(r json.RawMessage) string {
	var s string
	if json.Unmarshal(r, &s) == nil {
		return s
	}
	return string(r)
}

// parseCursorTime reads "Wednesday, Nov 18, 2026, 11:55 AM (UTC+5:30)" as epoch ms.
func parseCursorTime(s string) int64 {
	s = strings.TrimSpace(s)
	loc := time.Local
	if i := strings.Index(s, "(UTC"); i > 0 {
		off := strings.TrimSuffix(s[i+4:], ")")
		s = strings.TrimSpace(s[:i])
		sign := 1
		if strings.HasPrefix(off, "-") {
			sign = -1
		}
		off = strings.TrimLeft(off, "+-")
		h, m := 0, 0
		_, _ = fmt.Sscanf(off, "%d:%d", &h, &m)
		loc = time.FixedZone("cursor", sign*(h*3600+m*60))
	}
	t, err := time.ParseInLocation("Monday, Jan 2, 2006, 3:04 PM", s, loc)
	if err != nil {
		return 0
	}
	return t.UnixMilli()
}

// cursorChatCursor is a change marker for the client's after= polling.
func cursorChatCursor(items []*OItem) int64 {
	h := sha256.New()
	for _, it := range items {
		fmt.Fprintf(h, "%s|%s|%d|%s\n", it.ID, it.Status, len(it.Text), it.Output)
	}
	b := h.Sum(nil)
	return int64(binary.BigEndian.Uint64(b[:8]) >> 1)
}
