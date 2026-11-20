// /float: the compact always-on-top panel that AgentFloat.app hosts (approvals, live threads, a prompt box).
import { useEffect, useMemo, useRef, useState } from "react";
import { SpeechCtor, type SpeechRec } from "../../lib/voice";
import { api } from "../../api/client";
import { setTheme, useTheme } from "../../app/prefs";
import { dismissToast, showToast, useToasts } from "../../app/toast";
import { fmtAgo, projectName } from "../../lib/format";
import { useLive } from "../../lib/ws";
import { ApiError } from "../api/client";
import { agentLabel, createThread, mockWanted, sendMessage, spendBlockOf, useAgents, useOrch, useOrchThread, type OWhere } from "../api/orch";
import { AgentIcon } from "../components/AgentIcon";
import { LegacyPermCard } from "../components/ApprovalCard";
import { Markdown } from "../components/Markdown";
import { agentName, startV3Live, useV3 } from "../live";
import { activity, buildRows, outcomeOf, startOfToday } from "../model";
import { useTick } from "./workspace/DrivenChat";
import { serverDrivenRow, terminalTarget } from "./workspace/handover";
import { buildWRows, type WRow } from "./workspace/rows";
import { latestSession, usePane, watchedLiveness } from "./workspace/WatchedChat";
import { askFromApproval, AskKeysCtx, WsApproval } from "./workspace/WsApproval";
import { TermQuestionCard, termQuestionShown, useTermQuestion } from "./workspace/TermQuestion";
import { kindOf, useRecentFolders } from "./workspace/NewThread";
import { TERM_AGENTS, TermStarting, useTermStart, WherePick } from "./workspace/TermStart";
import "../styles/v3.css";
import "../styles/workspace.css";
import "../styles/p6.css";
import "../styles/float.css";
import { useNeedsYou, drivenNeedIds } from "../api/needsYou";

const browserUrl = (id: string) => `${window.location.origin}/w/${encodeURIComponent(id)}`;

type Handler = { postMessage: (v: unknown) => void };
const handler = (name: string) => (window as unknown as { webkit?: { messageHandlers?: Record<string, Handler | undefined> } }).webkit?.messageHandlers?.[name];
/** Posts to AgentFloat.app; a no-op in a plain browser. */
function native(name: string, v: unknown) {
  handler(name)?.postMessage(v);
}

// The native shell opens links in the default browser; a plain browser opens a new tab.
function openOut(url: string) {
  const h = handler("open");
  if (h) h.postMessage(url);
  else window.open(url, "_blank", "noopener");
}

