import { useEffect, useMemo, useRef, useState } from "react";
import { api } from "../../api/client";
import type { ContextRow, PermissionRequest, Session, TraceSummary } from "../../api/types";
import { agentTag, fmtAgo, fmtUsd, isLive, modelShort, projectName, stateColorVar, stateLabel, stateRank, titleFor, toolName } from "../../lib/format";
import { Icon, ToolLogo } from "../../lib/icons";
import { dropPerm, useLive } from "../../lib/ws";
import { BOUNDARY, stripAnsi } from "../trace/lib";
import "../trace/trace.css";
import "./tv.css";

const IGNORE_EV = new Set(["attachment", "file-history-snapshot", "last-prompt", "ai-title", "permission-mode", "system", "seed", "token_count"]);
const STUB_RE = /^\s*→\s*[\w.\-]+\s*$/;
const ERR_RE = /\b(error|failed|denied|no such|exit code [1-9]|fatal|traceback)\b/i;

declare global {
  interface Window {
    webkit?: { messageHandlers?: { open?: { postMessage: (s: string) => void } } };
  }
}

function lastLine(s: Session): { label: string; text: string; err: boolean } | null {
  for (let i = s.recentEvents.length - 1; i >= 0; i--) {
    const e = s.recentEvents[i];
    if (IGNORE_EV.has(e.kind)) continue;
    const t = (e.text || "").split("\n").find((l) => l.trim()) || "";
    if (!t || STUB_RE.test(t)) continue;
    const label = e.kind === "user" ? "you" : e.kind === "assistant" ? "asst" : e.kind.split(":")[0];
    return { label, text: t.trim(), err: ERR_RE.test(t) };
  }
  const t = (s.lastMessage || "").split("\n").find((l) => l.trim());
  return t ? { label: "last", text: t.trim(), err: ERR_RE.test(t) } : null;
}

