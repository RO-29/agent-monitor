import { useEffect, useState } from "react";
import { useNavigate, useParams, useSearchParams } from "react-router-dom";
import { api } from "../../api/client";
import type { Thread, TraceMeta } from "../../api/types";
import { fmtAgo, fmtTok, fmtUsd, modelShort, shortCwd, titleFor } from "../../lib/format";
import { Icon, ToolLogo } from "../../lib/icons";
import { useLive } from "../../lib/ws";
import { StatePill } from "../../app/ui";
import PaneBridge from "../../components/bridge/PaneBridge";
import TracePanel from "../trace/TracePanel";
import Transcript from "./Transcript";

type Tab = "trace" | "transcript" | "pane";

export default function SessionPage() {
  const { id: raw = "" } = useParams();
  const id = decodeURIComponent(raw);
  const live = useLive();
  const nav = useNavigate();
  const [sp, setSp] = useSearchParams();
  const tab = ((sp.get("tab") as Tab) || "trace") as Tab;
  const sess = live.sessions.get(id);
  const [meta, setMeta] = useState<TraceMeta | null>(null);
  const [thread, setThread] = useState<Thread | null>(null);
  const [errCount, setErrCount] = useState<number | null>(null);
  const [paneOpen, setPaneOpen] = useState(false);

  // Cost / context / errors come from the trace summary; thread from /api/threads.
  useEffect(() => {
    let alive = true;
    api
      .segments(id)
      .then((r) => {
        if (!alive) return;
        setMeta(r.meta);
        setErrCount(r.segments.reduce((a, s) => a + s.errors, 0));
      })
      .catch(() => alive && setMeta(null));
    api
      .threads()
      .then((r) => {
        if (!alive) return;
        const root = r.threadOf[id] || id;
        setThread(r.threads.find((t) => t.id === root) || null);
      })
      .catch(() => null);
    return () => {
      alive = false;
    };
  }, [id, sess?.messageCount]);

  const setTab = (t: Tab) => {
    const n = new URLSearchParams(sp);
    if (t === "trace") n.delete("tab");
    else n.set("tab", t);
    setSp(n, { replace: true });
  };

  if (!sess) {
    return (
      <div className="empty">
        {live.sessions.size === 0 ? "connecting…" : "Session not found in the live store."}
        <div style={{ marginTop: 8 }}>
          <button className="btn sm" onClick={() => nav("/")}>
            <Icon name="chevl" size={12} /> all threads
          </button>
        </div>
      </div>
    );
  }
  const tk = sess.tokens || { input: 0, output: 0, cacheRead: 0, cacheCreate: 0 };
  const total = tk.input + tk.output + tk.cacheRead + tk.cacheCreate;
  const threadId = thread?.id || id;
  const nSessions = thread?.sessions.length || 1;

  return (
    <div style={{ flex: 1, display: "flex", flexDirection: "column", minHeight: 0 }}>
      <div className="pagehead">
        <div className="row" style={{ gap: 10, flexWrap: "wrap" }}>
          <ToolLogo tool={sess.tool} size={16} />
          <h1 style={{ flex: "0 1 auto" }} title={sess.cwd}>{titleFor(sess) || "Untitled session"}</h1>
          <StatePill state={sess.state} />
          {sess.model && <span className="chip mono" style={{ fontSize: 11 }}>{modelShort(sess.model)}</span>}
          {meta && <span className="chip num" style={{ fontSize: 11 }} title={meta.costEstimated ? "estimated from a public price table" : "from the agent's own cost record"}>{fmtUsd(meta.costUsd, meta.costEstimated)}</span>}
          <span className="chip num" style={{ fontSize: 11 }} title={`in ${fmtTok(tk.input)} · out ${fmtTok(tk.output)} · cache ${fmtTok(tk.cacheRead + tk.cacheCreate)}`}>{fmtTok(total)} tok</span>
          <span className="chip num" style={{ fontSize: 11 }}>{sess.messageCount.toLocaleString()} turns</span>
          {errCount !== null && <span className="chip num" style={{ fontSize: 11, color: errCount ? "var(--red)" : undefined }}>{errCount} errors</span>}
          <span className="mono muted" style={{ fontSize: 11 }}>{shortCwd(sess.cwd)} · {fmtAgo(sess.lastActivityAt)}</span>
          <div style={{ flex: 1 }} />
          <button className="btn" onClick={() => nav(`/thread/${encodeURIComponent(threadId)}`)}>
            <Icon name="book" size={13} /> story
          </button>
          <button className="btn" onClick={() => nav(`/thread/${encodeURIComponent(threadId)}/learnings`)} title="thread learnings ledger">
            <Icon name="thread" size={13} /> whole thread · {nSessions} session{nSessions === 1 ? "" : "s"}
          </button>
          <button className={`btn ${paneOpen || tab === "pane" ? "on" : "pri"}`} onClick={() => (tab === "pane" ? setTab("trace") : setPaneOpen((o) => !o))}>
            <Icon name="terminal" size={13} color={paneOpen || tab === "pane" ? undefined : "#07080b"} /> {sess.state === "running" ? "pane" : "resume / pane"}
          </button>
        </div>
      </div>
      {paneOpen && tab !== "pane" && (
        <div style={{ flex: "none", borderBottom: "1px solid var(--border)", background: "var(--bg-1)", maxHeight: "50vh", overflowY: "auto" }}>
          <PaneBridge session={sess} />
        </div>
      )}
      <div className="tabs">
        {(["trace", "transcript", "pane"] as Tab[]).map((t) => (
          <button key={t} className={`tab ${tab === t ? "on" : ""}`} onClick={() => setTab(t)}>
            {t === "trace" ? "Trace" : t === "transcript" ? "Transcript" : "Pane"}
          </button>
        ))}
      </div>
      <div style={{ flex: 1, minHeight: 0, display: "flex", flexDirection: "column" }}>
        {tab === "trace" && <TracePanel sessionId={id} segment={sp.get("seg") ? Number(sp.get("seg")) : undefined} spanId={sp.get("span") || undefined} />}
        {tab === "transcript" && <Transcript session={sess} />}
        {tab === "pane" && (
          <div style={{ flex: 1, overflowY: "auto" }}>
            <PaneBridge session={sess} />
          </div>
        )}
      </div>
    </div>
  );
}
