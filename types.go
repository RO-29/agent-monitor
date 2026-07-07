package main

type Tool string

const (
	ToolClaude      Tool = "claude"
	ToolCodex       Tool = "codex"
	ToolOpencode    Tool = "opencode"
	ToolCursor      Tool = "cursor"
	ToolCursorAgent Tool = "cursor-agent"
)

type State string

const (
	StateRunning            State = "running"
	StateAwaitingPermission State = "awaiting-permission"
	StateAwaitingInput      State = "awaiting-input"
	StateIdle               State = "idle"
	StateCompleted          State = "completed"
	StateAbandoned          State = "abandoned"
)

type RecentEvent struct {
	Ts   int64  `json:"ts"`
	Kind string `json:"kind"`
	Text string `json:"text"`
}

type TokenUsage struct {
	Input         int64 `json:"input"`
	Output        int64 `json:"output"`
	CacheRead     int64 `json:"cacheRead"`
	CacheCreate   int64 `json:"cacheCreate"`
	WebSearch     int64 `json:"webSearch,omitempty"`
	WebFetch      int64 `json:"webFetch,omitempty"`
}

type Session struct {
	ID                string         `json:"id"`
	Tool              Tool           `json:"tool"`
	SessionID         string         `json:"sessionId"`
	Cwd               string         `json:"cwd"`
	State             State          `json:"state"`
	Pid               int            `json:"pid,omitempty"`
	StartedAt         int64          `json:"startedAt"`
	LastActivityAt    int64          `json:"lastActivityAt"`
	LastMessage       string         `json:"lastMessage,omitempty"`
	PermissionMessage string         `json:"permissionMessage,omitempty"`
	Title             string         `json:"title,omitempty"`
	FirstMessage      string         `json:"firstMessage,omitempty"`
	Model             string         `json:"model,omitempty"`
	Branch            string         `json:"branch,omitempty"`
	Mode              string         `json:"mode,omitempty"`
	MessageCount      int            `json:"messageCount"`
	Tokens            TokenUsage     `json:"tokens"`
	ToolUsage         map[string]int `json:"toolUsage,omitempty"`
	SubagentCount     int            `json:"subagentCount,omitempty"`
	FilesTouched      int            `json:"filesTouched,omitempty"`
	BgTasksCount      int            `json:"bgTasksCount,omitempty"`
	TranscriptPath    string         `json:"transcriptPath,omitempty"` // for /api/session/:id/full
	RecentEvents      []RecentEvent  `json:"recentEvents"`
}

// SessionDetail is the on-demand response for a single session — full message
// log, paired tool_use/tool_result, subagent dispatches, etc. Only computed
// when the user opens a session in the UI, since the JSONL parse is heavy.
type SessionDetail struct {
	Session   *Session       `json:"session"`
	Messages  []DetailMsg    `json:"messages"`
	ToolCalls []DetailTool   `json:"toolCalls"`
	Subagents []DetailSub    `json:"subagents"`
	Files     []string       `json:"files"`
	BgTasks   []DetailBgTask `json:"bgTasks"`
}

type DetailMsg struct {
	Ts        int64  `json:"ts"`
	Role      string `json:"role"`         // "user" or "assistant"
	Text      string `json:"text"`         // primary text content (clipped)
	ToolCount int    `json:"toolCount"`    // # tool_use parts on this turn
	Model     string `json:"model,omitempty"`
}

type DetailTool struct {
	ID         string `json:"id"`         // tool_use_id
	Name       string `json:"name"`       // Bash, Edit, Task…
	Ts         int64  `json:"ts"`
	Args       string `json:"args"`       // JSON-serialised input, clipped
	Result     string `json:"result"`     // tool_result text, clipped
	IsSubagent bool   `json:"isSubagent"` // Task with subagent_type
}

type DetailSub struct {
	ID           string `json:"id"`
	SubagentType string `json:"subagentType"`
	Description  string `json:"description"`
	Prompt       string `json:"prompt"`
	Result       string `json:"result"`
	Ts           int64  `json:"ts"`
}

type DetailBgTask struct {
	Ts       int64  `json:"ts"`
	ID       string `json:"id"`
	Status   string `json:"status"`
	Summary  string `json:"summary"`
}

// ServerEvent is sent over the WebSocket. JSON layout matches the original TS:
//
//	{ kind: "snapshot", sessions: [...] }
//	{ kind: "upsert", session: {...} }
//	{ kind: "remove", id: "..." }
type ServerEvent struct {
	Kind     string     `json:"kind"`
	Sessions []*Session `json:"sessions,omitempty"`
	Session  *Session   `json:"session,omitempty"`
	ID       string     `json:"id,omitempty"`
}
