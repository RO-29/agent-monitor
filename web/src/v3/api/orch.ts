// Orchestrator (driven threads) API: types per V3-SPEC "Orchestrator" phases 0 to 2, REST calls, SSE store.
import { useEffect, useSyncExternalStore } from "react";
import { ApiError, isMissing } from "./client";
import type { DecideBody } from "./types";

// Any registry id (phase 5): claude, codex, or an ACP agent such as opencode or gemini.
export type OAgent = "codex" | "claude" | (string & {});
export type OAgentKind = "claude" | "codex" | "acp";
export interface OModel {
  id: string;
  name?: string;
}
export interface OAgentInfo {
  id: string;
  name: string;
  kind: OAgentKind;
  installed: boolean;
  version?: string;
  path?: string;
  models?: (string | OModel)[];
}
export type OThreadStatus = "starting" | "idle" | "running" | "waiting" | "error" | "closed";
export type OSandbox = "read-only" | "workspace-write";
export type OPermissionMode = "manual" | "acceptEdits" | "plan" | "auto";
export type OGroup = "need" | "working" | "ready" | "settled";
export type OWorkspace = "shared" | "worktree" | "read-only";

export interface OThread {
  id: string;
  kind: "driven";
  agent: OAgent;
  title: string;
  cwd: string;
  repo: string;
  branch: string;
  model: string;
  sandbox: string;
  permissionMode?: OPermissionMode;
  status: OThreadStatus;
  nativeId: string;
  parentId?: string;
  createdAt: number;
  updatedAt: number;
  error?: string;
  // phase 3, server-derived
  children?: string[];
  depth?: number;
  worktree?: OWorktree;
  workspace?: OWorkspace;
  group?: OGroup;
  settledAt?: number;
  snoozedUntil?: number;
  pinned?: boolean;
  notifyParent?: boolean;
  // phase 7
  review?: OReview;
  scheduleId?: string;
  templateId?: string;
  reviewOf?: string;
  autoReview?: string;
  catchUp?: boolean;
}
export type OReviewState = "running" | "done" | "failed";
export interface OFinding {
  severity: string; // critical | high | medium | low | info; unknown values group under "other"
  file?: string;
  line?: number;
  title: string;
  body?: string;
}
export interface OReview {
  state: OReviewState;
  childId?: string; // the read-only child thread that ran the review
  threadId?: string; // older name, read as a fallback
  focus?: string;
  run?: number;
  agent?: OAgent;
  summary?: string;
  findings?: OFinding[];
  error?: string;
  startedAt?: number;
  endedAt?: number;
}
export interface OWorktree {
  path: string;
  branch: string;
  removed?: boolean;
  origin?: string; // phase 6: the checkout the worktree came from
}
export interface OUsage {
  inTokens: number;
  outTokens: number;
  costUsd?: number;
}
export interface ORun {
  id: string;
  threadId: string;
  index: number;
  prompt: string;
  status: "running" | "completed" | "interrupted" | "failed";
  startedAt: number;
  endedAt?: number;
  usage?: OUsage;
}
export type OItemKind = "user" | "agent" | "reasoning" | "command" | "fileChange" | "mcp" | "web" | "plan" | "error" | "other" | "review";
export interface OFile {
  path: string;
  kind: string;
  add: number;
  del: number;
}
export interface OItem {
  id: string;
  threadId: string;
  runId: string;
  kind: OItemKind;
  status: "started" | "completed" | "failed" | "declined";
  text: string;
  command?: string;
  cwd?: string;
  exitCode?: number;
  output?: string;
  files?: OFile[];
  startedAt: number;
  endedAt?: number;
  repeat?: number; // an error seen this many times; endedAt is the last time
  attachments?: OAttachment[]; // phase 7, on user items
  childId?: string; // phase 7, review items
  parentId?: string; // a tool call a Code Mode script made: the script item's id
  wakeRuns?: { n: number; since: number }; // set in the UI: this scheduled turn folds n identical earlier ones
  by?: "user" | "schedule"; // "user": a ! or slash command the user ran; "schedule": a turn a scheduled task started
  bg?: boolean; // a ! command moved to the background; output fills from its task file
}
export interface OAttachment {
  id: string;
  mime: string;
  name?: string;
  size?: number;
  url: string;
}
export interface ORequest {
  id: string;
  threadId: string;
  runId: string;
  itemId?: string;
  kind: "command" | "fileChange" | "permissions" | "question" | "plan";
  approvalId: string;
  state: "open" | "closed";
}
export type OEventType =
  | "thread.updated"
  | "run.started"
  | "run.ended"
  | "item.started"
  | "item.delta"
  | "item.completed"
  | "request.opened"
  | "request.closed"
  | "queue.updated";
