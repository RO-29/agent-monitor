package main

// buildSessionDetail produces an on-demand SessionDetail for the given
// Session by parsing its transcript file. For non-Claude tools (Codex,
// Cursor, opencode, cursor-agent) we don't yet have a parser, so we return
// the metadata-only response — the frontend will still show the header,
// recent events, and any aggregates we have.
func buildSessionDetail(sess *Session) *SessionDetail {
	det := &SessionDetail{
		Session:   sess,
		Messages:  []DetailMsg{},
		ToolCalls: []DetailTool{},
		Subagents: []DetailSub{},
		Files:     []string{},
		BgTasks:   []DetailBgTask{},
	}
	if sess.TranscriptPath == "" {
		return det
	}
	// maxMessages = 0 means "no cap". The user wants to scroll the full
	// history; we hand the entire transcript over and let the UI virtualise
	// rendering with content-visibility:auto. Memory cost: a few MB per
	// session for very long ones, served once on demand — acceptable.
	switch sess.Tool {
	case ToolClaude:
		_, _ = scanClaudeJSONL(sess.TranscriptPath, det, 0)
	case ToolCodex:
		_, _ = scanCodexJSONL(sess.TranscriptPath, det, 0)
	case ToolOpencode:
		_, partRoot := opencodeStoragePaths(sess.SessionID)
		_, _ = scanOpencodeStorage(sess.SessionID, sess.TranscriptPath, partRoot, det, 0)
	case ToolCursor:
		_, _ = scanCursorChat(sess.TranscriptPath, det, 0)
	}
	return det
}
