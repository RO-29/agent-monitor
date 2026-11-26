// Phase 6 settings: notification rules and spend limits, read from and written to /api/settings.
import { ApiError } from "./client";
import { mockWanted, orchApi, type OSpendStatus } from "./orch";

export type NKind = "needs-you" | "finished" | "error" | "spend";
export const NKINDS: { key: NKind; label: string; hint: string }[] = [
  { key: "needs-you", label: "Needs you", hint: "An agent asks for an approval, an answer or a plan review." },
  { key: "finished", label: "Finished", hint: "A root thread ends its turn. A fan-out sends one notice when all its subagents end." },
  { key: "error", label: "Errors", hint: "A driven thread stops with an error." },
  { key: "spend", label: "Spend limit", hint: "A thread or the day reaches 80% and 100% of its limit." },
];

export interface NotifySettings {
  events: Record<NKind, boolean>;
  quietStart: string; // "22:00", "" = quiet hours off
  quietEnd: string;
  quietAllowsNeedsYou: boolean;
  batchSec: number;
  remindMin: number; // 0 = off
}
export interface CodexRate {
  in: number; // USD per 1M input tokens
  out: number;
}
export interface SpendSettings {
  perThreadUsd: number; // 0 = off
  perDayUsd: number;
  codexRates: Record<string, CodexRate>;
}
export interface Phase6Settings {
  notify: NotifySettings;
  spend: SpendSettings;
}

export const DEFAULT_NOTIFY: NotifySettings = {
  events: { "needs-you": true, finished: true, error: true, spend: true },
  quietStart: "",
  quietEnd: "",
  quietAllowsNeedsYou: true,
  batchSec: 20,
  remindMin: 10,
};
export const DEFAULT_SPEND: SpendSettings = { perThreadUsd: 0, perDayUsd: 0, codexRates: {} };

// The Go side may send events as {needsYou,...} or {"needs-you",...}; accept both.
function normNotify(raw: unknown): NotifySettings {
  const r = (raw || {}) as Record<string, unknown>;
  const ev = (r.events || r) as Record<string, unknown>;
  const pick = (a: string, b: string, d: boolean) => (typeof ev[a] === "boolean" ? (ev[a] as boolean) : typeof ev[b] === "boolean" ? (ev[b] as boolean) : d);
  return {
    events: { "needs-you": pick("needs-you", "needsYou", true), finished: pick("finished", "finished", true), error: pick("error", "error", true), spend: pick("spend", "spend", true) },
    quietStart: typeof r.quietStart === "string" ? r.quietStart : "",
    quietEnd: typeof r.quietEnd === "string" ? r.quietEnd : "",
    quietAllowsNeedsYou: typeof r.quietAllowsNeedsYou === "boolean" ? r.quietAllowsNeedsYou : true,
    batchSec: Number(r.batchSec) || 20,
    remindMin: typeof r.remindMin === "number" ? r.remindMin : 10,
  };
}
function normSpend(raw: unknown): SpendSettings {
  const r = (raw || {}) as Record<string, unknown>;
  const rates: Record<string, CodexRate> = {};
  for (const [k, v] of Object.entries((r.codexRates || {}) as Record<string, Partial<CodexRate>>)) rates[k] = { in: Number(v?.in) || 0, out: Number(v?.out) || 0 };
  return { perThreadUsd: Number(r.perThreadUsd) || 0, perDayUsd: Number(r.perDayUsd) || 0, codexRates: rates };
}
const toWire = (n: Partial<NotifySettings>) => {
  const { events, ...rest } = n;
  return events ? { ...rest, needsYou: events["needs-you"], finished: events.finished, error: events.error, spend: events.spend } : rest;
};

async function req<T>(method: string, url: string, body?: unknown): Promise<T> {
  let r: Response;
  try {
    r = await fetch(url, { method, cache: "no-store", credentials: "same-origin", headers: body !== undefined ? { "Content-Type": "application/json" } : undefined, body: body !== undefined ? JSON.stringify(body) : undefined });
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

/** True when the daemon's settings carry the phase 6 blocks. */
export interface SettingsRead extends Phase6Settings {
  known: { notify: boolean; spend: boolean };
}
const parse = (r: Record<string, unknown>): SettingsRead => ({ notify: normNotify(r.notify), spend: normSpend(r.spend), known: { notify: !!r.notify, spend: !!r.spend } });

const real = {
  get: () => req<Record<string, unknown>>("GET", "/api/settings").then(parse),
  saveNotify: (n: Partial<NotifySettings>) => req<Record<string, unknown>>("POST", "/api/settings", { notify: toWire(n) }).then(parse),
  saveSpend: (sp: Partial<SpendSettings>) => req<Record<string, unknown>>("POST", "/api/settings", { spend: sp }).then(parse),
  spend: (threadId?: string) => orchApi.spend(threadId),
};

// ---- ?mock=1: in memory, nothing reaches the daemon ----
let mockNotify: NotifySettings = { ...DEFAULT_NOTIFY, quietStart: "22:30", quietEnd: "07:00" };
let mockSpend: SpendSettings = { perThreadUsd: 5, perDayUsd: 25, codexRates: { "gpt-5.5": { in: 1.25, out: 10 } } };
const later = <T,>(v: T) => new Promise<T>((r) => setTimeout(() => r(v), 120));
const MOCK_THREAD_SPEND: Record<string, number> = { tok: 2.84, cmt: 0.61, lead: 4.12, cq: 3.9, rdy: 5.02, k1: 0.94, cp: 0.38, oc: 0.22 };
const mock: typeof real = {
  get: () => later({ notify: { ...mockNotify }, spend: { ...mockSpend }, known: { notify: true, spend: true } }),
  saveNotify: (n) => {
    mockNotify = { ...mockNotify, ...n };
    return later({ notify: { ...mockNotify }, spend: { ...mockSpend }, known: { notify: true, spend: true } });
  },
  saveSpend: (sp) => {
    mockSpend = { ...mockSpend, ...sp };
    return later({ notify: { ...mockNotify }, spend: { ...mockSpend }, known: { notify: true, spend: true } });
  },
  spend: (threadId) => {
    const out: OSpendStatus = { day: { spent: 18.46, limit: mockSpend.perDayUsd } };
    if (threadId) {
      const spent = MOCK_THREAD_SPEND[threadId] ?? 0.12;
      out.thread = { spent, limit: mockSpend.perThreadUsd, blocked: !!mockSpend.perThreadUsd && spent >= mockSpend.perThreadUsd };
    }
    return later(out);
  },
};

export const settingsApi = () => (mockWanted() ? mock : real);