export default function Float() {
  useEffect(startV3Live, []);
  const [theme, toggleTheme] = useTheme();
  // With no stored choice the panel follows the system appearance.
  useEffect(() => {
    try {
      if (localStorage.getItem("agent-monitor-theme") === null && window.matchMedia?.("(prefers-color-scheme: light)").matches) setTheme("light");
    } catch {
      /* storage off */
    }
  }, []);
  const live = useLive();
  const v = useV3();
  const orch = useOrch();
  const toasts = useToasts();
  const minute = useTick(true, 60_000);
  const ny = useNeedsYou();
  const watchedRows = useMemo(() => buildRows(v, live, ny), [v.version, live.version, ny.version]); // eslint-disable-line react-hooks/exhaustive-deps
  const needIds = useMemo(() => drivenNeedIds(ny.items, v.approvals.values()), [ny.version, v.approvals]); // eslint-disable-line react-hooks/exhaustive-deps
  const rows = useMemo(() => buildWRows(orch, watchedRows, needIds, Date.now(), Infinity, live.sessions), [orch, watchedRows, needIds, minute]); // eslint-disable-line react-hooks/exhaustive-deps

  const pending = useMemo(() => [...v.approvals.values()].filter((a) => a.state === "pending").sort((a, b) => a.createdAt - b.createdAt), [v.approvals]);
  const perms = [...live.perms.values()];
  const today = startOfToday();
  const top = rows.filter((r) => !r.parentId);
  const lists: { key: string; label: string; rows: WRow[] }[] = [
    { key: "need", label: "Needs you", rows: top.filter((r) => r.group === "need") },
    { key: "working", label: "Working", rows: top.filter((r) => r.group === "working") },
    { key: "ready", label: "Ready", rows: top.filter((r) => r.group === "ready" && r.at >= today) },
  ];
  const [sel, setSel] = useState<string | null>(null);
  const [newOpen, setNewOpen] = useState(false);
  // A long queue keeps the threads in reach: the oldest few show, the rest wait behind a button.
  const [allAsks, setAllAsks] = useState(false);
  const ASK_MAX = 3;
  const shownAsks = allAsks ? pending : pending.slice(0, ASK_MAX);
  // The same count as every other surface: the needs-you list, once it has loaded.
  // Items with no card here (terminal questions and prompts, failed threads) still show, as rows.
  const others = ny.items.filter((i) => i.kind === "question" || i.kind === "prompt" || i.kind === "error");
  const needCount = ny.status === "loading" ? pending.length + perms.length : ny.count;
  const working = lists[1].rows.length;

  // The native shell shows the count on its menu bar item and paints its frame in the page theme.
  useEffect(() => { native("count", needCount); }, [needCount]);
  useEffect(() => { native("theme", theme); }, [theme]);

  const titleOf = (sessionId: string, drivenId?: string) => {
    const id = (drivenId && v.drivenRowOf[drivenId]) || v.threadOf[sessionId];
    const r = rows.find((x) => x.id === drivenId || x.id === id);
    return r ? { id: r.id, title: r.title, repo: r.repo } : undefined;
  };

  return (
    <div className="v3 flt">
      <header className="flt-h">
        <span className={`dot ${live.connected || orch.mock ? "d-ok" : "d-err"}`} title={live.connected ? "Live" : "Reconnecting"} />
        <b className={needCount ? "need-t" : ""}>{needCount} need you</b>
        <span className="muted xs">· {working} working</span>
        {orch.mock && <span className="pill p-mute xs" title="Sample data from ?mock=1">sample</span>}
        <span className="sp" />
        <button className={`btn sm ${newOpen ? "" : "ghost"}`} onClick={() => setNewOpen(!newOpen)} aria-expanded={newOpen} title="Start a new thread">
          ＋ New
        </button>
        <button className="btn sm ghost" onClick={toggleTheme} title={theme === "dark" ? "Light theme" : "Dark theme"} aria-label="Switch theme">
          {theme === "dark" ? "☀" : "☾"}
        </button>
        <button className="btn sm ghost" onClick={() => openOut(`${window.location.origin}/w`)} title="Open agent-monitor in the browser">
          Open app
        </button>
      </header>
      <main className="flt-body ws">
        {newOpen && (
          <FloatNew
            onDone={(id) => {
              setNewOpen(false);
              setSel(id);
            }}
          />
        )}
        {v.apprStatus === "error" && !orch.mock && <div className="flt-note err-t xs">Approvals failed to load: {v.apprError}</div>}
        {(pending.length > 0 || perms.length > 0 || others.length > 0) && (
          <section className="flt-sec" aria-label="Needs you">
            <div className="flt-eb need-t">Needs you</div>
            <AskKeysCtx.Provider value={false}>
              {shownAsks.map((a) => {
                const t = titleOf(a.sessionId, a.drivenId);
                return (
                  <div key={a.id} className="flt-ask">
                    <div className="flt-ctx xs">
                      <span className="trunc">{t ? `${t.repo} · ${t.title}` : projectName(a.cwd)}</span>
                      {t && (
                        <a className="flt-out" href={browserUrl(t.id)} onClick={(e) => (e.preventDefault(), openOut(browserUrl(t.id)))}>
                          Open in browser
                        </a>
                      )}
                    </div>
                    <WsApproval ask={askFromApproval(a)} />
                  </div>
                );
              })}
            </AskKeysCtx.Provider>
            {pending.length > ASK_MAX && (
              <button className="btn sm ghost flt-more" onClick={() => setAllAsks(!allAsks)} aria-expanded={allAsks}>
                {allAsks ? "Show fewer requests" : `Show ${pending.length - ASK_MAX} more ${pending.length - ASK_MAX === 1 ? "request" : "requests"}`}
              </button>
            )}
            {perms.map((p) => (
              <div key={p.id} className="flt-ask">
                <LegacyPermCard p={p} compact />
              </div>
            ))}
            {others.map((i) => {
              const tid = i.threadId || (i.sessionId ? v.threadOf[i.sessionId] || i.sessionId : "");
              return (
                <a key={i.key} className="flt-ask flt-other" href={browserUrl(tid)} onClick={(e) => (e.preventDefault(), openOut(browserUrl(tid)))}>
                  <span className={`dot ${i.kind === "error" ? "d-err" : "d-need"}`} />
                  <span className="trunc">{i.title || projectName(i.cwd || "")}</span>
                  <span className="muted xs">{i.kind === "error" ? "stopped with an error" : i.kind === "question" ? "asks a question" : "asks in the terminal"}</span>
                </a>
              );
            })}
          </section>
        )}
        {lists.map((l) =>
          l.rows.length ? (
            <section key={l.key} className="flt-sec" aria-label={l.label}>
              <div className="flt-eb">
                {l.label} <span className="muted">{l.rows.length}</span>
              </div>
              {l.rows.map((r) => (
                <ThreadRow key={r.id} row={r} open={sel === r.id} onToggle={() => setSel(sel === r.id ? null : r.id)} />
              ))}
            </section>
          ) : null,
        )}
        {!needCount && lists.every((l) => !l.rows.length) && (
          <div className="flt-empty">
            <b>All quiet</b>
            <span className="muted sm">{v.threadsStatus === "loading" ? "Loading threads…" : "No agent works or waits right now."}</span>
          </div>
        )}
      </main>
      {toasts.length > 0 && (
        <div className="flt-toasts">
          {toasts.map((t) => (
            <div key={t.id} className={`toast ${t.kind === "alert" ? "alert" : ""}`} onClick={() => dismissToast(t.id)}>
              <b>{t.title}</b>
              {t.body && <div className="fg2 xs">{t.body}</div>}
            </div>
          ))}
        </div>
      )}
    </div>
  );
}

