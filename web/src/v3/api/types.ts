// v3 API shapes from V3-SPEC.md (approvals, search/spans, ops). All timestamps epoch ms.
import type { Thread } from "../../api/types";

export type Agent = "claude" | "codex" | (string & {});
export type ApprovalKind = "permission" | "question" | "plan";
export type Risk = "low" | "medium" | "high";
export type ApprovalState = "pending" | "allowed" | "denied" | "expired" | "auto-allowed" | "auto-denied" | "passed" | "answered-in-terminal";

export interface Question {
  question: string;
  header?: string;
  multiSelect?: boolean;
  options: { label: string; description?: string }[];
}
export interface Approval {
  id: string;
  agent: Agent;
  sessionId: string;
  cwd: string;
  kind: ApprovalKind;
  toolName: string;
  input: Record<string, unknown>;
  summary: string;
  risk: Risk;
  riskReason: string;
  alwaysOk?: boolean;
  suggestions?: object[];
  questions?: Question[];
  plan?: string;
  createdAt: number;
  holdUntil: number;
  state: ApprovalState;
  decidedAt?: number;
  decidedBy?: string;
  message?: string;
  ruleId?: string;
  drivenId?: string; // orchestrator thread that raised it
  rootId?: string; // root of that thread's subagent tree (phase 3)
  options?: PermOption[]; // ACP: the permission options the agent offered
  source?: "terminal-shown" | (string & {}); // the CLI shows its own prompt too
  sentAt?: number; // a card decision was typed into the terminal and waits for confirmation
  choice?: string; // what the terminal answered, when it can be read
  toolUseId?: string;
}
export interface PermOption {
  optionId: string;
  name: string;
  kind: "allow_once" | "allow_always" | "reject_once" | "reject_always" | string;
}
export interface ApprovalsResponse {
  pending: Approval[];
  recent: Approval[];
}
export interface DecideBody {
  behavior: "allow" | "deny";
  message?: string;
  interrupt?: boolean;
  updatedInput?: Record<string, unknown>;
  always?: boolean;
  answers?: Record<string, string>;
  surface?: "web" | "mac" | "phone";
  optionId?: string; // ACP: the exact permission option the user picked
  applySuggestions?: boolean; // Claude: also return the CLI's permission_suggestions
}
export interface Rule {
  id: string;
  action: "allow" | "deny" | "hold";
  agent?: Agent;
  tool?: string;
  pattern?: string;
  cwdPrefix?: string;
  note?: string;
  createdAt: number;
  hits: number;
}
export interface ApprovalSettings {
  holdSeconds: number;
  questionHoldSeconds: number;
  skipWhenFocused: boolean;
  approvalsMode?: "both" | "hold";
}
export interface HooksSnippet {
  claude: unknown;
  codex: unknown;
}

// search
export interface Chip {
  key: string;
  value: string;
  source: "typed" | "inferred";
}
export interface Hit {
  id: number | string;
  session: string;
  agent: string;
  cwd: string;
  repo: string;
  ts: number;
  kind: string;
  tool?: string;
  turn?: number;
  span?: string;
  path?: string;
  snippet: string;
  threadId?: string;
  drivenId?: string; // phase 6: hit from a driven thread
}
export interface SearchResponse {
  query: { text: string; chips: Chip[] };
  total: number;
  facets: { kind?: Record<string, number>; agent?: Record<string, number>; repo?: Record<string, number>; tool?: Record<string, number> };
  hits: Hit[];
}
export interface SearchStatus {
  fts5: boolean;
  indexedFiles: number;
  totalFiles: number;
  docs: number;
  spans: number;
  indexing: boolean;
  lastRunAt: number;
}
export interface ContextDoc {
  id: number | string;
  kind: string;
  ts: number;
  text: string;
  tool?: string;
  turn?: number;
  session?: string;
  span?: string;
}
// The context endpoint returns the doc plus neighbours; accept either shape.
export interface ContextResponse {
  doc?: ContextDoc;
  before?: ContextDoc[];
  after?: ContextDoc[];
  docs?: ContextDoc[];
}
export interface AskResponse {
  answer: string;
  citations: { n: number; hitId: number | string; session: string; ts: number }[];
  costUsd: number;
}

// spans
export interface SpanRow {
  id: string;
  session: string;
  agent: string;
  cwd: string;
  repo: string;
  ts: number;
  dur: number;
  tool: string;
  fam: string;
  cmd: string;
  cmdKey: string;
  err: number | boolean;
  errSig?: string;
  errText?: string;
  child?: string;
}
export interface SpansResponse {
  total: number;
  failed: number;
  histogram: { buckets: { fromMs: number; toMs: number; n: number; failed: number }[] };
  groups: { key: string; runs: number; failed: number; p50: number; p95: number; repos: string[]; lastTs: number }[];
  spans?: SpanRow[];
}
export interface LatencyResponse {
  tools: { tool: string; calls: number; p50: number; p95: number; errRate: number; trend: number[] }[];
}
export interface ErrorsResponse {
  groups: { sig: string; sample: string; count: number; sessions: number; repos: string[] | number; firstTs: number; lastTs: number; tool: string; cmdKey: string; trend: number[] }[];
}
export interface GraphResponse {
  nodes: { id: string; label: string; kind: "agent" | "subagent" | "delegated" | "mcp"; count: number }[];
  edges: { from: string; to: string; calls: number; failRate: number; p95: number }[];
}

