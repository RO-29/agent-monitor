import { useEffect, useMemo, useState } from "react";
import { Link, useParams } from "react-router-dom";
import { api } from "../../api/client";
import type { SpanDetail } from "../../api/types";
import { fmtDur, fmtUsd, shortCwd } from "../../lib/format";
import { shortTool, stripAnsi, useTrace } from "../../pages/trace/lib";
import { useApi, v3 } from "../api/client";
import type { XTraceResponse, XTraceRow } from "../api/types";
import { AgentBadge, Empty, ErrorState, famPill, Loading } from "../components/ui";
import { useV3 } from "../live";
import { buildStory, cleanSpans } from "./thread/storyModel";

const AGENT_COLOR: Record<string, string> = { claude: "var(--claude)", codex: "var(--codex)", subagent: "var(--sub)", wait: "var(--need)" };

export default function TracePage() {
  const { sessionId = "", turn = "0" } = useParams();
  const sid = decodeURIComponent(sessionId);
  const x = useApi(() => v3.xtrace(sid, turn), [sid, turn]);
  if (x.data) return <TraceBody sessionId={sid} turn={turn} data={x.data} source="cross-agent" />;
  if (x.missing) return <Fallback sessionId={sid} turn={turn} />;
  if (x.error) return <ErrorState error={x.error} onRetry={x.reload} />;
  return <Loading rows={10} />;
}

/** Without /api/v3/trace, the same view is built from this session's own spans. */
function Fallback({ sessionId, turn }: { sessionId: string; turn: string }) {
  const tr = useTrace(sessionId);
  const data = useMemo<XTraceResponse | null>(() => {
    if (!tr.trace) return null;
    const spans = cleanSpans(tr.trace);
    const { groups } = buildStory(tr.trace, spans);
    const n = Number(turn);
    const g = groups[n];
    if (!g) return { root: {}, rows: [], byAgent: {} as Record<string, number>, wallMs: 0, workMs: 0, waitMs: 0, costUsd: 0 };
    const agent = tr.trace.tool;
    const t0 = g.g.turns[0];
    const rows: XTraceRow[] = [
      { id: t0?.id || g.g.id, depth: 0, kind: "turn", agent, name: t0?.name || "turn", res: t0?.model, ts: g.g.fromTs, dur: Math.max(g.g.toTs - g.g.fromTs, g.g.dur), err: g.errors.length > 0 },
      ...g.g.calls.map((c) => ({ id: c.id, depth: c.depth, kind: c.kind, agent: c.depth > 1 ? "subagent" : agent, name: c.name, res: c.res, ts: c.ts, dur: c.dur, err: c.err, child: c.child, session: sessionId, by: c.by })),
    ];
    const sub = g.g.calls.filter((c) => c.depth > 1).reduce((a, c) => a + c.dur, 0);
    return { prompt: g.g.prompt?.text, turn: n, turns: groups.length, root: { name: t0?.name, model: t0?.model }, rows, byAgent: { [agent]: Math.max(g.g.dur - sub, 0), subagent: sub } as Record<string, number>, wallMs: Math.max(g.g.toTs - g.g.fromTs, 0), workMs: g.g.dur, waitMs: 0, costUsd: 0 };
  }, [tr.trace, turn]);
  if (tr.error && !tr.trace) return <ErrorState error={tr.error} onRetry={tr.reload} />;
  if (!data) return <Loading rows={10} />;
  return <TraceBody sessionId={sessionId} turn={turn} data={data} source="session" />;
}