export interface OEvent {
  seq: number;
  threadId: string;
  at: number;
  type: OEventType;
  data: { thread?: OThread; run?: ORun; item?: OItem; itemId?: string; text?: string; field?: "text" | "output"; request?: ORequest; items?: unknown[] };
}
export interface OThreadsResponse {
  threads: OThread[];
  seq: number;
}
export interface OThreadDetail {
  thread: OThread;
  children?: OThread[];
  runs: ORun[];
  items: OItem[];
  requests: ORequest[];
  seq: number;
}
export interface OCreateBody {
  agent: OAgent;
  cwd: string;
  prompt: string;
  model?: string;
  sandbox?: OSandbox;
  permissionMode?: OPermissionMode;
  children?: OChildSpec[];
  workspace?: ORootWorkspace;
  attachments?: string[];
}
export type ORootWorkspace = "here" | "worktree";
// Session host: "terminal" runs `agent-monitor run <agent>` in a new tmux session; "headless" is a driven thread.
export type OWhere = "terminal" | "headless";
export type OApprovalPolicy = "untrusted" | "on-request" | "never";
export interface OTerminalBody {
  agent: "claude" | "codex";
  cwd: string;
  prompt?: string;
  model?: string;
  permissionMode?: OPermissionMode;
  sandbox?: OSandbox;
  approvalPolicy?: OApprovalPolicy;
  attachments?: string[];
}
export interface OTerminalStart {
  session: string;
  hostId: string;
  attach: string;
}
export interface OHostInfo {
  hostId: string;
  agent: string;
  cwd: string;
  tmuxPane?: string;
  tool?: string;
  sessionId?: string;
  agentId?: string; // "claude:<uuid>" once the agent's first hook links the session
  prompt?: OHostPrompt; // a first-run question on screen, only before the session links
  screenTail?: string[]; // the last screen lines, only before the session links
}
/** A first-run dialog the agent shows before it starts, such as the folder trust check. */
export interface OHostPrompt {
  kind: "trust" | "mcp" | "menu" | string;
  agent: string;
  title?: string;
  folder?: string;
  text: string;
  options: { id: string; label: string }[];
}
export interface OChildSpec {
  title: string;
  prompt: string;
  agent: OAgent;
  model?: string;
  workspace: OWorkspace;
  permissionMode?: OPermissionMode;
  sandbox?: OSandbox;
}

// ---- phase 4: git ----
export type OGitStatus = "added" | "modified" | "deleted" | "renamed" | "untracked" | "copied" | "typechange" | string;
export interface OGitFile {
  path: string;
  status: OGitStatus;
  add: number;
  del: number;
}
export interface OCheckpoint {
  runIndex: number;
  kind: "start" | "end" | "restore" | string;
  ref: string;
  sha: string;
  at: number | string;
  prompt: string;
  add: number;
  del: number;
  files: number;
}
export interface OGit {
  isRepo?: boolean;
  repo: string;
  root: string;
  branch: string;
  head: string;
  upstream?: string;
  ahead: number;
  behind: number;
  dirty: number;
  defaultBranch: string;
  files: OGitFile[];
  checkpoints: OCheckpoint[];
  worktree?: OWorktree;
}
export interface ODiffFile {
  path: string;
  oldPath?: string;
  status: OGitStatus;
  add: number;
  del: number;
  binary: boolean;
  patch: string;
  truncated?: boolean;
  hash?: string;
}
export interface ODiff {
  from: string;
  to: string;
  files: ODiffFile[];
  truncated: boolean;
}
export type OOpenTarget = "editor" | "terminal" | "finder";
export interface OPrBody {
  title: string;
  body?: string;
  draft?: boolean;
  base?: string;
}
export interface GitApi {
  git: (id: string) => Promise<OGit>;
  diff: (id: string, q: { from?: string; to?: string; path?: string; run?: number }) => Promise<ODiff>;
  cwdGit: (cwd: string) => Promise<OGit>;
  cwdDiff: (cwd: string, q: { from?: string; to?: string; path?: string }) => Promise<ODiff>;
  restore: (id: string, ref: string) => Promise<{ ok: boolean; safetyRef?: string }>;
  commit: (id: string, message: string, paths?: string[]) => Promise<{ ok: boolean; sha?: string }>;
  push: (id: string) => Promise<{ ok: boolean; output?: string }>;
  pr: (id: string, b: OPrBody) => Promise<{ ok: boolean; url?: string }>;
  open: (id: string, target: OOpenTarget) => Promise<{ ok: boolean }>;
  removeWorktree: (id: string, force?: boolean) => Promise<{ ok: boolean; thread?: OThread }>;
  revertHunk: (b: { thread?: string; cwd?: string; path: string; hunk: string; hash: string }) => Promise<{ ok: boolean; hash?: string }>;
}

// Epoch ms per the v3 convention; tolerate RFC3339 strings in case the Go side sends time.Time.
export const toMs = (v: number | string | undefined | null): number => (typeof v === "number" ? v : v ? Date.parse(v) || 0 : 0);