// ops (/api/v3/*)
export interface Shipped {
  kind: "commit" | "pr" | "push" | "page" | "merge";
  label: string;
  ref?: string;
  ts: number;
}
export interface V3Thread extends Thread {
  parentThread?: string;
  childKind?: string;
  childCount?: number;
  childAttention?: boolean | number;
  childErrors?: number;
  approvals?: number;
  liveActivity?: string;
  display?: string;
  repo?: string;
  outcomeLine?: string;
  shipped?: Shipped[];
  errorsOpen?: number;
  activity?: number[];
  activityErr?: number[];
  stackId?: string;
  drivenId?: string; // set when an orchestrator thread drives this session
  handover?: "driven" | "terminal"; // driven rows with a native session: who owns it now
  terminalSession?: string; // the session a terminal took over
  kind?: "driven" | "watched"; // phase 6: driven threads are first-class rows
  status?: string; // driven: the orchestrator status
  spendUsd?: number;
}
export interface Stack {
  id: string;
  parent: string;
  agent: string;
  kind: string;
  label: string;
  repo: string;
  total: number;
  running: number;
  waiting: number;
  done: number;
  failed: number;
  abandoned: number;
  findings: number;
  lastAt: number;
  members: string[];
}
export interface V3ThreadsResponse {
  threads: V3Thread[];
  threadOf: Record<string, string>;
  stacks?: Stack[];
}
export interface NowLane {
  threadId: string;
  display: string;
  agent: string;
  bars: { from: number; to: number; kind: "work" | "wait" | "error" }[];
}
export interface NowResponse {
  needsYou: number;
  running: number;
  doneToday: number;
  spendToday: number;
  errorsPerHour: number;
  lanes: NowLane[];
  from: number;
  to: number;
}
export interface ChildRun {
  threadId: string;
  sessionId: string;
  kind: "subagent" | "delegated";
  spanId: string;
  ts: number;
  title: string;
  state: string;
  findings?: number;
  errors: number;
  attention: boolean;
}
export interface ChildrenResponse {
  parent?: { threadId: string; sessionId: string; spanId: string; ts: number };
  children: ChildRun[];
}
export interface XTraceRow {
  id: string;
  depth: number;
  kind: string;
  agent: string;
  name: string;
  res?: string;
  ts: number;
  dur: number;
  err?: boolean;
  wait?: boolean;
  child?: string;
  session?: string;
  by?: string; // "user" on a slash or ! command the user ran
}
export interface XTraceResponse {
  prompt?: string | null;
  turn?: number;
  turns?: number;
  errors?: number;
  agents?: number;
  root?: { id?: string; name?: string; text?: string; ts?: number; dur?: number; model?: string; [k: string]: unknown };
  rows: XTraceRow[];
  byAgent: Record<string, number>;
  wallMs: number;
  workMs: number;
  waitMs: number;
  costUsd: number;
}
export interface TimelineBar {
  from: number;
  to: number;
  kind: "work" | "wait" | "idle" | "error";
  agent: string;
}
export interface TimelineLane {
  threadId: string;
  title: string;
  display?: string;
  agent: string;
  state?: string;
  parent?: string;
  childKind?: string;
  startTurnTs?: number;
  errors?: number;
  outcome?: string;
  bars: TimelineBar[];
  density?: { from: number; to: number; calls: number; errors: number }[];
  kind?: "stack";
  stack?: { total: number; done: number; running: number; failed: number };
  members?: string[];
  drivenId?: string; // phase 6: lane of a driven thread
}
export interface TimelineResponse {
  from: number;
  to: number;
  lanes: TimelineLane[];
  waitMs: number;
}
export interface Monitor {
  id: string;
  name: string;
  condition: string;
  state: "ok" | "warn" | "alert";
  detail: string;
  link?: string;
}
export interface MonitorsResponse {
  monitors: Monitor[];
}
// Intel is loosely specified; every field is optional and rendered only when present.
export interface IntelResponse {
  range?: string;
  waitMs?: number;
  workMs?: number;
  toolMs?: number;
  medianApprovalMs?: number;
  peakParallel?: number;
  tools?: { tool?: string; name?: string; ms: number; calls?: number }[];
  spendByProject?: { project?: string; repo?: string; usd: number; estimated?: boolean }[];
  repeatFailures?: { sig?: string; cmd?: string; cmdKey?: string; count: number; repo?: string; threads?: number }[];
  outputs?: { kind: string; label: string; ref?: string; ts?: number; threadId?: string }[];
  insights?: { title?: string; text: string; kind?: string }[];
  // shape served by ops.go today
  spendByRepo?: Record<string, number>;
  spendEstimated?: boolean;
  timeByFamily?: Record<string, number>;
  workByAgent?: Record<string, number>;
  shipped?: { Kind?: string; Label?: string; Ref?: string; Thread?: string; Ts?: number; kind?: string; label?: string; ref?: string; threadId?: string; ts?: number }[];
  waitKnown?: boolean;
  threads?: number;
}
export interface V3Health {
  hooks: { claudeHttp: boolean; codex: boolean };
  search?: SearchStatus;
  approvals?: { pending: number };
}
