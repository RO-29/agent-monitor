import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { useSearchParams } from "react-router-dom";
import { api } from "../../api/client";
import type { Segment } from "../../api/types";
import { famLabel, fmtDate, fmtDur, fmtDuration, fmtUsd } from "../../lib/format";
import { Icon } from "../../lib/icons";
import ChapterCard from "./ChapterBand";
import FlameGraph from "./FlameGraph";
import SpanDetail from "./SpanDetail";
import SpanTree from "./SpanTree";
import { BOUNDARY, breakdown, clamp, criticalPath, frac, indexSpans, segmentWindow, useTrace, useWidth, type TimeWindow } from "./lib";
import "./trace.css";

export interface TracePanelProps {
  sessionId: string;
  /** open on this segment (default = last segment) */
  segment?: number;
  /** preselect a span */
  spanId?: string;
}

// Session trace: one context row, one control row, then the data. Segment
// stats, the tool-family split and the chapter live in the first row; view
// filters and zoom sit behind two popovers. The URL carries ?seg= and ?span=.
export default function TracePanel({ sessionId, segment, spanId }: TracePanelProps) {
  const { trace, loading, error, loadMs, reload } = useTrace(sessionId);
  const [params, setParams] = useSearchParams();

  const segCount = trace?.segments.length || 0;
  const paramSeg = params.get("seg");
  const segIndex = clamp(segment ?? (paramSeg != null ? Number(paramSeg) : segCount - 1), 0, Math.max(segCount - 1, 0));
  const seg: Segment | undefined = trace?.segments[segIndex];
  const selected = spanId ?? params.get("span") ?? undefined;

  const [win, setWin] = useState<TimeWindow | null>(null);
  const [view, setView] = useState<"both" | "spans">("both");
  const [showTurns, setShowTurns] = useState(true);
  const [showTools, setShowTools] = useState(true);
  const [showAgents, setShowAgents] = useState(true);
  const [minDur, setMinDur] = useState(false);
  const [errorsOnly, setErrorsOnly] = useState(false);
  const [crit, setCrit] = useState(false);
  const [query, setQuery] = useState("");
  const [expanded, setExpanded] = useState<Set<string>>(new Set());
  const [chapterOpen, setChapterOpen] = useState(false);
  const [menu, setMenu] = useState<"" | "filters" | "fit" | "seg">("");
  const [enriching, setEnriching] = useState(false);
  const [enrichError, setEnrichError] = useState<string | null>(null);

  const idx = useMemo(() => indexSpans(trace?.spans || []), [trace]);
  const segSpans = useMemo(() => (trace ? trace.spans.filter((s) => s.seg === segIndex) : []), [trace, segIndex]);
  const flameSpans = useMemo(() => [...segSpans, ...idx.synthetic.filter((s) => s.seg === segIndex)], [segSpans, idx, segIndex]);
  const critSet = useMemo(() => (crit ? criticalPath(idx, flameSpans) : undefined), [crit, idx, flameSpans]);
  const bd = useMemo(() => breakdown(segSpans), [segSpans]);

  // Default window = the selected segment; a live session at its edge grows.
  useEffect(() => {
    if (!trace || !seg) return;
    const next = segmentWindow(seg, trace.lastTs);
    setWin((prev) => {
      if (!prev) return next;
      const sameSeg = prev.from >= seg.fromTs - 1 && prev.to <= next.to + 1;
      if (!sameSeg) return next;
      // extend when the user is at the live edge
      if (Math.abs(prev.to - (trace.lastTs - 1)) < 120_000 || prev.to > next.to - 1000) return { from: prev.from, to: Math.max(prev.to, next.to) };
      return prev;
    });
  }, [trace, seg?.id]);

  // Turn containing the selected span (or the last turn) starts open.
  useEffect(() => {
    if (!trace) return;
    setExpanded((prev) => {
      const n = new Set(prev);
      const sel = selected ? idx.byId.get(selected) : undefined;
      let cur = sel;
      while (cur && cur.parent) {
        n.add(cur.parent);
        cur = idx.byId.get(cur.parent);
      }
      if (!sel) {
        const turns = segSpans.filter((s) => s.kind === "turn");
        if (turns.length) n.add(turns[turns.length - 1].id);
      }
      return n;
    });
  }, [trace, selected, segIndex]);

  const setSeg = useCallback(
    (i: number) => {
      const p = new URLSearchParams(params);
      p.set("seg", String(i));
      p.delete("span");
      setParams(p, { replace: true });
    },
    [params, setParams],
  );
  const select = useCallback(
    (id: string) => {
      const p = new URLSearchParams(params);
      if (p.get("span") === id) p.delete("span");
      else p.set("span", id);
      if (!p.get("seg")) p.set("seg", String(segIndex));
      setParams(p, { replace: true });
      const sp = idx.byId.get(id);
      if (sp && win && (sp.ts < win.from || sp.ts > win.to)) {
        const s = trace?.segments[sp.seg];
        if (s && trace) setWin(segmentWindow(s, trace.lastTs));
      }
    },
    [params, setParams, segIndex, idx, win, trace],
  );
  const toggle = (id: string) =>
    setExpanded((prev) => {
      const n = new Set(prev);
      if (n.has(id)) n.delete(id);
      else n.add(id);
      return n;
    });

  // zoom around the cursor; pan by dragging the axis / minimap
  const onWheel = (e: React.WheelEvent<HTMLElement>) => {
    if (!win || !trace) return;
    if (Math.abs(e.deltaY) < Math.abs(e.deltaX)) return; // horizontal scroll = pan
    e.preventDefault();
    const rect = (e.currentTarget as HTMLElement).getBoundingClientRect();
    const f = clamp((e.clientX - rect.left) / rect.width, 0, 1);
    const factor = Math.exp(e.deltaY * 0.0015);
    const span = win.to - win.from;
    const nspan = clamp(span * factor, 2000, trace.lastTs - trace.firstTs + 60_000);
    const anchor = win.from + f * span;
    setWin({ from: anchor - f * nspan, to: anchor + (1 - f) * nspan });
  };
  const pan = (dxFrac: number) => {
    if (!win) return;
    const d = dxFrac * (win.to - win.from);
    setWin({ from: win.from + d, to: win.to + d });
  };
  const fitSegment = () => trace && seg && setWin(segmentWindow(seg, trace.lastTs));
  const fitSession = () => trace && setWin({ from: trace.firstTs, to: Math.max(trace.lastTs, trace.firstTs + 60_000) });

  const runEnrich = async () => {
    if (!trace) return;
    setEnriching(true);
    setEnrichError(null);
    try {
      await api.enrich(sessionId, segIndex, true);
      reload();
    } catch (e) {
      setEnrichError((e as Error).message);
    } finally {
      setEnriching(false);
    }
  };

  const [timelineRef, tlWidth] = useWidth<HTMLDivElement>();

  if (error) return <div className="tr-root"><div className="tr-error">trace: {error}</div></div>;
  if (!trace || !seg || !win) return <div className="tr-root"><div className="tr-loading">{loading ? "loading trace…" : "no trace for this session"}</div></div>;

  const segDur = (seg.toTs || trace.lastTs) - seg.fromTs;
  const totalSpan = Math.max(trace.lastTs - trace.firstTs, 1);
  const segErrors = seg.errors;
  const chapCount = (seg.chapter?.learnings?.length || 0) + (seg.chapter?.open?.length || 0) + (seg.chapter?.intentChanges?.length || 0);
  const filterState = { turns: showTurns, tools: showTools, agents: showAgents, minDur, errorsOnly, query };
  const selSpan = selected ? idx.byId.get(selected) || null : null;

  const famTitle = bd.byFam.map((b) => `${famLabel(b.fam)} ${b.pct.toFixed(0)}% · ${fmtDur(b.ms)}`).join("\n");
  const filtersOn = [!showTurns, !showTools, !showAgents, minDur, errorsOnly, crit].filter(Boolean).length;
  const segMeta = BOUNDARY[seg.boundary.kind] || BOUNDARY.start;

  return (
    <div className="tr-root">
      {/* row 1: what you are looking at */}
      <div className="tr-bar">
        <button className="tr-ico" disabled={segIndex === 0} onClick={() => setSeg(segIndex - 1)} title="previous segment">
          <Icon name="chevl" size={13} />
        </button>
        <Pop open={menu === "seg"} onOpen={(o) => setMenu(o ? "seg" : "")} label={
          <>
            <span className={`tr-bd sm ${segMeta.cls}`}>{segMeta.glyph}</span>
            seg <b>{segIndex + 1}</b>/{segCount}
            <span className="tr-dim">{seg.boundary.kind === "compact" ? `compact · ${seg.boundary.trigger || "auto"}` : segMeta.label}</span>
          </>
        }>
          <div className="tr-menu seg">
            {trace.segments.map((sg, i) => {
              const m = BOUNDARY[sg.boundary.kind] || BOUNDARY.start;
              return (
                <button key={sg.id} className={i === segIndex ? "on" : ""} onClick={() => { setSeg(i); setMenu(""); }}>
                  <span className={`tr-bd sm ${m.cls}`}>{m.glyph}</span>
                  <span className="num">{i + 1}</span>
                  <span className="ell">{sg.boundary.kind === "compact" ? "compact" : m.label}</span>
                  <span className="tr-dim num">{fmtDur((sg.toTs || trace.lastTs) - sg.fromTs)}</span>
                  {sg.boundary.droppedTokens ? <span className="tr-dim num">−{Math.round(sg.boundary.droppedTokens / 1000)}k</span> : null}
                </button>
              );
            })}
          </div>
        </Pop>
        <button className="tr-ico" disabled={segIndex >= segCount - 1} onClick={() => setSeg(segIndex + 1)} title="next segment">
          <Icon name="chev" size={13} />
        </button>

        <span className="tr-stats" title={`starts ${fmtDate(seg.fromTs)}`}>
          <b>{fmtDur(segDur)}</b>
          <i className="opt" />
          <span className="opt">{seg.spans.toLocaleString()} spans</span>
          <i />
          <span className={segErrors ? "bad" : ""}>{segErrors} err</span>
          <i className="opt" />
          <span className="opt">{fmtUsd(seg.usdEst, trace.costEstimated)}</span>
        </span>

        <span className="tr-fam" title={famTitle || "no tool time in this segment"}>
          {bd.byFam.map((b) => (
            <span key={b.fam} style={{ width: `${b.pct}%`, background: `var(--fam-${b.fam})` }} />
          ))}
        </span>

        <span className="point" title={seg.chapter?.point || ""}>{seg.chapter?.point || "no chapter for this segment yet"}</span>

        <button className={`tr-chip ${chapterOpen ? "on" : ""}`} onClick={() => setChapterOpen(!chapterOpen)}>
          chapter
          {chapCount > 0 && <span className="num">{chapCount}</span>}
          <Icon name={chapterOpen ? "chevd" : "chev"} size={10} />
        </button>
      </div>

      {chapterOpen && (
        <div className={`tr-chapter ${seg.boundary.kind}`}>
          <ChapterCard segment={seg} onEnrich={runEnrich} enriching={enriching} enrichError={enrichError} loadMs={loadMs} />
        </div>
      )}

      {/* row 2: how you are looking at it */}
      <div className="tr-bar2">
        <div
          className="track"
          title={`session · ${fmtDuration(totalSpan)} · ${fmtDate(trace.firstTs)} → ${fmtDate(trace.lastTs)}`}
          onClick={(e) => {
            const r = (e.currentTarget as HTMLDivElement).getBoundingClientRect();
            const t = trace.firstTs + ((e.clientX - r.left) / r.width) * totalSpan;
            const i = trace.segments.findIndex((s2, k) => t >= s2.fromTs && (k === trace.segments.length - 1 || t < trace.segments[k + 1].fromTs));
            if (i >= 0 && i !== segIndex) setSeg(i);
          }}
        >
          {trace.segments.map((s2, i) => (
            <div
              key={s2.id}
              className="mseg"
              style={{
                left: `${((s2.fromTs - trace.firstTs) / totalSpan) * 100}%`,
                width: `${(((s2.toTs || trace.lastTs) - s2.fromTs) / totalSpan) * 100}%`,
                background: s2.boundary.kind === "compact" ? `rgba(251,146,60,${i % 2 ? 0.14 : 0.22})` : s2.boundary.kind === "clear" ? "rgba(96,165,250,.2)" : "rgba(74,222,128,.16)",
              }}
            />
          ))}
          <div
            className="brush"
            style={{ left: `${clamp(frac(win.from, { from: trace.firstTs, to: trace.lastTs }), 0, 1) * 100}%`, width: `${clamp((win.to - win.from) / totalSpan, 0.004, 1) * 100}%` }}
            onMouseDown={(e) => {
              e.stopPropagation();
              const track = (e.currentTarget.parentElement as HTMLDivElement).getBoundingClientRect();
              let last = e.clientX;
              const move = (ev: MouseEvent) => {
                const d = ((ev.clientX - last) / track.width) * totalSpan;
                last = ev.clientX;
                setWin((w) => (w ? { from: w.from + d, to: w.to + d } : w));
              };
              const up = () => {
                window.removeEventListener("mousemove", move);
                window.removeEventListener("mouseup", up);
              };
              window.addEventListener("mousemove", move);
              window.addEventListener("mouseup", up);
            }}
            onClick={(e) => e.stopPropagation()}
          />
        </div>

        <label className="tr-filter">
          <Icon name="search" size={12} />
          <input placeholder="filter spans" value={query} onChange={(e) => setQuery(e.target.value)} />
        </label>

        <Pop open={menu === "filters"} onOpen={(o) => setMenu(o ? "filters" : "")} label={<>filters{filtersOn > 0 && <span className="num">{filtersOn}</span>}<Icon name="chevd" size={10} /></>} on={filtersOn > 0}>
          <div className="tr-menu">
            <div className="k">rows</div>
            <button className={showTurns ? "on" : ""} onClick={() => setShowTurns(!showTurns)}><Tick on={showTurns} /> turns</button>
            <button className={showTools ? "on" : ""} onClick={() => setShowTools(!showTools)}><Tick on={showTools} /> tools</button>
            <button className={showAgents ? "on" : ""} onClick={() => setShowAgents(!showAgents)}><Tick on={showAgents} /> subagents</button>
            <div className="k">only</div>
            <button className={minDur ? "on" : ""} onClick={() => setMinDur(!minDur)}><Tick on={minDur} /> slower than 1s</button>
            <button className={errorsOnly ? "on" : ""} onClick={() => setErrorsOnly(!errorsOnly)}><Tick on={errorsOnly} /> errors</button>
            <button className={crit ? "on" : ""} onClick={() => setCrit(!crit)}><Tick on={crit} /> critical path</button>
            <div className="k">layout</div>
            <button className={view === "both" ? "on" : ""} onClick={() => setView("both")}><Tick on={view === "both"} /> flame graph + spans</button>
            <button className={view === "spans" ? "on" : ""} onClick={() => setView("spans")}><Tick on={view === "spans"} /> spans only</button>
          </div>
        </Pop>

        <Pop open={menu === "fit"} onOpen={(o) => setMenu(o ? "fit" : "")} label={<><Icon name="zoom" size={11} /> fit<Icon name="chevd" size={10} /></>}>
          <div className="tr-menu">
            <button onClick={() => { fitSegment(); setMenu(""); }}>this segment</button>
            <button onClick={() => { fitSession(); setMenu(""); }}>whole session · {fmtDuration(totalSpan)}</button>
          </div>
        </Pop>
      </div>

      <div className="tr-main">
        <div className="tr-left">
          {view === "both" && (
            <div ref={timelineRef} style={{ position: "relative" }}>
              <FlameGraph
                segment={seg}
                segments={trace.segments}
                spans={flameSpans}
                idx={idx}
                win={win}
                width={Math.max(tlWidth - 120, 10)}
                selected={selected}
                crit={critSet}
                showTurns={showTurns}
                showTools={showTools}
                showAgents={showAgents}
                onSelect={select}
                onWheel={onWheel}
                onAxisDrag={pan}
              />
            </div>
          )}
          <SpanTree idx={idx} spans={segSpans} win={win} selected={selected} expanded={expanded} onToggle={toggle} onSelect={select} crit={critSet} filter={filterState} onWheel={onWheel} ticks={view !== "both"} />
        </div>
        <SpanDetail sessionId={sessionId} span={selSpan} segment={seg} idx={idx} model={trace.model} onSelect={select} onEnrich={runEnrich} enriching={enriching} enrichError={enrichError} />
      </div>
    </div>
  );
}

/** Small dropdown: a chip that opens a menu and closes on any outside click. */
function Pop({ open, onOpen, label, on, children }: { open: boolean; onOpen: (o: boolean) => void; label: React.ReactNode; on?: boolean; children: React.ReactNode }) {
  const ref = useRef<HTMLSpanElement>(null);
  useEffect(() => {
    if (!open) return;
    const away = (e: MouseEvent) => {
      if (!ref.current?.contains(e.target as Node)) onOpen(false);
    };
    const esc = (e: KeyboardEvent) => e.key === "Escape" && onOpen(false);
    window.addEventListener("mousedown", away);
    window.addEventListener("keydown", esc);
    return () => {
      window.removeEventListener("mousedown", away);
      window.removeEventListener("keydown", esc);
    };
  }, [open, onOpen]);
  return (
    <span className="tr-pop" ref={ref}>
      <span className={`tr-chip ${open || on ? "on" : ""}`} onClick={() => onOpen(!open)}>{label}</span>
      {open && children}
    </span>
  );
}

function Tick({ on }: { on: boolean }) {
  return <span className={`tr-tick ${on ? "on" : ""}`}>{on ? "✓" : ""}</span>;
}