export async function req<T>(method: string, url: string, body?: unknown): Promise<T> {
  let r: Response;
  try {
    r = await fetch(url, {
      method,
      cache: "no-store",
      headers: body !== undefined ? { "Content-Type": "application/json" } : undefined,
      body: body !== undefined ? JSON.stringify(body) : undefined,
    });
  } catch (e) {
    throw new ApiError(0, `daemon unreachable (${(e as Error).message})`);
  }
  const text = await r.text();
  let j: unknown;
  try {
    j = text ? JSON.parse(text) : undefined;
  } catch {
    j = undefined;
  }
  if (!r.ok) throw new ApiError(r.status, (j as { error?: string } | undefined)?.error || text.slice(0, 200) || `HTTP ${r.status}`, j);
  if (j === undefined && text.trimStart().startsWith("<")) throw new ApiError(404, "endpoint not available");
  return (j ?? {}) as T;
}
const enc = encodeURIComponent;

export const orchApi = {
  threads: () => req<OThreadsResponse>("GET", "/api/orch/threads"),
  create: (b: OCreateBody) => req<{ thread: OThread }>("POST", "/api/orch/threads", b),
  thread: (id: string) => req<OThreadDetail>("GET", `/api/orch/threads/${enc(id)}`),
  message: (id: string, text: string, attachments?: string[]) => req<unknown>("POST", `/api/orch/threads/${enc(id)}/messages`, attachments?.length ? { text, attachments } : { text }),
  interrupt: (id: string, cascade?: boolean) => req<unknown>("POST", `/api/orch/threads/${enc(id)}/interrupt`, cascade ? { cascade: true } : undefined),
  settle: (id: string, settled: boolean) => req<{ thread?: OThread }>("POST", `/api/orch/threads/${enc(id)}/settle`, { settled }),
  snooze: (id: string, until: number) => req<{ thread?: OThread }>("POST", `/api/orch/threads/${enc(id)}/snooze`, { until }),
  pin: (id: string, pinned: boolean) => req<{ thread?: OThread }>("POST", `/api/orch/threads/${enc(id)}/pin`, { pinned }),
  rename: (id: string, title: string) => req<{ thread?: OThread }>("POST", `/api/orch/threads/${enc(id)}/rename`, { title }),
  children: (id: string) => req<OThreadDetail>("GET", `/api/orch/threads/${enc(id)}`).then((r) => r.children || []),
  close: (id: string) => req<unknown>("POST", `/api/orch/threads/${enc(id)}/close`),
  continue: (sessionId: string, prompt?: string) => req<{ thread?: OThread; terminal?: { session: string } }>("POST", "/api/orch/continue", { sessionId, prompt: prompt || undefined }),
  streamUrl: (after: number) => `/api/orch/stream?after=${after}`,
  agents: () => req<OAgentInfo[] | { agents: OAgentInfo[] }>("GET", "/api/orch/agents").then((r) => (Array.isArray(r) ? r : r.agents || [])),
  // phase 6
  allowOnce: (id: string) => req<{ ok?: boolean; thread?: OThread }>("POST", `/api/orch/threads/${enc(id)}/allow-once`),
  continueParent: (id: string) => req<{ ok?: boolean; thread?: OThread }>("POST", `/api/orch/threads/${enc(id)}/worktree/parent`),
  costs: () => req<{ costs: Record<string, number> }>("GET", "/api/orch/costs"),
  spend: (threadId?: string) => req<OSpendStatus>("GET", `/api/orch/spend${threadId ? `?thread=${enc(threadId)}` : ""}`),
  terminal: (b: OTerminalBody) => req<{ ok?: boolean } & OTerminalStart>("POST", "/api/orch/terminal", b),
  host: (hostId: string) => req<OHostInfo>("GET", `/api/hosts/${enc(hostId)}`),
  answerHost: (hostId: string, option: string) => req<{ ok?: boolean }>("POST", `/api/hosts/${enc(hostId)}/answer`, { option }),
};

// ---- phase 6: spend limits ----
export interface OSpendScope {
  spent: number;
  limit: number; // 0 = off
  allowedOnce?: boolean;
  blocked?: boolean;
}
export interface OSpendStatus {
  day?: OSpendScope & { blocked?: boolean };
  thread?: OSpendScope;
}
/** Body of a 402 answer: the limit that refused the turn. */
export interface SpendBlock {
  error: string;
  spent: number;
  limit: number;
  scope?: "thread" | "day";
}
export function spendBlockOf(e: unknown): SpendBlock | null {
  if (!(e instanceof ApiError) || e.status !== 402) return null;
  const b = (e.body || {}) as Partial<SpendBlock>;
  return { error: b.error || e.message, spent: Number(b.spent) || 0, limit: Number(b.limit) || 0, scope: b.scope };
}

// ---- phase 5: agent registry ----

