import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { useSearchParams } from "react-router-dom";
import { api } from "../../api/client";
import type { Segment } from "../../api/types";
import { famLabel, fmtDate, fmtDur, fmtDuration, fmtUsd } from "../../lib/format";
import { Icon } from "../../lib/icons";
import ChapterCard from "./ChapterBand";
import ProfilePanel from "./ProfilePanel";
import SpanDetail from "./SpanDetail";
import TurnList from "./TurnList";
import { BOUNDARY, breakdown, buildGroups, clamp, indexSpans, useTrace } from "./lib";
import "./trace.css";

export interface TracePanelProps {
  sessionId: string;
  /** open on this segment (default = last segment) */
  segment?: number;
  /** preselect a span */
  spanId?: string;
}

// A session is a sequence of turns, not a distributed trace: 84% of spans sit
// at one depth and most of the wall-clock is the user being away. So the view
// is the narrative (prompt, what it ran, what it cost) with idle time collapsed
// to a labelled break. Ranked answers live behind `profile`.
export default function TracePanel({ sessionId, segment, spanId }: TracePanelProps) {
  const { trace, loading, error, loadMs, reload } = useTrace(sessionId);
  const [params, setParams] = useSearchParams();

  const segCount = trace?.segments.length || 0;
  const paramSeg = params.get("seg");
  const segIndex = clamp(segment ?? (paramSeg != null ? Number(paramSeg) : segCount - 1), 0, Math.max(segCount - 1, 0));
  const seg: Segment | undefined = trace?.segments[segIndex];
  const selected = spanId ?? params.get("span") ?? undefined;

  const panel = (params.get("panel") as "" | "chapter" | "profile") || "";
  const setPanel = (v: "" | "chapter" | "profile") => {
    const p = new URLSearchParams(params);
    if (v) p.set("panel", v);
    else p.delete("panel");
    setParams(p, { replace: true });
  };
  const [menu, setMenu] = useState<"" | "filters" | "seg">("");
  const [showTools, setShowTools] = useState(true);
  const [showAgents, setShowAgents] = useState(true);
  const [minDur, setMinDur] = useState(false);
  const [errorsOnly, setErrorsOnly] = useState(false);
  const [query, setQuery] = useState("");
  const [expanded, setExpanded] = useState<Set<string>>(new Set());
  const [enriching, setEnriching] = useState(false);
  const [enrichError, setEnrichError] = useState<string | null>(null);

  const idx = useMemo(() => indexSpans(trace?.spans || []), [trace]);
  const segSpans = useMemo(() => (trace ? trace.spans.filter((s) => s.seg === segIndex) : []), [trace, segIndex]);
  const bd = useMemo(() => breakdown(segSpans), [segSpans]);
  const groups = useMemo(() => buildGroups(idx, segIndex), [idx, segIndex]);

  // Open the turn holding the selected call, else the newest turn.
  useEffect(() => {
    if (!trace) return;
    setExpanded((prev) => {
      const n = new Set(prev);
      const g = selected ? groups.find((x) => x.calls.some((c) => c.id === selected)) : undefined;
      if (g) n.add(g.id);
      else if (!prev.size && groups.length) n.add(groups[groups.length - 1].id);
      return n;
    });
  }, [trace, selected, segIndex, groups]);

  const setSeg = useCallback(
    (i: number) => {
      const p = new URLSearchParams(params);
      p.set("seg", String(i));
      p.delete("span");
      setParams(p, { replace: true });
      setExpanded(new Set());
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
    },
    [params, setParams, segIndex],
  );
  const toggle = (id: string) =>
    setExpanded((prev) => {
      const n = new Set(prev);
      if (n.has(id)) n.delete(id);
      else n.add(id);
      return n;
    });

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

  if (error) return <div className="tr-root"><div className="tr-error">trace: {error}</div></div>;
  if (!trace || !seg) return <div className="tr-root"><div className="tr-loading">{loading ? "loading trace…" : "no trace for this session"}</div></div>;

  const wall = (seg.toTs || trace.lastTs) - seg.fromTs;
  const active = groups.reduce((a, g) => a + g.dur, 0);
  const away = groups.reduce((a, g) => a + (g.gapBefore > 180_000 ? g.gapBefore : 0), 0);
  const allCalls = groups.flatMap((g) => g.calls);
  const filterState = { tools: showTools, agents: showAgents, minDur, errorsOnly, query };
  const filtersOn = [!showTools, !showAgents, minDur, errorsOnly].filter(Boolean).length;
  const segMeta = BOUNDARY[seg.boundary.kind] || BOUNDARY.start;
  const famTitle = bd.byFam.map((b) => `${famLabel(b.fam)} ${b.pct.toFixed(0)}% · ${fmtDur(b.ms)}`).join("\n");
  const chapCount = (seg.chapter?.learnings?.length || 0) + (seg.chapter?.open?.length || 0) + (seg.chapter?.intentChanges?.length || 0);
  const selSpan = selected ? idx.byId.get(selected) || null : null;

  return (
    <div className="tr-root">
      {/* row 1: which segment, and what it cost */}
      <div className="tr-bar">
        <button className="tr-ico" disabled={segIndex === 0} onClick={() => setSeg(segIndex - 1)} title="previous segment">
          <Icon name="chevl" size={13} />
        </button>
        <Pop
          open={menu === "seg"}
          onOpen={(o) => setMenu(o ? "seg" : "")}
          label={
            <>
              <span className={`tr-bd sm ${segMeta.cls}`}>{segMeta.glyph}</span>
              seg <b>{segIndex + 1}</b>/{segCount}
              <span className="tr-dim">{seg.boundary.kind === "compact" ? `compact · ${seg.boundary.trigger || "auto"}` : segMeta.label}</span>
            </>
          }
        >
          <div className="tr-menu seg">
            {trace.segments.map((sg, i) => {
              const m = BOUNDARY[sg.boundary.kind] || BOUNDARY.start;
              return (
                <button key={sg.id} className={i === segIndex ? "on" : ""} onClick={() => { setSeg(i); setMenu(""); }}>
                  <span className={`tr-bd sm ${m.cls}`}>{m.glyph}</span>
                  <span className="num">{i + 1}</span>
                  <span className="ell">{sg.boundary.kind === "compact" ? "compact" : m.label}</span>
                  <span className="tr-dim num">{fmtDate(sg.fromTs)}</span>
                </button>
              );
            })}
          </div>
        </Pop>
        <button className="tr-ico" disabled={segIndex >= segCount - 1} onClick={() => setSeg(segIndex + 1)} title="next segment">
          <Icon name="chev" size={13} />
        </button>

        <span className="tr-stats" title={`starts ${fmtDate(seg.fromTs)}, ends ${fmtDate(seg.toTs || trace.lastTs)}`}>
          <b>{groups.length} turns</b>
          <i />
          <span title="summed turn time, not wall-clock">{fmtDur(active)}<span className="opt"> of work</span></span>
          {away > 0 && (
            <>
              <i className="opt" />
              <span className="opt dim" title={`wall-clock ${fmtDuration(wall)}`}>{fmtDuration(away)} away</span>
            </>
          )}
          <i />
          <span className={seg.errors ? "bad" : ""}>{seg.errors} err</span>
          <i className="opt" />
          <span className="opt">{fmtUsd(seg.usdEst, trace.costEstimated)}</span>
        </span>

        <span className="tr-fam" title={famTitle || "no tool time in this segment"}>
          {bd.byFam.map((b) => (
            <span key={b.fam} style={{ width: `${b.pct}%`, background: `var(--fam-${b.fam})` }} />
          ))}
        </span>

        <span className="point" title={seg.chapter?.point || ""}>{seg.chapter?.point || "no chapter for this segment yet"}</span>

        <button className={`tr-chip ${panel === "profile" ? "on" : ""}`} onClick={() => setPanel(panel === "profile" ? "" : "profile")}>
          profile
        </button>
        <button className={`tr-chip ${panel === "chapter" ? "on" : ""}`} onClick={() => setPanel(panel === "chapter" ? "" : "chapter")}>
          chapter
          {chapCount > 0 && <span className="num">{chapCount}</span>}
        </button>
      </div>

      {panel === "chapter" && (
        <div className={`tr-chapter ${seg.boundary.kind}`}>
          <ChapterCard segment={seg} onEnrich={runEnrich} enriching={enriching} enrichError={enrichError} loadMs={loadMs} />
        </div>
      )}
      {panel === "profile" && <ProfilePanel calls={allCalls} turnMs={active} onSelect={select} />}

      {/* row 2: what to show */}
      <div className="tr-bar2">
        <label className="tr-filter wide">
          <Icon name="search" size={12} />
          <input placeholder="filter turns and calls" value={query} onChange={(e) => setQuery(e.target.value)} />
        </label>
        <Pop
          open={menu === "filters"}
          onOpen={(o) => setMenu(o ? "filters" : "")}
          on={filtersOn > 0}
          label={<>filters{filtersOn > 0 && <span className="num">{filtersOn}</span>}<Icon name="chevd" size={10} /></>}
        >
          <div className="tr-menu">
            <div className="k">show</div>
            <button className={showTools ? "on" : ""} onClick={() => setShowTools(!showTools)}><Tick on={showTools} /> tool calls</button>
            <button className={showAgents ? "on" : ""} onClick={() => setShowAgents(!showAgents)}><Tick on={showAgents} /> subagents and nested calls</button>
            <div className="k">only</div>
            <button className={minDur ? "on" : ""} onClick={() => setMinDur(!minDur)}><Tick on={minDur} /> slower than 1s</button>
            <button className={errorsOnly ? "on" : ""} onClick={() => setErrorsOnly(!errorsOnly)}><Tick on={errorsOnly} /> errors</button>
          </div>
        </Pop>
        <button className="tr-chip" onClick={() => setExpanded(expanded.size ? new Set() : new Set(groups.map((g) => g.id)))}>
          {expanded.size ? "collapse all" : "expand all"}
        </button>
        <div style={{ flex: 1 }} />
        <span className="tr-dim" style={{ fontSize: 11 }}>{allCalls.length.toLocaleString()} calls</span>
      </div>

      <div className="tr-main">
        <div className="tr-left">
          <TurnList idx={idx} segNo={segIndex} model={trace.model} segUsd={seg.usdEst} costEstimated={trace.costEstimated} selected={selected} expanded={expanded} onToggle={toggle} onSelect={select} filter={filterState} />
        </div>
        {selSpan && <SpanDetail sessionId={sessionId} span={selSpan} segment={seg} idx={idx} model={trace.model} onSelect={select} onEnrich={runEnrich} enriching={enriching} enrichError={enrichError} />}
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