export default function TVPage() {
  const live = useLive();
  const [sums, setSums] = useState<Record<string, TraceSummary>>({});
  const [ctx, setCtx] = useState<Record<string, ContextRow>>({});
  const [health, setHealth] = useState<{ tmuxPanes: number; registeredPanes: number } | null>(null);
  const [open, setOpen] = useState<string | null>(null);
  const [toast, setToast] = useState<string | null>(null);
  const [, tick] = useState(0);

  useEffect(() => {
    document.documentElement.dataset.theme = "dark";
    const poll = () => {
      api.summaries().then((r) => setSums(r.summaries)).catch(() => {});
      api.contextLive().then((r) => setCtx(Object.fromEntries(r.context.map((c) => [c.sessionId, c])))).catch(() => {});
      api.health().then((h) => setHealth(h)).catch(() => {});
    };
    poll();
    const a = window.setInterval(poll, 5000);
    const b = window.setInterval(() => tick((n) => n + 1), 1000);
    return () => {
      window.clearInterval(a);
      window.clearInterval(b);
    };
  }, []);

  const sessions = useMemo(
    () => Array.from(live.sessions.values()).filter((s) => isLive(s.state)).sort((a, b) => stateRank(a.state) - stateRank(b.state) || b.lastActivityAt - a.lastActivityAt),
    [live],
  );
  const perms = Array.from(live.perms.values());
  const permFor = (s: Session): PermissionRequest | undefined => perms.find((p) => p.sessionId === s.sessionId || p.sessionId === s.id);
  const attention = sessions.filter((s) => s.state === "awaiting-permission").length + perms.filter((p) => !sessions.some((s) => s.sessionId === p.sessionId)).length;
  const todayCost = sessions.reduce((a, s) => a + (sums[s.id]?.costUsd || 0), 0);
  const costEst = sessions.some((s) => sums[s.id]?.costEstimated);

  const say = (t: string) => {
    setToast(t);
    window.setTimeout(() => setToast(null), 2400);
  };
  const openDashboard = (id: string) => {
    const url = `${location.origin}/session/${encodeURIComponent(id)}`;
    if (window.webkit?.messageHandlers?.open) window.webkit.messageHandlers.open.postMessage(url);
    else window.open(url, "_blank");
  };
  const respond = async (p: PermissionRequest, behavior: "allow" | "deny") => {
    try {
      await api.respondPerm(p.id, behavior);
      dropPerm(p.id);
      say(`${behavior === "allow" ? "allowed" : "denied"} · ${p.toolName}`);
    } catch (e) {
      say(`failed: ${(e as Error).message}`);
    }
  };
  const keys = async (s: Session, k: string) => {
    try {
      await api.sendKeys(s.id, [k, "Enter"]);
      say(`sent ${k} ↵`);
    } catch (e) {
      say(`keys failed: ${(e as Error).message}`);
    }
  };

  const detail = open ? live.sessions.get(open) : undefined;

  return (
    <div className="tv-root" style={{ position: "relative" }}>
      <div className="tv-head">
        <span className="logo"><Icon name="pulse" size={12} color="#07080b" sw={2.4} /></span>
        <span style={{ fontWeight: 600 }}>agent-monitor</span>
        <span className="tr-chip sm" style={{ color: "var(--green)", cursor: "default" }}>
          <span style={{ width: 6, height: 6, borderRadius: "50%", background: "var(--green)" }} /> {sessions.length} live
        </span>
        {attention > 0 && (
          <span className="tr-chip sm" style={{ color: "var(--red)", cursor: "default" }}>
            <Icon name="alert" size={10} color="var(--red)" sw={2.4} /> {attention}
          </span>
        )}
        <div style={{ flex: 1 }} />
        <span className="num muted" style={{ fontSize: 10.5 }}>{fmtUsd(todayCost, costEst)} live</span>
      </div>
      <div className="tv-body">
        {sessions.length === 0 && (
          <div className="tv-empty">
            <Icon name="check" size={20} color="var(--muted)" />
            <div style={{ fontWeight: 600, color: "var(--text-2)" }}>All quiet</div>
            <div style={{ fontSize: 11.5 }}>No sessions running right now.</div>
          </div>
        )}
        {sessions.map((s) => {
          const sum = sums[s.id];
          const c = ctx[s.id];
          const used = c?.used ?? sum?.contextUsed ?? 0;
          const window_ = c?.window ?? sum?.contextWindow ?? 0;
          const pctv = window_ ? Math.min(100, (used / window_) * 100) : 0;
          const gcol = pctv >= 80 ? "var(--orange)" : pctv >= 50 ? "var(--accent)" : "var(--green)";
          const p = permFor(s);
          const ll = lastLine(s);
          const segs = sum?.segKinds || [];
          const weights = sum?.segWeights || [];
          return (
            <div key={s.id} className={`tv-card ${s.state}`} onClick={() => setOpen(s.id)}>
              <div className="tv-row">
                <ToolLogo tool={s.tool} size={14} />
                <span className="tv-title">{titleFor(s)}</span>
                <span className="tr-pill" style={{ color: stateColorVar(s.state), background: "var(--bg-3)" }}>{stateLabel(s.state)}</span>
              </div>
              <div className="tv-row">
                <span className="tv-meta" style={{ color: `var(--tool-${s.tool})` }}>{toolName(s.tool)}</span>
                <span className="tv-meta">· {modelShort(s.model) || "—"} · #{agentTag(s)} · {projectName(s.cwd)}</span>
                <div style={{ flex: 1 }} />
                <span className="tv-meta">{fmtAgo(s.lastActivityAt, "")}</span>
              </div>
              {segs.length > 0 && (
                <div className="tr-segstrip">
                  {segs.map((k, i) => (
                    <div key={i} className={`tr-seg ${i === segs.length - 1 ? "cur" : "prev"}`} style={{ height: 8, flex: `${Math.max(weights[i] || 1, 1)} 1 0`, minWidth: 8, cursor: "default" }} title={BOUNDARY[k]?.label}>
                      {segs.length <= 8 && <span className={`tr-bd sm ${BOUNDARY[k]?.cls}`} style={{ width: 8, height: 8, fontSize: 6 }}>{BOUNDARY[k]?.glyph}</span>}
                    </div>
                  ))}
                </div>
              )}
              {window_ > 0 && (
                <div className="tv-row">
                  <span className="k" style={{ fontSize: 9.5 }}>ctx</span>
                  <div className="tr-gauge" style={{ flex: 1 }}><div style={{ width: `${pctv}%`, background: gcol }} /></div>
                  <span className="num" style={{ fontSize: 10.5, color: gcol, width: 70, textAlign: "right" }}>
                    {pctv.toFixed(0)}% · {window_ >= 1e6 ? `${(window_ / 1e6).toFixed(0)}M` : `${Math.round(window_ / 1000)}k`}
                  </span>
                </div>
              )}
              {(sum?.lastPoint || sum?.lastOutcome) && (
                <div className="tv-line">
                  {segs.length ? `segment ${segs.length} · ` : ""}
                  {sum.lastOutcome || sum.lastPoint}
                </div>
              )}
              {ll && (
                <div className={`tv-last ${ll.err ? "err" : ""}`}>
                  {ll.label} ← {ll.text}
                </div>
              )}
              {s.state === "awaiting-permission" && (
                <div className="tv-perm" onClick={(e) => e.stopPropagation()}>
                  <pre>{p ? `${p.toolName} · ${oneLine(p.input)}` : s.permissionMessage || "needs permission"}</pre>
                  <div className="tv-keys">
                    {p ? (
                      <>
                        <button className="tv-btn ok" onClick={() => respond(p, "allow")}>allow</button>
                        <button className="tv-btn no" onClick={() => respond(p, "deny")}>deny</button>
                      </>
                    ) : (
                      <>
                        <button className="tv-btn ok" onClick={() => keys(s, "y")}>y ↵</button>
                        <button className="tv-btn no" onClick={() => keys(s, "n")}>n ↵</button>
                      </>
                    )}
                    {["1", "2", "3", "4"].map((k) => (
                      <button key={k} className="tv-btn" style={{ flex: "0 0 34px" }} onClick={() => keys(s, k)}>{k}</button>
                    ))}
                  </div>
                </div>
              )}
            </div>
          );
        })}
      </div>
      <div className="tv-foot">
        <span>{health ? `${health.registeredPanes} panes · ${health.tmuxPanes} tmux` : "…"}</span>
        <div style={{ flex: 1 }} />
        <span className={live.connected ? "" : ""} style={{ width: 6, height: 6, borderRadius: "50%", background: live.connected ? "var(--green)" : "var(--red)" }} />
      </div>
      {detail && <Detail s={detail} onClose={() => setOpen(null)} onDashboard={() => openDashboard(detail.id)} say={say} />}
      {toast && <div className="tv-toast">{toast}</div>}
    </div>
  );
}

