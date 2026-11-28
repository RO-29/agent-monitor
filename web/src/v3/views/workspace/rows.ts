import { agentKey } from "../../components/AgentIcon";
// Merges driven (orch) threads and watched (v3) threads into one list for the Workspace.
import { projectName } from "../../../lib/format";
import { toMs, type OGroup, type OPermissionMode, type OThread, type OWorkspace, type OrchState } from "../../api/orch";
import type { Approval, V3Thread } from "../../api/types";
import type { Session } from "../../../api/types";
import { handoverOf, serverDrivenRow, terminalTarget } from "./handover";
import type { Row } from "../../model";
import { peerKey, type PeerThread } from "../../api/fleet";

export type WGroup = OGroup;
export interface WRow {
  id: string;
  mode: "driven" | "watched";
  group: WGroup;
  title: string;
  agent: string;
  repo: string;
  branch: string;
  status: string;
  at: number;
  pinned?: boolean;
  snoozedUntil?: number;
  parentId?: string;
  created?: number;
  driven?: OThread;
  watched?: Row;
  // A driven row whose session a terminal took over: a short note, and the row opens openId.
  note?: string;
  openId?: string;
  // A thread on another machine: its name, this machine's grant there, and the peer's thread.
  machine?: string;
  level?: string;
  peer?: PeerThread;
}
export const GROUPS: { key: WGroup; label: string; none: string }[] = [
  { key: "need", label: "Needs you", none: "Nothing needs you" },
  { key: "working", label: "Working", none: "Nothing running" },
  { key: "ready", label: "Ready", none: "Nothing to review" },
  { key: "settled", label: "Done", none: "Nothing done yet" },
];

const DAY = 24 * 3600_000;
type Own = "need" | "working" | null;
function ownState(t: OThread, open: number, needIds: Set<string>): Own {
  if (open > 0 || needIds.has(t.id) || t.status === "waiting" || t.status === "error") return "need";
  if (t.status === "running" || t.status === "starting") return "working";
  return null;
}

/** Latest copy of each driven thread: the detail snapshot wins when the list copy is older. */
export function drivenThreads(orch: OrchState): Map<string, OThread> {
  const m = new Map<string, OThread>();
  for (const t of orch.threads.values()) {
    const d = orch.details.get(t.id)?.thread;
    m.set(t.id, d && toMs(d.updatedAt) > toMs(t.updatedAt) ? d : t);
  }
  return m;
}

export function childMap(threads: Map<string, OThread>): Map<string, OThread[]> {
  const m = new Map<string, OThread[]>();
  const add = (pid: string, c: OThread) => {
    const arr = m.get(pid) || [];
    if (!arr.some((x) => x.id === c.id)) arr.push(c);
    m.set(pid, arr);
  };
  for (const t of threads.values()) {
    if (t.parentId && t.parentId !== t.id) add(t.parentId, t);
    for (const cid of t.children || []) {
      const c = threads.get(cid);
      if (c && cid !== t.id) add(t.id, c);
    }
  }
  for (const arr of m.values()) arr.sort((a, b) => toMs(a.createdAt) - toMs(b.createdAt));
  return m;
}

/** All descendants of id, depth first, guarded against cycles. */
export function descendants(id: string, kids: Map<string, OThread[]>): OThread[] {
  const out: OThread[] = [];
  const seen = new Set([id]);
  const walk = (pid: string) => {
    for (const c of kids.get(pid) || []) {
      if (seen.has(c.id)) continue;
      seen.add(c.id);
      out.push(c);
      walk(c.id);
    }
  };
  walk(id);
  return out;
}

export const snoozed = (t: OThread, now: number) => !!t.snoozedUntil && toMs(t.snoozedUntil) > now;

/** Uses the server's group; derives it the phase 3 way when an older daemon does not send one. */
export function groupOf(t: OThread, own: Own, desc: Own[], now: number): WGroup {
  if (snoozed(t, now)) return "settled";
  if (t.group) return t.group;
  if (own === "need" || desc.includes("need")) return "need";
  if (own === "working" || desc.includes("working")) return "working";
  if (t.settledAt || t.status === "closed" || now - toMs(t.updatedAt) > DAY) return "settled";
  return "ready";
}

