package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// opencodeAggregate captures the same shape as claudeAggregate / codexAggregate
// — pulled from opencode's per-message and per-part JSON files on disk.
type opencodeAggregate struct {
	Tokens       TokenUsage
	ToolUsage    map[string]int
	Files        map[string]struct{}
	MessageCount int
	FirstMessage string
	Model        string
	LastMessage  string
}

// opencodeMsgRec is a per-message JSON file (msg_*.json). Only the fields we
// rely on are typed; rest is left as map[string]any when we need it.
type opencodeMsgRec struct {
	ID        string `json:"id"`
	SessionID string `json:"sessionID"`
	Role      string `json:"role"`
	Time      struct {
		Created int64 `json:"created"`
	} `json:"time"`
	Model struct {
		ProviderID string `json:"providerID"`
		ModelID    string `json:"modelID"`
	} `json:"model"`
	Tokens struct {
		Input     float64 `json:"input"`
		Output    float64 `json:"output"`
		Reasoning float64 `json:"reasoning"`
		Cache     struct {
			Read  float64 `json:"read"`
			Write float64 `json:"write"`
		} `json:"cache"`
	} `json:"tokens"`
}

// opencodePartRec covers the common part shapes: text, tool, reasoning. Each
// part is a separate JSON file under part/<messageID>/.
type opencodePartRec struct {
	ID        string         `json:"id"`
	MessageID string         `json:"messageID"`
	Type      string         `json:"type"`
	Text      string         `json:"text"`
	Tool      string         `json:"tool"`
	State     map[string]any `json:"state"`
}

// scanOpencodeStorage extracts messages, tool calls, and aggregates for a
// single opencode session from its on-disk message + part files.
func scanOpencodeStorage(sessionID, messageDir, partRoot string, detailOut *SessionDetail, maxMessages int) (opencodeAggregate, error) {
	agg := opencodeAggregate{
		ToolUsage: map[string]int{},
		Files:     map[string]struct{}{},
	}

	entries, err := os.ReadDir(messageDir)
	if err != nil {
		return agg, err
	}
	type msgWithFile struct {
		fp  string
		msg opencodeMsgRec
	}
	var msgs []msgWithFile
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), "msg_") || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		fp := filepath.Join(messageDir, e.Name())
		data, err := os.ReadFile(fp)
		if err != nil {
			continue
		}
		var rec opencodeMsgRec
		if err := json.Unmarshal(data, &rec); err != nil {
			continue
		}
		msgs = append(msgs, msgWithFile{fp, rec})
	}
	// Order chronologically — opencode's IDs are time-ordered (ULID-style)
	// but sorting by time.created is safer when present.
	sort.Slice(msgs, func(i, j int) bool {
		ti, tj := msgs[i].msg.Time.Created, msgs[j].msg.Time.Created
		if ti != 0 || tj != 0 {
			return ti < tj
		}
		return msgs[i].msg.ID < msgs[j].msg.ID
	})

	for _, mw := range msgs {
		rec := mw.msg
		// Token tally
		agg.Tokens.Input += int64(rec.Tokens.Input)
		agg.Tokens.Output += int64(rec.Tokens.Output)
		agg.Tokens.CacheRead += int64(rec.Tokens.Cache.Read)
		agg.Tokens.CacheCreate += int64(rec.Tokens.Cache.Write)
		// Model
		if rec.Model.ModelID != "" {
			agg.Model = rec.Model.ProviderID + "/" + rec.Model.ModelID
		}

		// Read parts for this message
		partDir := filepath.Join(partRoot, rec.ID)
		partEntries, _ := os.ReadDir(partDir)
		var msgText []string
		toolCount := 0
		for _, pe := range partEntries {
			if !strings.HasPrefix(pe.Name(), "prt_") || !strings.HasSuffix(pe.Name(), ".json") {
				continue
			}
			pdata, err := os.ReadFile(filepath.Join(partDir, pe.Name()))
			if err != nil {
				continue
			}
			var part opencodePartRec
			if err := json.Unmarshal(pdata, &part); err != nil {
				continue
			}
			switch part.Type {
			case "text":
				if part.Text != "" {
					msgText = append(msgText, part.Text)
				}
			case "tool":
				if part.Tool != "" {
					agg.ToolUsage[part.Tool]++
					toolCount++
					if detailOut != nil {
						// Best-effort: pull the tool input/output from `state`
						// if present.
						input, _ := json.Marshal(part.State["input"])
						out, _ := part.State["output"].(string)
						if out == "" {
							if mout, ok := part.State["output"].(map[string]any); ok {
								if t, _ := mout["text"].(string); t != "" {
									out = t
								}
							}
						}
						detailOut.ToolCalls = append(detailOut.ToolCalls, DetailTool{
							ID:     part.ID,
							Name:   part.Tool,
							Ts:     rec.Time.Created,
							Args:   clipString(string(input), 8000),
							Result: clipString(out, 12000),
						})
					}
					// Sniff file paths from tool input (Edit/Write style)
					if input, _ := part.State["input"].(map[string]any); input != nil {
						for _, key := range []string{"path", "file_path", "filepath"} {
							if v, ok := input[key].(string); ok && v != "" {
								agg.Files[v] = struct{}{}
							}
						}
					}
				}
			}
		}

		text := strings.Join(msgText, "\n")
		if rec.Role == "user" || rec.Role == "assistant" {
			agg.MessageCount++
			if rec.Role == "user" && agg.FirstMessage == "" {
				agg.FirstMessage = clipString(text, 240)
			}
			if rec.Role == "assistant" && text != "" {
				agg.LastMessage = clipString(text, 240)
			}
			if detailOut != nil {
				detailOut.Messages = append(detailOut.Messages, DetailMsg{
					Ts:        rec.Time.Created,
					Role:      rec.Role,
					Text:      clipString(text, 6000),
					ToolCount: toolCount,
					Model:     agg.Model,
				})
			}
		}
	}

	if detailOut != nil {
		// Trim to LATEST maxMessages.
		if maxMessages > 0 && len(detailOut.Messages) > maxMessages {
			detailOut.Messages = detailOut.Messages[len(detailOut.Messages)-maxMessages:]
		}
		if maxMessages > 0 && len(detailOut.ToolCalls) > maxMessages {
			detailOut.ToolCalls = detailOut.ToolCalls[len(detailOut.ToolCalls)-maxMessages:]
		}
		for f := range agg.Files {
			detailOut.Files = append(detailOut.Files, f)
		}
		sort.Strings(detailOut.Files)
	}
	return agg, nil
}

// opencodeStoragePaths returns the on-disk message and part directories for an
// opencode session. The session's TranscriptPath holds the message dir.
func opencodeStoragePaths(sessionID string) (msgDir, partRoot string) {
	root := filepath.Join(homeDir(), ".local", "share", "opencode", "storage")
	return filepath.Join(root, "message", sessionID), filepath.Join(root, "part")
}
