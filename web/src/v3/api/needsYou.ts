// GET /api/needs-you: the one list of things that wait for the user. Every count and list on the web reads it.
import { useEffect, useSyncExternalStore } from "react";
import type { PermissionRequest } from "../../api/types";
import { mockWanted } from "./orch";
import type { TermQuestion } from "./p7";
import type { Approval } from "./types";

export type NeedsYouKind = "approval" | "mcp" | "question" | "prompt" | "error";
export interface NeedsYouItem {
  kind: NeedsYouKind;
  key: string;
  sessionId?: string;
  threadId?: string;
  agent?: string;
  cwd?: string;
  title?: string;
  at: number;
  answerable: boolean;
  why?: string;
  approval?: Approval;
  mcp?: PermissionRequest & { risk?: Approval["risk"]; riskReason?: string };
  question?: TermQuestion;
  message?: string;
  promptKey?: string;
}
export interface NeedsYouState {
  items: NeedsYouItem[];
  count: number;
  status: "loading" | "ok" | "error";
  error: string;
  version: number;
}

let st: NeedsYouState = { items: [], count: 0, status: "loading", error: "", version: 0 };
const subs = new Set<() => void>();
let users = 0;
let paused = 0;
let timer: number | undefined;
let inflight = false;
let again = false;
let every = 3000;

const emit = (n: Partial<NeedsYouState>) => {
  st = { ...st, ...n, version: st.version + 1 };
  subs.forEach((f) => f());
};

/** Reads the list now; a call during a read queues one more read. */
export async function refreshNeedsYou(): Promise<void> {
  if (inflight) {
    again = true;
    return;
  }
  inflight = true;
  try {
    if (mockWanted()) return emit({ items: [], count: 0, status: "ok", error: "" });
    const r = await fetch("/api/needs-you", { cache: "no-store", credentials: "same-origin" });
    if (!r.ok) throw new Error(`HTTP ${r.status}`);
    const d = (await r.json()) as { items?: NeedsYouItem[]; count?: number };
    const items = d.items || [];
    const sig = (l: NeedsYouItem[]) => l.map((i) => `${i.key}:${i.answerable ? 1 : 0}:${i.question?.step ?? ""}:${i.question?.sentAt ?? ""}:${i.message ?? ""}`).join("|");
    if (st.status !== "ok" || st.error || sig(items) !== sig(st.items)) emit({ items, count: d.count ?? items.length, status: "ok", error: "" });
  } catch (e) {
    // A failed read keeps the last list, so a short daemon hiccup does not empty every badge; error marks it as old.
    if (!st.error) emit({ status: st.status === "ok" ? "ok" : "error", error: (e as Error).message });
  } finally {
    inflight = false;
    if (again) {
      again = false;
      void refreshNeedsYou();
    }
  }
}

function arm() {
  window.clearInterval(timer);
  timer = undefined;
  if (users > 0 && paused === 0) {
    void refreshNeedsYou();
    timer = window.setInterval(() => void refreshNeedsYou(), every);
  }
}

/** The shared list, polled every 3 s while any component uses it (2 s with fast). */
export function useNeedsYou(opts: { fast?: boolean; paused?: boolean } = {}): NeedsYouState {
  const { fast = false, paused: off = false } = opts;
  useEffect(() => {
    users++;
    if (fast) every = 2000;
    arm();
    return () => {
      users--;
      if (fast) every = 3000;
      arm();
    };
  }, [fast]);
  useEffect(() => {
    if (!off) return;
    paused++;
    arm();
    return () => {
      paused--;
      arm();
    };
  }, [off]);
  return useSyncExternalStore(
    (f) => {
      subs.add(f);
      return () => void subs.delete(f);
    },
    () => st,
  );
}

/** Drops one item at once, for example after an answer, until the next read confirms it. */
export function dropNeedsYou(key: string) {
  if (!st.items.some((i) => i.key === key)) return;
  const items = st.items.filter((i) => i.key !== key);
  emit({ items, count: items.length });
}

export const sessionKey = (i: NeedsYouItem) => {
  const sid = i.sessionId || i.mcp?.sessionId || "";
  return sid && !sid.includes(":") && i.kind === "mcp" ? `claude:${sid}` : sid;
};

/** The thread ids that hold an item, mapped the same way the thread list maps sessions. */
export function needThreadIds(items: NeedsYouItem[], threadOf: Record<string, string>, drivenRowOf: Record<string, string>): Set<string> {
  const out = new Set<string>();
  for (const i of items) {
    const d = i.approval?.drivenId || i.threadId;
    if (d) out.add(d);
    if (d && drivenRowOf[d]) out.add(drivenRowOf[d]);
    const sid = sessionKey(i);
    if (sid) out.add(threadOf[sid] || sid);
  }
  return out;
}

/** Driven threads that need you: pending hook approvals plus the needs-you items (approvals and failed threads). */
export function drivenNeedIds(items: NeedsYouItem[], approvals: Iterable<Approval>): Set<string> {
  const out = new Set<string>();
  for (const a of approvals) if (a.drivenId && a.state === "pending") out.add(a.drivenId);
  for (const i of items) {
    if (i.approval?.drivenId) out.add(i.approval.drivenId);
    if (i.threadId) out.add(i.threadId);
  }
  return out;
}
