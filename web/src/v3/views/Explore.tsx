import { useEffect, useState } from "react";
import { Link, useSearchParams } from "react-router-dom";
import { fmtAgo, fmtDur, projectName } from "../../lib/format";
import { useApi, v3 } from "../api/client";
import type { SpansResponse } from "../api/types";
import { AgentBadge, Load } from "../components/ui";
import { useV3 } from "../live";

const GROUPS = [
  { k: "cmdKey", label: "command" },
  { k: "tool", label: "tool" },
  { k: "repo", label: "repo" },
  { k: "errSig", label: "error" },
  { k: "none", label: "none (spans)" },
];
export const SINCE = ["24h", "7d", "30d"];

export default function Explore() {
  const [sp, setSp] = useSearchParams();
  const st = useV3();
  const q = sp.get("q") || "";
  const groupBy = sp.get("groupBy") || "cmdKey";
  const since = sp.get("since") || "7d";
  const minDur = Number(sp.get("minDur") || 0);
  const [text, setText] = useState(q);
  useEffect(() => setText(q), [q]);
  const set = (k: string, v: string) => {
    const n = new URLSearchParams(sp);
    if (v) n.set(k, v);
    else n.delete(k);
    setSp(n, { replace: true });
  };
  const res = useApi(() => v3.spans({ q, groupBy, since, minDur: minDur || undefined, limit: 200 }), [q, groupBy, since, minDur]);

  return (
    <div className="view pad">
      <div className="viewhead">
        <div>
          <h2>Span explorer</h2>
          <p>Search tool calls across every transcript. Filter by tool, command, duration, outcome and repo, then group. Red is failed calls.</p>
        </div>
      </div>
      <div className="frame">
        <div className="tr-top">
          <form
            className="row"
            style={{ flex: "1 1 320px" }}
            onSubmit={(e) => {
              e.preventDefault();
              set("q", text.trim());
            }}
          >
            <input className="inp grow1" value={text} onChange={(e) => setText(e.target.value)} placeholder="tool:Bash is:error repo:agent-monitor go test" aria-label="Span query" />
            <button className="btn sm" type="submit">
              Run
            </button>
          </form>
          <span className="seg">
            {SINCE.map((s) => (
              <button key={s} className={since === s ? "on" : ""} onClick={() => set("since", s)}>
                {s}
              </button>
            ))}
          </span>
          <select className="inp" value={String(minDur)} onChange={(e) => set("minDur", e.target.value === "0" ? "" : e.target.value)} aria-label="Minimum duration" style={{ padding: "4px 6px" }}>
            <option value="0">any duration</option>
            <option value="1000">&gt; 1s</option>
            <option value="10000">&gt; 10s</option>
            <option value="60000">&gt; 60s</option>
            <option value="300000">&gt; 5m</option>
          </select>
          <span className="seg">
            {GROUPS.map((g) => (
              <button key={g.k} className={groupBy === g.k ? "on" : ""} onClick={() => set("groupBy", g.k === "cmdKey" ? "" : g.k)}>
                {g.label}
              </button>
            ))}
          </span>
        </div>
        <Load st={res} endpoint="/api/spans" rows={8} isEmpty={(d) => !d.total} empty={<div className="state"><b>No spans match</b><div>{q ? "Loosen the query or widen the time range." : "The span index is empty. It fills as the indexer reads transcripts."}</div></div>}>
          {(d) => <Results d={d} groupBy={groupBy} threadOf={st.threadOf} onGroup={(k) => set("q", groupBy === "tool" ? `tool:${k}` : groupBy === "repo" ? `repo:${k}` : k)} />}
        </Load>
      </div>
    </div>
  );
}

