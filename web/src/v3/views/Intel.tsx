import { useState } from "react";
import { Link } from "react-router-dom";
import { fmtDuration, fmtUsd } from "../../lib/format";
import { useApi, v3 } from "../api/client";
import type { IntelResponse } from "../api/types";
import { durOr0, Load } from "../components/ui";

const FAM: Record<string, string> = { Bash: "var(--ok)", Read: "var(--read)", Grep: "var(--read)", Edit: "var(--need)", Write: "var(--need)", model: "var(--line-2)", Task: "var(--sub)", Agent: "var(--sub)" };

export default function Intel() {
  const [range, setRange] = useState<"today" | "7d">("today");
  const res = useApi(() => v3.intel(range), [range], { poll: 120_000 });
  return (
    <div className="view pad">
      <div className="viewhead">
        <div>
          <h2>Intelligence</h2>
          <p>What the transcripts say about how the work goes. Every number comes from parsed spans. Written insights appear only when enabled.</p>
        </div>
        <span className="seg">
          <button className={range === "today" ? "on" : ""} onClick={() => setRange("today")}>
            Today
          </button>
          <button className={range === "7d" ? "on" : ""} onClick={() => setRange("7d")}>
            This week
          </button>
        </span>
      </div>
      <Load st={res} endpoint="/api/v3/intel" rows={8}>
        {(d) => <Body d={d} range={range} />}
      </Load>
    </div>
  );
}

const FAM_LABEL: Record<string, string> = { bash: "Bash", read: "Read / Grep", edit: "Edit / Write", agent: "Agent calls", subagent: "Subagents", mcp: "MCP", web: "Web", other: "Other", model: "Model (thinking)" };

/** Accepts both the spec's list shape and the map shape ops.go serves. */
function normalize(d: IntelResponse): IntelResponse {
  const out: IntelResponse = { ...d };
  if (!out.tools && d.timeByFamily) out.tools = Object.entries(d.timeByFamily).map(([k, ms]) => ({ name: FAM_LABEL[k] || k, ms }));
  if (!out.spendByProject && d.spendByRepo) out.spendByProject = Object.entries(d.spendByRepo).map(([k, usd]) => ({ project: k, usd, estimated: d.spendEstimated }));
  if (out.workMs === undefined && d.workByAgent) out.workMs = Object.values(d.workByAgent).reduce((a, b) => a + b, 0);
  if (!out.outputs && d.shipped) out.outputs = d.shipped.map((o) => ({ kind: o.kind || o.Kind || "output", label: o.label || o.Label || "", ref: o.ref || o.Ref, ts: o.ts || o.Ts, threadId: o.threadId || o.Thread }));
  if (out.waitMs !== undefined && d.waitKnown === false) out.waitMs = undefined;
  return out;
}

