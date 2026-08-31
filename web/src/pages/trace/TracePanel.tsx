import { useCallback, useEffect, useMemo, useState } from "react";
import { useSearchParams } from "react-router-dom";
import { api } from "../../api/client";
import type { Segment } from "../../api/types";
import { famLabel, fmtDate, fmtDur, fmtDuration, fmtTime, fmtUsd } from "../../lib/format";
import { Icon } from "../../lib/icons";
import { ChapterBand } from "./ChapterBand";
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

// Session trace: summary strip · chapter band · toolbar · minimap · flame
// graph · span tree · span detail. The URL carries ?seg= and ?span= so a
// permalink reopens the same view.
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
  const filterState = { turns: showTurns, tools: showTools, agents: showAgents, minDur, errorsOnly, query };
  const selSpan = selected ? idx.byId.get(selected) || null : null;

  return (
    <div className="tr-root">
      {/* segment strip */}
      <div style={{ display: "flex", alignItems: "center", gap: 12, padding: "8px 16px", borderBottom: "1px solid var(--border)", flex: "none" }}>
        <span className="k" style={{ flex: "none" }}>segments</span>
        <div className="tr-segstrip">
          {trace.segments.map((s, i) => {
            const w = Math.max((s.toTs || trace.lastTs) - s.fromTs, 60_000);
            const meta = BOUNDARY[s.boundary.kind] || BOUNDARY.start;
            return (
              <div
                key={s.id}
                className={`tr-seg ${i === segIndex ? "cur" : i < segIndex ? "prev" : ""}`}
                style={{ flex: `${w} 1 0` }}
                onClick={() => setSeg(i)}
                title={`segment ${i + 1} · ${meta.label} · ${fmtDate(s.fromTs)} · ${fmtDur(w)}`}
              >
                <span className={`tr-bd sm ${meta.cls}`}>{meta.glyph}</span>
                <span style={{ overflow: "hidden", textOverflow: "ellipsis" }}>
                  {s.boundary.kind === "compact" ? `compact ${i}` : s.boundary.kind === "start" ? "start" : s.boundary.kind}
                  {s.boundary.droppedTokens ? ` · −${(s.boundary.droppedTokens / 1000).toFixed(0)}k` : ""}
                </span>
              </div>
            );
          })}
        </div>
        <span className="num muted" style={{ fontSize: 11, flex: "none" }}>
          {trace.segments.filter((s) => s.boundary.kind === "compact").length} compacts · {trace.segments.filter((s) => s.boundary.kind === "clear").length} clear ·{" "}
          {fmtTokShort(trace.segments.reduce((a, s) => a + (s.boundary.droppedTokens || 0), 0))} tokens dropped
        </span>
      </div>

      {/* summary strip */}
      <div className="tr-summary">
        <div className="tr-kpis">
          {[
            ["trace · segment " + (segIndex + 1), fmtDur(segDur), "var(--text)"],
            ["start", fmtTime(seg.fromTs), "var(--text)"],
            ["spans", String(seg.spans), "var(--text)"],
            ["errors", String(segErrors), segErrors ? "var(--red)" : "var(--text)"],
            ["cost", fmtUsd(seg.usdEst, trace.costEstimated), "var(--text)"],
          ].map(([k, v, c]) => (
            <div key={k}>
              <div className="k">{k}</div>
              <div className="v" style={{ color: c }}>{v}</div>
            </div>
          ))}
        </div>
        <div className="tr-exec">
          <div style={{ display: "flex", justifyContent: "space-between", alignItems: "center" }}>
            <span className="k">execution time by tool family</span>
            <span className={`tr-chip sm ${crit ? "on" : ""}`} onClick={() => setCrit(!crit)}>
              <Icon name="pulse" size={10} /> critical path
            </span>
          </div>
          <div className="tr-exec-bar">
            {bd.byFam.map((b) => (
              <div key={b.fam} style={{ width: `${b.pct}%`, background: `var(--fam-${b.fam})` }} title={`${famLabel(b.fam)} ${fmtDur(b.ms)}`} />
            ))}
          </div>
          <div className="tr-legend">
            {bd.byFam.map((b) => (
              <span key={b.fam}>
                <i style={{ background: `var(--fam-${b.fam})` }} /> {famLabel(b.fam)} <span className="num muted">{b.pct.toFixed(0)}%</span>
              </span>
            ))}
          </div>
        </div>
      </div>

      <ChapterBand segment={seg} onEnrich={runEnrich} enriching={enriching} enrichError={enrichError} loadMs={loadMs} />

      {/* toolbar */}
      <div className="tr-toolbar">
        <span className={`tr-chip ${view === "both" ? "on" : ""}`} onClick={() => setView("both")}>flame + spans</span>
        <span className={`tr-chip ${view === "spans" ? "on" : ""}`} onClick={() => setView("spans")}>spans only</span>
        <span className="tr-sep" />
        <span className={`tr-chip ${showTurns ? "on" : ""}`} onClick={() => setShowTurns(!showTurns)}>turns</span>
        <span className={`tr-chip ${showTools ? "on" : ""}`} onClick={() => setShowTools(!showTools)}>tools</span>
        <span className={`tr-chip ${showAgents ? "on" : ""}`} onClick={() => setShowAgents(!showAgents)}>subagents</span>
        <span className={`tr-chip ${minDur ? "on" : ""}`} onClick={() => setMinDur(!minDur)}>min dur ≥ 1s</span>
        <span className={`tr-chip err ${errorsOnly ? "on" : ""}`} onClick={() => setErrorsOnly(!errorsOnly)}>errors only</span>
        <span className="tr-sep" />
        <span className="tr-chip" onClick={fitSegment}><Icon name="zoom" size={11} /> fit segment</span>
        <span className="tr-chip" onClick={fitSession}>fit session</span>
        <div style={{ flex: 1 }} />
        <label className="tr-filter">
          <Icon name="search" size={12} />
          <input placeholder="filter spans · resource, tool, text" value={query} onChange={(e) => setQuery(e.target.value)} />
        </label>
      </div>

      {/* minimap */}
      <div className="tr-minimap">
        <div className="lab k" style={{ whiteSpace: "nowrap" }}>session · {fmtDuration(totalSpan)}</div>
        <div
          className="track"
          onClick={(e) => {
            const r = (e.currentTarget as HTMLDivElement).getBoundingClientRect();
            const t = trace.firstTs + ((e.clientX - r.left) / r.width) * totalSpan;
            const i = trace.segments.findIndex((s, k) => t >= s.fromTs && (k === trace.segments.length - 1 || t < trace.segments[k + 1].fromTs));
            if (i >= 0 && i !== segIndex) setSeg(i);
          }}
        >
          {trace.segments.map((s, i) => (
            <div
              key={s.id}
              className="mseg"
              style={{
                left: `${((s.fromTs - trace.firstTs) / totalSpan) * 100}%`,
                width: `${(((s.toTs || trace.lastTs) - s.fromTs) / totalSpan) * 100}%`,
                background: s.boundary.kind === "compact" ? `rgba(251,146,60,${i % 2 ? 0.14 : 0.2})` : s.boundary.kind === "clear" ? "rgba(96,165,250,.18)" : "rgba(74,222,128,.14)",
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
          <span className="mlab" style={{ left: 6 }}>{fmtDate(trace.firstTs)}</span>
          <span className="mlab" style={{ right: 6 }}>{fmtDate(trace.lastTs)}</span>
        </div>
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
          <SpanTree idx={idx} spans={segSpans} win={win} selected={selected} expanded={expanded} onToggle={toggle} onSelect={select} crit={critSet} filter={filterState} onWheel={onWheel} />
        </div>
        <SpanDetail sessionId={sessionId} span={selSpan} segment={seg} idx={idx} model={trace.model} onSelect={select} onEnrich={runEnrich} enriching={enriching} enrichError={enrichError} />
      </div>
    </div>
  );
}

function fmtTokShort(n: number) {
  if (n >= 1e6) return (n / 1e6).toFixed(1) + "M";
  if (n >= 1e3) return (n / 1e3).toFixed(0) + "k";
  return String(n);
}