/** Compact new thread: prompt, folder, agent and where. The full form lives at /w/new. */
function FloatNew({ onDone }: { onDone: (rowId: string) => void }) {
  const reg = useAgents();
  const recent = useRecentFolders();
  const [prompt, setPrompt] = useState("");
  const [cwd, setCwd] = useState("");
  const [agent, setAgent] = useState("claude");
  const [whereSel, setWhere] = useState<OWhere>("terminal");
  const [busy, setBusy] = useState(false);
  const term = useTermStart((_, href) => onDone(decodeURIComponent(href.slice(3))));
  const kind = kindOf(agent, reg.list);
  const termOff = TERM_AGENTS.has(kind) ? "" : `${agentLabel(agent)} runs headless only.`;
  const where: OWhere = termOff ? "headless" : whereSel;
  const dir = cwd.trim() || recent[0] || "";
  const can = !!dir && !!prompt.trim() && !busy;
  const start = async () => {
    if (!can) return;
    setBusy(true);
    try {
      if (where === "terminal") await term.start({ agent: kind as "claude" | "codex", cwd: dir, prompt: prompt.trim() });
      else onDone((await createThread({ agent, cwd: dir, prompt: prompt.trim() })).id);
    } catch (e) {
      showToast("Could not start the thread", (e as Error).message, { kind: "alert" });
    }
    setBusy(false);
  };
  if (term.st && term.st.phase !== "error")
    return (
      <section className="flt-sec" aria-label="Starting">
        <TermStarting st={term.st} compact onClose={term.reset} />
      </section>
    );
  return (
    <section className="flt-sec flt-new" aria-label="New thread">
      <div className="flt-eb">New thread</div>
      <textarea
        className="inp"
        rows={2}
        value={prompt}
        autoFocus
        placeholder="Describe the task. ⌘↵ starts"
        onChange={(e) => setPrompt(e.target.value)}
        onKeyDown={(e) => {
          e.stopPropagation();
          if (e.key === "Enter" && (e.metaKey || e.ctrlKey)) {
            e.preventDefault();
            void start();
          }
        }}
        aria-label="Prompt"
      />
      <div className="flt-new-r">
        <input className="inp" list="flt-cwds" value={cwd} placeholder={recent[0] ? projectName(recent[0]) : "/path/to/repo"} title={dir} onChange={(e) => setCwd(e.target.value)} aria-label="Folder" />
        <datalist id="flt-cwds">
          {recent.map((c) => (
            <option key={c} value={c} />
          ))}
        </datalist>
        <select className="inp" value={agent} onChange={(e) => setAgent(e.target.value)} aria-label="Agent">
          {reg.list.map((a) => (
            <option key={a.id} value={a.id} disabled={!a.installed}>
              {a.name}
            </option>
          ))}
        </select>
      </div>
      <WherePick where={where} onChange={setWhere} termOff={termOff} compact />
      {term.st?.phase === "error" && <TermStarting st={term.st} compact onClose={term.reset} />}
      <div className="flt-pb-bar">
        <span className="muted xs trunc">{dir ? `in ${projectName(dir)}` : "Pick a folder"}</span>
        <span className="sp" />
        <button className="btn sm pri" disabled={!can} onClick={() => void start()}>
          {busy ? "Starting…" : where === "terminal" ? "Start in a terminal" : "Start"}
        </button>
      </div>
    </section>
  );
}

