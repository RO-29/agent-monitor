import { useMemo } from "react";
import type { Segment, Span } from "../../api/types";
import { fmtClock, fmtDur } from "../../lib/format";
import { axisTicks, clamp, frac, tickLabel, type SpanIndex, type TimeWindow } from "./lib";

interface Props {
  segment: Segment;
  segments: Segment[];
  spans: Span[]; // spans of the selected segment
  idx: SpanIndex;
  win: TimeWindow;
  width: number; // px width of the timeline area
  selected?: string;
  crit?: Set<string>;
  showTurns: boolean;
  showTools: boolean;
  showAgents: boolean;
  onSelect: (id: string) => void;
  onWheel: (e: React.WheelEvent<HTMLElement>) => void;
  onAxisDrag: (dxFrac: number) => void;
}

const ROW_H = 19;
const BAR_H = 16;
const ROWS = ["segment", "turns", "tools", "subagents", "depth 2"];

// Rows by depth: 0 = the segment bar, 1 = turns (+ user marks), 2 = tools,
// 3 = subagent children, 4 = deeper. Bars thinner than 0.5px are skipped unless
// selected — a 9k-span session must stay smooth.
export default function FlameGraph({ segment, segments, spans, idx, win, width, selected, crit, showTurns, showTools, showAgents, onSelect, onWheel, onAxisDrag }: Props) {
  const W = Math.max(width, 10);
  const x = (ts: number) => frac(ts, win) * W;
  const span = win.to - win.from || 1;

  const bars = useMemo(() => {
    const out: { s: Span; row: number; x: number; w: number }[] = [];
    for (const s of spans) {
      if (s.ts + s.dur < win.from || s.ts > win.to) continue;
      let row: number;
      if (s.kind === "user" || s.kind === "turn") row = 1;
      else if (s.depth <= 1) row = 2;
      else if (s.depth === 2) row = 3;
      else row = 4;
      if (row === 1 && !showTurns) continue;
      if (row === 2 && !showTools && s.kind === "tool") continue;
      if ((s.kind === "agent" || row >= 3) && !showAgents) continue;
      const bx = x(s.ts);
      const bw = s.kind === "user" ? 2 : Math.max((s.dur / span) * W, 1);
      if (bw < 0.5 && s.id !== selected) continue;
      out.push({ s, row, x: bx, w: bw });
    }
    return out;
  }, [spans, win, W, showTurns, showTools, showAgents, selected, span]);

  const ticks = axisTicks(win);
  const boundaries = segments.filter((sg) => sg.boundary.at >= win.from && sg.boundary.at <= win.to);
  const segFrom = clamp(x(segment.fromTs), 0, W);
  const segTo = clamp(x(segment.toTs || win.to), 0, W);

  let dragX: number | null = null;
  const onDown = (e: React.MouseEvent) => {
    dragX = e.clientX;
    const move = (ev: MouseEvent) => {
      if (dragX == null) return;
      const dx = ev.clientX - dragX;
      dragX = ev.clientX;
      onAxisDrag(-dx / W);
    };
    const up = () => {
      dragX = null;
      window.removeEventListener("mousemove", move);
      window.removeEventListener("mouseup", up);
    };
    window.addEventListener("mousemove", move);
    window.addEventListener("mouseup", up);
  };

  const famFill = (s: Span) => (s.kind === "user" ? "var(--fam-user)" : `var(--fam-${s.fam})`);
  const label = `${segment.boundary.kind === "compact" ? "⟲ compact" : segment.boundary.kind} · ${fmtClock(segment.fromTs)} · segment ${segment.index + 1} · ${fmtDur(segment.toTs - segment.fromTs)}`;

  return (
    <div className="tr-flame">
      <div className="tr-axis">
        <div className="lab k" style={{ width: 120 }}>flame graph</div>
        <div className="ticks" onMouseDown={onDown} onWheel={onWheel}>
          {ticks.map((t) => (
            <span key={t} className="tick" style={{ left: `${frac(t, win) * 100}%`, transform: frac(t, win) < 0.02 ? "translateX(4px)" : undefined }}>
              {tickLabel(t, win)}
            </span>
          ))}
        </div>
      </div>
      <div className="tr-flame-rows" style={{ height: ROWS.length * ROW_H + 10 }} onWheel={onWheel}>
        {ROWS.map((r) => (
          <div key={r} className="tr-flame-row">
            <div className="lab">{r}</div>
          </div>
        ))}
        <div className="tr-flame-svg">
          <svg viewBox={`0 0 ${W} ${ROWS.length * ROW_H}`} preserveAspectRatio="none">
            {/* segment bar */}
            <rect className="bar" x={segFrom} y={0} width={Math.max(segTo - segFrom, 1)} height={BAR_H} fill="var(--bg-4)" />
            {segTo - segFrom > 240 && (
              <text x={segFrom + 8} y={12}>{label}</text>
            )}
            {/* boundaries */}
            {boundaries.map((sg) => (
              <line key={sg.id} x1={x(sg.boundary.at)} x2={x(sg.boundary.at)} y1={0} y2={ROWS.length * ROW_H} stroke="var(--orange)" strokeDasharray="3 3" strokeWidth={1.5} />
            ))}
            {bars.map(({ s, row, x: bx, w }) => {
              const dim = crit && crit.size > 0 && !crit.has(s.id) && s.kind !== "user";
              const sel = s.id === selected;
              return (
                <g key={s.id} className={dim ? "dim" : undefined} onClick={() => onSelect(s.id)} style={{ cursor: "pointer" }}>
                  <rect className="bar" x={bx} y={row * ROW_H} width={w} height={BAR_H} fill={famFill(s)} opacity={s.kind === "turn" ? 1 : 0.95} stroke={sel ? "var(--text)" : s.err ? "var(--red)" : "none"} strokeWidth={sel ? 2 : 1.5} />
                  {s.kind === "turn" && w > 70 && <text x={bx + 5} y={row * ROW_H + 12}>{`${s.name} · ${fmtDur(s.dur)}`}</text>}
                  <title>{`${s.name}${s.res ? " · " + s.res : ""}\n${fmtClock(s.ts)} · ${fmtDur(s.dur)}${s.err ? " · error" : ""}`}</title>
                </g>
              );
            })}
          </svg>
        </div>
      </div>
    </div>
  );
}
