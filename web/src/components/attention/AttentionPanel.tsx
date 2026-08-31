import { useState } from "react";
import { useNavigate } from "react-router-dom";
import { api } from "../../api/client";
import type { PermissionRequest } from "../../api/types";
import { fmtAgo, shortCwd, titleFor } from "../../lib/format";
import { Icon, ToolLogo } from "../../lib/icons";
import { dropPerm, dropTalk, useLive } from "../../lib/ws";
import { StatePill } from "../../app/ui";
import { showToast } from "../../app/toast";

function oneLine(input: Record<string, unknown>): string {
  const v = (input.command || input.file_path || input.url || input.query) as string | undefined;
  return v ? String(v) : "";
}

function PermCard({ r }: { r: PermissionRequest }) {
  const [busy, setBusy] = useState(false);
  const respond = async (b: "allow" | "deny") => {
    setBusy(true);
    try {
      await api.respondPerm(r.id, b);
      dropPerm(r.id);
    } catch (e) {
      setBusy(false);
      showToast("Permission response failed", String((e as Error).message));
    }
  };
  const line = oneLine(r.input);
  return (
    <div className="sec">
      <div className="row" style={{ gap: 8 }}>
        <Icon name="alert" size={13} color="var(--red)" />
        <span style={{ fontWeight: 600 }}>Permission requested</span>
        <span className="chip mono" style={{ height: 20, fontSize: 11 }}>{r.toolName}</span>
        <span className="num muted" style={{ fontSize: 10.5, marginLeft: "auto" }}>{fmtAgo(r.createdAt)}</span>
      </div>
      {r.cwd && <div className="mono muted" style={{ fontSize: 11 }}>{shortCwd(r.cwd)}</div>}
      <pre className="code">{line || JSON.stringify(r.input, null, 2)}</pre>
      <div className="row" style={{ gap: 6 }}>
        <button className="btn ok" disabled={busy} onClick={() => respond("allow")}>
          <Icon name="check" size={12} /> allow once
        </button>
        <button className="btn no" disabled={busy} onClick={() => respond("deny")}>
          deny
        </button>
      </div>
    </div>
  );
}

/** Right-hand panel on the threads page: MCP permission requests, sessions
 *  waiting on the user, talks to deliver. */
export default function AttentionPanel({ open }: { open?: boolean }) {
  const live = useLive();
  const nav = useNavigate();
  const perms = [...live.perms.values()].sort((a, b) => a.createdAt - b.createdAt);
  const waiting = [...live.sessions.values()]
    .filter((s) => s.state === "awaiting-permission" || s.state === "awaiting-input")
    .sort((a, b) => (a.state === "awaiting-permission" ? 0 : 1) - (b.state === "awaiting-permission" ? 0 : 1) || b.lastActivityAt - a.lastActivityAt);
  const talks = [...live.talks.values()].sort((a, b) => a.createdAt - b.createdAt);
  const total = perms.length + waiting.length + talks.length;
  return (
    <aside className={`side ${open ? "open" : ""}`}>
      <div className="sec" style={{ flexDirection: "row", alignItems: "center", gap: 8 }}>
        <span className="k">needs attention · {total}</span>
      </div>
      {perms.map((r) => <PermCard key={r.id} r={r} />)}
      {waiting.map((s) => (
        <div key={s.id} className="sec">
          <div className="row" style={{ gap: 8, minWidth: 0 }}>
            <ToolLogo tool={s.tool} size={14} />
            <span className="ell" style={{ fontWeight: 600, flex: 1 }}>{titleFor(s)}</span>
            <StatePill state={s.state} />
          </div>
          {(s.permissionMessage || s.lastMessage) && (
            <div className="muted" style={{ fontSize: 11.5, lineHeight: 1.4 }}>
              {(s.permissionMessage || s.lastMessage || "").slice(0, 220)}
            </div>
          )}
          <div className="row" style={{ gap: 6 }}>
            <button className="btn sm" onClick={() => nav(`/session/${encodeURIComponent(s.id)}?tab=pane`)}>
              <Icon name="terminal" size={12} /> pane
            </button>
            <button className="btn sm" onClick={() => nav(`/session/${encodeURIComponent(s.id)}`)}>
              open <Icon name="arrow" size={11} />
            </button>
            <span className="num muted" style={{ fontSize: 10.5, marginLeft: "auto" }}>{fmtAgo(s.lastActivityAt)}</span>
          </div>
        </div>
      ))}
      {talks.map((t) => (
        <div key={t.id} className="sec">
          <div className="row" style={{ gap: 8 }}>
            <Icon name="arrow" size={12} color="var(--magenta)" />
            <span style={{ fontWeight: 600 }}>talk · {t.fromLabel} → {t.toLabel || t.toAgent.slice(0, 14)}</span>
            <span className="num muted" style={{ fontSize: 10.5, marginLeft: "auto" }}>{fmtAgo(t.createdAt)}</span>
          </div>
          <pre className="code">{t.message}</pre>
          <div className="row" style={{ gap: 6 }}>
            <button className="btn sm ok" onClick={() => api.respondTalk(t.id, "allow").then(() => dropTalk(t.id)).catch((e) => showToast("Talk failed", String(e.message)))}>
              deliver to pane
            </button>
            <button className="btn sm no" onClick={() => api.respondTalk(t.id, "deny").then(() => dropTalk(t.id)).catch((e) => showToast("Talk failed", String(e.message)))}>
              deny
            </button>
          </div>
        </div>
      ))}
      {total === 0 && <div className="sec muted" style={{ fontSize: 12 }}>Nothing waits on you. Permission prompts, agents waiting for a reply, and talks appear here.</div>}
      <div className="sec">
        <span className="k">recent learnings</span>
        <div className="muted" style={{ fontSize: 12 }}>learnings appear on each thread's ledger</div>
      </div>
    </aside>
  );
}
