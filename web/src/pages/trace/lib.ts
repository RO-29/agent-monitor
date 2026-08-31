import { useCallback, useEffect, useRef, useState } from "react";
import { api } from "../../api/client";
import type { BoundaryKind, Chapter, Segment, Span, SpanFamily, TraceFull } from "../../api/types";
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

export interface TimeWindow {
  from: number;
  to: number;
}

export const clamp = (v: number, a: number, b: number) => Math.max(a, Math.min(b, v));

/** x position (0..1) of a timestamp inside a window */
export const frac = (ts: number, w: TimeWindow) => (w.to > w.from ? (ts - w.from) / (w.to - w.from) : 0);

export function segmentWindow(seg: Segment, lastTs: number): TimeWindow {
  const to = seg.toTs > seg.fromTs ? seg.toTs : Math.max(lastTs, seg.fromTs + 60_000);
  return { from: seg.fromTs, to };
}

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

/** Axis tick label: clock only inside a day, date · clock for multi-day windows. */
export function tickLabel(t: number, w: TimeWindow): string {
  return w.to - w.from > 20 * 3_600_000 ? fmtDate(t) : fmtClock(t);
}

export function famCounts(spans: Span[]): Record<string, number> {
  const c: Record<string, number> = {};
  for (const s of spans) if (s.kind === "tool" || s.kind === "agent") c[s.name] = (c[s.name] || 0) + 1;
  return c;
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

// ── critical path: longest child at every level ───────────────────────────
export function criticalPath(idx: SpanIndex, segSpans: Span[]): Set<string> {
  const set = new Set<string>();
  const turns = segSpans.filter((s) => s.kind === "turn");
  if (!turns.length) return set;
  let cur: Span | undefined = turns.reduce((a, b) => (b.dur > a.dur ? b : a));
  while (cur) {
    set.add(cur.id);
    const kids: Span[] = idx.children.get(cur.id) || [];
    cur = kids.length ? kids.reduce((a: Span, b: Span) => (b.dur > a.dur ? b : a)) : undefined;
  }
  return set;
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

/** Width of an element, tracked with ResizeObserver. Callback ref so a node
 *  mounted after the first render (behind a loading state) is still observed. */
export function useWidth<T extends HTMLElement>(): [(el: T | null) => void, number] {
  const [node, setNode] = useState<T | null>(null);
  const [w, setW] = useState(0);
  useEffect(() => {
    if (!node) return;
    const ro = new ResizeObserver(() => setW(node.clientWidth));
    ro.observe(node);
    setW(node.clientWidth);
    return () => ro.disconnect();
  }, [node]);
  return [setNode, w];
}

export function outputHref(o: { kind: string; ref: string }): string | undefined {
  return o.kind === "pr" || o.kind === "artifact" ? o.ref : undefined;
}

/** Tick positions for a time axis: ~7 ticks at a round interval. */
export function axisTicks(w: TimeWindow): number[] {
  const span = w.to - w.from;
  if (span <= 0) return [];
  const steps = [1000, 5000, 10_000, 30_000, 60_000, 120_000, 300_000, 600_000, 900_000, 1_800_000, 3_600_000, 7_200_000, 21_600_000, 43_200_000, 86_400_000];
  const step = steps.find((s) => span / s <= 8) || 86_400_000 * Math.ceil(span / 86_400_000 / 8);
  const out: number[] = [];
  for (let t = Math.ceil(w.from / step) * step; t <= w.to; t += step) out.push(t);
  return out;
}
