import { useEffect, useMemo, useRef } from "react";
import type { Span } from "../../api/types";
import { fmtClock, fmtDur, fmtDuration, fmtUsd } from "../../lib/format";
import { Icon } from "../../lib/icons";
import { buildGroups, costWeight, errorMarks, famSummary, ribbonGradient, shortTool, type SpanIndex, type TurnGroup } from "./lib";

interface Props {
  idx: SpanIndex;
  segNo: number;
  model: string;
  /** the segment's authoritative cost, split across turns by weight */
  segUsd: number;
  costEstimated: boolean;
  selected?: string;
  expanded: Set<string>;
  onToggle: (id: string) => void;
  onSelect: (id: string) => void;
  filter: { tools: boolean; agents: boolean; minDur: boolean; errorsOnly: boolean; query: string };
}

const GAP_MIN = 180_000; // 3 min of nothing is a break, not a pause

// MCP calls repeat their own name in the resource slot; blank it rather than print it twice.
const sameAsName = (name: string, res?: string) => !!res && shortTool(name).includes(res.split("·").pop()!.trim());

export default function TurnList({ idx, segNo, model, segUsd, costEstimated, selected, expanded, onToggle, onSelect, filter }: Props) {
  const groups = useMemo(() => buildGroups(idx, segNo), [idx, segNo]);
  const usdOf = useMemo(() => {
    const w = new Map(groups.map((g) => [g.id, costWeight(model, g.tokens)]));
    const total = [...w.values()].reduce((a, b) => a + b, 0);
    return (g: TurnGroup) => (total > 0 ? (segUsd * (w.get(g.id) || 0)) / total : 0);
  }, [groups, model, segUsd]);

  const q = filter.query.trim().toLowerCase();
  const keepCall = (c: Span) => {
    if (!filter.tools && c.kind === "tool") return false;
    if (!filter.agents && (c.kind === "agent" || c.depth > 1)) return false;
    if (filter.minDur && c.dur < 1000) return false;
    if (filter.errorsOnly && !c.err) return false;
    if (q && !`${c.name} ${c.res || ""} ${c.text || ""}`.toLowerCase().includes(q)) return false;
    return true;
  };
  const shown = useMemo(() => {
    if (!q && !filter.errorsOnly && !filter.minDur) return groups;
    return groups.filter((g) => {
      if (q && `${g.prompt?.text || ""} ${g.turns.map((t) => t.text || "").join(" ")}`.toLowerCase().includes(q)) return true;
      return g.calls.some(keepCall);
    });
  }, [groups, q, filter.errorsOnly, filter.minDur, filter.tools, filter.agents]); // eslint-disable-line react-hooks/exhaustive-deps

  const ref = useRef<HTMLDivElement>(null);
  // the row only exists once its turn is expanded, so wait for that paint
  useEffect(() => {
    if (!selected) return;
    const id = requestAnimationFrame(() =>
      ref.current?.querySelector(`[data-span="${CSS.escape(selected)}"]`)?.scrollIntoView({ block: "center" }),
    );
    return () => cancelAnimationFrame(id);
  }, [selected, expanded]);

  if (!groups.length) return <div className="tr-empty">No turns in this segment.</div>;
  if (!shown.length) return <div className="tr-empty">No turn matches the current filters.</div>;

  return (
    <div className="tn-list" ref={ref}>
      {shown.map((g) => (
        <Group
          key={g.id}
          g={g}
          idx={idx}
          usd={usdOf(g)}
          usdEst={costEstimated}
          open={expanded.has(g.id)}
          selected={selected}
          onToggle={() => onToggle(g.id)}
          onSelect={onSelect}
          keepCall={keepCall}
        />
      ))}
    </div>
  );
}

function Group({
  g,
  idx,
  usd,
  usdEst,
  open,
  selected,
  onToggle,
  onSelect,
  keepCall,
}: {
  g: TurnGroup;
  idx: SpanIndex;
  usd: number;
  usdEst: boolean;
  open: boolean;
  selected?: string;
  onToggle: () => void;
  onSelect: (id: string) => void;
  keepCall: (c: Span) => boolean;
}) {
  const marks = useMemo(() => errorMarks(g.calls), [g.calls]);
  const grad = useMemo(() => ribbonGradient(g.calls), [g.calls]);
  const slowest = g.calls.reduce<Span | null>((a, b) => (!a || b.dur > a.dur ? b : a), null);
  const prompt = g.prompt?.text?.trim() || "";
  const calls = open ? g.calls.filter(keepCall) : [];

  return (
    <>
      {g.gapBefore > GAP_MIN && (
        <div className="tn-gap">
          <span>{fmtDuration(g.gapBefore)} away</span>
        </div>
      )}
      <div className={`tn-turn ${open ? "open" : ""}`}>
        <div className="tn-head" onClick={onToggle}>
          <Icon name={open ? "chevd" : "chev"} size={11} color="var(--muted)" />
          <span className="who">{g.prompt ? "you" : "continued"}</span>
          <span className="prompt" title={prompt}>
            {prompt || (g.prompt ? "(empty prompt)" : "assistant carried on after the boundary")}
          </span>
          <span className="clock">{fmtClock(g.fromTs)}</span>
        </div>
        {g.calls.length > 0 && <div className="tn-body">
          <div className="tn-ribbon" onClick={onToggle} title={famSummary(g.calls, 8) || "no tool calls"}>
            <div className="fill" style={{ background: grad }} />
            {marks.map((m, i) => (
              <span key={i} className="err" style={{ left: `${m}%` }} />
            ))}
          </div>
          <div className="tn-stats">
            <span className="n">{g.calls.length} calls</span>
            <span className="fams">{famSummary(g.calls)}</span>
            <span style={{ flex: 1 }} />
            {g.errors > 0 && <span className="bad">{g.errors} err</span>}
            <span className="num">{fmtDur(g.dur)}</span>
            <span className="num usd" title="this turn's share of the segment cost">{fmtUsd(usd, usdEst)}</span>
          </div>
        </div>}
        {g.calls.length === 0 && <div className="tn-body"><div className="tn-stats"><span className="fams">no tool calls</span></div></div>}
        {open && (
          <div className="tn-calls">
            {calls.length === 0 && <div className="tn-none">no call matches the filters</div>}
            {calls.map((c) => {
              const kids = idx.children.get(c.id) || [];
              return (
                <div
                  key={c.id}
                  data-span={c.id}
                  className={`tn-call ${selected === c.id ? "sel" : ""} ${c.err ? "err" : ""}`}
                  onClick={() => onSelect(c.id)}
                  style={{ paddingLeft: 26 + (c.depth - 1) * 14 }}
                >
                  <span className="sq" style={{ background: `var(--fam-${c.fam})` }} />
                  <span className="nm" title={c.name}>{shortTool(c.name)}</span>
                  <span className="res" title={c.res || c.text || ""}>{sameAsName(c.name, c.res) ? "" : c.res || c.text || ""}</span>
                  {kids.length > 0 && <span className="kids">{kids.length} nested</span>}
                  {c.err && <span className="bad">error</span>}
                  <span className="num">{fmtDur(c.dur)}</span>
                  {slowest && c.id === slowest.id && g.calls.length > 2 && <span className="slow">slowest</span>}
                </div>
              );
            })}
          </div>
        )}
      </div>
    </>
  );
}
