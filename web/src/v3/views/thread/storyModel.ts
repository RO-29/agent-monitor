// Turns a session trace into story items: dividers, prompts, work, errors, children, replies.
import type { Segment, Span, TraceFull } from "../../../api/types";
import { buildGroups, indexSpans, type SpanIndex, type TurnGroup } from "../../../pages/trace/lib";
import { spanDur } from "../../components/ui";

export interface StoryGroup {
  seg: Segment;
  g: TurnGroup;
  turnNo: number; // number from "turn N", used by the trace route
  reply?: Span; // last turn with prose
  errors: Span[];
  children: Span[];
  first: boolean; // first group of its segment
}

export function cleanSpans(t: TraceFull | null): Span[] {
  if (!t) return [];
  return t.spans.map((s) => ({ ...s, dur: spanDur(s) }));
}

export const turnNo = (s?: Span): number => {
  const m = s ? /turn\s+([\d,]+)/.exec(s.name) : null;
  return m ? Number(m[1].replace(/,/g, "")) : -1;
};

export function buildStory(t: TraceFull | null, spans: Span[]): { idx: SpanIndex; groups: StoryGroup[] } {
  const idx = indexSpans(spans);
  const out: StoryGroup[] = [];
  if (!t) return { idx, groups: out };
  const segs = t.segments.length ? t.segments : [{ index: 0, boundary: { kind: "start", at: t.firstTs }, fromTs: t.firstTs, toTs: t.lastTs } as Segment];
  for (const seg of segs) {
    const gs = buildGroups(idx, seg.index);
    gs.forEach((g, i) => {
      const reply = [...g.turns].reverse().find((x) => (x.text || "").trim());
      out.push({
        seg,
        g,
        turnNo: turnNo(g.turns[0]),
        reply,
        errors: g.calls.filter((c) => c.err),
        children: g.calls.filter((c) => c.child || c.kind === "agent"),
        first: i === 0,
      });
    });
  }
  return { idx, groups: out };
}

/** Collapses runs of the same tool into one waterfall row. */
export interface WfRow {
  name: string;
  fam: string;
  from: number;
  to: number;
  dur: number;
  count: number;
  err: boolean;
  first: Span;
  depth: number;
  child?: string;
}
export function waterfall(calls: Span[], max = 40): WfRow[] {
  const rows: WfRow[] = [];
  for (const c of calls) {
    const last = rows[rows.length - 1];
    const label = c.fam === "bash" || c.kind === "agent" ? `${c.name} ${c.res || ""}`.trim() : c.name;
    if (last && last.name === c.name && c.fam !== "bash" && c.kind !== "agent" && !c.child && last.depth === c.depth) {
      last.count++;
      last.to = Math.max(last.to, c.ts + c.dur);
      last.dur += c.dur;
      last.err = last.err || !!c.err;
      continue;
    }
    rows.push({ name: label, fam: c.fam, from: c.ts, to: c.ts + c.dur, dur: c.dur, count: 1, err: !!c.err, first: c, depth: c.depth, child: c.child });
  }
  return rows.slice(0, max);
}
