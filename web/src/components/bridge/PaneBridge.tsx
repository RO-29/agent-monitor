import { useCallback, useEffect, useRef, useState } from "react";
import { api } from "../../api/client";
import type { PaneRegistration, PaneStatus, Session, TmuxPane } from "../../api/types";
import { fmtAgo, shortCwd, toolName } from "../../lib/format";
import { Icon } from "../../lib/icons";
import { dropTalk, useLive } from "../../lib/ws";
import { getNoAutoRegister, setNoAutoRegister } from "../../app/prefs";
import { showToast } from "../../app/toast";
import { ansiToHtml } from "./ansi";

// Quick keys (exact legacy list): label → key array for /api/session/keys.
const QUICK: { label: string; keys: string[]; deny?: boolean }[] = [
  { label: "1 ↵", keys: ["1", "Enter"] },
  { label: "2 ↵", keys: ["2", "Enter"] },
  { label: "3 ↵", keys: ["3", "Enter"], deny: true },
  { label: "y", keys: ["y", "Enter"] },
  { label: "n", keys: ["n", "Enter"] },
  { label: "↵", keys: ["Enter"] },
  { label: "esc", keys: ["Escape"] },
  { label: "↑", keys: ["Up"] },
  { label: "↓", keys: ["Down"] },
  { label: "⇥", keys: ["Tab"] },
];

function paneAffinity(p: TmuxPane, cwd: string): number {
  if (!cwd) return 0;
  if (p.CurrentPath === cwd) return 100;
  if (cwd.startsWith(p.CurrentPath + "/")) return 50;
  if (p.CurrentPath.startsWith(cwd + "/")) return 25;
  return 0;
}

/** Pane bridge for one session: registration state → picker | stale banner | live viewer. */
export default function PaneBridge({ session }: { session: Session }) {
  const id = session.id;
  const [status, setStatus] = useState<PaneStatus | null>(null);
  const [err, setErr] = useState("");
  const [gen, setGen] = useState(0); // bump to re-check registration
  const refresh = useCallback(() => setGen((g) => g + 1), []);

  useEffect(() => {
    let alive = true;
    setStatus(null);
    api
      .paneStatus(id)
      .then((s) => alive && setStatus(s))
      .catch((e) => alive && setErr(String(e.message || e)));
    return () => {
      alive = false;
    };
  }, [id, gen]);

  if (err) return <div className="bridge muted">pane status failed: {err}</div>;
  if (!status) return <div className="bridge muted">checking pane registration…</div>;
  if (!status.registered) return <PanePicker session={session} onRegistered={refresh} />;
  if (!status.ok)
    return (
      <div className="bridge">
        <div className="card" style={{ padding: 12, borderColor: "rgba(248,113,113,.35)" }}>
          <div style={{ fontWeight: 600 }}>Registered pane is gone</div>
          <div className="mono muted" style={{ fontSize: 11.5, marginTop: 4 }}>
            {status.paneId} · {status.error || "pane not found in tmux"}
          </div>
          <div className="row" style={{ marginTop: 10, gap: 6 }}>
            <button
              className="btn no sm"
              onClick={async () => {
                setNoAutoRegister(id, true);
                await api.forgetPane(id).catch(() => null);
                refresh();
              }}
            >
              forget registration
            </button>
            <button className="btn sm" onClick={refresh}>
              <Icon name="refresh" size={12} /> re-check
            </button>
          </div>
        </div>
      </div>
    );
  return <LiveBridge session={session} status={status} onForget={refresh} />;
}

