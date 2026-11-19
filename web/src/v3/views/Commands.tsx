// History of ! commands from every session: filter, copy, open the thread, or run again where it ran.
import { useEffect, useMemo, useState } from "react";
import { Link } from "react-router-dom";
import { req } from "../api/orch";
import { useV3 } from "../live";
import { cliApi } from "../api/queue";
import { copy } from "../../lib/clipboard";
import { showToast } from "../../app/toast";
import type { CmdRun } from "./workspace/bash";
import type { PromptRun } from "./workspace/prompts";
import "../styles/settings.css";

const ago = (ms?: number) => {
  if (!ms) return "";
  const s = Math.max(0, (Date.now() - ms) / 1000);
  return s < 60 ? "now" : s < 3600 ? `${Math.floor(s / 60)} min` : s < 86400 ? `${Math.floor(s / 3600)} h` : `${Math.floor(s / 86400)} d`;
};
const base = (p?: string) => (p || "").split("/").filter(Boolean).pop() || "";

export default function History() {
  const [mode, setMode] = useState<"prompts" | "commands">(() => {
    try {
      return localStorage.getItem("am-history-mode") === "commands" ? "commands" : "prompts";
    } catch {
      return "prompts";
    }
  });
  const pick = (m: "prompts" | "commands") => {
    setMode(m);
    try {
      localStorage.setItem("am-history-mode", m);
    } catch {
      /* storage blocked */
    }
  };
  return (
    <div className="view pad sx">
      <div className="viewhead">
        <div>
          <h2>History</h2>
          <p>{mode === "prompts" ? "Every prompt you typed to Claude Code and Codex, newest first." : "Every ! command from your Claude Code and Codex sessions, newest first."}</p>
        </div>
      </div>
      <div className="seg" role="tablist" aria-label="History kind" style={{ marginBottom: 10 }}>
        <button role="tab" aria-selected={mode === "prompts"} className={mode === "prompts" ? "on" : ""} onClick={() => pick("prompts")}>
          Prompts
        </button>
        <button role="tab" aria-selected={mode === "commands"} className={mode === "commands" ? "on" : ""} onClick={() => pick("commands")}>
          ! Commands
        </button>
      </div>
      {mode === "prompts" ? <Prompts /> : <Commands />}
    </div>
  );
}

function Prompts() {
  const st = useV3();
  const [q, setQ] = useState("");
  const [list, setList] = useState<PromptRun[] | null>(null);
  const [err, setErr] = useState("");
  const [open, setOpen] = useState<number | null>(null);
  useEffect(() => {
    const t = setTimeout(() => {
      req<{ items?: PromptRun[] }>("GET", `/api/prompts?bash=no&limit=300&q=${encodeURIComponent(q.trim())}`)
        .then((r) => (setList(r.items || []), setErr("")))
        .catch((e) => setErr(e instanceof Error ? e.message : String(e)));
    }, q ? 250 : 0);
    return () => clearTimeout(t);
  }, [q]);
  return (
    <>
      <input className="inp" placeholder="Search prompts or folders" value={q} onChange={(e) => setQ(e.target.value)} aria-label="Search prompts" />
      {err && <p className="err-t">{err}</p>}
      {list && !list.length && <p className="sx-p muted">{q ? "No prompt matches." : "No prompts yet."}</p>}
      <div className="sx-col" style={{ gap: 6, marginTop: 10 }}>
        {(list || []).map((c, i) => (
          <div key={`${c.sessionId}|${c.at}|${i}`} className="ibox" style={{ padding: "8px 10px", minWidth: 0 }}>
            <button
              style={{ display: "block", width: "100%", textAlign: "left", background: "none", border: 0, color: "inherit", cursor: "pointer", whiteSpace: open === i ? "pre-wrap" : "nowrap", overflow: "hidden", textOverflow: "ellipsis", minWidth: 0 }}
              onClick={() => setOpen(open === i ? null : i)}
              title={open === i ? undefined : c.text}
            >
              {c.text}
            </button>
            <div style={{ display: "flex", gap: 6, alignItems: "center", flexWrap: "wrap", marginTop: 6 }}>
              <span className={`pill ${c.agent === "codex" ? "p-mute" : "p-ok"}`}>{c.agent || "claude"}</span>
              {c.sessionId && (
                <Link className="muted sm" to={`/w/${encodeURIComponent(st.threadOf[c.sessionId] || c.sessionId)}`}>
                  {base(c.cwd) || "thread"}
                </Link>
              )}
              <small className="muted">{ago(c.at)}</small>
              <span style={{ flex: 1 }} />
              <button className="btn ghost sm" onClick={() => void copy(c.text).then(() => showToast("Prompt copied."))}>
                Copy
              </button>
            </div>
          </div>
        ))}
      </div>
    </>
  );
}

