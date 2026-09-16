import { useCallback, useEffect, useRef, useState } from "react";
import { api } from "../../api/client";
import type { BoundaryKind, Chapter, Segment, Span, SpanFamily, TokenUsage, TraceFull } from "../../api/types";
import { fmtClock, fmtDate } from "../../lib/format";
import { onLiveEvent } from "../../lib/ws";

// ── boundary presentation ─────────────────────────────────────────────────
export const BOUNDARY: Record<BoundaryKind, { glyph: string; cls: string; label: string }> = {
  start: { glyph: "▶", cls: "bd-st", label: "session start" },
  compact: { glyph: "⟲", cls: "bd-cmp", label: "compact" },
  clear: { glyph: "⌫", cls: "bd-clr", label: "/clear → new session" },
  resume: { glyph: "↻", cls: "bd-res", label: "resume" },
};

export const FAM_ORDER: SpanFamily[] = ["bash", "agent", "mcp", "edit", "read", "web", "other", "model"];

export const clamp = (v: number, a: number, b: number) => Math.max(a, Math.min(b, v));



// ── span index ────────────────────────────────────────────────────────────
export interface SpanIndex {
  byId: Map<string, Span>;
  children: Map<string, Span[]>; // parent id → children sorted by ts
  roots: Span[]; // depth 0 (user + turn) sorted by ts
  turns: Span[];
  synthetic: Span[]; // turns synthesised for orphan tool runs (see below)
}

export function indexSpans(spans: Span[]): SpanIndex {
  const byId = new Map<string, Span>();
  const children = new Map<string, Span[]>();
  const rawRoots: Span[] = [];
  for (const s of spans) byId.set(s.id, s);
  for (const s of spans) {
    if (s.parent && byId.has(s.parent)) {
      const arr = children.get(s.parent) || [];
      arr.push(s);
      children.set(s.parent, arr);
    } else {
      rawRoots.push(s);
    }
  }
  rawRoots.sort((a, b) => a.ts - b.ts);
  // Tool calls that follow a compaction have no parent turn (the assistant
  // continued without a new prompt). Group each consecutive run under a
  // synthetic "continued" turn so the tree can collapse them.
  const roots: Span[] = [];
  const synthetic: Span[] = [];
  let run: Span[] = [];
  const flush = () => {
    if (!run.length) return;
    const first = run[0];
    const last = run[run.length - 1];
    const t: Span = { id: `synth-${first.id}`, kind: "turn", name: "turn · continued", ts: first.ts, dur: Math.max(last.ts + last.dur - first.ts, 0), depth: 0, seg: first.seg, fam: "model", text: "assistant continued after the boundary without a new prompt" };
    for (const r of run) r.parent = t.id;
    byId.set(t.id, t);
    children.set(t.id, run);
    synthetic.push(t);
    roots.push(t);
    run = [];
  };
  for (const s of rawRoots) {
    if (s.kind === "user" || s.kind === "turn") {
      flush();
      roots.push(s);
    } else if (run.length && s.seg !== run[0].seg) {
      flush();
      run.push(s);
    } else run.push(s);
  }
  flush();
  roots.sort((a, b) => a.ts - b.ts);
  children.forEach((arr) => arr.sort((a, b) => a.ts - b.ts));
  return { byId, children, roots, turns: roots.filter((s) => s.kind === "turn"), synthetic };
}



/** All descendants of a span (flattened), used for collapsed-turn counts. */
export function descendants(idx: SpanIndex, id: string, out: Span[] = []): Span[] {
  for (const c of idx.children.get(id) || []) {
    out.push(c);
    descendants(idx, c.id, out);
  }
  return out;
}

// ── execution time by family ──────────────────────────────────────────────
export interface Breakdown {
  total: number;
  byFam: { fam: SpanFamily; ms: number; pct: number }[];
}
export function breakdown(spans: Span[]): Breakdown {
  let turnMs = 0;
  const by: Partial<Record<SpanFamily, number>> = {};
  let toolMs = 0;
  for (const s of spans) {
    if (s.kind === "turn") turnMs += s.dur;
    if ((s.kind === "tool" || s.kind === "agent") && s.depth === 1) {
      by[s.fam] = (by[s.fam] || 0) + s.dur;
      toolMs += s.dur;
    }
  }
  const model = Math.max(turnMs - toolMs, 0);
  by.model = model;
  const total = Math.max(turnMs, toolMs + model) || 1;
  const byFam = FAM_ORDER.filter((f) => (by[f] || 0) > 0).map((f) => ({ fam: f, ms: by[f] || 0, pct: ((by[f] || 0) / total) * 100 }));
  return { total, byFam };
}