export function drivenLabel(t: OThread, openRequests = 0, kidState?: Own, now = Date.now()): string {
  if (openRequests > 0 || t.status === "waiting") return "Needs you";
  if (t.status === "running") return "Working";
  if (t.status === "starting") return "Starting";
  if (t.status === "error") return "Error";
  if (kidState === "need") return "Subagent needs you";
  if (kidState === "working") return "Subagents running";
  if (snoozed(t, now)) return "Snoozed";
  if (t.status === "closed") return "Closed";
  return t.settledAt ? "Done" : "Ready";
}

const h0 = (t: OThread, sessions?: Map<string, Session>) => (t.nativeId ? sessions?.get(`${t.agent}:${t.nativeId}`) : undefined);

function isDrivenCopy(t: V3Thread, natives: Set<string>): boolean {
  if (t.drivenId) return true;
  if (!natives.size) return false;
  return t.sessions.some((sid) => natives.has(sid) || natives.has(sid.replace(/^[a-z-]+:/, "")));
}

export function buildWRows(orch: OrchState, watched: Row[], needIds: Set<string>, now = Date.now(), settledWatchedMax = Infinity, sessions?: Map<string, Session>): WRow[] {
  const out: WRow[] = [];
  const serverThreads = watched.flatMap((r) => (r.thread ? [r.thread] : []));
  const natives = new Set<string>();
  const threads = drivenThreads(orch);
  const kids = childMap(threads);
  const own = new Map<string, Own>();
  for (const t of threads.values()) {
    const det = orch.details.get(t.id);
    const open = det ? det.requests.filter((r) => r.state === "open").length : 0;
    own.set(t.id, ownState(t, open, needIds));
  }
  for (const t of threads.values()) {
    const det = orch.details.get(t.id);
    const h = handoverOf(t, serverDrivenRow(serverThreads, t.id), h0(t, sessions), det?.runs);
    if (t.nativeId && !h.terminal) natives.add(t.nativeId);
    const open = det ? det.requests.filter((r) => r.state === "open").length : 0;
    const desc = descendants(t.id, kids).map((d) => own.get(d.id) ?? null);
    const kidState: Own = desc.includes("need") ? "need" : desc.includes("working") ? "working" : null;
    out.push({
      id: t.id,
      mode: "driven",
      group: groupOf(t, own.get(t.id) ?? null, desc, now),
      title: t.title || projectName(t.cwd),
      agent: t.agent,
      repo: t.repo || projectName(t.cwd),
      branch: t.worktree?.branch || t.branch,
      status: ((l) => (l === "Ready" && t.parentId ? "Done" : l))(drivenLabel(t, open + (needIds.has(t.id) ? 1 : 0), kidState, now)),
      at: toMs(t.updatedAt),
      pinned: !!t.pinned,
      snoozedUntil: snoozed(t, now) ? toMs(t.snoozedUntil) : undefined,
      parentId: t.parentId && threads.has(t.parentId) ? t.parentId : undefined,
      created: toMs(t.createdAt),
      driven: t,
      ...(h.terminal ? { note: "Continued in a terminal", openId: terminalTarget(h.sid, serverThreads) } : {}),
    });
  }
  let settled = 0;
  for (const r of [...watched].sort((a, b) => b.lastAt - a.lastAt)) {
    if (r.thread && (r.thread.kind === "driven" || isDrivenCopy(r.thread, natives))) continue;
    if (!r.thread && r.approvals.length && r.approvals.every((a) => a.drivenId)) continue;
    const group: WGroup = r.bucket === "needs-you" ? "need" : r.bucket === "running" ? "working" : r.bucket === "idle-today" ? "ready" : "settled";
    const mk = r.thread?.mark;
    const snoozeAt = mk?.snoozedUntil && mk.snoozedUntil > now ? mk.snoozedUntil : undefined;
    // A finished thread with no prompt and no outcome is noise (a CLI opened and closed); Cursor does not count turns.
    if (group === "settled" && r.thread && !r.thread.turns && !r.thread.outcomeLine && agentKey(r.id) !== "cursor") continue;
    if (group === "settled" && ++settled > settledWatchedMax) continue;
    out.push({
      id: r.id,
      mode: "watched",
      group,
      title: r.title,
      agent: agentKey(r.id) || r.agent,
      repo: r.thread?.repo || projectName(r.cwd),
      branch: r.thread?.ref || "",
      status: group === "need" ? "Needs you" : group === "working" ? "Working" : r.bucket === "idle-today" ? "Ready" : "Done",
      at: r.lastAt,
      pinned: !!mk?.pinned,
      snoozedUntil: group === "settled" ? snoozeAt : undefined,
      watched: r,
    });
  }
  return out.sort((a, b) => b.at - a.at);
}