/** The phase 4 pair, used when the daemon has no /api/orch/agents. */
export const STATIC_AGENTS: OAgentInfo[] = [
  { id: "codex", name: "Codex", kind: "codex", installed: true },
  { id: "claude", name: "Claude Code", kind: "claude", installed: true },
];
const KNOWN: Record<string, { name: string; cls: string }> = {
  claude: { name: "Claude", cls: "cl" },
  codex: { name: "Codex", cls: "cx" },
  opencode: { name: "OpenCode", cls: "oc" },
  gemini: { name: "Gemini", cls: "gm" },
  cursor: { name: "Cursor", cls: "cu" },
  "cursor-agent": { name: "Cursor", cls: "cu" },
  pi: { name: "Pi", cls: "ot" },
};
/** CSS class suffix for an agent dot; unknown ids get the neutral "ot". */
export const agentCls = (id?: string) => KNOWN[(id || "").toLowerCase()]?.cls || "ot";
export const agentVar = (id?: string) => ({ cl: "var(--claude)", cx: "var(--codex)", oc: "var(--opencode)", gm: "var(--gemini)", cu: "var(--cursor)" })[agentCls(id)] || "var(--muted)";
/** Display name: registry name, then a built-in name, then the id itself. */
export function agentLabel(id?: string): string {
  const k = (id || "").toLowerCase();
  const reg = agentsSnap.list.find((a) => a.id === id);
  if (reg?.name && k !== "claude") return reg.name;
  return KNOWN[k]?.name || id || "agent";
}
export const modelsOf = (a?: OAgentInfo): OModel[] => (a?.models || []).map((m) => (typeof m === "string" ? { id: m } : m)).filter((m) => m.id);

export interface AgentsState {
  status: "loading" | "ok" | "fallback";
  list: OAgentInfo[];
  error?: string;
}
let agentsSnap: AgentsState = { status: "loading", list: STATIC_AGENTS };
let agentsStarted = false;
const agentListeners = new Set<() => void>();
function setAgents(n: AgentsState) {
  agentsSnap = n;
  agentListeners.forEach((l) => l());
  emit();
}
export function loadAgents(force = false) {
  if (agentsStarted && !force) return;
  agentsStarted = true;
  if (mockWanted()) {
    void (import.meta.env.DEV ? import("./orchMock") : Promise.reject()).then((m) => setAgents({ status: "ok", list: m.MOCK_AGENTS }));
    return;
  }
  orchApi
    .agents()
    .then((list) => setAgents(list.length ? { status: "ok", list } : { status: "fallback", list: STATIC_AGENTS }))
    .catch((e) => setAgents({ status: "fallback", list: STATIC_AGENTS, error: isMissing(e) ? undefined : (e as Error).message }));
}
/** The agent registry, fetched once per page load; falls back to Codex and Claude on 404. */
export function useAgents(): AgentsState {
  useEffect(() => loadAgents(), []);
  return useSyncExternalStore(
    (l) => {
      agentListeners.add(l);
      return () => {
        agentListeners.delete(l);
      };
    },
    () => agentsSnap,
    () => agentsSnap,
  );
}

const gitBase = (id: string) => `/api/orch/threads/${enc(id)}`;
export const realGitApi: GitApi = {
  git: (id) => req<OGit>("GET", `${gitBase(id)}/git`),
  diff: (id, q) => {
    const p = new URLSearchParams();
    if (q.from) p.set("from", q.from);
    if (q.to) p.set("to", q.to);
    if (q.path) p.set("path", q.path);
    if (q.run != null) p.set("run", String(q.run));
    const qs = p.toString();
    return req<ODiff>("GET", `${gitBase(id)}/diff${qs ? `?${qs}` : ""}`);
  },
  cwdGit: (cwd) => req<OGit>("GET", `/api/orch/git?cwd=${enc(cwd)}`),
  cwdDiff: (cwd, q) => {
    const p = new URLSearchParams({ cwd, from: q.from || "head", to: q.to || "worktree" });
    if (q.path) p.set("path", q.path);
    return req<ODiff>("GET", `/api/orch/git/diff?${p}`);
  },
  restore: (id, ref) => req("POST", `${gitBase(id)}/restore`, { ref }),
  commit: (id, message, paths) => req("POST", `${gitBase(id)}/commit`, paths?.length ? { message, paths } : { message }),
  push: (id) => req("POST", `${gitBase(id)}/push`),
  pr: (id, b) => req("POST", `${gitBase(id)}/pr`, b),
  open: (id, target) => req("POST", `${gitBase(id)}/open`, { target }),
  removeWorktree: (id, force) => req("POST", `${gitBase(id)}/worktree/remove`, force ? { force: true } : {}),
  revertHunk: (b) => req("POST", "/api/orch/git/revert-hunk", b),
};
export let mockGitApi: GitApi | null = null;
export function setMockGitApi(g: GitApi) {
  mockGitApi = g;
}
/** The live git API, or the dev mock under ?mock=1 (never posts to the daemon then). */
const notReady = () => Promise.reject(new ApiError(503, "sample data is still loading"));
const mockPending: GitApi = { git: notReady, diff: notReady, cwdGit: notReady, cwdDiff: notReady, restore: notReady, commit: notReady, push: notReady, pr: notReady, open: notReady, removeWorktree: notReady, revertHunk: notReady };
export const gitApi = (): GitApi => (s.mock ? mockGitApi || mockPending : realGitApi);

// ---- store ----