function PanePicker({ session, onRegistered }: { session: Session; onRegistered: () => void }) {
  const [panes, setPanes] = useState<TmuxPane[] | null>(null);
  const [error, setError] = useState("");
  const [auto, setAuto] = useState("");
  const blocked = getNoAutoRegister().includes(session.id);

  const register = useCallback(
    async (paneId: string, source: string) => {
      try {
        await api.registerPane({ tool: session.tool, sessionId: session.sessionId, paneId, cwd: session.cwd, source });
        setNoAutoRegister(session.id, false);
        showToast("Pane registered", `${paneId} → ${toolName(session.tool)}`);
        onRegistered();
      } catch (e) {
        showToast("Register failed", String((e as Error).message));
      }
    },
    [session, onRegistered],
  );

  useEffect(() => {
    let alive = true;
    api
      .tmuxPanes()
      .then((r) => {
        if (!alive) return;
        if (!r.ok) {
          setError("tmux unavailable: " + (r.error || "unknown"));
          return;
        }
        const list = [...(r.panes || [])].sort((a, b) => paneAffinity(b, session.cwd) - paneAffinity(a, session.cwd));
        setPanes(list);
        const exact = list.filter((p) => paneAffinity(p, session.cwd) >= 100);
        if (exact.length === 1 && !blocked) {
          setAuto(exact[0].PaneID);
          register(exact[0].PaneID, "manual");
        }
      })
      .catch((e) => alive && setError("tmux fetch failed: " + String(e.message || e)));
    return () => {
      alive = false;
    };
  }, [session.id, session.cwd, blocked, register]);

  return (
    <div className="bridge">
      <div className="card" style={{ padding: 12 }}>
        <div style={{ fontWeight: 600 }}>No tmux pane registered for this session</div>
        <div className="muted" style={{ fontSize: 12, marginTop: 4 }}>
          New sessions: launch with <span className="mono">agent-monitor run {session.tool}</span> inside tmux (auto-registers). For this running session, pick its pane below.
        </div>
        {blocked && (
          <button className="btn sm" style={{ marginTop: 8 }} onClick={() => { setNoAutoRegister(session.id, false); onRegistered(); }}>
            <Icon name="refresh" size={12} /> re-enable auto-register
          </button>
        )}
        <div style={{ marginTop: 10, display: "flex", flexDirection: "column", gap: 6 }}>
          {error && <div className="muted">{error}</div>}
          {!error && panes === null && <div className="muted">loading tmux panes…</div>}
          {auto && <div className="muted">→ auto-registering {auto} (only pane with a matching cwd)…</div>}
          {!error && panes && panes.length === 0 && <div className="muted">no tmux panes found — start a tmux session first</div>}
          {panes?.map((p) => {
            const a = paneAffinity(p, session.cwd);
            return (
              <button key={p.PaneID} className="btn" style={{ justifyContent: "flex-start", height: "auto", padding: "6px 10px" }} onClick={() => register(p.PaneID, "manual")}>
                <span className="mono" style={{ color: "var(--accent)" }}>{p.PaneID}</span>
                <span className="mono muted">{p.SessionName}</span>
                <span className="mono">{p.CurrentCommand}</span>
                <span className="mono muted ell">{shortCwd(p.CurrentPath)}</span>
                {a >= 100 && <span className="src" style={{ background: "rgba(74,222,128,.16)", color: "var(--green)" }}>cwd match</span>}
                {a >= 50 && a < 100 && <span className="src" style={{ background: "rgba(125,211,252,.16)", color: "var(--accent)" }}>in subtree</span>}
              </button>
            );
          })}
        </div>
      </div>
    </div>
  );
}

