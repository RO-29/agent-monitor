// Derived lists shared by the rail, inbox, board and menubar.
import type { PermissionRequest, Session, Span } from "../api/types";
import { firstRealLine, projectName } from "../lib/format";
import type { LiveState } from "../lib/ws";
import type { Approval, Shipped, Stack, V3Thread } from "./api/types";
import { needThreadIds, type NeedsYouState } from "./api/needsYou";
import { agentName, approvalsByThread, type V3State } from "./live";

export type ListKey = "needs-you" | "running" | "idle-today" | "done-today" | "all";
export const LISTS: { key: ListKey; label: string; dot: string }[] = [
  { key: "needs-you", label: "Needs you", dot: "d-need" },
  { key: "running", label: "Working", dot: "d-run" },
  { key: "idle-today", label: "Ready", dot: "d-idle" },
  { key: "done-today", label: "Done", dot: "d-ok" },
];

export interface Row {
  id: string; // thread id
  thread?: V3Thread;
  title: string;
  agent: string;
  cwd: string;
  session?: Session; // newest live session
  approvals: Approval[];
  perms: PermissionRequest[];
  needs: boolean;
  bucket: Exclude<ListKey, "all"> | "older";
  lastAt: number;
  sinceAt: number; // when it started waiting, for "oldest first"
}

export const startOfToday = () => {
  const d = new Date();
  d.setHours(0, 0, 0, 0);
  return d.getTime();
};

export const threadTitle = (t: V3Thread, firstMsg?: string) => displayOf(t, firstMsg);

/** Rows for every thread; "needs you" comes from GET /api/needs-you once it has loaded. */
export function buildRows(st: V3State, live: LiveState, ny?: NeedsYouState): Row[] {
  const byThread = approvalsByThread(st);
  const need = ny && ny.status !== "loading" ? needThreadIds(ny.items, st.threadOf, st.drivenRowOf) : null;
  const permsBy = new Map<string, PermissionRequest[]>();
  for (const p of live.perms.values()) {
    const sid = p.sessionId ? (p.sessionId.includes(":") ? p.sessionId : `claude:${p.sessionId}`) : "";
    const tid = st.threadOf[sid] || sid || "?";
    permsBy.set(tid, [...(permsBy.get(tid) || []), p]);
  }
  const today = startOfToday();
  const seen = new Set<string>();
  const rows: Row[] = [];
  const mk = (t: V3Thread | undefined, id: string, appr: Approval[]): Row => {
    const sid = t ? t.sessions[t.sessions.length - 1] : appr[0]?.sessionId;
    const session = sid ? live.sessions.get(sid) : undefined;
    const state = session?.state || t?.state || "idle";
    // The daemon decides the phase; the state rules below only cover an older daemon.
    const phase = session?.phase || t?.phase;
    const perms = permsBy.get(id) || [];
    const needs = (need ? need.has(id) : appr.length > 0 || perms.length > 0) || (phase ? phase === "needs-you" : state === "awaiting-permission");
    const lastAt = Math.max(t?.lastAt || 0, session?.lastActivityAt || 0, ...appr.map((a) => a.createdAt));
    let bucket: Row["bucket"] = "older";
    if (needs) bucket = "needs-you";
    else if (phase ? phase === "working" : state === "running") bucket = "running";
    else if (phase ? phase === "ready" : lastAt >= today && (state === "idle" || state === "awaiting-input")) bucket = "idle-today";
    else if (lastAt >= today) bucket = "done-today";
    const sinceAt = appr.length ? appr[0].createdAt : perms.length ? perms[0].createdAt : lastAt;
    return {
      id,
      thread: t,
      title: t ? displayOf(t, firstMsgOf(t, live)) : appr[0] ? `${projectName(appr[0].cwd)} · ${appr[0].toolName}` : id,
      agent: t?.tools?.[0] || appr[0]?.agent || session?.tool || "claude",
      cwd: t?.cwd || appr[0]?.cwd || session?.cwd || "",
      session,
      approvals: appr,
      perms,
      needs,
      bucket,
      lastAt,
      sinceAt,
    };
  };
  for (const t of st.threads) {
    seen.add(t.id);
    rows.push(mk(t, t.id, byThread.get(t.id) || []));
  }
  // approvals whose thread is hidden (a child run) or not indexed yet still reach the queue
  if (st.threadsStatus !== "loading") for (const [tid, appr] of byThread) if (!seen.has(tid)) (seen.add(tid), rows.push(mk(undefined, tid, appr)));
  if (need && ny && st.threadsStatus !== "loading")
    for (const i of ny.items) {
      const tid = [...needThreadIds([i], st.threadOf, st.drivenRowOf)].pop();
      if (!tid || seen.has(tid) || (i.approval?.drivenId && st.drivenRowOf[i.approval.drivenId])) continue;
      seen.add(tid);
      rows.push({ ...mk(undefined, tid, []), title: i.title || projectName(i.cwd || "") || tid, agent: i.agent || "claude", cwd: i.cwd || "", sinceAt: i.at });
    }
  return rows;
}

