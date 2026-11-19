import { useMemo, useState } from "react";
import { useNavigate, useSearchParams } from "react-router-dom";
import { fmtClock, fmtDuration } from "../../lib/format";
import { useApi, v3 } from "../api/client";
import type { TimelineBar, TimelineLane, TimelineResponse } from "../api/types";
import { AgentBadge, DrivenTag, durOr0, Empty, ErrorState, Loading, Missing } from "../components/ui";
import { useOrch } from "../api/orch";
import { drivenLanes, drivenSig } from "../driven";
import { useV3 } from "../live";
import { densityFromBars, displayOf, stackLabel, verbClass, type Density } from "./control/data";
import "../styles/control.css";

type Zoom = "1h" | "day" | "week";
const FOLD_MS = 20 * 60_000;

const ymd = (d: Date) => `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, "0")}-${String(d.getDate()).padStart(2, "0")}`;
const shiftDay = (day: string, n: number) => {
  const d = new Date(day + "T00:00:00");
  d.setDate(d.getDate() + n);
  return ymd(d);
};

async function loadRange(day: string, zoom: Zoom, stacks: boolean): Promise<TimelineResponse> {
  if (zoom !== "week") return v3.timeline(day, stacks);
  const days = Array.from({ length: 7 }, (_, i) => shiftDay(day, i - 6));
  const all = await Promise.all(days.map((d) => v3.timeline(d, stacks)));
  const lanes = new Map<string, TimelineLane>();
  for (const r of all)
    for (const l of r.lanes || []) {
      const cur = lanes.get(l.threadId);
      if (cur) {
        cur.bars.push(...l.bars);
        if (l.density) cur.density = [...(cur.density || []), ...l.density];
      } else lanes.set(l.threadId, { ...l, bars: [...l.bars], density: l.density ? [...l.density] : undefined });
    }
  return { from: all[0].from, to: all[all.length - 1].to, lanes: [...lanes.values()], waitMs: all.reduce((a, r) => a + (r.waitMs || 0), 0) };
}

/** Maps time to x% with idle gaps over 20 minutes folded to a sliver. */
function makeScale(from: number, to: number, bars: TimelineBar[], fold: boolean) {
  const segs: { a: number; b: number }[] = [];
  if (fold) {
    const iv = bars.filter((b) => b.kind !== "idle" && b.to > from && b.from < to).map((b) => [Math.max(b.from, from), Math.min(b.to, to)] as [number, number]).sort((x, y) => x[0] - y[0]);
    let cur = from;
    for (const [a, b] of iv) {
      if (a - cur > FOLD_MS) segs.push({ a: cur, b: a });
      cur = Math.max(cur, b);
    }
    if (to - cur > FOLD_MS) segs.push({ a: cur, b: to });
  }
  const foldW = 1.2;
  const foldedMs = segs.reduce((s, g) => s + (g.b - g.a), 0);
  const openMs = Math.max(to - from - foldedMs, 1);
  const openPct = 100 - segs.length * foldW;
  const x = (t: number) => {
    t = Math.min(Math.max(t, from), to);
    let pct = 0;
    let at = from;
    for (const g of segs) {
      if (t <= g.a) break;
      pct += ((g.a - at) / openMs) * openPct;
      if (t < g.b) return pct + ((t - g.a) / (g.b - g.a)) * foldW;
      pct += foldW;
      at = g.b;
    }
    return pct + ((t - at) / openMs) * openPct;
  };
  return { x, folds: segs };
}

const RUNNING = new Set(["running", "awaiting-input", "awaiting-permission"]);
const DONE = new Set(["completed", "idle"]);

/** Client grouping for daemons without ?stacks=1: children with one parent, agent and verb become one lane. */
function clientStacks(lanes: TimelineLane[]): TimelineLane[] {
  if (lanes.some((l) => l.kind === "stack")) return lanes;
  const groups = new Map<string, TimelineLane[]>();
  for (const l of lanes) if (l.parent) groups.set(`${l.parent}|${l.agent}|${verbClass(l.display || l.title)}`, [...(groups.get(`${l.parent}|${l.agent}|${verbClass(l.display || l.title)}`) || []), l]);
  const out = lanes.filter((l) => !l.parent);
  const solo: TimelineLane[] = [];
  for (const [key, ms] of groups) {
    if (ms.length < 2) {
      solo.push(...ms);
      continue;
    }
    const [parent, agent, kind] = key.split("|");
    out.push({
      threadId: `stack:${key}`,
      title: stackLabel(ms.length, agent, kind),
      display: stackLabel(ms.length, agent, kind),
      agent,
      parent,
      kind: "stack",
      stack: { total: ms.length, done: ms.filter((m) => DONE.has(m.state || "")).length, running: ms.filter((m) => RUNNING.has(m.state || "")).length, failed: ms.filter((m) => m.state === "abandoned").length },
      members: ms.map((m) => m.threadId),
      bars: ms.flatMap((m) => m.bars),
      density: ms.every((m) => m.density) ? ms.flatMap((m) => m.density || []) : undefined,
    });
  }
  return [...out, ...solo];
}

