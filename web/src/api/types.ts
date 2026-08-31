// JSON shapes served by the Go daemon. Field names mirror the Go struct tags in
// types.go, trace.go, threads.go, perm.go, talk.go, pane_registry.go.

export type Tool = "claude" | "codex" | "opencode" | "cursor" | "cursor-agent";
export type SessionState =
  | "running"
  | "awaiting-permission"
  | "awaiting-input"
  | "idle"
  | "completed"
  | "abandoned";

export interface TokenUsage {
  input: number;
  output: number;
  cacheRead: number;
  cacheCreate: number;
  webSearch?: number;
  webFetch?: number;
}

export interface RecentEvent {
  ts: number;
  kind: string;
  text: string;
}

export interface Session {
  id: string; // "<tool>:<sessionId>"
  tool: Tool;
  sessionId: string;
  cwd: string;
  state: SessionState;
  pid?: number;
  startedAt: number;
  lastActivityAt: number;
  lastMessage?: string;
  permissionMessage?: string;
  title?: string;
  firstMessage?: string;
  model?: string;
  branch?: string;
  mode?: string;
  messageCount: number;
  tokens: TokenUsage;
  toolUsage?: Record<string, number>;
  subagentCount?: number;
  filesTouched?: number;
  bgTasksCount?: number;
  transcriptPath?: string;
  recentEvents: RecentEvent[];
}

// ── legacy detail (/api/session/:id/full) ─────────────────────────────────
export interface DetailMsg {
  ts: number;
  role: "user" | "assistant" | string;
  text: string;
  toolCount: number;
  model?: string;
}
export interface DetailTool {
  id: string;
  name: string;
  ts: number;
  args: string;
  result: string;
  isSubagent: boolean;
}
export interface DetailSub {
  id: string;
  subagentType: string;
  description: string;
  prompt: string;
  result: string;
  ts: number;
}
export interface DetailBgTask {
  ts: number;
  id: string;
  status: string;
  summary: string;
}
export interface SessionDetail {
  session: Session;
  messages: DetailMsg[];
  toolCalls: DetailTool[];
  subagents: DetailSub[];
  files: string[];
  bgTasks: DetailBgTask[];
}

// ── trace model (/api/trace/:id/*) ────────────────────────────────────────
export type BoundaryKind = "start" | "compact" | "clear" | "resume";
export interface Boundary {
  kind: BoundaryKind;
  at: number;
  trigger?: "auto" | "manual" | string;
  preTokens?: number;
  postTokens?: number;
  droppedTokens?: number;
  cumulativeDropped?: number;
  durationMs?: number;
  summary?: string;
  sections?: Record<string, string>;
}
export type LearningSource = "memory" | "correction" | "summary" | "output";
export interface Learning {
  id: string;
  source: LearningSource;
  text: string;
  evidence: string;
  ts: number;
  seg: number;
  heuristic?: boolean;
  ref?: string;
}
export type OutputKind = "pr" | "artifact" | "doc" | "commit";
export interface Output {
  kind: OutputKind;
  label: string;
  ref: string;
  ts: number;
  seg: number;
}
export interface IntentChange {
  ts: number;
  text: string;
}
export interface Chapter {
  point: string;
  intentChanges: IntentChange[];
  outcome: string;
  learnings: Learning[];
  open: string[];
  outputs: Output[];
  source: "deterministic" | "enriched";
  enrichedAt?: number;
  model?: string;
}
export interface Segment {
  id: string; // "<sessionId>#<index>"
  index: number;
  boundary: Boundary;
  fromTs: number;
  toTs: number;
  turns: number;
  spans: number;
  errors: number;
  tokens: TokenUsage;
  usdEst: number;
  chapter?: Chapter;
}
export type SpanKind = "user" | "turn" | "tool" | "agent";
export type SpanFamily = "bash" | "read" | "edit" | "agent" | "mcp" | "web" | "other" | "model" | "user";
export interface Span {
  id: string;
  kind: SpanKind;
  name: string;
  res?: string;
  ts: number;
  dur: number; // ms
  parent?: string;
  depth: number;
  seg: number;
  err?: boolean;
  fam: SpanFamily;
  tokens?: TokenUsage;
  model?: string;
  flag?: "correction" | "aborted" | string;
  text?: string;
  child?: string; // linked child session id (Codex subagent thread)
}
export interface TraceMeta {
  costUsd: number;
  costEstimated: boolean;
  contextUsed: number;
  contextWindow: number;
  model: string;
  firstTs: number;
  lastTs: number;
  spans: number;
  learnings: number;
  outputs: number;
}
export interface TraceFull {
  sessionId: string;
  tool: Tool;
  segments: Segment[];
  spans: Span[];
  learnings: Learning[];
  outputs: Output[];
  costUsd: number;
  costEstimated: boolean;
  contextUsed: number;
  contextWindow: number;
  model: string;
  firstTs: number;
  lastTs: number;
  generatedAt: number;
  spanTotal: number;
}
export interface SpanDetail {
  span: Span;
  args: string;
  result: string;
  same: { id: string; ts: number; dur: number; err: boolean; seg: number }[] | null;
  parents: Span[] | null;
  next: Span | null;
}
export interface TraceSummary {
  segments: number;
  compacts: number;
  clears: number;
  spans: number;
  errors: number;
  costUsd: number;
  costEstimated: boolean;
  contextUsed: number;
  contextWindow: number;
  lastPoint?: string;
  lastOutcome?: string;
  learnings: number;
  outputs: number;
  segWeights: number[];
  segKinds: BoundaryKind[];
}