export function listRows(rows: Row[], key: string): Row[] {
  let out: Row[];
  if (key.startsWith("project:")) {
    const p = key.slice(8);
    out = rows.filter((r) => projectName(r.cwd) === p);
  } else if (key === "all") out = rows;
  else out = rows.filter((r) => r.bucket === key);
  if (key === "needs-you") return out.sort((a, b) => a.sinceAt - b.sinceAt);
  return out.sort((a, b) => b.lastAt - a.lastAt);
}

export function counts(rows: Row[]) {
  const c: Record<string, number> = { "needs-you": 0, running: 0, "idle-today": 0, "done-today": 0 };
  for (const r of rows) if (r.bucket in c) c[r.bucket]++;
  return c;
}


const GENERIC_RE = /is waiting for your (input|reply)|needs your (attention|permission)|^prompt_input_exit$/i;

/** One mono line: what the agent does or wants right now. */
export function activity(r: Row): string {
  const a = r.approvals[0];
  if (a) {
    if (a.kind === "question") return `asks: ${a.questions?.[0]?.question || a.summary}`;
    if (a.kind === "plan") return `plan ready · ${a.summary}`;
    return `wants ${a.toolName} · ${a.summary}`;
  }
  if (r.perms[0]) return `wants ${r.perms[0].toolName}`;
  const s = r.session;
  if (s?.state === "awaiting-permission") return `${agentName(r.agent)} asks: ${firstRealLine(s.permissionMessage || s.lastMessage) || "permission"}`;
  const lm = s?.lastMessage && !GENERIC_RE.test(s.lastMessage) ? firstRealLine(s.lastMessage) : "";
  if (s?.state === "awaiting-input") return (r.thread && outcomeOf(r.thread)) || lm || "ready";
  if (lm) return lm;
  return r.thread ? outcomeOf(r.thread) : "";
}

// ---- v4 Desk helpers: each prefers the backend field and falls back to a client-side guess.

export const repoOf = (t: { repo?: string; cwd?: string }) => t.repo || projectName(t.cwd);

/** First prompt of the newest live session that has one. */
export function firstMsgOf(t: V3Thread, live: LiveState): string | undefined {
  for (const sid of t.sessions || []) {
    const m = live.sessions.get(sid)?.firstMessage;
    if (m && firstRealLine(m)) return m;
  }
  return undefined;
}