function TraceBody({ sessionId, turn, data, source }: { sessionId: string; turn: string; data: XTraceResponse; source: "cross-agent" | "session" }) {
  const st = useV3();
  const [sel, setSel] = useState<string | null>(null);
  const rows = data.rows || [];
  const row = rows.find((r) => r.id === sel) || rows.find((r) => r.err && r.depth > 0) || rows[1] || rows[0];
  const t0 = Math.min(...rows.filter((r) => r.ts > 0).map((r) => r.ts), Infinity);
  const t1 = Math.max(...rows.map((r) => r.ts + r.dur), 0);
  const span = Math.max(t1 - t0, 1);
  const total = Object.values(data.byAgent || {}).reduce((a, b) => a + b, 0) + (data.waitMs && !(data.byAgent || {}).wait ? data.waitMs : 0);
  const parts = Object.entries({ ...(data.byAgent || {}), ...(data.waitMs && !(data.byAgent || {}).wait ? { wait: data.waitMs } : {}) }).filter(([, v]) => v > 0);
  const nErr = rows.filter((r) => r.err).length;
  const agents = new Set(rows.map((r) => r.agent)).size;
  const tid = st.threadOf[sessionId] || sessionId;
  const ticks = [0, 0.25, 0.5, 0.75, 1];

  return (
    <div className="view pad">
      <div className="frame">
        <div className="tr-top">
          <b style={{ fontSize: 14 }} className="trunc">
            {data.prompt || (data.root?.text as string) || (data.root?.name as string) || `turn ${turn}`}
          </b>
          {nErr > 0 && <span className="pill p-err">{nErr} error{nErr > 1 ? "s" : ""}</span>}
          <span className="pill p-sub">{agents} agent{agents > 1 ? "s" : ""}</span>
          {source === "session" && <span className="pill p-mute" title="/api/v3/trace answered 404">this session only</span>}
          <span className="ml-auto sm fg2 tab-num">
            {fmtDur(data.wallMs)} wall · {fmtDur(data.workMs)} work{data.waitMs ? ` · ${fmtDur(data.waitMs)} waiting on you` : ""}
            {data.costUsd ? ` · ${fmtUsd(data.costUsd)}` : ""}
          </span>
          {data.turns !== undefined && (
            <span className="row" style={{ gap: 4 }}>
              <Link className={`btn sm ${Number(turn) <= 0 ? "ghost" : ""}`} to={`/traces/${encodeURIComponent(sessionId)}/${Math.max(Number(turn) - 1, 0)}`} aria-label="Previous turn">
                ‹
              </Link>
              <span className="xs muted tab-num">
                {(data.turn ?? Number(turn)) + 1} / {data.turns}
              </span>
              <Link className="btn sm" to={`/traces/${encodeURIComponent(sessionId)}/${Math.min(Number(turn) + 1, data.turns - 1)}`} aria-label="Next turn">
                ›
              </Link>
            </span>
          )}
          <Link className="btn sm" to={`/t/${encodeURIComponent(tid)}?session=${encodeURIComponent(sessionId)}${data.rows[1] ? `&span=${encodeURIComponent(data.rows[1].id)}` : ""}`}>
            Open thread
          </Link>
        </div>
        {parts.length > 0 && (
          <>
            <div className="row xs muted" style={{ padding: "10px 16px 0", gap: 14, flexWrap: "wrap" }}>
              <span>Time by agent</span>
              {parts.map(([k, v]) => (
                <span key={k} className="row" style={{ gap: 5 }}>
                  <i style={{ width: 10, height: 8, background: AGENT_COLOR[k] || "var(--run)", display: "inline-block", borderRadius: 2 }} />
                  {k} {Math.round((v / (total || 1)) * 100)}%
                </span>
              ))}
            </div>
            <div className="tr-break">
              {parts.map(([k, v]) => (
                <i key={k} style={{ flex: v, background: AGENT_COLOR[k] || "var(--run)" }} />
              ))}
            </div>
          </>
        )}
        {rows.length === 0 ? (
          <Empty title="No spans for this turn" />
        ) : (
          <div className="trgrid">
            <div className="wfx" style={{ borderRight: "1px solid var(--line)" }}>
              <div className="hd">Span</div>
              <div className="hd tk" style={{ justifyContent: "space-between" }}>
                {ticks.map((t) => (
                  <span key={t}>{fmtDur(span * t)}</span>
                ))}
              </div>
              <div className="hd" style={{ justifyContent: "flex-end" }}>
                Dur
              </div>
              {rows.slice(0, 400).map((r) => {
                const on = r.id === row?.id;
                const left = r.ts > 0 ? ((r.ts - t0) / span) * 100 : 0;
                const w = Math.max((r.dur / span) * 100, 0.3);
                const color = r.wait ? "repeating-linear-gradient(135deg,var(--need) 0 4px,transparent 4px 8px)" : r.err ? "var(--err)" : r.depth === 0 ? `color-mix(in srgb, ${AGENT_COLOR[r.agent] || "var(--run)"} 35%, transparent)` : r.agent === "subagent" ? "var(--sub)" : r.child ? "var(--codex)" : `var(--fam-${famOf(r)})`;
                const cls = `${on ? "sel" : ""}`;
                return (
                  <div key={r.id} className="rowc" onClick={() => setSel(r.id)}>
                    <div className={`nm ${cls}`} style={{ paddingLeft: 10 + r.depth * 16 }}>
                      {r.depth === 0 || r.kind === "agent" || r.child ? <AgentBadge tool={r.kind === "agent" ? "subagent" : r.agent} /> : r.wait ? <span className="pill p-need">wait</span> : <span className={`pill ${famPill[famOf(r)] || "p-mute"}`}>{shortTool(r.name)}</span>}
                      {r.by === "user" && <span className="pill p-need" title="A command you ran">you</span>}
                      {r.depth === 0 || r.kind === "agent" || r.child ? <b className="trunc">{r.name}</b> : null}
                      <span className="trunc mono xs fg2">{r.res}</span>
                    </div>
                    <div className={`tl ${cls}`}>
                      <i style={{ left: `${Math.min(left, 99.7)}%`, width: `${Math.min(w, 100 - left)}%`, background: color }} />
                    </div>
                    <div className={`du ${cls}`} style={r.err ? { color: "var(--err)" } : r.wait ? { color: "var(--need)" } : undefined}>
                      {fmtDur(r.dur)}
                    </div>
                  </div>
                );
              })}
            </div>
            {row && <SpanPanel key={row.id} row={row} sessionId={row.session || sessionId} />}
          </div>
        )}
      </div>
    </div>
  );
}