export type OrchStatus = "loading" | "ok" | "missing" | "error";
export interface ODetail {
  thread: OThread;
  children?: OThread[];
  runs: ORun[];
  items: OItem[];
  requests: ORequest[];
  seq: number;
  status: "ok" | "missing" | "error";
  error?: string;
}
export interface OrchState {
  status: OrchStatus;
  error: string;
  threads: Map<string, OThread>;
  details: Map<string, ODetail>;
  detailStatus: Map<string, "loading" | "notfound" | "error">;
  seq: number;
  connected: boolean;
  mock: boolean;
  version: number;
  blocked: Map<string, SpendBlock>; // threads whose last send hit a spend limit
  costs: Map<string, number>; // USD spent per thread, from GET /api/orch/costs
}

const s: OrchState = {
  status: "loading",
  error: "",
  threads: new Map(),
  details: new Map(),
  detailStatus: new Map(),
  seq: 0,
  connected: false,
  mock: false,
  version: 0,
  blocked: new Map(),
  costs: new Map(),
};
let snap: OrchState = { ...s };
const listeners = new Set<() => void>();
function emit() {
  s.version++;
  snap = { ...s };
  listeners.forEach((l) => l());
}
const subscribe = (l: () => void) => {
  listeners.add(l);
  return () => {
    listeners.delete(l);
  };
};

function upsert<T extends { id: string }>(arr: T[], v: T): T[] {
  const i = arr.findIndex((x) => x.id === v.id);
  if (i < 0) return [...arr, v];
  const out = arr.slice();
  out[i] = v;
  return out;
}

/** Applies one event to the store. Exported so the dev mock can drive the same path. */
const recent: OEvent[] = [];
export function applyEvent(e: OEvent) {
  recent.push(e);
  if (recent.length > 800) recent.splice(0, recent.length - 800);
  applyOne(e);
}
/** queue.ts registers here, so queue events need no import cycle. */
export const queueSink: { fn?: (threadId: string, items: never[]) => void } = {};
function applyOne(e: OEvent) {
  if (e.seq > s.seq) s.seq = e.seq;
  const d = e.data || {};
  if (e.type === "queue.updated") {
    queueSink.fn?.(e.threadId, (d.items || []) as never[]);
    return;
  }
  if (e.type === "thread.updated" && d.thread) {
    s.threads = new Map(s.threads).set(d.thread.id, d.thread);
  }
  if (e.type === "run.ended") loadCostsSoon();
  const det = s.details.get(e.threadId);
  if (det && e.seq > det.seq) {
    const n: ODetail = { ...det, seq: e.seq };
    switch (e.type) {
      case "thread.updated":
        if (d.thread) n.thread = d.thread;
        break;
      case "run.started":
      case "run.ended":
        if (d.run) n.runs = upsert(n.runs, d.run);
        break;
      case "item.started":
      case "item.completed":
        if (d.item) {
          // A completed item carries the full text; a re-sent started item must not wipe streamed text.
          const was = n.items.find((x) => x.id === d.item!.id);
          const item = e.type === "item.started" && was && !d.item.text ? { ...d.item, text: was.text } : d.item;
          n.items = upsert(n.items, capItem(item));
        }
        break;
      case "item.delta":
        if (d.itemId && d.text) {
          const out = d.field === "output";
          n.items = n.items.map((x) => (x.id !== d.itemId ? x : out ? { ...x, output: ((x.output || "") + d.text).slice(-8192) } : capItem({ ...x, text: (x.text || "") + d.text })));
        }
        break;
      case "request.opened":
      case "request.closed":
        if (d.request) n.requests = upsert(n.requests, d.request);
        break;
    }
    s.details = new Map(s.details).set(e.threadId, n);
  }
  if (e.type === "thread.updated" && d.thread?.status === "closed") prune();
  emit();
}

// Text kept in memory per item; the daemon keeps the full text.
const MAX_ITEM_TEXT = 256 * 1024;
const CUT_NOTE = "\n\n[agent-monitor: this view keeps the first 256 KB of this message]";
function capItem(x: OItem): OItem {
  if (!x.text || x.text.length <= MAX_ITEM_TEXT + CUT_NOTE.length) return x;
  return { ...x, text: x.text.slice(0, MAX_ITEM_TEXT) + CUT_NOTE };
}

// Threads on screen; details of a closed thread that nobody views are dropped and fetched again on open.
const viewing = new Map<string, number>();
function prune() {
  if (s.mock) return;
  let n: Map<string, ODetail> | null = null;
  for (const [id, d] of s.details) {
    if (viewing.has(id) || (d.thread.status !== "closed" && s.threads.get(id)?.status !== "closed")) continue;
    n = n || new Map(s.details);
    n.delete(id);
  }
  if (n) s.details = n;
}
function view(id: string) {
  viewing.set(id, (viewing.get(id) || 0) + 1);
  return () => {
    const c = (viewing.get(id) || 1) - 1;
    if (c) viewing.set(id, c);
    else viewing.delete(id);
    prune();
    emit();
  };
}