function LiveBridge({ session, status, onForget }: { session: Session; status: PaneStatus; onForget: () => void }) {
  const id = session.id;
  const pre = useRef<HTMLPreElement>(null);
  const [autoScroll, setAutoScroll] = useState(true);
  const [tick, setTick] = useState({ n: 0, ms: 0 });
  const [text, setText] = useState("");
  const [talkOpen, setTalkOpen] = useState(false);
  const [recipients, setRecipients] = useState<PaneRegistration[]>([]);
  const [talkTo, setTalkTo] = useState("");
  const [talkMsg, setTalkMsg] = useState("");
  const live = useLive();
  const inbox = [...live.talks.values()].filter((t) => t.toAgent === id).sort((a, b) => a.createdAt - b.createdAt);
  const lastContent = useRef<string | null>(null);
  const bump = useRef(0);

  // One poller: 1200 ms after each completed fetch, 400 ms when one is in flight.
  useEffect(() => {
    let alive = true;
    let inFlight = false;
    let timer: number | undefined;
    const paint = (content: string) => {
      const el = pre.current;
      if (!el) return;
      const nearBottom = el.scrollHeight - el.scrollTop - el.clientHeight < 16;
      el.innerHTML = content.trim() ? ansiToHtml(content) : '<span class="a-d">pane buffer is empty — send a prompt or wait for the CLI to render</span>';
      if (autoScroll || nearBottom) el.scrollTop = el.scrollHeight;
    };
    const loop = async () => {
      if (!alive) return;
      if (inFlight) {
        timer = window.setTimeout(loop, 400);
        return;
      }
      inFlight = true;
      const t0 = performance.now();
      try {
        const v = await api.paneView(id, 200);
        if (!alive) return;
        const content = v.ok ? v.content || "" : `\x1b[31m${v.error || "no pane"}\x1b[0m`;
        if (content !== lastContent.current) {
          lastContent.current = content;
          paint(content);
        }
        setTick((t) => ({ n: t.n + 1, ms: Math.round(performance.now() - t0) }));
      } catch {
        /* network blip: keep last frame */
      } finally {
        inFlight = false;
      }
      timer = window.setTimeout(loop, 1200);
    };
    lastContent.current = null;
    loop();
    return () => {
      alive = false;
      window.clearTimeout(timer);
    };
  }, [id, autoScroll, bump.current]); // eslint-disable-line react-hooks/exhaustive-deps

  const send = async (enter: boolean) => {
    if (!text.trim()) return;
    try {
      const r = await api.sendText(id, text, enter);
      showToast("Sent to pane", `${r.pane} (${r.session})`);
      setText("");
    } catch (e) {
      showToast("Send failed", String((e as Error).message));
    }
  };
  const keys = async (k: string[]) => {
    try {
      const r = await api.sendKeys(id, k);
      showToast("Keys sent", `${r.pane} · ${k.join(" ")}`);
    } catch (e) {
      showToast("Keys failed", String((e as Error).message));
    }
  };
  const cancel = async () => {
    try {
      const r = await api.cancel(id);
      showToast("Ctrl-C sent", r.pane);
    } catch (e) {
      showToast("Cancel failed", String((e as Error).message));
    }
  };

  return (
    <div className="bridge">
      <div className="row" style={{ gap: 8, flexWrap: "wrap" }}>
        <span className="k">live pane</span>
        <span className="mono" style={{ fontSize: 11.5 }}>
          <span style={{ color: "var(--accent)" }}>{status.paneId}</span>
          {status.alias ? <> · <b>{status.alias}</b></> : null} <span className="muted">· via {status.source}</span>
        </span>
        <span className="num muted" style={{ fontSize: 10.5 }}>
          tick {tick.n} · {tick.ms}ms
        </span>
        <div style={{ flex: 1 }} />
        <label className="row" style={{ gap: 5, fontSize: 11.5, color: "var(--text-2)" }}>
          <input type="checkbox" checked={autoScroll} onChange={(e) => setAutoScroll(e.target.checked)} /> auto-scroll
        </label>
        <button className="btn sm icon" title="refresh" onClick={() => { lastContent.current = null; bump.current++; setTick((t) => ({ ...t })); }}>
          <Icon name="refresh" size={12} />
        </button>
        <button
          className="btn sm"
          onClick={async () => {
            const alias = window.prompt("Alias for this pane (empty clears)", status.alias || "");
            if (alias === null) return;
            await api.namePane(id, alias).catch((e) => showToast("Rename failed", String(e.message)));
            onForget();
          }}
        >
          name…
        </button>
        <button
          className="btn sm"
          onClick={async () => {
            setTalkOpen((o) => !o);
            if (!recipients.length) {
              const r = await api.panes().catch(() => ({ panes: [] as PaneRegistration[] }));
              setRecipients(r.panes.filter((p) => p.agentId !== id));
            }
          }}
        >
          talk to <Icon name="arrow" size={11} />
        </button>
        <button
          className="btn sm no"
          onClick={async () => {
            if (!window.confirm(`Forget pane ${status.paneId} for this session?`)) return;
            setNoAutoRegister(id, true);
            await api.forgetPane(id).catch((e) => showToast("Forget failed", String(e.message)));
            onForget();
          }}
        >
          forget
        </button>
      </div>

      {talkOpen && (
        <div className="card" style={{ padding: 10, display: "flex", flexDirection: "column", gap: 6 }}>
          <div className="k">propose a talk to another agent</div>
          <select className="sel" value={talkTo} onChange={(e) => setTalkTo(e.target.value)}>
            <option value="">pick a recipient…</option>
            {recipients.map((p) => (
              <option key={p.agentId} value={p.agentId}>
                {p.alias || p.agentId} · {p.tool} · {p.paneId} · {shortCwd(p.cwd)}
              </option>
            ))}
          </select>
          <textarea className="ta" rows={3} value={talkMsg} onChange={(e) => setTalkMsg(e.target.value)} placeholder="Message the recipient agent will decide to accept…" />
          <div className="row" style={{ gap: 6 }}>
            <button
              className="btn sm pri"
              disabled={!talkTo || !talkMsg.trim()}
              onClick={async () => {
                try {
                  const r = await api.talkRequest(id, talkTo, talkMsg);
                  showToast("Talk proposed", `→ ${r.toAgent} (id ${r.id}) — they decide`);
                  setTalkMsg("");
                  setTalkOpen(false);
                } catch (e) {
                  showToast("Talk failed", String((e as Error).message));
                }
              }}
            >
              propose
            </button>
            <button className="btn sm" onClick={() => setTalkOpen(false)}>cancel</button>
          </div>
        </div>
      )}

      {inbox.map((t) => (
        <div key={t.id} className="card" style={{ padding: 10, borderColor: "rgba(192,132,252,.35)", display: "flex", flexDirection: "column", gap: 6 }}>
          <div className="row" style={{ gap: 8 }}>
            <Icon name="arrow" size={12} color="var(--magenta)" />
            <span style={{ fontWeight: 600 }}>incoming from {t.fromLabel}</span>
            <span className="num muted" style={{ fontSize: 10.5 }}>{fmtAgo(t.createdAt)}</span>
          </div>
          <pre className="code">{t.message}</pre>
          <div className="row" style={{ gap: 6 }}>
            <button className="btn sm ok" onClick={() => api.respondTalk(t.id, "allow").then(() => dropTalk(t.id)).catch((e) => showToast("Talk failed", String(e.message)))}>
              <Icon name="check" size={12} /> deliver to my pane
            </button>
            <button className="btn sm no" onClick={() => api.respondTalk(t.id, "deny").then(() => dropTalk(t.id)).catch((e) => showToast("Talk failed", String(e.message)))}>
              deny
            </button>
          </div>
        </div>
      ))}

      <pre ref={pre} className="paneview" />
      <textarea
        className="ta"
        rows={3}
        value={text}
        onChange={(e) => setText(e.target.value)}
        placeholder="Type a prompt to send into the tmux pane running this session…"
        onKeyDown={(e) => {
          if (e.key === "Enter" && (e.metaKey || e.ctrlKey)) {
            e.preventDefault();
            send(true);
          }
        }}
      />
      <div className="row" style={{ gap: 6, flexWrap: "wrap" }}>
        <button className="btn pri" onClick={() => send(true)}>Send + Enter</button>
        <button className="btn" onClick={() => send(false)}>Send (no Enter)</button>
        <button className="btn no" onClick={cancel}>Cancel (Ctrl-C)</button>
      </div>
      <div className="keys">
        <span className="k">quick keys</span>
        {QUICK.map((q) => (
          <button key={q.label} className={`btn sm ${q.deny ? "no" : ""}`} onClick={() => keys(q.keys)}>
            {q.label}
          </button>
        ))}
      </div>
    </div>
  );
}
