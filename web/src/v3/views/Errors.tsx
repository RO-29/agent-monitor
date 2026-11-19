import { useState } from "react";
import { Link } from "react-router-dom";
import { fmtAgo } from "../../lib/format";
import { useApi, v3 } from "../api/client";
import { Load, Spark } from "../components/ui";
import { SINCE } from "./Explore";

export default function Errors() {
  const [since, setSince] = useState("7d");
  const res = useApi(() => v3.errors({ since }), [since], { poll: 60_000 });
  const dayStart = new Date().setHours(0, 0, 0, 0);
  return (
    <div className="view pad">
      <div className="viewhead">
        <div>
          <h2>Errors</h2>
          <p>Failed tool calls grouped by a normalised signature, so the same failure in many threads reads as one row.</p>
        </div>
        <span className="seg">
          {SINCE.map((s) => (
            <button key={s} className={since === s ? "on" : ""} onClick={() => setSince(s)}>
              {s}
            </button>
          ))}
        </span>
      </div>
      <div className="ibox">
        <Load st={res} endpoint="/api/errors" rows={8} isEmpty={(d) => !d.groups?.length} empty={<div className="state"><b>No failed calls in this range</b></div>}>
          {(d) =>
            d.groups.map((g) => {
              const repos = Array.isArray(g.repos) ? g.repos : [];
              const isNew = g.firstTs >= dayStart;
              return (
                <div key={g.sig} className="insight">
                  <div className="ic tab-num" style={{ background: g.count >= 10 ? "var(--err-soft)" : "var(--need-soft)", color: g.count >= 10 ? "var(--err)" : "var(--need)" }}>
                    {g.count}
                  </div>
                  <div style={{ flex: 1, minWidth: 0 }}>
                    <div className="row">
                      <b className="trunc mono sm">{g.sig || g.sample}</b>
                      <span className={`pill ${isNew ? "p-err" : "p-mute"} ml-auto`} style={{ flex: "none" }}>
                        {isNew ? "new today" : `since ${fmtAgo(g.firstTs)}`}
                      </span>
                    </div>
                    <div className="xs muted trunc">
                      {g.cmdKey || g.tool} · {repos.length ? repos.slice(0, 3).join(", ") : typeof g.repos === "number" ? `${g.repos} repos` : ""} · {g.sessions} sessions · last {fmtAgo(g.lastTs)}
                    </div>
                    {g.sample && g.sample !== g.sig && <div className="xs fg2 mono trunc">{g.sample}</div>}
                  </div>
                  <div style={{ flex: "none", display: "flex", flexDirection: "column", alignItems: "flex-end", gap: 4 }}>
                    <Spark points={g.trend || []} color="var(--err)" />
                    <Link className="xs" to={`/explore?q=${encodeURIComponent(`is:error ${g.cmdKey || ""}`.trim())}&groupBy=none&since=${since}`}>
                      runs ›
                    </Link>
                  </div>
                </div>
              );
            })
          }
        </Load>
      </div>
    </div>
  );
}