function Commands() {
  const st = useV3();
  const [list, setList] = useState<CmdRun[] | null>(null);
  const [err, setErr] = useState("");
  const [q, setQ] = useState("");
  const [open, setOpen] = useState<string | null>(null);
  const [confirm, setConfirm] = useState<string | null>(null);
  const load = () =>
    req<{ items?: CmdRun[] }>("GET", "/api/commands/recent?limit=200")
      .then((r) => (setList((r.items || []).filter((x) => x?.cmd?.trim())), setErr("")))
      .catch((e) => setErr(e instanceof Error ? e.message : String(e)));
  useEffect(() => {
    load();
  }, []);
  const shown = useMemo(() => {
    const t = q.trim().toLowerCase();
    return (list || []).filter((c) => !t || c.cmd.toLowerCase().includes(t) || (c.cwd || "").toLowerCase().includes(t));
  }, [list, q]);
  const key = (c: CmdRun) => `${c.sessionId}|${c.at}|${c.cmd}`;
  const run = async (c: CmdRun) => {
    setConfirm(null);
    try {
      const queued = await cliApi.send(c.sessionId!, "!" + c.cmd);
      showToast(queued ? "The agent is busy; it runs the command next." : "Sent to the terminal.");
      setTimeout(load, 3000);
    } catch (e) {
      showToast(e instanceof Error ? e.message : String(e));
    }
  };
  return (
    <>
      <input className="inp" placeholder="Filter by command or folder" value={q} onChange={(e) => setQ(e.target.value)} aria-label="Filter commands" />
      {err && <p className="err-t">{err}</p>}
      {list && !shown.length && <p className="sx-p muted">{q ? "No command matches." : "No commands yet. Type ! and a command in a thread's message box."}</p>}
      <div className="sx-col" style={{ gap: 6, marginTop: 10 }}>
        {shown.map((c) => {
          const k = key(c);
          const failed = c.exitCode != null && c.exitCode !== 0;
          return (
            <div key={k} className="ibox" style={{ padding: "8px 10px", minWidth: 0 }}>
              <div style={{ display: "flex", gap: 8, alignItems: "center", minWidth: 0 }}>
                <span className={`pill ${failed ? "p-err" : c.exitCode === 0 ? "p-ok" : "p-mute"}`} title={c.exitCode != null ? `exit ${c.exitCode}` : "no exit code"}>
                  {c.exitCode != null ? c.exitCode : "·"}
                </span>
                <button className="mono" style={{ flex: 1, minWidth: 0, textAlign: "left", overflow: "hidden", textOverflow: "ellipsis", whiteSpace: "nowrap", background: "none", border: 0, color: "inherit", cursor: "pointer" }} onClick={() => setOpen(open === k ? null : k)} title={c.cmd}>
                  ! {c.cmd}
                </button>
                <small className="muted">{ago(c.at)}</small>
              </div>
              <div style={{ display: "flex", gap: 6, alignItems: "center", flexWrap: "wrap", marginTop: 6 }}>
                {c.sessionId && (
                  <Link className="muted sm" to={`/w/${encodeURIComponent(st.threadOf[c.sessionId] || c.sessionId)}`}>
                    {base(c.cwd) || c.sessionId.split(":")[0]}
                  </Link>
                )}
                <span style={{ flex: 1 }} />
                <button className="btn ghost sm" onClick={() => void copy(c.cmd).then(() => showToast("Command copied."))}>
                  Copy
                </button>
                {c.sessionId &&
                  (confirm === k ? (
                    <>
                      <button className="btn pri sm" onClick={() => void run(c)}>
                        Run in its terminal
                      </button>
                      <button className="btn ghost sm" onClick={() => setConfirm(null)}>
                        Cancel
                      </button>
                    </>
                  ) : (
                    <button className="btn ghost sm" onClick={() => setConfirm(k)}>
                      Run again
                    </button>
                  ))}
              </div>
              {open === k && c.output && <pre className="mono" style={{ overflowX: "auto", maxHeight: 280, margin: "8px 0 0", fontSize: 12 }}>{c.output}</pre>}
            </div>
          );
        })}
      </div>
    </>
  );
}