function oneLine(input: Record<string, unknown>): string {
  const v = input.command ?? input.file_path ?? input.url ?? input.query;
  if (typeof v === "string") return v.slice(0, 160);
  return JSON.stringify(input).slice(0, 160);
}

function Detail({ s, onClose, onDashboard, say }: { s: Session; onClose: () => void; onDashboard: () => void; say: (t: string) => void }) {
  const [pane, setPane] = useState<string | null>(null);
  const ref = useRef<HTMLPreElement>(null);
  useEffect(() => {
    const poll = () =>
      api
        .paneView(s.id, 80)
        .then((v) => setPane(v.ok && v.content ? stripAnsi(v.content) : null))
        .catch(() => {});
    poll();
    const t = window.setInterval(poll, 2000);
    return () => window.clearInterval(t);
  }, [s.id]);
  useEffect(() => {
    const el = ref.current;
    if (el && el.scrollHeight - el.scrollTop - el.clientHeight < 30) el.scrollTop = el.scrollHeight;
  }, [pane]);
  const feed = s.recentEvents.filter((e) => !IGNORE_EV.has(e.kind) && e.text).slice(-24);
  const goTerminal = async () => {
    try {
      const r = await api.focus(s.id);
      if (r.ok) say(`→ ${r.terminal || ""} ${r.sessionName || ""} ${r.paneId || ""}`);
      else {
        say("No live terminal — opening dashboard");
        onDashboard();
      }
    } catch {
      say("No live terminal — opening dashboard");
      onDashboard();
    }
  };
  return (
    <div className="tv-detail">
      <div className="dhead">
        <div className="tv-row">
          <button className="tv-btn" onClick={onClose} style={{ height: 24, padding: "0 8px" }}><Icon name="chevl" size={12} /> back</button>
          <span className="tr-pill" style={{ color: stateColorVar(s.state), background: "var(--bg-3)" }}>{stateLabel(s.state)}</span>
          <div style={{ flex: 1 }} />
          <span className="tv-meta">{fmtAgo(s.lastActivityAt)}</span>
        </div>
        <div style={{ fontWeight: 600, fontSize: 14 }}>{titleFor(s)}</div>
        <div className="tv-meta">{toolName(s.tool)} · {projectName(s.cwd)} · {modelShort(s.model)} · {s.messageCount} turns · {Object.values(s.toolUsage || {}).reduce((a, b) => a + b, 0)} tools</div>
        <div className="tv-row">
          <button className="tv-btn" onClick={goTerminal}><Icon name="terminal" size={12} /> open terminal</button>
          <button className="tv-btn" onClick={onDashboard}><Icon name="frame" size={12} /> dashboard</button>
        </div>
      </div>
      <pre className="pv" ref={ref}>
        {pane ?? (feed.length ? feed.map((e) => `${e.kind === "user" ? "you" : e.kind === "assistant" ? "asst" : e.kind.split(":")[0]}  ${(e.text.split("\n").find((l) => l.trim()) || "").trim()}`).join("\n") : "No live pane. Open the dashboard for the full transcript.")}
      </pre>
    </div>
  );
}