type RowKind = "lane" | "child" | "stack" | "member";
interface RowT {
  lane: TimelineLane;
  kind: RowKind;
  parentRow?: number;
}

const COLOR: Record<string, string> = { claude: "var(--claude)", codex: "var(--codex)", subagent: "var(--sub)" };

export default function Timeline() {
  const [sp, setSp] = useSearchParams();
  const nav = useNavigate();
  const today = ymd(new Date());
  const day = sp.get("day") || today;
  const [zoom, setZoom] = useState<Zoom>("day");
  const [fold, setFold] = useState(true);
  const [kids, setKids] = useState(true);
  const [open, setOpen] = useState<Set<string>>(new Set());
  const st = useV3();
  const byId = useMemo(() => new Map(st.threads.map((t) => [t.id, t])), [st.threads]);
  const nameOf = (l: TimelineLane) => {
    if (l.display) return l.display;
    const t = byId.get(l.threadId);
    return t ? displayOf(t) : l.title;
  };
  const tl = useApi(() => loadRange(day, zoom, true), [day, zoom], { poll: day === today ? 60_000 : undefined });
  const serverStacks = !!tl.data?.lanes.some((l) => l.kind === "stack");
  const plain = useApi(() => loadRange(day, zoom, false), [day, zoom], { skip: !serverStacks || open.size === 0 });
  const setDay = (d: string) => {
    const n = new URLSearchParams(sp);
    n.set("day", d);
    setSp(n, { replace: true });
  };
  const toggle = (id: string) => {
    const n = new Set(open);
    if (n.has(id)) n.delete(id);
    else n.add(id);
    setOpen(n);
  };

  const orch = useOrch();
  const dsig = useMemo(() => drivenSig(orch), [orch.version]); // eslint-disable-line react-hooks/exhaustive-deps
  const isDriven = (l: TimelineLane) => !!l.drivenId || orch.threads.has(l.threadId);
  const view = useMemo(() => {
    const d = tl.data;
    if (!d) return null;
    const clean = (l: TimelineLane) => ({ ...l, bars: l.bars.filter((b) => b.from > 0 && b.to >= b.from) });
    // Driven threads the daemon did not send as lanes come from the orchestrator store.
    const dl = drivenLanes(orch, d.from, d.to).filter((l) => !d.lanes.some((x) => x.threadId === l.threadId || x.drivenId === l.drivenId));
    const lanes = clientStacks([...d.lanes, ...dl].map(clean));
    const memberSrc = new Map<string, TimelineLane>();
    for (const l of [...d.lanes, ...(plain.data?.lanes || [])]) if (l.kind !== "stack") memberSrc.set(l.threadId, clean(l));
    const bars = lanes.flatMap((l) => l.bars);
    let from = d.from;
    let to = d.to;
    if (zoom === "1h") {
      to = Math.min(Date.now(), Math.max(...bars.map((b) => b.to), from + 3600_000));
      from = to - 3600_000;
    }
    const scale = makeScale(from, to, bars, fold && zoom !== "1h");
    const ids = new Set(lanes.map((l) => l.threadId));
    const firstAt = (l: TimelineLane) => Math.min(...l.bars.map((b) => b.from), Infinity);
    const tops = lanes.filter((l) => !l.parent || !ids.has(l.parent)).sort((a, b) => firstAt(a) - firstAt(b));
    const order: RowT[] = [];
    const pushStack = (s: TimelineLane, top: boolean) => {
      order.push({ lane: s, kind: top ? "lane" : "stack" });
      if (open.has(s.threadId)) for (const id of s.members || []) {
        const m = memberSrc.get(id);
        if (m) order.push({ lane: m, kind: "member" });
      }
    };
    for (const p of tops) {
      if (p.kind === "stack") {
        pushStack(p, true);
        continue;
      }
      const pr = order.length;
      order.push({ lane: p, kind: "lane" });
      const under = lanes.filter((l) => l.parent === p.threadId).sort((a, b) => firstAt(a) - firstAt(b));
      for (const c of under) {
        if (c.kind === "stack") pushStack(c, false);
        else if (kids) order.push({ lane: c, kind: "child", parentRow: pr });
      }
    }
    const visible = order.filter((o) => o.lane.bars.some((b) => b.to > from && b.from < to));
    const idx = new Map(visible.map((o, i) => [o.lane.threadId, i]));
    const dens = new Map<string, Density>();
    let max = 1;
    for (const o of visible) {
      const ds = o.lane.density?.length ? o.lane.density : densityFromBars(o.lane.bars, from, to);
      const merged = new Map<number, Density[number]>();
      for (const x of ds) {
        const cur = merged.get(x.from);
        if (cur) {
          cur.calls += x.calls;
          cur.errors += x.errors;
        } else merged.set(x.from, { ...x });
      }
      const arr = [...merged.values()].filter((x) => x.to > from && x.from < to).sort((a, b) => a.from - b.from);
      dens.set(o.lane.threadId, arr);
      if (o.kind !== "stack" && !(o.kind === "lane" && o.lane.kind === "stack")) for (const x of arr) max = Math.max(max, x.calls);
    }
    return { from, to, scale, dens, max, rows: visible.map((o) => ({ ...o, parentRow: o.kind === "child" && o.lane.parent ? idx.get(o.lane.parent) : undefined })) };
  }, [tl.data, plain.data, zoom, fold, kids, open, dsig]); // eslint-disable-line react-hooks/exhaustive-deps

  const ticks = useMemo(() => {
    if (!view) return [];
    const span = view.to - view.from;
    const step = zoom === "1h" ? 10 * 60_000 : zoom === "week" ? 864e5 : span > 12 * 3600_000 ? 3 * 3600_000 : 3600_000;
    const out: number[] = [];
    const start = Math.ceil(view.from / step) * step;
    const inFold = (t: number) => view.scale.folds.some((f) => t > f.a && t < f.b);
    for (let t = start; t <= view.to; t += step) if (!inFold(t)) out.push(t);
    const laneW = Math.max(window.innerWidth - (window.innerWidth <= 760 ? 130 : 512), 120);
    const minGap = (52 / laneW) * 100;
    const kept: number[] = [];
    for (const t of out) if (!kept.length || view.scale.x(t) - view.scale.x(kept[kept.length - 1]) > minGap) kept.push(t);
    return kept;
  }, [view, zoom]);
  const now = Date.now();
  const ROW = 32;

  return (
    <div className="view tl4">
      <div className="bbar">
        <b style={{ fontSize: 14 }}>{new Date(day + "T00:00:00").toLocaleDateString(undefined, { weekday: "short", day: "numeric", month: "short" })}</b>
        <button className="btn sm" onClick={() => setDay(shiftDay(day, -1))} aria-label="Previous day">
          ‹
        </button>
        <input className="inp" type="date" value={day} max={today} onChange={(e) => e.target.value && setDay(e.target.value)} style={{ padding: "3px 6px" }} aria-label="Day" />
        <button className="btn sm" onClick={() => setDay(shiftDay(day, 1))} disabled={day >= today} aria-label="Next day">
          ›
        </button>
        <span className="seg">
          {(["1h", "day", "week"] as Zoom[]).map((z) => (
            <button key={z} className={zoom === z ? "on" : ""} onClick={() => setZoom(z)}>
              {z === "1h" ? "1h" : z === "day" ? "Day" : "Week"}
            </button>
          ))}
        </span>
        <button className={`chip ${kids ? "on" : "t"}`} onClick={() => setKids(!kids)}>
          show children
        </button>
        <button className={`chip ${fold ? "on" : "t"}`} onClick={() => setFold(!fold)} disabled={zoom === "1h"}>
          fold idle &gt; 20m
        </button>
        {tl.data && (
          <span className="ml-auto sm fg2">
            You kept agents waiting <b className="need-t">{durOr0(tl.data.waitMs, fmtDuration)}</b> · {view?.rows.length ?? 0} lanes
          </span>
        )}
      </div>
      {tl.missing ? (
        <Missing endpoint="/api/v3/timeline" />
      ) : tl.error && !tl.data ? (
        <ErrorState error={tl.error} onRetry={tl.reload} />
      ) : !view ? (
        <Loading rows={10} />
      ) : view.rows.length === 0 ? (
        <Empty title="No agent activity in this window">Pick another day, or switch to Week.</Empty>
      ) : (
        <>
          <div className="tax">
            <div>Thread</div>
            <div className="ticks">
              {ticks.map((t) => (
                <span key={t} style={{ left: `${view.scale.x(t)}%` }}>
                  {zoom === "week" ? new Date(t).toLocaleDateString(undefined, { weekday: "short", day: "numeric" }) : fmtClock(t)}
                </span>
              ))}
            </div>
          </div>
          <div className="tbody">
            {view.rows.map((o, i) => {
              const l = o.lane;
              const isStack = l.kind === "stack";
              const color = COLOR[o.kind === "child" && l.agent === "claude" ? "subagent" : l.agent] || "var(--run)";
              const ds = view.dens.get(l.threadId) || [];
              const smax = isStack ? Math.max(...ds.map((x) => x.calls), 1) : view.max;
              const blocks: { a: number; b: number; lvl: number; calls: number; errors: number }[] = [];
              for (const x of ds) {
                const lvl = Math.min(4, Math.floor((x.calls / smax) * 4.999));
                const last = blocks[blocks.length - 1];
                if (last && last.lvl === lvl && last.b >= x.from) {
                  last.b = x.to;
                  last.calls += x.calls;
                  last.errors += x.errors;
                } else blocks.push({ a: x.from, b: x.to, lvl, calls: x.calls, errors: x.errors });
              }
              const ticksE: number[] = [];
              const addTick = (t: number) => {
                const p = view.scale.x(t);
                if (!ticksE.some((q) => Math.abs(q - p) < 0.3)) ticksE.push(p);
              };
              for (const b of l.bars) if (b.kind === "error" && b.from > view.from && b.from < view.to) addTick(b.from);
              if (l.density?.length) for (const x of ds) if (x.errors > 0) addTick((x.from + x.to) / 2);
              const s = l.stack;
              const sub = isStack && s ? [s.done ? `${s.done} done` : "", s.running ? `${s.running} running` : "", s.failed ? `${s.failed} failed` : ""].filter(Boolean).join(" · ") : "";
              return (
                <div key={`${o.kind}:${l.threadId}`} className={`tln k-${o.kind} ${isStack ? "k-stack" : ""}`} style={{ height: ROW }}>
                  <div
                    className="nm"
                    title={nameOf(l)}
                    onClick={() => (isStack ? toggle(l.threadId) : nav(isDriven(l) ? `/w/${encodeURIComponent(l.drivenId || l.threadId)}` : `/t/${encodeURIComponent(l.threadId)}`))}
                    role="button"
                    aria-expanded={isStack ? open.has(l.threadId) : undefined}
                  >
                    {isStack && <span className="tw">{open.has(l.threadId) ? "▾" : "▸"}</span>}
                    <AgentBadge tool={o.kind === "child" && l.agent === "claude" ? "subagent" : l.agent} small />
                    <span className="trunc">{o.kind === "lane" && !isStack ? <b style={{ fontWeight: 500 }}>{nameOf(l)}</b> : nameOf(l)}</span>
                    {!isStack && isDriven(l) && <DrivenTag />}
                    {sub && <small>{sub}</small>}
                  </div>
                  <div className="trk">
                    <div className="grid">
                      {ticks.map((t) => (
                        <i key={t} style={{ left: `${view.scale.x(t)}%` }} />
                      ))}
                    </div>
                    {view.scale.folds.map((f, k) => (
                      <div key={k} className="fold" style={{ left: `${view.scale.x(f.a)}%`, width: `${view.scale.x(f.b) - view.scale.x(f.a)}%` }} title={`idle ${fmtDuration(f.b - f.a)} folded`} />
                    ))}
                    {o.kind === "child" && l.startTurnTs && o.parentRow !== undefined && (
                      <div className="lk" style={{ left: `${view.scale.x(l.startTurnTs)}%`, top: -(i - o.parentRow) * ROW + ROW / 2, height: (i - o.parentRow) * ROW - ROW / 2 + 10 }} />
                    )}
                    {blocks.map((b, k) => {
                      const left = view.scale.x(b.a);
                      return (
                        <div
                          key={k}
                          className="d"
                          style={{ left: `${left}%`, width: `${Math.max(view.scale.x(b.b) - left, 0.2)}%`, background: `color-mix(in srgb, ${color} ${30 + b.lvl * 17}%, transparent)` }}
                          title={`${fmtClock(b.a)} to ${fmtClock(b.b)} · ${l.density?.length ? `${b.calls} tool calls` : "working"}${b.errors ? ` · ${b.errors} errors` : ""}`}
                        />
                      );
                    })}
                    {l.bars
                      .filter((b) => b.kind === "wait" && b.to > view.from && b.from < view.to)
                      .map((b, k) => {
                        const left = view.scale.x(b.from);
                        return <div key={`w${k}`} className="w" style={{ left: `${left}%`, width: `${Math.max(view.scale.x(b.to) - left, 0.2)}%` }} title={`waiting for you ${fmtClock(b.from)} to ${fmtClock(b.to)} · ${fmtDuration(b.to - b.from)}`} />;
                      })}
                    {ticksE.map((p, k) => (
                      <div key={`e${k}`} className="e" style={{ left: `${p}%` }} />
                    ))}
                    {now > view.from && now < view.to && <div className="now" style={{ left: `${view.scale.x(now)}%` }} />}
                  </div>
                </div>
              );
            })}
          </div>
          <div className="legend">
            <span>
              <i className="ramp" />
              working (darker = more tool calls per 5 min)
            </span>
            <span>
              <i className="hat" />
              waiting for you
            </span>
            <span>
              <i className="tick" />
              error
            </span>
            <span>▸ a stack lane groups children of one kind; click to expand</span>
          </div>
        </>
      )}
    </div>
  );
}
