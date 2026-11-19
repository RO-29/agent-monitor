// Shared derivations for Control, Messages and Timeline, with fallbacks for daemons that lack the v4 fields.
import type { Span } from "../../../api/types";
import { firstRealLine, projectName } from "../../../lib/format";
import { plainText } from "../../../lib/markdown";
import type { Stack, TimelineBar, TimelineLane, V3Thread } from "../../api/types";

export const repoOf = (t: { repo?: string; cwd?: string }) => t.repo || (/^\/Users\/[^/]+\/?$/.test(t.cwd || "") ? "home" : projectName(t.cwd)) || "home";

const JUNK = /^(\*\*|\[|#|\||\/|~\/|<)/;
const sentence = (s: string) => (s ? s[0].toUpperCase() + s.slice(1) : s);

export function displayOf(t: V3Thread, firstPrompt?: string): string {
  if (t.display) return t.display;
  let raw = plainText(firstRealLine(t.title || "") || t.title || "").trim();
  const repo = repoOf(t);
  if ((!raw || raw === repo || raw === projectName(t.cwd) || JUNK.test(raw)) && firstPrompt) {
    const p = plainText(firstRealLine(firstPrompt) || firstPrompt).replace(/^(you are reviewing|in this repository,?|please)\s+/i, "");
    if (p) raw = p;
  }
  if (JUNK.test(raw)) raw = raw.replace(/^[*#[|<\s]+/, "");
  if (/^[a-z0-9]+(-[a-z0-9]+)+$/i.test(raw)) raw = sentence(raw.replace(/-/g, " "));
  raw = raw.replace(/^(you are reviewing|in this repository,?|please)\s+/i, "");
  if (raw.length > 60) raw = raw.slice(0, 60).replace(/\s+\S*$/, "") + "…";
  return sentence(raw) || repoOf(t);
}

export function outcomeOf(t: V3Thread): string {
  const s = t.outcomeLine || plainText(t.lastOutcome || t.lastPoint || "");
  return s.length > 160 ? s.slice(0, 159) + "…" : s;
}

/** An error is recovered when a later call of the same tool in the same turn succeeds. */
export function recoveredSet(calls: Span[]): Set<string> {
  const out = new Set<string>();
  calls.forEach((c, i) => {
    if (c.err && calls.slice(i + 1).some((n) => n.name === c.name && !n.err)) out.add(c.id);
  });
  return out;
}

export const verbClass = (title: string): string => {
  const w = (title || "").trim().toLowerCase().split(/\s+/)[0] || "";
  if (/^review/.test(w)) return "review";
  if (/^fix/.test(w)) return "fix";
  if (/^test/.test(w)) return "test";
  return "other";
};
const agentLabel = (a: string) => (a === "codex" ? "Codex" : a === "claude" ? "Claude" : a);
export const stackLabel = (n: number, agent: string, kind: string) =>
  `${n} ${agentLabel(agent)} ${kind === "review" ? (n === 1 ? "review" : "reviews") : kind === "fix" ? "fixes" : kind === "test" ? "test runs" : "subagents"}`;

const FINDING = /finding|defect|\bP[12]\b|issue/i;
const CLEAN = /no findings|clean/i;

/** Stacks from children that share a parent, agent and verb class (2 or more members). */
export function deriveStacks(threads: V3Thread[]): Stack[] {
  const groups = new Map<string, V3Thread[]>();
  for (const t of threads) {
    if (!t.parentThread) continue;
    const agent = t.tools?.[0] || "claude";
    const key = `${t.parentThread}|${agent}|${verbClass(t.display || t.title)}`;
    groups.set(key, [...(groups.get(key) || []), t]);
  }
  const out: Stack[] = [];
  for (const [key, ms] of groups) {
    if (ms.length < 2) continue;
    const [parent, agent, kind] = key.split("|");
    const st = (s: string) => ms.filter((m) => m.state === s).length;
    out.push({
      id: `stack:${key}`,
      parent,
      agent,
      kind,
      label: stackLabel(ms.length, agent, kind),
      repo: repoOf(ms[0]),
      total: ms.length,
      running: st("running"),
      waiting: ms.filter((m) => m.state === "awaiting-input" || m.state === "awaiting-permission").length,
      done: st("completed") + st("idle"),
      failed: ms.filter((m) => (m.errorsOpen ?? 0) > 0).length,
      abandoned: st("abandoned"),
      findings: ms.filter((m) => {
        const o = outcomeOf(m);
        return FINDING.test(o) && !CLEAN.test(o);
      }).length,
      lastAt: Math.max(...ms.map((m) => m.lastAt)),
      members: ms.map((m) => m.id),
    });
  }
  return out.sort((a, b) => b.lastAt - a.lastAt);
}

export const BUCKET = 5 * 60_000;
export type Density = { from: number; to: number; calls: number; errors: number }[];

/** 5-minute buckets from work and error bars; calls approximate busy minutes when the daemon sends no density. */
export function densityFromBars(bars: TimelineBar[], from: number, to: number): Density {
  const out: Density = [];
  const start = Math.floor(from / BUCKET) * BUCKET;
  const work = bars.filter((b) => b.kind === "work" && b.from > 0 && b.to > from && b.from < to);
  const errs = bars.filter((b) => b.kind === "error" && b.from > 0 && b.from >= from && b.from < to);
  for (let a = start; a < to; a += BUCKET) {
    const b = a + BUCKET;
    let busy = 0;
    for (const w of work) busy += Math.max(0, Math.min(w.to, b) - Math.max(w.from, a));
    const e = errs.filter((x) => x.from >= a && x.from < b).length;
    const starts = work.filter((w) => w.from >= a && w.from < b).length;
    const calls = Math.round((Math.min(busy, BUCKET) / BUCKET) * 3) + starts * 2;
    if (calls > 0 || e > 0) out.push({ from: a, to: b, calls: Math.max(calls, e ? 1 : 0), errors: e });
  }
  return out;
}

/** Last hour as 12 buckets, oldest first: [calls, errors]. */
export function activityOf(t: V3Thread | undefined, lane: TimelineLane | undefined, now = Date.now()): { calls: number[]; errs: number[] } | null {
  if (t?.activity?.length) return { calls: t.activity, errs: t.activityErr || [] };
  if (!lane) return null;
  const from = now - 12 * BUCKET;
  const dens = lane.density?.length ? lane.density : densityFromBars(lane.bars, from, now);
  const calls = Array(12).fill(0);
  const errs = Array(12).fill(0);
  for (const d of dens) {
    const i = Math.floor((d.from - from) / BUCKET);
    if (i >= 0 && i < 12) {
      calls[i] += d.calls;
      errs[i] += d.errors;
    }
  }
  return calls.some((c) => c > 0) || errs.some((e) => e > 0) ? { calls, errs } : null;
}

let fontsAdded = false;
export function ensurePlexFonts() {
  if (fontsAdded || typeof document === "undefined") return;
  fontsAdded = true;
  if (document.querySelector('link[data-am="plex"]')) return;
  const l = document.createElement("link");
  l.rel = "stylesheet";
  l.dataset.am = "plex";
  l.href = "https://fonts.googleapis.com/css2?family=IBM+Plex+Mono:wght@400;500&family=IBM+Plex+Sans:wght@400;500;600&display=swap";
  document.head.appendChild(l);
}

export const ago = (ts: number) => {
  const s = Math.max(0, (Date.now() - ts) / 1000);
  if (s < 60) return "now";
  if (s < 3600) return `${Math.floor(s / 60)}m`;
  if (s < 86400) return `${Math.floor(s / 3600)}h`;
  return `${Math.floor(s / 86400)}d`;
};