function Body({ d: raw, range }: { d: IntelResponse; range: string }) {
  const d = normalize(raw);
  const tools = (d.tools || []).map((t) => ({ name: t.tool || t.name || "?", ms: t.ms })).sort((a, b) => b.ms - a.ms);
  const tmax = Math.max(...tools.map((t) => t.ms), 1);
  const spend = (d.spendByProject || []).map((s) => ({ name: s.project || s.repo || "?", usd: s.usd, est: s.estimated })).sort((a, b) => b.usd - a.usd);
  const smax = Math.max(...spend.map((s) => s.usd), 0.01);
  const rf = d.repeatFailures || [];
  const label = range === "today" ? "today" : "this week";
  return (
    <>
      <div className="grid3">
        <div className="ibox">
          <h3>
            <span className="dot d-need" />
            Waiting on you
          </h3>
          {d.waitMs !== undefined ? (
            <div className="bignum">{durOr0(d.waitMs, fmtDuration)}</div>
          ) : (
            <div className="muted">not reported</div>
          )}
          {d.medianApprovalMs !== undefined && <div className="sm fg2" style={{ marginTop: 6 }}>Median approval took {fmtDuration(d.medianApprovalMs)}.</div>}
          <Link className="btn sm" to="/rules" style={{ marginTop: 10 }}>
            Approval rules
          </Link>
        </div>
        <div className="ibox">
          <h3>
            <span className="dot d-run" />
            Agent work time
          </h3>
          {d.workMs !== undefined ? <div className="bignum">{fmtDuration(d.workMs)}</div> : <div className="muted">not reported</div>}
          {d.peakParallel !== undefined && <div className="sm fg2" style={{ marginTop: 6 }}>Peak {d.peakParallel} agents in parallel.</div>}
          {d.workByAgent && (
            <div className="sm fg2" style={{ marginTop: 6 }}>
              {Object.entries(d.workByAgent)
                .map(([k, v]) => `${k} ${fmtDuration(v)}`)
                .join(" · ")}
              {d.threads ? ` · ${d.threads} threads` : ""}
            </div>
          )}
        </div>
        <div className="ibox">
          <h3>
            <span className="dot d-err" />
            Repeat failures
          </h3>
          {rf.length ? (
            <>
              <div className="bignum">
                {rf[0].count}×<small>same failure</small>
              </div>
              <div className="sm fg2" style={{ marginTop: 6 }}>
                <code>{rf[0].cmd || rf[0].cmdKey || rf[0].sig}</code>
                {rf[0].repo ? ` in ${rf[0].repo}` : ""}
                {rf[0].threads ? `, ${rf[0].threads} threads` : ""}.
              </div>
            </>
          ) : (
            <div className="muted">None {label}.</div>
          )}
        </div>
      </div>
      <div className="grid2" style={{ marginTop: 16 }}>
        <div className="ibox">
          <h3>Where agent time went · {label}</h3>
          {tools.length === 0 && <div className="muted sm">No tool time reported.</div>}
          {tools.slice(0, 8).map((t) => (
            <div key={t.name} className="hbar">
              <span className="trunc">{t.name}</span>
              <div className="tr">
                <i style={{ width: `${(t.ms / tmax) * 100}%`, background: FAM[t.name] || (t.name.startsWith("mcp") ? "var(--run)" : t.name.includes("codex") ? "var(--codex)" : "var(--accent)") }} />
              </div>
              <span className="v">{fmtDuration(t.ms)}</span>
            </div>
          ))}
        </div>
        <div className="ibox">
          <h3>Spend by project · {label}</h3>
          {spend.length === 0 && <div className="muted sm">No spend reported.</div>}
          {spend.slice(0, 8).map((s) => (
            <div key={s.name} className="hbar">
              <span className="trunc">{s.name}</span>
              <div className="tr">
                <i style={{ width: `${(s.usd / smax) * 100}%`, background: "var(--accent)" }} />
              </div>
              <span className="v">{fmtUsd(s.usd, s.est)}</span>
            </div>
          ))}
          {spend.some((s) => s.est) && <div className="xs muted" style={{ marginTop: 6 }}>≈ marks a list-price estimate.</div>}
        </div>
      </div>
      {(d.outputs?.length || 0) > 0 && (
        <div className="ibox" style={{ marginTop: 16 }}>
          <h3>
            Shipped · {label}
            <span className="muted xs">{d.outputs!.filter((o) => o.kind === "commit").length} commits</span>
          </h3>
          <div className="row" style={{ flexWrap: "wrap", gap: 6 }}>
            {d.outputs!.filter((o) => o.kind !== "commit").slice(0, 30).map((o, i) =>
              o.ref && /^https?:/.test(o.ref) ? (
                <a key={i} className="pill p-ok" href={o.ref} target="_blank" rel="noreferrer">
                  {o.kind} · {o.label}
                </a>
              ) : (
                <span key={i} className="pill p-mute" title={o.ref}>
                  {o.kind} · {o.label}
                </span>
              ),
            )}
          </div>
        </div>
      )}
      <div className="ibox" style={{ marginTop: 16 }}>
        <h3>Insights</h3>
        {d.insights?.length ? (
          d.insights.map((x, i) => (
            <div key={i} className="insight">
              <div className="ic" style={{ background: "var(--accent-soft)", color: "var(--accent)" }}>
                i
              </div>
              <div>
                {x.title && <b>{x.title} </b>}
                <span className="fg2">{x.text}</span>
              </div>
            </div>
          ))
        ) : (
          <div className="muted sm">No written insights. They are opt-in and use the daily model budget in settings.</div>
        )}
      </div>
    </>
  );
}