/** Rows for threads on other machines, grouped with the same rules as this machine's watched rows. */
export function peerRows(threads: PeerThread[], now = Date.now()): WRow[] {
  return threads.map((t) => {
    const today = now - t.lastAt < DAY;
    const group: WGroup = t.attention || t.state === "awaiting-permission" || t.state === "awaiting-input" ? "need" : t.state === "running" ? "working" : today ? "ready" : "settled";
    return {
      id: peerKey(t.machine, t.id),
      mode: "watched" as const,
      group,
      title: t.display || t.title || projectName(t.cwd),
      agent: agentKey(t.sessions?.[t.sessions.length - 1] || t.id) || t.tools?.[0] || "",
      repo: t.repo || projectName(t.cwd),
      branch: t.ref || "",
      status: group === "need" ? "Needs you" : group === "working" ? "Working" : today ? "Ready" : "Done",
      at: t.lastAt,
      machine: t.machine,
      level: t.level,
      peer: t,
    };
  });
}

export interface WTree {
  top: Map<WGroup, WRow[]>;
  kids: Map<string, WRow[]>;
}
/** Top rows per group (pinned first, then newest) and child rows per parent (oldest first, as delegated). */
export function treeOf(rows: WRow[]): WTree {
  const kids = new Map<string, WRow[]>();
  const top = new Map<WGroup, WRow[]>(GROUPS.map((g) => [g.key, []]));
  for (const r of rows) {
    if (r.parentId) kids.set(r.parentId, [...(kids.get(r.parentId) || []), r]);
    else top.get(r.group)!.push(r);
  }
  for (const arr of top.values()) arr.sort((a, b) => Number(!!b.pinned) - Number(!!a.pinned) || b.at - a.at);
  for (const arr of kids.values()) arr.sort((a, b) => (a.created || a.at) - (b.created || b.at));
  return { top, kids };
}
/** The list order for j/k: groups, then each parent followed by its subtree. */
export function visualOrder(tree: WTree): WRow[] {
  const out: WRow[] = [];
  const seen = new Set<string>();
  const walk = (r: WRow) => {
    if (seen.has(r.id)) return;
    seen.add(r.id);
    out.push(r);
    for (const k of tree.kids.get(r.id) || []) walk(k);
  };
  for (const g of GROUPS) for (const r of tree.top.get(g.key) || []) walk(r);
  return out;
}

export const workspaceOf = (t: OThread): OWorkspace => t.workspace || (t.worktree ? "worktree" : t.sandbox === "read-only" ? "read-only" : "shared");
export const WORKSPACE_LABEL: Record<OWorkspace, string> = { shared: "shared", worktree: "own worktree", "read-only": "read-only" };

export const PERM_MODES: { key: OPermissionMode; label: string; hint: string }[] = [
  { key: "manual", label: "Ask each time", hint: "Each edit and command waits for your answer here." },
  { key: "acceptEdits", label: "Accept edits", hint: "File edits go through. Commands still wait for you." },
  { key: "plan", label: "Plan first", hint: "Claude reads and plans. You approve the plan before it edits." },
  { key: "auto", label: "Auto", hint: "Claude asks only for actions it judges risky." },
];
export const permModeLabel = (m?: string) => PERM_MODES.find((p) => p.key === m)?.label || m || "";

// Leading `export PATH=...;` and `cd dir &&` steps (or lines) hide the command that matters in a one-line label.
const CMD_NOISE = /^\s*(?:export\s+PATH=(?:"[^"]*"|'[^']*'|[^\s;&]+)|cd\s+(?:"[^"]*"|'[^']*'|[^\s;&]+))[ \t]*(?:;|&&|\n)\s*/;
export function cmdLabel(cmd: string): string {
  let s = cmd;
  for (let m = CMD_NOISE.exec(s); m && m[0].length < s.length; m = CMD_NOISE.exec(s)) s = s.slice(m[0].length);
  return s;
}