const STATUS_CLS: Record<string, string> = { "Needs you": "need", Working: "run", Starting: "run", "Subagents running": "run", Error: "err" };

function ThreadRow({ row, open, onToggle }: { row: WRow; open: boolean; onToggle: () => void }) {
  const at = row.at ? fmtAgo(row.at, "") : "";
  return (
    <div className={`flt-th ${open ? "open" : ""}`}>
      <button className="flt-row" onClick={onToggle} aria-expanded={open}>
        <AgentIcon agent={row.agent} size={14} />
        <span className="flt-tt">
          <span className="trunc flt-title">{row.title}</span>
          <span className="trunc muted xs">
            {row.repo}
            {row.note ? ` · ${row.note}` : ""}
          </span>
        </span>
        <span className={`flt-st ${STATUS_CLS[row.status] || ""}`}>{row.status}</span>
        {at && <span className="muted xs flt-at">{at}</span>}
      </button>
      {open && (row.mode === "driven" ? <DrivenOpen row={row} /> : <WatchedOpen row={row} />)}
    </div>
  );
}

function OpenLink({ id }: { id: string }) {
  return (
    <a className="flt-out" href={browserUrl(id)} onClick={(e) => (e.preventDefault(), openOut(browserUrl(id)))}>
      Open in browser
    </a>
  );
}

function LastMsg({ text, empty }: { text: string; empty: string }) {
  return <div className="flt-last">{text ? <Markdown text={text} collapseAt={10} /> : <span className="muted xs">{empty}</span>}</div>;
}

function DrivenOpen({ row }: { row: WRow }) {
  const t = row.driven!;
  const v = useV3();
  const { detail } = useOrchThread(t.id);
  const last = useMemo(() => [...(detail?.items || [])].reverse().find((i) => i.kind === "agent" && i.text)?.text || "", [detail?.items]);
  const [handover, setHandover] = useState("");
  const termTo = t.nativeId ? terminalTarget(`${t.agent}:${t.nativeId}`, v.threads) : "";
  const moved = serverDrivenRow(v.threads, t.id)?.handover === "terminal";
  const closed = t.status === "closed";
  const gone = !!t.worktree?.removed;
  const send = async (body: string) => {
    setHandover("");
    try {
      await sendMessage(t.id, body);
      return true;
    } catch (e) {
      const b = spendBlockOf(e);
      if (b) showToast("Spend limit reached", `${b.error}. Open the thread in the browser to allow one more turn.`, { kind: "alert" });
      else if (e instanceof ApiError && e.status === 409 && (e.body as { error?: string } | undefined)?.error === "live") setHandover("The terminal is using this session. Messages here wait until it ends.");
      else if (e instanceof ApiError && e.status === 409) showToast("Cannot steer now", `${agentName(t.agent)} cannot take a message while this turn runs. Wait, or stop it in the browser.`);
      else showToast("Send failed", (e as Error).message, { kind: "alert" });
      return false;
    }
  };
  const off = closed ? "This thread is closed." : gone ? "The worktree of this thread was removed." : "";
  return (
    <div className="flt-open">
      <LastMsg text={last} empty={detail ? "No agent message yet." : "Loading…"} />
      {(handover || moved) && (
        <div className="flt-moved xs" role="status">
          <span>{handover || "Continued in a terminal. A message here takes the session back."}</span>
          {termTo && <OpenLink id={termTo} />}
        </div>
      )}
      <PromptBox key={t.id} off={off} placeholder={t.status === "running" ? `Steer ${agentName(t.agent)}…` : `Message ${agentName(t.agent)}…`} onSend={send} />
      <div className="flt-foot">
        <OpenLink id={row.id} />
      </div>
    </div>
  );
}