// ── chapter helpers ───────────────────────────────────────────────────────
export function chapterCounts(ch?: Chapter) {
  const learnings = ch?.learnings || [];
  return {
    intent: ch?.intentChanges?.length || 0,
    corrections: learnings.filter((l) => l.source === "correction").length,
    learnings: learnings.length,
    open: ch?.open?.length || 0,
    outputs: ch?.outputs?.length || 0,
  };
}

export const stripAnsi = (s: string) => s.replace(/\][^]*(|\\)/g, "").replace(/\[[0-9;?]*[ -/]*[@-~]/g, "");

export function copyText(t: string) {
  try {
    void navigator.clipboard?.writeText(t);
  } catch {
    /* clipboard unavailable in some embeds */
  }
}

// ── live trace hook ───────────────────────────────────────────────────────
export interface TraceState {
  trace: TraceFull | null;
  loading: boolean;
  error: string | null;
  loadMs: number;
  reload: () => void;
}

/** Loads the full trace and re-fetches (1.5 s debounce) on live events for this session. */
export function useTrace(sessionId: string): TraceState {
  const [trace, setTrace] = useState<TraceFull | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [loadMs, setLoadMs] = useState(0);
  const timer = useRef<number>();
  const lastCount = useRef<number>(-1);

  const load = useCallback(() => {
    const t0 = performance.now();
    setLoading(true);
    api
      .trace(sessionId)
      .then((t) => {
        setTrace(t);
        setError(null);
        setLoadMs(Math.round(performance.now() - t0));
      })
      .catch((e: Error) => setError(e.message))
      .finally(() => setLoading(false));
  }, [sessionId]);

  useEffect(() => {
    setTrace(null);
    load();
  }, [load]);

  useEffect(() => {
    const schedule = () => {
      window.clearTimeout(timer.current);
      timer.current = window.setTimeout(load, 1500);
    };
    return onLiveEvent((e) => {
      if (e.kind === "upsert" && e.session.id === sessionId) {
        if (e.session.messageCount !== lastCount.current) {
          lastCount.current = e.session.messageCount;
          schedule();
        }
      } else if ((e.kind === "segment" || e.kind === "chapter") && e.sessionId === sessionId) {
        schedule();
      }
    });
  }, [sessionId, load]);

  return { trace, loading, error, loadMs, reload: load };
}


export function outputHref(o: { kind: string; ref: string }): string | undefined {
  return o.kind === "pr" || o.kind === "artifact" ? o.ref : undefined;
}


// ── turn groups: the narrative unit ───────────────────────────────────────
// A session is a flat sequence of prompt → work. 84% of spans sit at depth 1,
// so there is no call tree worth drawing; there are turns, and what each ran.
export interface TurnGroup {
  id: string;
  prompt?: Span; // the user span that started it
  turns: Span[];
  calls: Span[]; // every tool/agent call under those turns, in time order
  fromTs: number;
  toTs: number;
  dur: number; // summed turn duration, not wall-clock
  errors: number;
  tokens: TokenUsage;
  gapBefore: number; // idle ms since the previous group
}

export function buildGroups(idx: SpanIndex, segNo: number): TurnGroup[] {
  const out: TurnGroup[] = [];
  let cur: TurnGroup | null = null;
  const start = (prompt?: Span, turn?: Span): TurnGroup => {
    const g: TurnGroup = {
      id: (prompt || turn)!.id,
      prompt,
      turns: [],
      calls: [],
      fromTs: (prompt || turn)!.ts,
      toTs: (prompt || turn)!.ts,
      dur: 0,
      errors: 0,
      tokens: { input: 0, output: 0, cacheRead: 0, cacheCreate: 0 },
      gapBefore: 0,
    };
    out.push(g);
    return g;
  };
  for (const r of idx.roots) {
    if (r.seg !== segNo) continue;
    if (r.kind === "user") {
      cur = start(r);
    } else if (r.kind === "turn") {
      if (!cur || cur.turns.length) cur = start(undefined, r);
      cur.turns.push(r);
      cur.dur += r.dur;
      cur.toTs = Math.max(cur.toTs, r.ts + r.dur);
      if (r.tokens) {
        cur.tokens.input += r.tokens.input;
        cur.tokens.output += r.tokens.output;
        cur.tokens.cacheRead += r.tokens.cacheRead;
        cur.tokens.cacheCreate += r.tokens.cacheCreate;
      }
      for (const c of descendants(idx, r.id)) {
        if (c.kind !== "tool" && c.kind !== "agent") continue;
        cur.calls.push(c);
        if (c.err) cur.errors++;
        cur.toTs = Math.max(cur.toTs, c.ts + c.dur);
      }
    }
  }
  for (const g of out) g.calls.sort((a, b) => a.ts - b.ts);
  for (let i = 1; i < out.length; i++) out[i].gapBefore = Math.max(out[i].fromTs - out[i - 1].toTs, 0);
  return out.filter((g) => g.prompt || g.turns.length);
}

/** Relative weight of a turn, for splitting a segment's cost across its turns.
 *  Never shown as money on its own: pricing cache reads at list rate reads as
 *  hundreds of dollars on a session that actually cost ten. */
export function costWeight(model: string, t?: TokenUsage): number {
  if (!t) return 0;
  const m = (model || "").toLowerCase();
  let r = [3, 15, 0.3, 3.75];
  if (m.includes("haiku")) r = [1, 5, 0.1, 1.25];
  else if (m.includes("opus") || m.includes("fable") || m.includes("mythos")) r = [15, 75, 1.5, 18.75];
  else if (m.includes("gpt") || m.includes("codex")) r = [1.25, 10, 0.125, 0];
  return (t.input * r[0] + t.output * r[1] + t.cacheRead * r[2] + t.cacheCreate * r[3]) / 1e6;
}

/** Ribbon: one gradient instead of one node per call, so a 700-call turn stays cheap. */
export function ribbonGradient(calls: Span[]): string {
  const total = calls.reduce((a, c) => a + Math.max(c.dur, 1), 0) || 1;
  const stops: string[] = [];
  let at = 0;
  // A hairline between calls: without it 115 Bash runs draw as one flat block.
  const gap = calls.length > 1 && calls.length <= 400 ? Math.min(0.35, 40 / calls.length) : 0;
  for (const c of calls) {
    const w = (Math.max(c.dur, 1) / total) * 100;
    const end = at + w;
    stops.push(`var(--fam-${c.fam}) ${at.toFixed(3)}% ${Math.max(at, end - gap).toFixed(3)}%`);
    if (gap) stops.push(`var(--bg-0) ${Math.max(at, end - gap).toFixed(3)}% ${end.toFixed(3)}%`);
    at = end;
  }
  return stops.length ? `linear-gradient(90deg, ${stops.join(", ")})` : "none";
}

/** Where each error sits along the ribbon, as a percentage. */
export function errorMarks(calls: Span[]): number[] {
  const total = calls.reduce((a, c) => a + Math.max(c.dur, 1), 0) || 1;
  const out: number[] = [];
  let at = 0;
  for (const c of calls) {
    const w = (Math.max(c.dur, 1) / total) * 100;
    if (c.err) out.push(at + w / 2);
    at += w;
  }
  return out;
}

/** mcp__clickstack-staging__clickstack_sql reads as clickstack_sql: the server
 *  and transport prefix repeats on every row. */
export const shortTool = (n: string) => (n.startsWith("mcp__") ? n.split("__").slice(-1)[0] : n);

export function famSummary(calls: Span[], max = 4): string {
  const c: Record<string, number> = {};
  for (const s of calls) c[s.name] = (c[s.name] || 0) + 1;
  return Object.entries(c)
    .sort((a, b) => b[1] - a[1])
    .slice(0, max)
    .map(([n, k]) => `${n} ${k}`)
    .join(" · ");
}
