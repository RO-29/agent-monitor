import { useEffect, useMemo, useRef, useState } from "react";
import type { Span } from "../../api/types";
import { fmtClock, fmtDur } from "../../lib/format";
import { Icon } from "../../lib/icons";
import { axisTicks, descendants, famCounts, frac, tickLabel, type SpanIndex, type TimeWindow } from "./lib";

interface Props {
  idx: SpanIndex;
  spans: Span[]; // segment spans
  win: TimeWindow;
  selected?: string;
  expanded: Set<string>;
  onToggle: (id: string) => void;
  onSelect: (id: string) => void;
  crit?: Set<string>;
  filter: { turns: boolean; tools: boolean; agents: boolean; minDur: boolean; errorsOnly: boolean; query: string };
  onWheel: (e: React.WheelEvent<HTMLElement>) => void;
}

interface Row {
  s: Span;
  depth: number;
  hasKids: boolean;
  open: boolean;
  count?: string;
}

const ROW_H = 24;

// Visible rows: depth-0 spans (user prompts, turns) in time order; children of
// expanded turns/agents follow their parent. Only the rows inside the scroll
// viewport are mounted (row height is fixed at 24px).
export default function SpanTree({ idx, spans, win, selected, expanded, onToggle, onSelect, crit, filter, onWheel }: Props) {
  const rows = useMemo<Row[]>(() => {
    const out: Row[] = [];
    // synthetic "continued" turns are not in `spans`, so match on the segment number
    const segNo = spans.length ? spans[0].seg : -1;
    const q = filter.query.trim().toLowerCase();
    const match = (s: Span) => {
      if (filter.errorsOnly && !s.err && s.kind !== "turn" && s.kind !== "user") return false;
      if (filter.minDur && (s.kind === "tool" || s.kind === "agent") && s.dur < 1000) return false;
      if (q && !(`${s.name} ${s.res || ""} ${s.text || ""}`.toLowerCase().includes(q))) return false;
      return true;
    };
    const walk = (s: Span, depth: number) => {
      const kids = (idx.children.get(s.id) || []).filter((k) => (k.kind === "agent" ? filter.agents : filter.tools) && (k.depth <= 1 || filter.agents));
      const hasKids = kids.length > 0;
      const open = hasKids && expanded.has(s.id);
      let count: string | undefined;
      if (hasKids && !open) {
        const all = descendants(idx, s.id);
        const c = famCounts(all);
        const top = Object.entries(c).sort((a, b) => b[1] - a[1]).slice(0, 4).map(([n, k]) => `${n} ${k}`);
        count = `${all.length} spans · ${top.join(" · ")}`;
      }
      out.push({ s, depth, hasKids, open, count });
      if (open) for (const k of kids) if (match(k)) walk(k, depth + 1);
    };
    for (const r of idx.roots) {
      if (r.seg !== segNo) continue;
      if (r.kind === "turn" && !filter.turns) continue;
      if (r.kind === "user" && !filter.turns) continue;
      if (r.kind === "turn" || r.kind === "user") {
        if (r.kind === "turn" && q) {
          // when filtering, only keep turns with a matching descendant or match themselves
          const any = match(r) || descendants(idx, r.id).some(match);
          if (!any) continue;
        }
        walk(r, 0);
      } else if (match(r)) walk(r, 0);
    }
    return out;
  }, [idx, spans, expanded, filter]);

  const ref = useRef<HTMLDivElement>(null);
  const [scroll, setScroll] = useState(0);
  const [h, setH] = useState(600);
  useEffect(() => {
    const el = ref.current;
    if (!el) return;
    const ro = new ResizeObserver(() => setH(el.clientHeight));
    ro.observe(el);
    setH(el.clientHeight);
    return () => ro.disconnect();
  }, []);

  // keep the selected row in view when the selection changes
  useEffect(() => {
    if (!selected || !ref.current) return;
    const i = rows.findIndex((r) => r.s.id === selected);
    if (i < 0) return;
    const top = i * ROW_H;
    const el = ref.current;
    if (top < el.scrollTop || top + ROW_H > el.scrollTop + el.clientHeight) el.scrollTop = Math.max(0, top - el.clientHeight / 2);
  }, [selected, rows]);

  const total = rows.length * ROW_H;
  const first = Math.max(0, Math.floor(scroll / ROW_H) - 4);
  const last = Math.min(rows.length, Math.ceil((scroll + h) / ROW_H) + 4);
  // the tree's timeline column is narrower than the flame graph's: thin out
  // date-style labels so they never overlap
  const allTicks = axisTicks(win);
  const ticks = (win.to - win.from > 20 * 3_600_000 ? allTicks.filter((_, i) => i % 2 === 0) : allTicks).filter((t) => frac(t, win) < 0.93);
  const winSpan = win.to - win.from || 1;

  return (
    <>
      <div className="tr-tree-head">
        <div className="c-name k">span · resource</div>
        <div className="c-time">
          {ticks.map((t) => (
            <span key={t} className="tick" style={{ position: "absolute", left: `${frac(t, win) * 100}%`, top: 7, fontFamily: "var(--font-mono)", fontSize: 10, color: "var(--muted)", transform: frac(t, win) < 0.02 ? "translateX(4px)" : "translateX(-50%)" }}>
              {tickLabel(t, win)}
            </span>
          ))}
        </div>
        <div className="c-dur k">dur</div>
        <div className="c-pct k">%</div>
      </div>
      <div className="tr-tree" ref={ref} onScroll={(e) => setScroll((e.target as HTMLDivElement).scrollTop)} onWheel={onWheel}>
        {rows.length === 0 && <div className="tr-empty">No spans match the current filters.</div>}
        <div className="spacer" style={{ height: total }}>
          {rows.slice(first, last).map((r, i) => {
            const s = r.s;
            const y = (first + i) * ROW_H;
            const left = frac(s.ts, win) * 100;
            const w = Math.max((s.dur / winSpan) * 100, 0.15);
            const off = s.ts + s.dur < win.from || s.ts > win.to;
            const isRoot = s.kind === "turn" || s.kind === "user";
            const critDim = crit && crit.size > 0 && !crit.has(s.id) && s.kind !== "user";
            return (
              <div
                key={s.id}
                className={`tr-row ${s.id === selected ? "sel" : ""} ${r.hasKids && !r.open && s.kind === "turn" ? "dim" : ""} ${critDim ? "crit-dim" : ""}`}
                style={{ top: y }}
                onClick={() => onSelect(s.id)}
                title={`${s.name}${s.res ? " · " + s.res : ""} · ${fmtClock(s.ts)} · ${fmtDur(s.dur)}`}
              >
                <div className="c-name" style={{ paddingLeft: 10 + r.depth * 16 }}>
                  <span
                    className="chev"
                    onClick={(e) => {
                      if (!r.hasKids) return;
                      e.stopPropagation();
                      onToggle(s.id);
                    }}
                  >
                    {r.hasKids ? <Icon name={r.open ? "chevd" : "chev"} size={11} color="var(--muted)" /> : null}
                  </span>
                  <span className={`sq ${s.err ? "err" : ""}`} style={{ background: s.kind === "user" ? "var(--fam-user)" : `var(--fam-${s.fam})` }} />
                  <span className={`nm ${isRoot ? "strong" : ""}`}>{s.kind === "user" ? "you" : s.name}</span>
                  <span className="res">{s.kind === "user" || s.kind === "turn" ? s.text : s.res}</span>
                  {r.count && <span className="cnt">{r.count}</span>}
                  {s.flag === "correction" && (
                    <span className="tr-src correction" style={{ marginLeft: 4 }}>
                      <Icon name="flag" size={9} color="var(--yellow)" sw={2.4} /> learning
                    </span>
                  )}
                  {s.flag === "aborted" && <span className="tr-src heur" style={{ marginLeft: 4 }}>aborted</span>}
                  {s.err && <span className="tr-src error" style={{ marginLeft: 4 }}>error</span>}
                </div>
                <div className="c-time">
                  {!off &&
                    (s.kind === "user" ? (
                      <span className="mark" style={{ left: `${left}%` }} />
                    ) : (
                      <span className={`bar ${s.err ? "err" : ""}`} style={{ left: `${left}%`, width: `${w}%`, background: `var(--fam-${s.fam})`, opacity: s.kind === "turn" ? 1 : 0.95 }} />
                    ))}
                </div>
                <div className="c-dur">{s.kind === "user" ? "" : fmtDur(s.dur)}</div>
                <div className="c-pct">{s.kind === "user" ? "" : `${((s.dur / winSpan) * 100).toFixed(1)}%`}</div>
              </div>
            );
          })}
        </div>
      </div>
    </>
  );
}