let es: EventSource | null = null;
let retry = 0;
let retryTimer: number | undefined;
function openStream() {
  if (s.mock || typeof EventSource === "undefined") return;
  es?.close();
  const src = new EventSource(orchApi.streamUrl(s.seq));
  es = src;
  src.onopen = () => {
    retry = 0;
    s.connected = true;
    emit();
  };
  src.onmessage = (m) => {
    try {
      applyEvent(JSON.parse(m.data) as OEvent);
    } catch {
      /* heartbeat or malformed line */
    }
  };
  src.onerror = () => {
    // Reconnect ourselves so the replay starts after the last seq we applied.
    src.close();
    if (es === src) es = null;
    s.connected = false;
    emit();
    window.clearTimeout(retryTimer);
    retryTimer = window.setTimeout(openStream, Math.min(15_000, 1000 * 2 ** retry++));
  };
}

let costTimer = 0;
// Run ends come in bursts (a fan-out); one fetch after the last of them.
function loadCostsSoon() {
  window.clearTimeout(costTimer);
  costTimer = window.setTimeout(loadCosts, 2000);
}
async function loadCosts() {
  if (s.mock) return;
  try {
    s.costs = new Map(Object.entries((await orchApi.costs()).costs || {}));
    emit();
  } catch {
    // an older daemon has no /costs route: rows just show no spend
  }
}

export async function loadOrchThreads() {
  if (s.mock) return;
  void loadCosts();
  try {
    const r = await orchApi.threads();
    s.threads = new Map((r.threads || []).map((t) => [t.id, t]));
    if (!es) s.seq = Math.max(s.seq, r.seq || 0);
    s.status = "ok";
    s.error = "";
    emit();
    if (!es) openStream();
  } catch (e) {
    s.status = isMissing(e) ? "missing" : "error";
    s.error = (e as Error).message;
    emit();
    if (s.status === "error") {
      window.clearTimeout(retryTimer);
      retryTimer = window.setTimeout(loadOrchThreads, 15_000);
    }
  }
}

export async function loadOrchThread(id: string) {
  if (s.mock) return;
  if (!s.details.has(id)) {
    s.detailStatus = new Map(s.detailStatus).set(id, "loading");
    emit();
  }
  try {
    const r = await orchApi.thread(id);
    s.details = new Map(s.details).set(id, { thread: r.thread, children: r.children, runs: r.runs || [], items: (r.items || []).map(capItem), requests: r.requests || [], seq: r.seq || 0, status: "ok" });
    s.threads = new Map(s.threads).set(id, r.thread);
    // Children may be missing from the list snapshot; never overwrite a fresher copy from the stream.
    for (const c of r.children || []) {
      const was = s.threads.get(c.id);
      if (!was || toMs(was.updatedAt) <= toMs(c.updatedAt)) s.threads.set(c.id, c);
    }
    const ds = new Map(s.detailStatus);
    ds.delete(id);
    s.detailStatus = ds;
    // Replay events that arrived while the snapshot was in flight.
    for (const e of recent.slice()) if (e.threadId === id && e.seq > (r.seq || 0)) applyOne(e);
  } catch (e) {
    s.detailStatus = new Map(s.detailStatus).set(id, isMissing(e) ? "notfound" : "error");
    s.error = (e as Error).message;
  }
  emit();
}

/** Adds a thread returned by POST /threads so the list shows it before the stream does. */
export function rememberThread(t: OThread) {
  s.threads = new Map(s.threads).set(t.id, t);
  const det = s.details.get(t.id);
  if (det) s.details = new Map(s.details).set(t.id, { ...det, thread: t });
  emit();
}

/** Mock mode for local layout checks: `?mock=1` once, kept for the tab session. Dev builds only. */
export function mockWanted(): boolean {
  if (!import.meta.env.DEV) return false;
  try {
    const q = new URLSearchParams(window.location.search).get("mock");
    if (q === "1") sessionStorage.setItem("am-orch-mock", "1");
    if (q === "0") sessionStorage.removeItem("am-orch-mock");
    return sessionStorage.getItem("am-orch-mock") === "1";
  } catch {
    return new URLSearchParams(window.location.search).get("mock") === "1";
  }
}

export interface MockHooks {
  send: (id: string, text: string, attachments?: string[]) => void | Promise<void>;
  close?: (id: string) => void;
  allowOnce?: (id: string) => void;
  continueParent?: (id: string) => OThread | undefined;
  interrupt: (id: string, cascade?: boolean) => void;
  decide: (approvalId: string, allow: boolean, body?: DecideBody) => void;
}
export let mockHooks: MockHooks | null = null;

/** Replaces the whole store, used by the dev mock only. */
export function seedStore(threads: OThread[], details: ODetail[], hooks: MockHooks) {
  s.mock = true;
  s.status = "ok";
  s.connected = true;
  s.threads = new Map(threads.map((t) => [t.id, t]));
  s.details = new Map(details.map((d) => [d.thread.id, d]));
  s.seq = Math.max(0, ...details.map((d) => d.seq));
  s.costs = new Map(details.map((d) => [d.thread.id, d.runs.reduce((n, r) => n + (r.usage?.costUsd || 0), 0)]));
  mockHooks = hooks;
  emit();
}

let started = false;
export function startOrch() {
  if (started) return;
  started = true;
  if (mockWanted()) {
    s.mock = true;
    void (import.meta.env.DEV ? import("./orchMock") : Promise.reject()).then((m) => m.installMock());
    return;
  }
  void loadOrchThreads();
}