function Results({ d, groupBy, threadOf, onGroup }: { d: SpansResponse; groupBy: string; threadOf: Record<string, string>; onGroup: (k: string) => void }) {
  const buckets = d.histogram?.buckets || [];
  const max = Math.max(...buckets.map((b) => b.n), 1);
  return (
    <>
      <div className="row sm fg2" style={{ padding: "10px 16px 0" }}>
        <span className="tab-num">
          {d.total.toLocaleString()} spans · <span className="err-t">{d.failed.toLocaleString()} failed</span>
        </span>
      </div>
      {buckets.length > 0 && (
        <>
          <div className="hist" aria-label="Duration histogram">
            {buckets.map((b, i) => (
              <i key={i} style={{ height: `${(b.n / max) * 100}%` }} title={`${fmtDur(b.fromMs)} to ${b.toMs > 0 ? fmtDur(b.toMs) : "∞"}: ${b.n} spans, ${b.failed} failed`}>
                {b.failed > 0 && <b style={{ height: `${(b.failed / Math.max(b.n, 1)) * 100}%` }} />}
              </i>
            ))}
          </div>
          <div className="row xs muted" style={{ justifyContent: "space-between", padding: "2px 16px 10px" }}>
            <span>{fmtDur(buckets[0].fromMs)}</span>
            <span>{fmtDur(buckets[Math.floor(buckets.length / 2)].fromMs)}</span>
            <span>{buckets[buckets.length - 1].toMs > 0 ? fmtDur(buckets[buckets.length - 1].toMs) : `${fmtDur(buckets[buckets.length - 1].fromMs)}+`}</span>
          </div>
        </>
      )}
      <div style={{ overflowX: "auto", borderTop: "1px solid var(--line)" }}>
        {groupBy === "none" ? (
          <table className="tbl">
            <thead>
              <tr>
                <th>Command</th>
                <th>Tool</th>
                <th>Repo</th>
                <th>Duration</th>
                <th>When</th>
              </tr>
            </thead>
            <tbody>
              {(d.spans || []).map((s) => (
                <tr key={s.id}>
                  <td style={{ maxWidth: 520 }}>
                    <Link to={`/t/${encodeURIComponent(threadOf[s.session] || s.session)}?session=${encodeURIComponent(s.session)}&span=${encodeURIComponent(s.id)}`} className="row" style={{ color: "var(--fg)" }}>
                      <AgentBadge tool={s.agent} small />
                      <code className="trunc">{s.cmd}</code>
                    </Link>
                    {s.err ? <div className="xs err-t trunc">{s.errText || s.errSig}</div> : null}
                  </td>
                  <td>{s.tool}</td>
                  <td>{s.repo || projectName(s.cwd)}</td>
                  <td className="n" style={s.err ? { color: "var(--err)" } : undefined}>
                    {fmtDur(s.dur)}
                  </td>
                  <td className="n muted">{fmtAgo(s.ts)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        ) : (
          <table className="tbl">
            <thead>
              <tr>
                <th>{GROUPS.find((g) => g.k === groupBy)?.label || "Group"}</th>
                <th>Runs</th>
                <th>p50</th>
                <th>p95</th>
                <th>Failed</th>
                <th>Repos</th>
                <th>Last</th>
              </tr>
            </thead>
            <tbody>
              {d.groups.map((g) => (
                <tr key={g.key} className="clk" onClick={() => onGroup(g.key)}>
                  <td style={{ maxWidth: 460 }}>
                    <code className="trunc" style={{ display: "block" }}>
                      {g.key || "(none)"}
                    </code>
                  </td>
                  <td className="n">{g.runs.toLocaleString()}</td>
                  <td className="n">{fmtDur(g.p50)}</td>
                  <td className="n">{fmtDur(g.p95)}</td>
                  <td className="n" style={g.failed ? { color: "var(--err)" } : undefined}>
                    {g.failed}
                  </td>
                  <td className="trunc" style={{ maxWidth: 220 }}>
                    {(g.repos || []).slice(0, 2).join(", ")}
                    {(g.repos || []).length > 2 ? ` +${g.repos.length - 2}` : ""}
                  </td>
                  <td className="n muted">{fmtAgo(g.lastTs)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </div>
    </>
  );
}
