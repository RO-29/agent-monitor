import { useEffect, useMemo } from "react";
import { Link } from "react-router-dom";
import { useTheme } from "../../app/prefs";
import { dismissToast, useToasts } from "../../app/toast";
import { fmtAgo, projectName } from "../../lib/format";
import { useLive } from "../../lib/ws";
import { LegacyPermCard } from "../components/ApprovalCard";
import { AskKeysCtx, askFromApproval, WsApproval } from "./workspace/WsApproval";
import { AgentBadge } from "../components/ui";
import { startV3Live, useV3 } from "../live";
import { activity, buildRows, counts, listRows } from "../model";
import "../styles/v3.css";
import { useNeedsYou } from "../api/needsYou";

/** Compact page for the Mac menu bar popover: pending approvals, running list, today's totals. */
export default function Menubar() {
  useEffect(startV3Live, []);
  useTheme();
  const live = useLive();
  const st = useV3();
  const toasts = useToasts();
  const ny = useNeedsYou();
  const rows = useMemo(() => buildRows(st, live, ny), [st.version, live.version, ny.version]); // eslint-disable-line react-hooks/exhaustive-deps
  const c = counts(rows);
  const items = ny.items;
  const need = ny.status === "loading" ? c["needs-you"] : ny.count;
  const running = listRows(rows, "running");
  const open = (path: string) => window.open(path, "_blank", "noopener");

  return (
    <div className="v3 mb">
      <div className="mbh">
        <span className={`dot ${live.connected ? "d-ok" : "d-err"}`} />
        <b>{need ? `${need} need${need > 1 ? "" : "s"} you` : "Nothing needs you"}</b>
        <span className="xs muted">· {c.running} running</span>
        <button className="btn sm ghost ml-auto" onClick={() => open("/")}>
          Open window
        </button>
      </div>
      {st.apprStatus === "missing" && <div className="mbs xs muted">Hook approvals are not available on this daemon; only pane prompts show.</div>}
      {st.apprStatus === "error" && <div className="mbs xs err-t">Approvals failed to load: {st.apprError}</div>}
      {ny.status === "error" && !items.length && <div className="mbs xs err-t">Could not read the list: {ny.error}</div>}
      {items.map((i) => {
        const a = i.approval && (st.approvals.get(i.approval.id) || i.approval);
        const tid = i.threadId || (i.sessionId ? st.threadOf[i.sessionId] || i.sessionId : "");
        if (a)
          return (
            <div key={i.key} className="mbs">
              <AskKeysCtx.Provider value={false}><WsApproval ask={askFromApproval(a)} /></AskKeysCtx.Provider>
            </div>
          );
        if (i.mcp)
          return (
            <div key={i.key} className="mbs">
              <LegacyPermCard p={i.mcp} compact />
            </div>
          );
        return (
          <Link key={i.key} className="lr mbs" to={`/w/${encodeURIComponent(tid)}`} target="_blank">
            <span className={`dot ${i.kind === "error" ? "d-err" : "d-need"}`} />
            <span className="trunc">{i.title || projectName(i.cwd || "")}</span>
            <span className="muted xs">{i.kind}</span>
            <span className="muted xs ml-auto">{fmtAgo(i.at, "")}</span>
          </Link>
        );
      })}
      <div className="mbs" style={{ borderBottom: 0 }}>
        {running.length === 0 && <div className="lr muted xs">Nothing running.</div>}
        {running.slice(0, 8).map((r) => (
          <Link key={r.id} className="lr" to={`/t/${encodeURIComponent(r.id)}`} target="_blank">
            <span className="dot d-run" />
            <AgentBadge tool={r.agent} small />
            <span className="trunc" style={{ maxWidth: 140 }}>
              {r.title}
            </span>
            <span className="muted xs trunc mono">· {activity(r)}</span>
          </Link>
        ))}
        <div className="lr muted xs">
          Today: {c["done-today"]} done · {c["idle-today"]} ready · {need} need you
        </div>
      </div>
      {toasts.length > 0 && (
        <div className="toasts">
          {toasts.map((t) => (
            <div key={t.id} className="toast" onClick={() => dismissToast(t.id)}>
              <b>{t.title}</b>
              {t.body && <div className="fg2 xs">{t.body}</div>}
            </div>
          ))}
        </div>
      )}
    </div>
  );
}