export function useOrch(): OrchState {
  useEffect(startOrch, []);
  return useSyncExternalStore(subscribe, () => snap, () => snap);
}

/** Loads the snapshot of one thread when it is opened; events then keep it current. */
export function useOrchThread(id: string | undefined): { detail?: ODetail; status: "loading" | "ok" | "missing" | "notfound" | "error" | "none" } {
  const st = useOrch();
  useEffect(() => {
    if (id && !st.mock && st.status === "ok") void loadOrchThread(id);
  }, [id, st.mock, st.status]);
  useEffect(() => (id ? view(id) : undefined), [id]);
  if (!id) return { status: "none" };
  const detail = st.details.get(id);
  if (detail) return { detail, status: "ok" };
  if (st.status === "missing") return { status: "missing" };
  return { status: st.detailStatus.get(id) || "loading" };
}

// ---- actions (mock-aware) ----

/** Sends a message. A 402 marks the thread blocked by a spend limit, then rethrows. */
export async function sendMessage(id: string, text: string, attachments?: string[]) {
  try {
    if (s.mock && mockHooks) await mockHooks.send(id, text, attachments);
    else await orchApi.message(id, text, attachments);
    if (s.blocked.has(id)) setBlocked(id, null);
  } catch (e) {
    const b = spendBlockOf(e);
    if (b) setBlocked(id, b);
    throw e;
  }
}
export function setBlocked(id: string, b: SpendBlock | null) {
  const n = new Map(s.blocked);
  if (b) n.set(id, b);
  else n.delete(id);
  s.blocked = n;
  emit();
}
/** "Allow this thread once more": lifts the spend block for one more turn. */
export async function allowOnce(id: string) {
  if (s.mock) mockHooks?.allowOnce?.(id);
  else {
    const r = await orchApi.allowOnce(id);
    if (r?.thread?.id === id) rememberThread(r.thread);
  }
  setBlocked(id, null);
}
/** Moves a thread whose worktree was removed back to the parent checkout. */
export async function continueInParent(id: string) {
  if (s.mock) {
    const t = mockHooks?.continueParent?.(id);
    if (t) rememberThread(t);
    return;
  }
  const r = await orchApi.continueParent(id);
  if (r?.thread?.id === id) rememberThread(r.thread);
  else void loadOrchThread(id);
}
export async function interruptThread(id: string, cascade?: boolean) {
  if (s.mock && mockHooks) return mockHooks.interrupt(id, cascade);
  await orchApi.interrupt(id, cascade);
}

// Patches the list copy and the detail copy at once; returns the previous thread for a revert.
function patchThread(id: string, patch: Partial<OThread>): OThread | undefined {
  const was = s.threads.get(id) || s.details.get(id)?.thread;
  if (!was) return undefined;
  const next = { ...was, ...patch };
  s.threads = new Map(s.threads).set(id, next);
  const det = s.details.get(id);
  if (det) s.details = new Map(s.details).set(id, { ...det, thread: { ...det.thread, ...patch } });
  emit();
  return was;
}
// Optimistic: the list moves at once, the server's thread.updated then sets the real group.
async function optimistic(id: string, patch: Partial<OThread>, call: () => Promise<{ thread?: OThread }>) {
  const was = patchThread(id, patch);
  if (s.mock) return;
  try {
    const r = await call();
    if (r?.thread?.id === id) rememberThread(r.thread);
  } catch (e) {
    if (was) patchThread(id, was);
    throw e;
  }
}
export function settleThread(id: string, settled: boolean) {
  return optimistic(id, settled ? { settledAt: Date.now(), group: "settled" } : { settledAt: undefined, snoozedUntil: undefined, group: undefined }, () => orchApi.settle(id, settled));
}
export function snoozeThread(id: string, until: number) {
  return optimistic(id, until ? { snoozedUntil: until, group: "settled" } : { snoozedUntil: undefined, group: undefined }, () => orchApi.snooze(id, until));
}
export function pinThread(id: string, pinned: boolean) {
  return optimistic(id, { pinned }, () => orchApi.pin(id, pinned));
}
export function renameThread(id: string, title: string) {
  return optimistic(id, { title }, () => orchApi.rename(id, title));
}
/** Closes a thread; the daemon also closes its descendants (cascade is the default). */
export async function closeThread(id: string) {
  if (s.mock) return mockHooks?.close?.(id);
  await orchApi.close(id);
}
export async function createThread(b: OCreateBody): Promise<OThread> {
  if (s.mock) {
    const bad = (b.children || []).findIndex((c) => /fail/i.test(c.title));
    if (bad >= 0) {
      const error = `subagent ${bad + 1} ("${b.children![bad].title}") did not start: git worktree add failed, the branch already exists`;
      throw new ApiError(422, error, { error });
    }
    const now = Date.now();
    const t: OThread = { id: `mock-${now}`, kind: "driven", agent: b.agent, title: b.prompt.split("\n")[0].slice(0, 80), cwd: b.cwd, repo: b.cwd.split("/").pop() || "", branch: "main", model: b.model || (b.agent === "claude" ? "" : "gpt-5.5"), sandbox: b.agent === "claude" ? "" : b.sandbox || "read-only", permissionMode: b.agent === "claude" ? b.permissionMode || "manual" : undefined, status: "starting", nativeId: "", createdAt: now, updatedAt: now, depth: 0 };
    if (b.workspace === "worktree") Object.assign(t, { branch: `am/new-${String(now).slice(-4)}`, worktree: { path: `~/.agent-monitor/wt/${t.id}`, branch: `am/new-${String(now).slice(-4)}` } });
    const kids: OThread[] = (b.children || []).map((c, i) => ({ ...t, id: `${t.id}-c${i + 1}`, agent: c.agent, title: c.title, model: c.model || "", sandbox: c.agent === "codex" ? c.sandbox || "workspace-write" : "", permissionMode: c.agent === "claude" ? c.permissionMode || "manual" : undefined, parentId: t.id, depth: 1, workspace: c.workspace, worktree: c.workspace === "worktree" ? { path: `~/.agent-monitor/wt/${t.id}-c${i + 1}`, branch: `am/child-${i + 1}` } : undefined }));
    if (kids.length) t.children = kids.map((k) => k.id);
    s.details = new Map(s.details).set(t.id, { thread: t, children: kids, runs: [], items: [{ id: `${t.id}-u`, threadId: t.id, runId: "", kind: "user", status: "completed", text: b.prompt, startedAt: now }], requests: [], seq: s.seq, status: "ok" });
    for (const k of kids) s.details.set(k.id, { thread: k, runs: [], items: [{ id: `${k.id}-u`, threadId: k.id, runId: "", kind: "user", status: "completed", text: b.children![kids.indexOf(k)].prompt, startedAt: now }], requests: [], seq: s.seq, status: "ok" });
    for (const k of kids) s.threads = new Map(s.threads).set(k.id, k);
    rememberThread(t);
    return t;
  }
  const r = await orchApi.create(b);
  rememberThread(r.thread);
  return r.thread;
}

