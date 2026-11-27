// v3 live store: approvals, threads and monitors, fed by REST plus the shared /ws socket (lib/ws.ts).
import { useSyncExternalStore } from "react";
import { api } from "../api/client";
import type { WsEvent } from "../api/types";
import { KEYS } from "../app/prefs";
import { playBeep } from "../app/notify";
import { showToast } from "../app/toast";
import { onLiveEvent } from "../lib/ws";
import { isMissing, v3 } from "./api/client";
import type { Approval, Monitor, V3Thread } from "./api/types";

export type LoadStatus = "loading" | "ok" | "missing" | "error";

export interface V3State {
  approvals: Map<string, Approval>;
  recent: Approval[];
  apprStatus: LoadStatus;
  apprError: string;
  threads: V3Thread[];
  threadOf: Record<string, string>;
  threadsStatus: LoadStatus;
  threadsError: string;
  threadsV3: boolean;
  monitors: Map<string, Monitor>;
  drivenRowOf: Record<string, string>; // orchestrator thread id -> row id
  version: number;
}

const s: V3State = {
  approvals: new Map(),
  recent: [],
  apprStatus: "loading",
  apprError: "",
  threads: [],
  threadOf: {},
  threadsStatus: "loading",
  threadsError: "",
  threadsV3: false,
  monitors: new Map(),
  drivenRowOf: {},
  version: 0,
};
let snap: V3State = { ...s };
const listeners = new Set<() => void>();
function emit() {
  s.version++;
  snap = { ...s };
  listeners.forEach((l) => l());
}

export function useV3(): V3State {
  return useSyncExternalStore(
    (l) => {
      listeners.add(l);
      return () => listeners.delete(l);
    },
    () => snap,
    () => snap,
  );
}

function notifyOn(): boolean {
  try {
    return localStorage.getItem(KEYS.notify) !== "0";
  } catch {
    return true;
  }
}
function notifyApproval(a: Approval) {
  if (!document.hidden || !notifyOn()) return;
  if (typeof Notification === "undefined" || Notification.permission !== "granted") return;
  try {
    const title = a.kind === "question" ? `${agentName(a.agent)} asks a question` : a.kind === "plan" ? `${agentName(a.agent)} wants plan approval` : `${agentName(a.agent)} wants ${a.toolName}`;
    const n = new Notification(title, { body: a.summary.slice(0, 180), tag: `appr-${a.id}`, requireInteraction: true });
    n.onclick = () => {
      window.focus();
      n.close();
    };
    playBeep();
  } catch {
    /* blocked */
  }
}
const NAMES: Record<string, string> = { codex: "Codex", claude: "Claude Code", opencode: "OpenCode", gemini: "Gemini", cursor: "Cursor" };
export const agentName = (a: string) => NAMES[a] || a;

function upsertApproval(a: Approval) {
  const was = s.approvals.has(a.id);
  s.approvals = new Map(s.approvals);
  if (a.state === "pending") {
    s.approvals.set(a.id, a);
    if (!was) notifyApproval(a);
  } else {
    s.approvals.delete(a.id);
    s.recent = [a, ...s.recent.filter((r) => r.id !== a.id)].slice(0, 100);
    if (was && a.state === "answered-in-terminal") showToast(`Answered in the terminal${a.choice ? `: ${a.choice}` : ""}`, `${agentName(a.agent)} · ${a.summary.slice(0, 120)}`);
  }
  emit();
}
export function applyDecision(a: Approval) {
  if (a.state !== "pending") mockAppr.delete(a.id);
  upsertApproval(a);
}
const mockAppr = new Map<string, Approval>();
/** Dev mock only (?mock=1): pending approvals kept across reloads from the daemon. */
export function addMockApprovals(list: Approval[]) {
  for (const a of list) mockAppr.set(a.id, a);
  for (const a of list) upsertApproval(a);
}

let apprTimer: number | undefined;
let apprInflight = false;
let apprAgain = false;
export async function loadApprovals() {
  if (apprInflight) {
    apprAgain = true;
    return;
  }
  apprInflight = true;
  window.clearTimeout(apprTimer);
  try {
    const r = await v3.approvals();
    s.approvals = new Map([...(r.pending || []), ...mockAppr.values()].map((a) => [a.id, a]));
    s.recent = r.recent || [];
    s.apprStatus = "ok";
    s.apprError = "";
  } catch (e) {
    s.apprStatus = isMissing(e) ? "missing" : "error";
    s.apprError = (e as Error).message;
  } finally {
    apprInflight = false;
  }
  emit();
  if (apprAgain) {
    apprAgain = false;
    return void loadApprovals();
  }
  apprTimer = window.setTimeout(loadApprovals, s.apprStatus === "ok" ? 20_000 : 30_000);
}

