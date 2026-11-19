import { useState } from "react";
import { useNavigate } from "react-router-dom";
import { fmtDur } from "../../lib/format";
import { useApi, v3 } from "../api/client";
import type { GraphResponse } from "../api/types";
import { Load, Spark } from "../components/ui";
import { SINCE } from "./Explore";

const nodeQuery = (id: string, kind: string) =>
  kind === "mcp" ? `tool:mcp__${id.replace(/^mcp:/, "")}` : kind === "subagent" ? "tool:Agent" : kind === "delegated" ? "codex exec" : `agent:${id}`;

const KIND_COLOR: Record<string, string> = { agent: "var(--claude)", subagent: "var(--sub)", delegated: "var(--codex)", mcp: "var(--run)" };

export default function Tracing() {
  const [since, setSince] = useState("7d");
  const lat = useApi(() => v3.latency({ since }), [since]);
  const graph = useApi(() => v3.graph({ since }), [since]);
  const nav = useNavigate();
  return (
    <div className="view pad">
      <div className="viewhead">
        <div>
          <h2>Tracing</h2>
          <p>Which agents call which, and how long each tool takes. Open a work card's "open trace" link to follow one prompt across every agent it reached.</p>
        </div>
        <span className="seg">
          {SINCE.map((s) => (
            <button key={s} className={since === s ? "on" : ""} onClick={() => setSince(s)}>
              {s}
            </button>
          ))}
        </span>
      </div>
      <div className="grid2">
        <div className="ibox">
          <h3>Agent graph · last {since}</h3>
          <Load st={graph} endpoint="/api/graph" isEmpty={(d) => !d.nodes?.length} empty={<div className="state"><b>No agent calls recorded</b></div>}>
            {(d) => <Graph d={d} onNode={(id, kind) => nav(`/explore?since=${since}&q=${encodeURIComponent(nodeQuery(id, kind))}`)} />}
          </Load>
          <div className="xs muted" style={{ marginTop: 6 }}>
            Line width = number of calls. Red dashes mark edges that fail more than 10% of the time. Click a node to explore its spans.
          </div>
        </div>
        <div className="ibox">
          <h3>Tool latency · last {since}</h3>
          <Load st={lat} endpoint="/api/tools/latency" isEmpty={(d) => !d.tools?.length} empty={<div className="state"><b>No tool calls recorded</b></div>}>
            {(d) => (
              <div style={{ overflowX: "auto" }}>
                <table className="tbl">
                  <thead>
                    <tr>
                      <th>Tool</th>
                      <th>Calls</th>
                      <th>p50</th>
                      <th>p95</th>
                      <th>Errors</th>
                      <th>Trend</th>
                    </tr>
                  </thead>
                  <tbody>
                    {d.tools.map((t) => (
                      <tr key={t.tool} className="clk" onClick={() => nav(`/explore?q=${encodeURIComponent(`tool:${t.tool}`)}&since=${since}`)}>
                        <td className="mono trunc" style={{ maxWidth: 200 }}>
                          {t.tool}
                        </td>
                        <td className="n">{t.calls.toLocaleString()}</td>
                        <td className="n">{fmtDur(t.p50)}</td>
                        <td className="n">{fmtDur(t.p95)}</td>
                        <td className="n" style={t.errRate > 0.05 ? { color: "var(--err)" } : undefined}>
                          {(t.errRate * 100).toFixed(1)}%
                        </td>
                        <td>
                          <Spark points={t.trend || []} color={t.errRate > 0.05 ? "var(--err)" : "var(--accent)"} />
                        </td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            )}
          </Load>
        </div>
      </div>
    </div>
  );
}

/** Sources on the left, targets on the right; edge width by calls, one label per target. */
function Graph({ d, onNode }: { d: GraphResponse; onNode: (id: string, kind: string) => void }) {
  const targets = new Set(d.edges.map((e) => e.to));
  const left = d.nodes.filter((n) => !targets.has(n.id) || n.kind === "agent");
  const right = d.nodes.filter((n) => !left.includes(n)).sort((a, b) => b.count - a.count).slice(0, 10);
  const GAP = 78;
  const H = Math.max(right.length, left.length, 3) * GAP + 10;
  const W = 730;
  const pos = new Map<string, { x: number; y: number }>();
  left.forEach((n, i) => pos.set(n.id, { x: 90, y: ((i + 0.5) * H) / left.length }));
  right.forEach((n, i) => pos.set(n.id, { x: 380, y: 30 + i * GAP }));
  const maxCalls = Math.max(...d.edges.map((e) => e.calls), 1);
  const inbound = new Map<string, { calls: number; fail: number; p95: number }>();
  for (const e of d.edges) {
    const v = inbound.get(e.to) || { calls: 0, fail: 0, p95: 0 };
    v.fail = (v.fail * v.calls + e.failRate * e.calls) / Math.max(v.calls + e.calls, 1);
    v.calls += e.calls;
    v.p95 = Math.max(v.p95, e.p95);
    inbound.set(e.to, v);
  }
  return (
    <div className="graph">
      <svg viewBox={`0 0 ${W} ${H}`} role="img" aria-label="Agent call graph">
        <defs>
          <marker id="v3ar" viewBox="0 0 10 10" refX="9" refY="5" markerWidth="7" markerHeight="7" markerUnits="userSpaceOnUse" orient="auto">
            <path d="M0,0 L10,5 L0,10 z" fill="var(--fg-2)" />
          </marker>
        </defs>
        {d.edges.map((e, i) => {
          const a = pos.get(e.from);
          const b = pos.get(e.to);
          if (!a || !b) return null;
          const tn = d.nodes.find((n) => n.id === e.to);
          return (
            <path
              key={i}
              d={`M${a.x + 46} ${a.y} C ${(a.x + b.x) / 2 + 30} ${a.y}, ${(a.x + b.x) / 2} ${b.y}, ${b.x - 4} ${b.y}`}
              stroke={e.failRate > 0.1 ? "var(--err)" : KIND_COLOR[tn?.kind || "mcp"]}
              strokeWidth={1.2 + (e.calls / maxCalls) * 5}
              strokeDasharray={e.failRate > 0.1 ? "5 3" : undefined}
              fill="none"
              opacity={0.75}
              markerEnd="url(#v3ar)"
            />
          );
        })}
        {left.map((n) => {
          const p = pos.get(n.id)!;
          return (
            <g key={n.id} className="node" onClick={() => onNode(n.id, n.kind)} tabIndex={0}>
              <circle cx={p.x} cy={p.y} r={44} fill="var(--panel-2)" stroke={KIND_COLOR[n.kind]} strokeWidth={2} />
              <text x={p.x} y={p.y - 3} textAnchor="middle" fontWeight={600}>
                {n.label}
              </text>
              <text x={p.x} y={p.y + 13} textAnchor="middle" className="lbl">
                {n.count.toLocaleString()} calls
              </text>
            </g>
          );
        })}
        {right.map((n) => {
          const p = pos.get(n.id)!;
          const v = inbound.get(n.id);
          return (
            <g key={n.id} className="node" onClick={() => onNode(n.id, n.kind)} tabIndex={0}>
              <rect x={p.x} y={p.y - 22} width={150} height={44} rx={9} fill="var(--panel-2)" stroke={KIND_COLOR[n.kind]} strokeWidth={2} />
              <text x={p.x + 75} y={p.y - 3} textAnchor="middle" fontWeight={600}>
                {n.label.length > 18 ? n.label.slice(0, 17) + "…" : n.label}
              </text>
              <text x={p.x + 75} y={p.y + 13} textAnchor="middle" className="lbl">
                {n.kind}
              </text>
              {v && (
                <text x={p.x + 160} y={p.y + 4} className="mono">
                  {v.calls.toLocaleString()} · {(v.fail * 100).toFixed(0)}% fail · p95 {fmtDur(v.p95)}
                </text>
              )}
            </g>
          );
        })}
      </svg>
    </div>
  );
}
