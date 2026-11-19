// Settings → Agents: which agent config files agent-monitor edits, and the one card that approves a kind of edit.
import { useEffect, useState } from "react";
import { req } from "../../api/orch";
import { SettingsTabs } from "./Tabs";
import "../../styles/settings.css";
import "../../styles/p6.css";

interface WireResult {
  agent: string;
  kind: string;
  file: string;
  action: string;
  entries?: string[];
  detail?: string;
  restartNeeded?: boolean;
  unverified?: boolean;
}
interface WireState {
  results: WireResult[];
  pending: { kind: string; items: WireResult[] }[];
  autoWire: Record<string, string>;
  active: boolean;
  why: string;
  installDir: string;
  legacyBackups: string[];
}

const KIND: Record<string, string> = { mcp: "MCP server entries", hook: "Hook entries", extension: "Extension files" };

export default function Wiring() {
  const [st, setSt] = useState<WireState | null>(null);
  const [err, setErr] = useState("");
  const [busy, setBusy] = useState("");
  const load = () =>
    req<WireState>("GET", "/api/agents/wiring")
      .then((s) => (setSt(s), setErr("")))
      .catch((e) => setErr(e instanceof Error ? e.message : String(e)));
  useEffect(() => {
    load();
    const t = window.setInterval(load, 15000);
    return () => window.clearInterval(t);
  }, []);

  const answer = async (kind: string, value: string) => {
    setBusy(kind);
    try {
      await req("POST", "/api/agents/wiring/confirm", { kind, value });
      window.setTimeout(load, 1500);
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy("");
    }
  };

  return (
    <div className="view pad sx">
      <SettingsTabs />
      <div className="viewhead">
        <div>
          <h2>Agents</h2>
          <p>agent-monitor adds its MCP server and hook entries to your agent config files. It asks once for each kind of edit. It never changes permission settings.</p>
        </div>
      </div>
      <div className="sx-col">
        {err && <p className="err-t">{err}</p>}
        {st && !st.active && <p className="sx-p">Wiring is paused: {st.why}.</p>}
        {st?.pending.map((p) => (
          <div className="ibox" key={p.kind}>
            <h3>Approve: {KIND[p.kind] ?? p.kind}</h3>
            <p className="sx-p">agent-monitor will edit these files. It saves a backup first, and `agent-monitor unwire` removes only what it added.</p>
            <ul>
              {p.items.map((it) => (
                <li key={it.agent + it.file}>
                  <b>{it.agent}</b> <code>{it.file}</code>
                  {it.entries?.length ? `: ${it.entries.join(", ")}` : ""}
                  {it.unverified ? " (format taken from the agent's docs, not tested)" : ""}
                </li>
              ))}
            </ul>
            <div className="row" style={{ display: "flex", gap: 8, flexWrap: "wrap" }}>
              <button className="btn pri" disabled={busy === p.kind} onClick={() => answer(p.kind, "on")}>
                Allow
              </button>
              <button className="btn deny" disabled={busy === p.kind} onClick={() => answer(p.kind, "off")}>
                Never for this kind
              </button>
            </div>
          </div>
        ))}
        <div className="ibox">
          <h3>Wiring status</h3>
          {st && st.results.length === 0 && <p className="sx-p">No run yet.</p>}
          <table className="tbl">
            <thead>
              <tr>
                <th>Agent</th>
                <th>Kind</th>
                <th>File</th>
                <th>Result</th>
              </tr>
            </thead>
            <tbody>
              {st?.results.map((r) => (
                <tr key={r.agent + r.kind + r.file}>
                  <td>{r.agent}</td>
                  <td>{r.kind}</td>
                  <td>
                    <code>{r.file}</code>
                  </td>
                  <td>
                    {r.action}
                    {r.restartNeeded ? " (restart the agent)" : ""}
                    {r.detail && r.action.startsWith("manual") ? <pre className="sx-p">{r.detail}</pre> : null}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
        {st && st.legacyBackups.length > 0 && (
          <div className="ibox">
            <h3>Old backup files</h3>
            <p className="sx-p">Older installers left these. agent-monitor does not delete them.</p>
            <ul>
              {st.legacyBackups.map((f) => (
                <li key={f}>
                  <code>{f}</code>
                </li>
              ))}
            </ul>
          </div>
        )}
      </div>
    </div>
  );
}