function WatchedOpen({ row }: { row: WRow }) {
  const w = row.watched!;
  const lv = useLive();
  const sid = latestSession(w);
  // A live registered pane owns the session even after an hour idle marks it completed.
  const pane = usePane(sid, !!sid);
  const owned = watchedLiveness(w, lv.connected).owned || pane.ok;
  const ss = w.session?.state;
  const tq = useTermQuestion(sid, owned && w.agent === "claude" && !w.approvals.length && (ss === "awaiting-permission" || ss === "awaiting-input"));
  const last = w.session?.lastMessage || (w.thread ? outcomeOf(w.thread) : "") || activity(w);
  const off = !owned ? "This session ended. Open it in the browser to continue it." : !pane.ok || !pane.target ? "Runs in a terminal agent-monitor cannot reach. Type in that terminal." : "";
  const send = async (body: string) => {
    if (mockWanted()) {
      showToast("Sample data", `Not sent. A real send types into tmux pane ${pane.paneId || ""}.`);
      return true;
    }
    try {
      await api.sendText(pane.target!, body, true);
      return true;
    } catch (e) {
      showToast("Send failed", (e as Error).message, { kind: "alert" });
      return false;
    }
  };
  return (
    <div className="flt-open">
      {termQuestionShown(tq.q) ? <TermQuestionCard sessionId={sid} st={tq} /> : <LastMsg text={last} empty="No message yet." />}
      <PromptBox key={row.id} off={off} placeholder={pane.paneId ? `Type into pane ${pane.paneId}…` : "Message the agent in its pane…"} onSend={send} />
      <div className="flt-foot">
        <OpenLink id={row.id} />
      </div>
    </div>
  );
}

function PromptBox({ off, placeholder, onSend }: { off: string; placeholder: string; onSend: (body: string) => Promise<boolean> }) {
  const [text, setText] = useState("");
  const [busy, setBusy] = useState(false);
  const [listening, setListening] = useState(false);
  const rec = useRef<SpeechRec | null>(null);
  const base = useRef("");
  const ta = useRef<HTMLTextAreaElement>(null);
  const canSpeak = !!SpeechCtor();
  useEffect(() => () => rec.current?.stop(), []);
  useEffect(() => {
    if (!off) ta.current?.focus();
  }, [off]);

  const send = async () => {
    const body = text.trim();
    if (!body || busy || off) return;
    rec.current?.stop();
    setBusy(true);
    try {
      if (await onSend(body)) setText("");
    } finally {
      setBusy(false);
    }
  };
  const mic = () => {
    if (listening) return rec.current?.stop();
    const C = SpeechCtor();
    if (!C) return;
    const r = new C();
    r.continuous = true;
    r.interimResults = true;
    r.lang = navigator.language || "en-US";
    base.current = text ? text.replace(/\s*$/, " ") : "";
    r.onresult = (e) => {
      let fin = "";
      let tmp = "";
      for (let i = 0; i < e.results.length; i++) {
        const x = e.results[i];
        if (x.isFinal) fin += x[0].transcript;
        else tmp += x[0].transcript;
      }
      setText(base.current + fin + tmp);
    };
    r.onend = () => setListening(false);
    r.onerror = (e) => {
      setListening(false);
      if (e.error === "not-allowed" || e.error === "service-not-allowed") showToast("Microphone blocked", "Allow the microphone and speech recognition for AgentFloat, or press fn twice to dictate.");
    };
    rec.current = r;
    try {
      r.start();
      setListening(true);
    } catch (err) {
      showToast("Dictation failed", (err as Error).message);
    }
  };

  if (off) return <div className="flt-off xs">{off}</div>;
  return (
    <div className="flt-pb">
      <textarea
        ref={ta}
        className="inp"
        rows={2}
        value={text}
        placeholder={placeholder}
        disabled={busy}
        onChange={(e) => setText(e.target.value)}
        onKeyDown={(e) => {
          e.stopPropagation();
          if (e.key === "Enter" && (e.metaKey || e.ctrlKey)) {
            e.preventDefault();
            void send();
          }
        }}
        aria-label="Prompt"
      />
      <div className="flt-pb-bar">
        <span className="muted xs">Dictate: press fn twice</span>
        <span className="sp" />
        {canSpeak && (
          <button className={`btn sm ghost flt-mic ${listening ? "on" : ""}`} onClick={mic} aria-pressed={listening} title={listening ? "Stop listening" : "Speak your prompt"} aria-label={listening ? "Stop listening" : "Speak your prompt"}>
            <svg viewBox="0 0 16 16" width="14" height="14" fill="none" stroke="currentColor" strokeWidth="1.5" strokeLinecap="round" aria-hidden>
              <rect x="5.5" y="1.8" width="5" height="8" rx="2.5" />
              <path d="M3.2 7.6a4.8 4.8 0 0 0 9.6 0M8 12.4v2" />
            </svg>
          </button>
        )}
        <button className="btn sm pri" onClick={() => void send()} disabled={busy || !text.trim()} title="Send (⌘↵)">
          {busy ? "Sending…" : "Send"}
        </button>
      </div>
    </div>
  );
}
