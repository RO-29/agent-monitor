// Plan usage windows (5-hour and weekly) per agent, from GET /api/usage/limits.
import { ApiError, useApi, v3 } from "./client";
import { mreq } from "./machines";

export interface UsagePoint {
  at: number;
  pct: number;
}
export interface UsageWindow {
  /** "5h", "weekly", "weekly_scoped" (label "Week (Model)") or another key the daemon adds. */
  key: string;
  label: string;
  windowMinutes: number;
  usedPercent: number | null;
  lastPercent?: number;
  resetsAt?: number;
  expired?: boolean;
  history: UsagePoint[];
}
export interface UsageTokens {
  input: number;
  output: number;
  cacheRead: number;
  cacheCreate: number;
}
export interface UsageLocal {
  key: "5h" | "weekly";
  label: string;
  from: number;
  aligned: boolean;
  tokens: UsageTokens;
  costUsd: number;
  costEstimated: boolean;
  messages: number;
}
export interface ModelUsage {
  model: string;
  window: "5h" | "weekly";
  inTokens: number;
  outTokens: number;
  cacheRead: number;
  cacheWrite: number;
  costUsd: number;
  messages: number;
}
// Daemon shape: {source, measuredAt?, hasCredits?, unlimited?, balance?: number|string, spendUsd?}.
// Spend limit shape: spent and limit in major units of currency, percent 0 to 100.
export type UsageCredits =
  | number
  | { source?: string; measuredAt?: number; hasCredits?: boolean; unlimited?: boolean; balance?: number | string | null; spendUsd?: number; spent?: number; limit?: number; currency?: string; percent?: number; capReached?: boolean };
export type AgentId = "claude" | "codex" | "opencode" | "cursor" | "pi";
export interface AgentUsage {
  agent: AgentId | string;
  installed?: boolean;
  source: string;
  sourcePath?: string;
  measuredAt?: number;
  plan?: string;
  note?: string;
  windows: UsageWindow[];
  local: UsageLocal[];
  localSource: string;
  models?: ModelUsage[];
  credits?: UsageCredits;
  /** A reached cap, shown as a red line. */
  alert?: string;
  /** The plan reading is older than one hour. */
  stale?: boolean;
  /** The subscription: a hashed key, a masked email and the organisation. */
  account?: { key: string; label?: string; org?: string };
  /** Set in fleet readings: the machine the reading came from. */
  machine?: string;
  /** In a merged card: the machine the local token counts came from. */
  localMachine?: string;
}
export interface UsageLimits {
  generatedAt: number;
  agents: AgentUsage[];
}

async function fetchLimits(): Promise<UsageLimits> {
  const j = await v3.usageLimits();
  const agents = (Array.isArray(j.agents) ? j.agents : []).map((a) => ({ ...a, windows: a.windows || [], local: a.local || [], localSource: a.localSource || "", source: a.source || "" }));
  return { generatedAt: j.generatedAt || 0, agents };
}

export const useUsageLimits = (poll = 60_000) => useApi(fetchLimits, [], { poll });

export function fmtResetIn(at?: number, now = Date.now()): string {
  if (!at) return "";
  const s = Math.floor((at - now) / 1000);
  if (s <= 0) return "resets now";
  const d = Math.floor(s / 86400);
  const h = Math.floor((s % 86400) / 3600);
  const m = Math.floor((s % 3600) / 60);
  if (d) return `resets in ${d} d ${h} h`;
  if (h) return `resets in ${h} h ${m} m`;
  return `resets in ${Math.max(1, m)} m`;
}

export const usageLevel = (p: number | null) => (p == null ? "off" : p >= 90 ? "over" : p >= 70 ? "warn" : "ok");

/** One subscription: its newest reading, the machines that use it, and every machine's reading. */
export interface UsageGroup {
  key: string;
  agent: string;
  a: AgentUsage;
  machines: string[];
  all: AgentUsage[];
}

const fresh = (a: AgentUsage) => (a.windows.some((w) => w.usedPercent != null) || a.credits != null ? a.measuredAt || 1 : 0);

/** Groups readings by agent and account; a reading with no account groups by agent and machine. */
export function groupUsage(agents: AgentUsage[]): UsageGroup[] {
  const m = new Map<string, UsageGroup>();
  const installedAgents = new Set(agents.filter((a) => a.installed !== false).map((a) => a.agent));
  for (const a of agents) {
    // Not installed here but installed on another machine: that machine's card says enough.
    if (a.installed === false && installedAgents.has(a.agent)) continue;
    const key = a.agent + "|" + (a.account?.key ? "acct:" + a.account.key : a.installed === false ? "off" : "m:" + (a.machine || ""));
    const g = m.get(key);
    if (!g) {
      m.set(key, { key, agent: a.agent, a, machines: a.machine ? [a.machine] : [], all: [a] });
      continue;
    }
    g.all.push(a);
    if (a.machine && !g.machines.includes(a.machine)) g.machines.push(a.machine);
    if (fresh(a) > fresh(g.a)) g.a = { ...a, local: g.a.local, models: g.a.models, localSource: g.a.localSource, localMachine: g.a.localMachine ?? g.a.machine };
  }
  return [...m.values()];
}

/** Every machine's usage when the fleet route exists, else this machine's alone; grouped by account. */
async function fetchGroups(): Promise<{ groups: UsageGroup[]; machines: number }> {
  try {
    const r = await mreq<{ machines: { name: string; self?: boolean }[]; agents: AgentUsage[] }>("GET", "/api/fleet/usage");
    const self = (r.machines || []).find((m) => m.self)?.name;
    // This machine reads as "this Mac", whatever its name.
    const agents = (r.agents || []).map((a) => ({ ...a, machine: self && a.machine === self ? "this Mac" : a.machine, windows: a.windows || [], local: a.local || [], localSource: a.localSource || "", source: a.source || "" }));
    return { groups: groupUsage(agents), machines: (r.machines || []).length || 1 };
  } catch (e) {
    if (!(e instanceof ApiError) || (e.status !== 404 && e.status !== 403)) throw e;
    const own = await fetchLimits();
    return { groups: groupUsage(own.agents), machines: 1 };
  }
}

export const useUsageGroups = (poll = 60_000) => useApi(fetchGroups, [], { poll });