const JUNK_RE = /^(\*\*|\[|#|\||\/|~\/|<|`|\{)/;
const SLUG_RE = /^[a-z0-9]+(?:[-_][a-z0-9]+)+$/;
const cap1 = (s: string) => (s ? s[0].toUpperCase() + s.slice(1) : s);

function clip(s: string, n: number): string {
  if (s.length <= n) return s;
  const cut = s.slice(0, n);
  const sp = cut.lastIndexOf(" ");
  return (sp > n * 0.5 ? cut.slice(0, sp) : cut).replace(/[\s,;:.\-]+$/, "") + "…";
}

/** Strips Markdown to plain prose: tables, emphasis, code ticks, links to their text. */
export function stripMd(text: string): string {
  return text
    .split("\n")
    .filter((l) => !/^\s*\|/.test(l) && !/^\s*```/.test(l) && !/^\s*[-*_]{3,}\s*$/.test(l))
    .map((l) => l.replace(/^\s*#+\s*/, "").replace(/^\s*(?:[-*+]|\d+[.)])\s+/, "").replace(/^\s*>\s*/, ""))
    .join(" ")
    .replace(/!?\[([^\]]*)\]\([^)]*\)/g, "$1")
    .replace(/(\*\*|__)(.+?)\1/g, "$2")
    .replace(/(^|\s)[*_]([^*_\s][^*_]*?)[*_](?=\s|$|[.,;:])/g, "$1$2")
    .replace(/`+([^`]*)`+/g, "$1")
    .replace(/\s+/g, " ")
    .trim();
}

function firstSentence(s: string): string {
  const m = /^(.{12,}?[.!?])(\s|$)/.exec(s);
  return m ? m[1] : s;
}

function cleanPrompt(raw: string): string {
  let s = stripMd(firstRealLine(raw.replace(/\[Image[^\]]*\]/g, "")) || "");
  s = s
    .replace(/\s*\((?:run\s+)?`?git [^)]*\)/gi, "")
    .replace(/^(please|hey|hi|ok|okay|so|now)[,\s]+/i, "")
    .replace(/^in this (repository|repo|directory|project),?\s*/i, "")
    .replace(/^you are reviewing\s+/i, "Review of ")
    .replace(/,\s*read-only(?=[.\s]|$)/i, "");
  s = firstSentence(s).replace(/[.!?]$/, "");
  s = s.replace(/^review (the|this|my|commit|today|latest)\b/i, (_m, w) => `Review of ${String(w).toLowerCase()}`).replace(/\s+in this (repository|repo)\b/i, "");
  return cap1(s.trim());
}

/** A real title: the session title when it is a name, else one derived from the first prompt or the latest outcome. */
export function displayOf(t: V3Thread, firstMsg?: string): string {
  if (t.display) return t.display;
  const raw = (t.title || "").trim();
  const repo = repoOf(t);
  const line = firstRealLine(raw) || "";
  const junk = !line || line === repo || line === projectName(t.cwd) || JUNK_RE.test(raw) || /^\[image/i.test(raw);
  if (!junk) {
    if (SLUG_RE.test(line)) return cap1(line.replace(/[-_]+/g, " "));
    return clip(cleanPrompt(line) || line, 60);
  }
  const fromPrompt = firstMsg ? cleanPrompt(firstMsg) : "";
  if (fromPrompt.length > 3) return clip(fromPrompt, 60);
  const out = stripMd(t.lastOutcome || "");
  const m = /^(?:I will|I'll|I am going to|I am|I'm)\s+(.+)$/i.exec(firstSentence(out));
  if (m) return clip(cap1(m[1].replace(/[.!?]$/, "")), 60);
  const who = agentName(t.tools?.[0] || "agent");
  return `${who} session at ${new Date(t.startedAt || t.lastAt).toTimeString().slice(0, 5)}`;
}

/** One clean sentence (or two short ones) from the last reply, max 160 chars. */
export function outcomeOf(t: V3Thread): string {
  if (t.outcomeLine) return t.outcomeLine;
  const s = stripMd(t.lastOutcome || t.lastPoint || "");
  if (!s) return "";
  const one = firstSentence(s);
  const two = one.length < 70 ? firstSentence(s.slice(one.length).trim()) : "";
  return clip(two && one.length + two.length < 160 ? `${one} ${two}` : one, 160);
}

/** Recovered = a later call in the same turn with the same tool name succeeded. */
export function isRecovered(err: Span, later: Span[]): boolean {
  return later.some((s) => s !== err && s.name === err.name && !s.err && s.ts >= err.ts);
}

export type Verb = "review" | "fix" | "test" | "other";
export function verbOf(display: string): Verb {
  const w = display.trim().split(/\s+/)[0]?.toLowerCase() || "";
  if (/^(review|reviewed|audit|check|verify|standards|security|full)/.test(w) || /\breview\b/i.test(display.slice(0, 40))) return "review";
  if (/^(fix|fixes|repair)/.test(w)) return "fix";
  if (/^(test|tests|run)/.test(w)) return "test";
  return "other";
}
const NOUN: Record<Verb, [string, string]> = { review: ["review", "reviews"], fix: ["fix", "fixes"], test: ["test run", "test runs"], other: ["run", "runs"] };
export function stackLabel(n: number, agent: string, verb: Verb, childKind?: string): string {
  if (verb === "other" && childKind === "subagent") return `${n} ${agentName(agent)} subagent${n === 1 ? "" : "s"}`;
  return `${n} ${agentName(agent)} ${NOUN[verb][n === 1 ? 0 : 1]}`;
}

const FINDING_RE = /\b(finding|findings|defect|defects|P1|P2|P3|issue|issues|bug|bugs)\b/i;
const CLEAN_RE = /\b(no (independent )?(findings|defects|issues)|clean|no .*warrants a finding|lgtm)\b/i;
export const hasFindings = (t: V3Thread) => {
  const o = stripMd(t.lastOutcome || t.outcomeLine || "");
  return FINDING_RE.test(o) && !CLEAN_RE.test(o);
};

/** Groups threads by key into Stack objects; groups of one are dropped. */
export function groupStacks(threads: V3Thread[], keyOf: (t: V3Thread) => string | null, firstMsg?: (t: V3Thread) => string | undefined): Stack[] {
  const m = new Map<string, V3Thread[]>();
  for (const t of threads) {
    const k = keyOf(t);
    if (!k) continue;
    m.set(k, [...(m.get(k) || []), t]);
  }
  const out: Stack[] = [];
  for (const [k, ms] of m) {
    if (ms.length < 2) continue;
    const t0 = ms[0];
    const verb = verbOf(displayOf(t0, firstMsg?.(t0)));
    const st = { running: 0, waiting: 0, done: 0, failed: 0, abandoned: 0, findings: 0 };
    for (const t of ms) {
      if (t.state === "running") st.running++;
      else if (t.state === "awaiting-permission" || t.attention) st.waiting++; // a finished turn (awaiting-input) is ready, not waiting
      else if (t.state === "abandoned") st.abandoned++;
      else if ((t.errorsOpen || 0) > 0) st.failed++;
      else st.done++;
      if (hasFindings(t)) st.findings++;
    }
    out.push({
      id: `stack:${k}`,
      parent: t0.parentThread || "",
      agent: t0.tools?.[0] || "agent",
      kind: verb,
      label: stackLabel(ms.length, t0.tools?.[0] || "agent", verb, t0.childKind),
      repo: repoOf(t0),
      total: ms.length,
      ...st,
      lastAt: Math.max(...ms.map((t) => t.lastAt)),
      members: ms.sort((a, b) => b.lastAt - a.lastAt).map((t) => t.id),
    });
  }
  return out;
}

/** Stacks from the backend, else children of one parent with the same agent and verb class. */
export function stacksOf(threads: V3Thread[], server?: Stack[], firstMsg?: (t: V3Thread) => string | undefined): Stack[] {
  if (server) return server;
  return groupStacks(threads, (t) => (t.parentThread ? `${t.parentThread}|${t.tools?.[0]}|${verbOf(displayOf(t, firstMsg?.(t)))}` : null), firstMsg);
}

/** 12 five-minute buckets (oldest first) of tool calls and errors for the last hour. */
export function bucketize(ts: { ts: number; err?: boolean | number }[], now = Date.now()): { calls: number[]; errs: number[] } {
  const calls = new Array(12).fill(0);
  const errs = new Array(12).fill(0);
  const from = now - 3600_000;
  for (const s of ts) {
    if (s.ts < from || s.ts > now) continue;
    const i = Math.min(11, Math.floor((s.ts - from) / 300_000));
    calls[i]++;
    if (s.err) errs[i]++;
  }
  return { calls, errs };
}

const COMMIT_MSG = /git\s+commit\b[^\n]*?-m\s+(?:"([^"]+)"|'([^']+)'|\$'([^']+)')/;
/** Shipped items from the backend, else from trace outputs with "commit git commit" folded away. */
export function shippedOf(t: V3Thread | undefined, outputs?: { kind: string; label: string; ref: string; ts: number }[]): Shipped[] {
  if (t?.shipped) return t.shipped;
  const out: Shipped[] = [];
  const seen = new Set<string>();
  for (const o of [...(outputs || [])].sort((a, b) => b.ts - a.ts)) {
    let kind = o.kind as Shipped["kind"];
    let label = o.label;
    if (o.kind === "commit") {
      const m = COMMIT_MSG.exec(o.ref || "");
      label = m ? (m[1] || m[2] || m[3]).split("\n")[0] : "";
    } else if (o.kind === "artifact" || o.kind === "page") {
      kind = "page";
      label = (o.label || o.ref).replace(/^https?:\/\/[^/]+\//, "").replace(/\/$/, "");
    } else if (o.kind === "pr") kind = "pr";
    else if (o.kind !== "push" && o.kind !== "merge") continue;
    const key = `${kind}|${label}|${kind === "commit" && !label ? o.ts : ""}`;
    if (seen.has(key)) continue;
    seen.add(key);
    out.push({ kind, label, ref: o.kind === "pr" || o.kind === "artifact" ? o.ref : undefined, ts: o.ts });
    if (out.length >= 12) break;
  }
  return out;
}

/** "4 commits, 1 PR, published dr-router-stats", empty when nothing shipped. */
export function shippedSummary(items: Shipped[]): string {
  if (!items.length) return "";
  const n = (k: string) => items.filter((i) => i.kind === k).length;
  const parts: string[] = [];
  const c = n("commit");
  if (c) parts.push(`${c} commit${c > 1 ? "s" : ""}`);
  const p = n("pr");
  if (p) parts.push(`${p} PR${p > 1 ? "s" : ""}`);
  const mg = n("merge");
  if (mg) parts.push(`${mg} merge${mg > 1 ? "s" : ""}`);
  const pages = items.filter((i) => i.kind === "page").map((i) => `"${i.label}"`);
  if (pages.length) parts.push(`published ${pages.slice(0, 2).join(", ")}${pages.length > 2 ? ` +${pages.length - 2}` : ""}`);
  return parts.join(", ");
}