// ── threads (/api/threads, /api/thread/:id[/story|/learnings]) ────────────
export interface ThreadEdge {
  from: string;
  to: string;
  kind: "clear" | "resume" | "handoff";
}
export interface Thread {
  id: string; // root session id
  title: string;
  cwd: string;
  ref?: string;
  tools: Tool[];
  sessions: string[]; // oldest → newest
  state: SessionState;
  startedAt: number;
  lastAt: number;
  tokens: TokenUsage;
  turns: number;
  attention: boolean;
  model?: string;
  edges: ThreadEdge[];
  segments: number;
  compacts: number;
  clears: number;
  errors: number;
  costUsd: number;
  costEstimated: boolean;
  learnings: number;
  outputs: number;
  lastPoint?: string;
  lastOutcome?: string;
  segWeights: number[];
  segKinds: BoundaryKind[];
  sessionSegs: number[];
}
export interface ThreadsResponse {
  threads: Thread[];
  threadOf: Record<string, string>;
}
export interface ThreadResponse {
  thread: Thread;
  sessions: Session[];
  summaries: Record<string, TraceSummary>;
}
export interface ThreadStoryMember {
  session: Session;
  segments: Segment[];
  trace: TraceMeta | null;
}
export interface ThreadStoryResponse {
  thread: Thread;
  members: ThreadStoryMember[];
}
export interface ThreadLearningsResponse {
  thread: Thread;
  learnings: Learning[];
  outputs: Output[] | null;
}
export interface ContextRow {
  sessionId: string;
  used: number;
  window: number;
  segments: number;
  state: SessionState;
}
export interface Settings {
  enrichEnabled: boolean;
  enrichModel: string;
  dailyCapUsd: number;
  spentTodayUsd: number;
  spentDay: string;
}

// ── panes / permissions / talks ───────────────────────────────────────────
export interface PaneRegistration {
  agentId: string;
  tool: Tool;
  sessionId: string;
  paneId: string;
  tmuxSocket?: string;
  cwd: string;
  pid?: number;
  alias?: string;
  registeredAt: number;
  lastSeenAt: number;
  source: "hook" | "wrapper" | "manual" | "register_self" | string;
}
export interface PaneStatus {
  registered: boolean;
  ok: boolean;
  agentId?: string;
  paneId?: string;
  alias?: string;
  source?: string;
  registeredAt?: number;
  lastSeenAt?: number;
  sessionName?: string;
  command?: string;
  path?: string;
  pid?: number;
  error?: string;
}
export interface PaneView {
  ok: boolean;
  paneId?: string;
  content?: string;
  command?: string;
  error?: string;
}
// /api/tmux/panes returns Go structs WITHOUT json tags → PascalCase keys.
export interface TmuxPane {
  PaneID: string;
  SessionName: string;
  WindowID: string;
  PanePID: number;
  CurrentCommand: string;
  CurrentPath: string;
  StartCommand: string;
}
export interface PermissionRequest {
  id: string;
  toolName: string;
  input: Record<string, unknown>;
  toolUseId?: string;
  cwd?: string;
  sessionId?: string;
  createdAt: number;
}
export interface Talk {
  id: string;
  fromAgent: string;
  fromLabel: string;
  toAgent: string;
  toLabel?: string;
  message: string;
  status: "pending" | "pending_reply" | "delivered" | "replied" | "denied" | "timeout" | "error" | string;
  reason?: string;
  reply?: string;
  createdAt: number;
  resolvedAt?: number;
  repliedAt?: number;
}
export interface Health {
  ok: boolean;
  sessions: number;
  tmuxPanes: number;
  registeredPanes: number;
}

// ── WebSocket frames (/ws) ────────────────────────────────────────────────
export type WsEvent =
  | { kind: "snapshot"; sessions: Session[] }
  | { kind: "upsert"; session: Session }
  | { kind: "remove"; id: string }
  | { kind: "perm-add"; request: PermissionRequest }
  | { kind: "perm-remove"; id: string }
  | { kind: "talk-request"; talk: Talk }
  | { kind: "talk-resolved"; talk: Talk }
  | { kind: "pane-register"; registration: PaneRegistration }
  | { kind: "pane-forget"; agentId: string }
  | { kind: "pane-name"; registration: PaneRegistration }
  | { kind: "segment"; sessionId: string; segments: number }
  | { kind: "chapter"; sessionId: string; segment: number }
  | { kind: "context"; sessionId: string };