// Driven rows from the daemon win; client-built rows fill in for daemons that do not list them yet.
let serverThreads: V3Thread[] = [];
let synthDriven: V3Thread[] = [];
function mergeThreads() {
  const have = new Set(serverThreads.filter((t) => t.kind === "driven").map((t) => t.drivenId || t.id));
  s.threads = [...serverThreads, ...synthDriven.filter((t) => !have.has(t.drivenId || t.id))];
  const m: Record<string, string> = {};
  for (const t of s.threads) if (t.kind === "driven") m[t.drivenId || t.id] = t.id;
  s.drivenRowOf = m;
}
export function setDrivenThreads(list: V3Thread[]) {
  synthDriven = list;
  mergeThreads();
  emit();
}

let mockThreads: V3Thread[] = [];
/** Dev mock only (?mock=1): watched threads appended to every load. */
export function addMockThreads(t: V3Thread[]) {
  mockThreads = t;
  serverThreads = [...serverThreads.filter((x) => !t.some((y) => y.id === x.id)), ...t];
  mergeThreads();
  emit();
}

let thTimer: number | undefined;
let thInflight = false;
let thAgain = false;
export async function loadThreads() {
  if (thInflight) {
    thAgain = true;
    return;
  }
  thInflight = true;
  window.clearTimeout(thTimer);
  try {
    try {
      const r = await v3.threads();
      serverThreads = r.threads || [];
      s.threadOf = r.threadOf || {};
      s.threadsV3 = true;
    } catch (e) {
      if (!isMissing(e)) throw e;
      const r = await api.threads();
      serverThreads = r.threads || [];
      s.threadOf = r.threadOf || {};
      s.threadsV3 = false;
    }
    if (mockThreads.length) serverThreads = [...serverThreads.filter((x) => !mockThreads.some((y) => y.id === x.id)), ...mockThreads];
    mergeThreads();
    s.threadsStatus = "ok";
    s.threadsError = "";
  } catch (e) {
    s.threadsStatus = "error";
    s.threadsError = (e as Error).message;
  } finally {
    thInflight = false;
  }
  emit();
  if (thAgain) {
    thAgain = false;
    return void loadThreads();
  }
  thTimer = window.setTimeout(loadThreads, 30_000);
}
// A throttle, not a debounce: steady events from many agents must not push the reload out forever.
let debounce: number | undefined;
function soonThreads() {
  if (debounce !== undefined) return;
  debounce = window.setTimeout(() => {
    debounce = undefined;
    void loadThreads();
  }, 2500);
}

let started = false;
export function startV3Live() {
  if (started) return;
  started = true;
  void loadApprovals();
  void loadThreads();
  onLiveEvent((raw: WsEvent) => {
    const e = raw as unknown as { kind: string; approval?: Approval; monitor?: Monitor };
    if (e.kind === "approval" && e.approval) upsertApproval(e.approval);
    else if (e.kind === "approval-done" && e.approval) upsertApproval({ ...e.approval, state: e.approval.state === "pending" ? "passed" : e.approval.state });
    else if (e.kind === "monitor" && e.monitor) {
      s.monitors = new Map(s.monitors);
      s.monitors.set(e.monitor.id, e.monitor);
      emit();
    } else if (e.kind === "upsert" || e.kind === "remove") soonThreads();
    else if (e.kind === "snapshot") {
      void loadApprovals();
      soonThreads();
    }
  });
}

/** Pending approvals grouped by thread id, oldest first. */
export function approvalsByThread(st: V3State): Map<string, Approval[]> {
  const out = new Map<string, Approval[]>();
  for (const a of [...st.approvals.values()].sort((x, y) => x.createdAt - y.createdAt)) {
    const t = (a.drivenId && st.drivenRowOf[a.drivenId]) || st.threadOf[a.sessionId] || a.sessionId;
    const arr = out.get(t) || [];
    arr.push(a);
    out.set(t, arr);
  }
  return out;
}