/** Starts an agent in a new tmux session through `agent-monitor run`. */
export async function startTerminal(b: OTerminalBody): Promise<OTerminalStart> {
  if (s.mock) return (await (import.meta.env.DEV ? import("./orchMock") : Promise.reject(new Error("no mock in this build")))).mockTerminal(b);
  const r = await orchApi.terminal(b);
  return { session: r.session, hostId: r.hostId, attach: r.attach || `tmux attach -t ${r.session}` };
}

/** The host as the daemon knows it, or null while it has not registered (404). */
export async function hostInfo(hostId: string): Promise<OHostInfo | null> {
  if (s.mock) return (await (import.meta.env.DEV ? import("./orchMock") : Promise.reject(new Error("no mock in this build")))).mockHost(hostId);
  try {
    return await orchApi.host(hostId);
  } catch (e) {
    if (e instanceof ApiError && e.status === 404) return null;
    throw e;
  }
}

/** Picks an option of the prompt a starting host shows: "trust" or "cancel". */
export async function answerHost(hostId: string, option: string): Promise<void> {
  if (s.mock) return;
  await orchApi.answerHost(hostId, option);
}

/** Takes over a watched session that no live process owns. A 409 {error:"live"} becomes a plain message. */
/** null: a Code Mode session, which the daemon resumed in a terminal instead. */
export async function continueSession(sessionId: string, prompt?: string): Promise<OThread | null> {
  const p = prompt?.trim() || undefined;
  if (s.mock) {
    const now = Date.now();
    const agent: OAgent = sessionId.startsWith("codex:") ? "codex" : "claude";
    const t: OThread = { id: `mock-cont-${now}`, kind: "driven", agent, title: p ? p.split("\n")[0].slice(0, 80) : "Continued session", cwd: "/Users/dev/agent-monitor", repo: "agent-monitor", branch: "main", model: agent === "claude" ? "opus" : "gpt-5.5", sandbox: "workspace-write", permissionMode: agent === "claude" ? "manual" : undefined, status: p ? "running" : "idle", nativeId: sessionId.replace(/^[a-z-]+:/, ""), createdAt: now, updatedAt: now };
    const items: OItem[] = p ? [{ id: `${t.id}-u`, threadId: t.id, runId: `${t.id}-r1`, kind: "user", status: "completed", text: p, startedAt: now }] : [];
    s.details = new Map(s.details).set(t.id, { thread: t, runs: [], items, requests: [], seq: s.seq, status: "ok" });
    rememberThread(t);
    return t;
  }
  try {
    const r = await orchApi.continue(sessionId, p);
    if (!r.thread) return null;
    rememberThread(r.thread);
    return r.thread;
  } catch (e) {
    if (e instanceof ApiError && e.status === 409) throw new ApiError(409, "The terminal still owns this session. Exit it there first, then try again.", e.body);
    throw e;
  }
}

/** The current copy of one thread, for the dev mock. */
export const orchThread = (id: string): OThread | undefined => s.threads.get(id) || s.details.get(id)?.thread;
export const orchSeq = () => s.seq;
