import { Link } from "react-router-dom";
import { useApi, v3 } from "../api/client";
import type { Monitor } from "../api/types";
import { Load } from "../components/ui";
import { useV3 } from "../live";

const STATE_LABEL: Record<string, string> = { ok: "OK", warn: "Warn", alert: "Alert" };

export default function Monitors() {
  const st = useV3();
  const res = useApi(() => v3.monitors(), [], { poll: 30_000 });
  return (
    <div className="view pad">
      <div className="viewhead">
        <div>
          <h2>Monitors</h2>
          <p>Conditions the daemon checks every 30 seconds. A change of state arrives live.</p>
        </div>
      </div>
      <div className="tblwrap">
        <Load st={res} endpoint="/api/v3/monitors" rows={6} isEmpty={(d) => !d.monitors?.length} empty={<div className="state"><b>No monitors defined</b></div>}>
          {(d) => {
            // live WS updates win over the last poll
            const list: Monitor[] = d.monitors.map((m) => st.monitors.get(m.id) || m);
            return (
              <table className="tbl">
                <thead>
                  <tr>
                    <th>Monitor</th>
                    <th>Condition</th>
                    <th>State</th>
                    <th>Detail</th>
                  </tr>
                </thead>
                <tbody>
                  {list.map((m) => (
                    <tr key={m.id}>
                      <td style={{ fontWeight: 600 }}>{m.name}</td>
                      <td className="fg2">{m.condition}</td>
                      <td className={`mon-${m.state}`}>
                        <span className={`dot ${m.state === "ok" ? "d-ok" : m.state === "warn" ? "d-need" : "d-err"}`} style={{ marginRight: 6 }} />
                        <b>{STATE_LABEL[m.state] || m.state}</b>
                      </td>
                      <td>
                        {m.link ? (
                          m.link.startsWith("/") ? (
                            <Link to={m.link}>{m.detail || "open"}</Link>
                          ) : (
                            <a href={m.link} target="_blank" rel="noreferrer">
                              {m.detail || "open"}
                            </a>
                          )
                        ) : (
                          <span className="fg2">{m.detail}</span>
                        )}
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            );
          }}
        </Load>
      </div>
    </div>
  );
}