const famOf = (r: XTraceRow) => {
  const n = r.name;
  if (n === "Bash" || n === "exec" || n === "shell" || n === "exec_command" || n === "!") return "bash";
  if (["Read", "Grep", "Glob", "LS"].includes(n)) return "read";
  if (["Edit", "Write", "MultiEdit", "apply_patch", "NotebookEdit"].includes(n)) return "edit";
  if (n.startsWith("mcp__")) return "mcp";
  if (n.startsWith("Web")) return "web";
  if (r.kind === "agent") return "agent";
  return "other";
};

function SpanPanel({ row, sessionId }: { row: XTraceRow; sessionId: string }) {
  const st = useV3();
  const [d, setD] = useState<SpanDetail | null>(null);
  const [err, setErr] = useState("");
  useEffect(() => {
    if (row.depth === 0) return;
    let alive = true;
    api
      .span(row.session || sessionId, row.id)
      .then((x) => alive && setD(x))
      .catch((e) => alive && setErr((e as Error).message));
    return () => {
      alive = false;
    };
  }, [row.session, row.id, sessionId, row.depth]);
  const same = d?.same || [];
  const failedBefore = same.filter((s) => s.err).length;
  const tid = st.threadOf[sessionId] || sessionId;
  const cmdKey = (row.res || "").split(/\s+/).slice(0, 3).join(" ");
  return (
    <div style={{ padding: 14, display: "flex", flexDirection: "column", gap: 12, fontSize: 12, minWidth: 0 }}>
      <div className="row">
        <span className="pill p-mute">{shortTool(row.name)}</span>
        <b className="trunc">{row.res || row.name}</b>
        {row.err && <span className="pill p-err ml-auto">failed</span>}
      </div>
      <dl className="spd">
        <dt>start</dt>
        <dd>{row.ts > 0 ? new Date(row.ts).toTimeString().slice(0, 8) : "unknown"}</dd>
        <dt>duration</dt>
        <dd>{fmtDur(row.dur)}</dd>
        <dt>agent</dt>
        <dd>{row.agent}</dd>
        <dt>session</dt>
        <dd>{shortCwd(sessionId)}</dd>
        {same.length > 0 && (
          <>
            <dt>same call in session</dt>
            <dd>{same.length} runs</dd>
            <dt>failed before</dt>
            <dd>{failedBefore}×</dd>
          </>
        )}
      </dl>
      {row.depth > 0 && !d && !err && <div className="muted">loading span…</div>}
      {err && <div className="xs muted">Span detail unavailable here ({err}).</div>}
      {d?.result && (
        <pre className="out" style={{ maxHeight: 300 }}>
          {stripAnsi(d.result).slice(0, 3000)}
        </pre>
      )}
      <div className="row" style={{ gap: 6, flexWrap: "wrap" }}>
        <Link className="btn sm" to={`/t/${encodeURIComponent(tid)}?session=${encodeURIComponent(row.session || sessionId)}&span=${encodeURIComponent(row.id)}`}>
          Open in thread
        </Link>
        {row.res && (
          <Link className="btn sm" to={`/explore?q=${encodeURIComponent(cmdKey)}&groupBy=none`}>
            Same command, all runs
          </Link>
        )}
        {row.err && (
          <Link className="btn sm" to="/errors">
            Error groups ›
          </Link>
        )}
        {row.child && (
          <Link className="btn sm" to={`/t/${encodeURIComponent(st.threadOf[row.child] || row.child)}`}>
            Open child
          </Link>
        )}
      </div>
    </div>
  );
}
