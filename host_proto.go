package main

import (
	"crypto/rand"
	"encoding/hex"
	"regexp"
)

// The session host protocol the daemon speaks with am-helper's pty host (the Rust helper is the only host).

var hostIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{4,64}$`)

// hostRegistration is the POST /api/hosts body.
type hostRegistration struct {
	HostID   string `json:"hostId"`
	Pid      int    `json:"pid"`
	AgentPid int    `json:"agentPid"`
	Agent    string `json:"agent"`
	Cwd      string `json:"cwd"`
	Tty      string `json:"tty,omitempty"`
	TmuxPane string `json:"tmuxPane,omitempty"`
	Socket   string `json:"socket"`
}

func newHostID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

type hostRequest struct {
	Op    string   `json:"op"`
	Data  string   `json:"data,omitempty"`
	Keys  []string `json:"keys,omitempty"`
	Lines int      `json:"lines,omitempty"`
}

type hostResponse struct {
	OK       bool   `json:"ok"`
	Error    string `json:"error,omitempty"`
	Text     string `json:"text,omitempty"`
	Cols     int    `json:"cols,omitempty"`
	Rows     int    `json:"rows,omitempty"`
	HostID   string `json:"hostId,omitempty"`
	Agent    string `json:"agent,omitempty"`
	AgentPid int    `json:"agentPid,omitempty"`
	OutBytes int64  `json:"outBytes,omitempty"`
	IdleMs   int64  `json:"idleMs,omitempty"`
}
