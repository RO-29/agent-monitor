// Plan usage windows (5-hour and weekly) per agent, from GET /api/usage/limits.
import { useApi, v3 } from "./client";

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
