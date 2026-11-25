import { useEffect, useMemo, useRef, useState } from "react";
import { usd } from "../components/Spend";
import { Link, useNavigate, useParams } from "react-router-dom";
import { api } from "../../api/client";
import type { Session } from "../../api/types";
import { showToast } from "../../app/toast";
import { fmtClock, fmtDuration, fmtUsd, modelShort } from "../../lib/format";
import { Markdown } from "../components/Markdown";
import { useLive } from "../../lib/ws";
import { useTrace } from "../../pages/trace/lib";
import { ApiError, v3 } from "../api/client";
import type { Approval, DecideBody } from "../api/types";
import { AgentBadge, DrivenTag, Empty, Loading } from "../components/ui";
import { drivenIdOf } from "../driven";
import { allowOnce, sendMessage, spendBlockOf, toMs, useOrch, useOrchThread, type OItem } from "../api/orch";
import { useShell } from "../ctx";
import { agentName, applyDecision, loadApprovals, useV3 } from "../live";
import { buildRows, startOfToday, type Row } from "../model";
import { buildStory, cleanSpans, type StoryGroup } from "./thread/storyModel";
import { ago, displayOf, outcomeOf, recoveredSet, repoOf } from "./control/data";
import { TermQuestionCard, termQuestionShown, useTermQuestion } from "./workspace/TermQuestion";
import { sentToTerminal, TermShownNote } from "../components/TermShown";
import "../styles/messages.css";
import { useNeedsYou } from "../api/needsYou";

type Seg = "active" | "today" | "all";
const PAGE = 30;

function usePhone() {
  const q = "(max-width: 699px)";
  const [m, setM] = useState(() => typeof window !== "undefined" && window.matchMedia(q).matches);
  useEffect(() => {
    const mq = window.matchMedia(q);
    const on = () => setM(mq.matches);
    mq.addEventListener("change", on);
    return () => mq.removeEventListener("change", on);
  }, []);
  return m;
}

const dotOf = (r: Row) => {
  if (r.needs) return "need";
  const s = r.session?.state || r.thread?.state;
  if (s === "running") return "run";
  if ((r.thread?.errorsOpen ?? 0) > 0) return "err";
  if (s === "abandoned") return "mut";
  return "ok";
};

function lastLine(r: Row): string {
  const a = r.approvals[0];
  if (a) return a.kind === "question" ? a.questions?.[0]?.question || a.summary : a.kind === "plan" ? `Plan ready: ${a.summary}` : `Wants to run ${a.summary || a.toolName}`;
  if (r.perms[0]) return `Wants ${r.perms[0].toolName}`;
  const lm = (r.session?.lastMessage || "").trim();
  if (r.session?.state === "running" && lm && !lm.startsWith("<")) return lm.replace(/\s+/g, " ").slice(0, 120);
  return r.thread ? outcomeOf(r.thread) : "";
}

export default function Messages() {
  const { threadId: raw } = useParams();
  const threadId = raw ? decodeURIComponent(raw) : "";
  const st = useV3();
  const live = useLive();
  const nav = useNavigate();
  const phone = usePhone();
  const [seg, setSeg] = useState<Seg>("active");
  const ny = useNeedsYou();
  const rows = useMemo(() => buildRows(st, live, ny), [st, live, ny]);

  const list = useMemo(() => {
    const t0 = startOfToday();
    const soon = Date.now() - 2 * 3600_000;
    let out = rows;
    if (seg === "active") out = rows.filter((r) => r.needs || r.bucket === "running" || (r.session && r.lastAt > soon));
    else if (seg === "today") out = rows.filter((r) => r.lastAt >= t0);
    out = [...out].sort((a, b) => (a.needs !== b.needs ? (a.needs ? -1 : 1) : a.needs ? a.sinceAt - b.sinceAt : b.lastAt - a.lastAt));
    return out.slice(0, 200);
  }, [rows, seg]);

  const selected = threadId ? rows.find((r) => r.id === threadId) : phone ? undefined : list[0];
  const showList = !phone || !threadId;
  const showChat = !phone || !!threadId;

  return (
    <div className={`msgv ${phone ? "phone" : ""}`}>
      {showList && (
        <div className="mls">
          <div className="top">
            <h3>Agents</h3>
            <div className="mseg" role="tablist">
              {(["active", "today", "all"] as Seg[]).map((s) => (
                <button key={s} role="tab" aria-selected={seg === s} className={seg === s ? "on" : ""} onClick={() => setSeg(s)}>
                  {s === "active" ? "Active" : s === "today" ? "Today" : "All"}
                </button>
              ))}
            </div>
          </div>
          <div className="mscroll">
            {st.threadsStatus === "loading" && <Loading rows={6} />}
            {st.threadsStatus !== "loading" && list.length === 0 && <div className="mnone">No conversations here. Try Today or All.</div>}
            {list.map((r) => {
              const pend = r.approvals.length + r.perms.length;
              return (
                <Link key={r.id} to={`/messages/${encodeURIComponent(r.id)}`} className={`cv ${selected?.id === r.id ? "on" : ""}`}>
                  <div className="avs">
                    <AgentBadge tool={r.agent} />
                    <span className={`st ${dotOf(r)}`} />
                  </div>
                  <div style={{ minWidth: 0 }}>
                    <div className="nm ell">
                      {drivenIdOf(r.thread) && <DrivenTag />}
                      {r.thread ? displayOf(r.thread, r.session?.firstMessage) : r.title}
                    </div>
                    <div className="ln ell">{lastLine(r)}</div>
                  </div>
                  {pend > 0 || r.needs ? <span className="badge">{pend || 1}</span> : <span className="when">{ago(r.lastAt)}</span>}
                </Link>
              );
            })}
          </div>
        </div>
      )}
      {showChat &&
        (selected ? (
          drivenIdOf(selected.thread) ? (
            <DrivenMsgChat key={selected.id} r={selected} id={drivenIdOf(selected.thread)!} phone={phone} onBack={() => nav("/messages")} />
          ) : (
            <Chat key={selected.id} r={selected} phone={phone} onBack={() => nav("/messages")} />
          )
        ) : threadId ? (
          <div className="mchat">
            {phone && (
              <div className="mhd">
                <button className="mback" onClick={() => nav("/messages")} aria-label="Back">
                  ‹
                </button>
              </div>
            )}
            {st.threadsStatus === "loading" ? <Loading rows={6} /> : <Empty title="Conversation not found">No thread with id <code>{threadId}</code> in the current list.</Empty>}
          </div>
        ) : (
          <div className="mchat">
            <Empty title="Pick a conversation" />
          </div>
        ))}
    </div>
  );
}

function Chat({ r, phone, onBack }: { r: Row; phone: boolean; onBack: () => void }) {
  const t = r.thread;
  const shell = useShell();
  const live = useLive();
  const sid = r.session?.id || (t ? t.sessions[t.sessions.length - 1] : r.approvals[0]?.sessionId) || "";
  const session: Session | undefined = live.sessions.get(sid) || r.session;
  const tr = useTrace(sid);
  const spans = useMemo(() => cleanSpans(tr.trace), [tr.trace]);
  const story = useMemo(() => buildStory(tr.trace, spans), [tr.trace, spans]);
  const [shown, setShown] = useState(PAGE);
  const ref = useRef<HTMLDivElement>(null);
  const stuck = useRef(true);
  const groups = story.groups;
  const first = Math.max(0, groups.length - shown);
  const vis = groups.slice(first);
  const cost = t ? fmtUsd(t.costUsd, t.costEstimated) : "";
  const model = session?.model || t?.model;
  const name = t ? displayOf(t, session?.firstMessage) : r.title;

  useEffect(() => {
    const el = ref.current;
    if (el && stuck.current) el.scrollTop = el.scrollHeight;
  }, [groups.length, r.approvals.length, tr.trace]);

  const awaiting = session?.state === "awaiting-permission" || session?.state === "awaiting-input";
  const tq = useTermQuestion(sid, r.agent === "claude" && r.approvals.length === 0 && awaiting);
  const tqShown = termQuestionShown(tq.q);

  const turnRoute = () => {
    const last = groups[groups.length - 1];
    return last && last.turnNo >= 0 ? `/traces/${encodeURIComponent(sid)}/${last.turnNo - 1}` : `/t/${encodeURIComponent(r.id)}?mode=transcript`;
  };

  return (
    <div className="mchat">
      <div className="mhd">
        {phone && (
          <button className="mback" onClick={onBack} aria-label="Back to conversations">
            ‹
          </button>
        )}
        <AgentBadge tool={r.agent} />
        <div className="mgrow">
          <b className="ell" style={{ display: "block" }}>
            {name}
          </b>
          {!phone && (
            <div className="msub">
              {[t ? repoOf(t) : repoOf({ cwd: r.cwd }), model ? modelShort(model) : "", cost].filter(Boolean).join(" · ")}
            </div>
          )}
        </div>
        <Link className="btn ghost" to={sid ? turnRoute() : `/t/${encodeURIComponent(r.id)}`}>
          Trace
        </Link>
        {session && (
          <button className="btn ghost" onClick={() => shell.openPane(session)}>
            Pane
          </button>
        )}
      </div>
      <div
        className="mbody"
        ref={ref}
        onScroll={(e) => {
          const el = e.currentTarget;
          stuck.current = el.scrollHeight - el.scrollTop - el.clientHeight < 80;
        }}
      >
        {!sid && <div className="mday">No session for this thread yet.</div>}
        {sid && tr.loading && !tr.trace && <div className="mday">Loading the conversation…</div>}
        {tr.error && !tr.trace && <div className="mday err">Could not load the transcript: {tr.error}</div>}
        {first > 0 && (
          <button className="mmore" onClick={() => setShown(shown + PAGE)}>
            Show {Math.min(PAGE, first)} earlier turns
          </button>
        )}
        {vis.map((g, j) => (
          <Turn key={g.g.id} g={g} prev={j > 0 ? vis[j - 1] : first > 0 ? groups[first - 1] : undefined} agent={agentName(tr.trace?.tool || r.agent)} />
        ))}
        {r.approvals.map((a) => (a.kind === "question" ? <QuestionBubble key={a.id} a={a} /> : <ApprovalBubble key={a.id} a={a} />))}
        {tqShown && (
          <div className="ws">
            <TermQuestionCard sessionId={sid} st={tq} agent={agentName(r.agent)} onOpenPane={session ? () => shell.openPane(session) : undefined} />
          </div>
        )}
        {!tqShown && r.approvals.length === 0 && (r.perms.length > 0 || session?.state === "awaiting-permission") && (
          <div className="mreq">
            <b>{r.perms[0] ? `${r.perms[0].toolName} needs your answer in the terminal` : `${agentName(r.agent)} asks in the terminal`}</b>
            {session && (
              <button className="btn" onClick={() => shell.openPane(session)}>
                Open pane
              </button>
            )}
          </div>
        )}
        {session?.state === "running" && <div className="mtool live">⌁ working{session.lastMessage && !session.lastMessage.trim().startsWith("<") ? ` · ${session.lastMessage.replace(/\s+/g, " ").slice(0, 80)}` : ""}</div>}
      </div>
      <Composer sessionId={sid} session={session} />
    </div>
  );
}

function Turn({ g, prev, agent }: { g: StoryGroup; prev?: StoryGroup; agent: string }) {
  const at = g.g.prompt?.ts || g.g.fromTs;
  const pAt = prev ? prev.g.toTs : 0;
  const newDay = !prev || new Date(at).toDateString() !== new Date(pAt).toDateString();
  const gap = !prev || at - pAt > 30 * 60_000;
  const calls = g.g.calls;
  const rec = recoveredSet(calls);
  const nErr = calls.filter((c) => c.err).length;
  const open = calls.filter((c) => c.err && !rec.has(c.id)).length;
  const prompt = (g.g.prompt?.text || "").trim();
  const reply = (g.reply?.text || "").trim();
  return (
    <>
      {(newDay || gap) && <span className="mday">{newDay ? `${new Date(at).toDateString() === new Date().toDateString() ? "Today" : new Date(at).toLocaleDateString(undefined, { weekday: "short", day: "numeric", month: "short" })} ${fmtClock(at)}` : fmtClock(at)}</span>}
      {prompt && (
        <div className="mb mb-me">
          <span className="txt">{prompt.length > 3000 ? prompt.slice(0, 3000) + "…" : prompt}</span>
        </div>
      )}
      {calls.length > 0 && (
        <div className={`mtool ${open ? "bad" : ""}`}>
          ⌁ {calls.length} tool call{calls.length === 1 ? "" : "s"} · {fmtDuration(Math.max(g.g.toTs - g.g.fromTs, 0))}
          {nErr > 0 && (open ? ` · ${nErr} error${nErr === 1 ? "" : "s"}, ${open} not recovered` : ` · ${nErr} error${nErr === 1 ? "" : "s"}, recovered`)}
        </div>
      )}
      {reply && (
        <div className="mb mb-ag" title={`${agent} · ${fmtClock((g.reply?.ts || 0) + (g.reply?.dur || 0))}`}>
          <Markdown text={reply.slice(0, 6000)} />
        </div>
      )}
    </>
  );
}

async function sendDecision(a: Approval, b: DecideBody, setBusy: (v: boolean) => void) {
  setBusy(true);
  try {
    const r = await v3.decide(a.id, b);
    applyDecision(r && r.id ? r : { ...a, state: b.behavior === "allow" ? "allowed" : "denied" });
  } catch (e) {
    if (e instanceof ApiError && e.status === 409) showToast("Already answered", "Another surface or the terminal answered first.");
    else showToast("Decision failed", (e as Error).message, { kind: "alert" });
    void loadApprovals();
  } finally {
    setBusy(false);
  }
}

function ApprovalBubble({ a }: { a: Approval }) {
  const [busy0, setBusy] = useState(false);
  const busy = busy0 || sentToTerminal(a);
  const cmd = typeof a.input?.command === "string" ? (a.input.command as string) : a.summary;
  const title = a.kind === "plan" ? "Approve this plan?" : a.toolName === "Bash" || a.toolName === "exec_command" ? "Run a command?" : `Use ${a.toolName}?`;
  return (
    <div className="mreq">
      <div className="row">
        <b className="mgrow">{title}</b>
        {a.kind === "permission" && <span className={`pill ${a.risk === "high" ? "p-err" : a.risk === "low" ? "p-ok" : "p-need"}`}>{a.risk}</span>}
      </div>
      <code>{(a.kind === "plan" ? a.plan || a.summary : cmd).slice(0, 800)}</code>
      <TermShownNote a={a} />
      <div className="macts">
        <button className="btn ok" disabled={busy} onClick={() => sendDecision(a, a.kind === "plan" ? { behavior: "allow", updatedInput: a.input } : { behavior: "allow" }, setBusy)}>
          Allow
        </button>
        <button className="btn" disabled={busy} onClick={() => sendDecision(a, { behavior: "deny" }, setBusy)}>
          Deny
        </button>
      </div>
    </div>
  );
}

function QuestionBubble({ a }: { a: Approval }) {
  const [busy0, setBusy] = useState(false);
  const busy = busy0 || sentToTerminal(a);
  const [writing, setWriting] = useState<string | null>(null);
  const [text, setText] = useState("");
  const qs = a.questions?.length ? a.questions : [{ question: a.summary, options: [] }];
  return (
    <div className="mreq">
      {qs.map((q) => (
        <div key={q.question} className="qq">
          <b>{q.question}</b>
          <div className="mopts">
            {q.options.map((o) => (
              <button key={o.label} disabled={busy} title={o.description} onClick={() => sendDecision(a, { behavior: "allow", answers: { [q.question]: o.label } }, setBusy)}>
                {o.label}
              </button>
            ))}
            {writing === q.question ? (
              <input
                autoFocus
                value={text}
                placeholder="Your answer · Enter sends"
                onChange={(e) => setText(e.target.value)}
                onKeyDown={(e) => {
                  if (e.key === "Enter" && !e.nativeEvent.isComposing && text.trim()) void sendDecision(a, { behavior: "allow", answers: { [q.question]: text.trim() } }, setBusy);
                  else if (e.key === "Escape") setWriting(null);
                }}
              />
            ) : (
              <button className="w" onClick={() => setWriting(q.question)}>
                Write an answer…
              </button>
            )}
          </div>
        </div>
      ))}
      <TermShownNote a={a} />
    </div>
  );
}

function Composer({ sessionId, session }: { sessionId: string; session?: Session }) {
  const [text, setText] = useState("");
  const [busy, setBusy] = useState(false);
  const shell = useShell();
  const send = async () => {
    const t = text.trim();
    if (!t || !sessionId) return;
    setBusy(true);
    try {
      await api.sendText(sessionId, t, true);
      setText("");
      showToast("Sent", "Typed into the agent's pane.");
    } catch (e) {
      showToast("Send failed", `${(e as Error).message}. Open the pane to register it.`, { kind: "alert", onClick: session ? () => shell.openPane(session) : undefined });
    } finally {
      setBusy(false);
    }
  };
  return (
    <div className="mcompose">
      <input
        value={text}
        placeholder="Message"
        aria-label="Message"
        title="Types the message into the agent's tmux pane and presses Enter"
        disabled={!sessionId}
        onChange={(e) => setText(e.target.value)}
        onKeyDown={(e) => {
          if (e.key === "Enter" && !e.shiftKey && !e.nativeEvent.isComposing) {
            e.preventDefault();
            void send();
          }
        }}
      />
      <button className="btn pri" disabled={busy || !text.trim()} onClick={send}>
        Send
      </button>
    </div>
  );
}

// ---- driven threads: items from the orchestrator, messages go through the orch API ----

type Bub = { k: "me"; it: OItem } | { k: "ag"; it: OItem } | { k: "tools"; items: OItem[] };
function bubbles(items: OItem[]): Bub[] {
  const out: Bub[] = [];
  for (const it of [...items].sort((a, b) => toMs(a.startedAt) - toMs(b.startedAt))) {
    if (it.kind === "user" || it.kind === "agent") out.push(it.kind === "user" ? { k: "me", it } : { k: "ag", it });
    else if (it.kind !== "reasoning") {
      const last = out[out.length - 1];
      if (last?.k === "tools") last.items.push(it);
      else out.push({ k: "tools", items: [it] });
    }
  }
  return out;
}

function DrivenMsgChat({ r, id, phone, onBack }: { r: Row; id: string; phone: boolean; onBack: () => void }) {
  const orch = useOrch();
  const { detail } = useOrchThread(id);
  const t = detail?.thread || orch.threads.get(id);
  const ref = useRef<HTMLDivElement>(null);
  const items = detail?.items || [];
  const list = useMemo(() => bubbles(items), [items]);
  const cost = (detail?.runs || []).reduce((n, x) => n + (x.usage?.costUsd || 0), 0);
  const running = t?.status === "running" || t?.status === "starting";
  useEffect(() => {
    const el = ref.current;
    if (el) el.scrollTop = el.scrollHeight;
  }, [items.length, r.approvals.length]);
  return (
    <div className="mchat">
      <div className="mhd">
        {phone && (
          <button className="mback" onClick={onBack} aria-label="Back to conversations">
            ‹
          </button>
        )}
        <AgentBadge tool={r.agent} />
        <div className="mgrow">
          <b className="ell" style={{ display: "block" }}>
            <DrivenTag />
            {t?.title || r.title}
          </b>
          {!phone && <div className="msub">{[t?.repo || repoOf({ cwd: r.cwd }), t?.model ? modelShort(t.model) : "", cost ? fmtUsd(cost) : ""].filter(Boolean).join(" · ")}</div>}
        </div>
        <Link className="btn ghost" to={`/w/${encodeURIComponent(id)}`}>
          Workspace
        </Link>
      </div>
      <div className="mbody" ref={ref}>
        {!detail && <div className="mday">Loading the conversation…</div>}
        {list.map((b, i) =>
          b.k === "me" ? (
            <div key={b.it.id} className="mb mb-me">
              <span className="txt">{b.it.text.slice(0, 3000)}</span>
            </div>
          ) : b.k === "ag" ? (
            <div key={b.it.id} className="mb mb-ag">
              {b.it.text ? <Markdown text={b.it.text.slice(0, 6000)} /> : "…"}
            </div>
          ) : (
            <div key={`t${i}`} className={`mtool ${b.items.some((x) => x.status === "failed") ? "bad" : ""}`}>
              ⌁ {b.items.length} {b.items.length === 1 ? "step" : "steps"}
              {b.items.some((x) => x.status === "failed") ? ` · ${b.items.filter((x) => x.status === "failed").length} failed` : ""}
              {b.items[b.items.length - 1].command ? ` · ${String(b.items[b.items.length - 1].command).slice(0, 60)}` : ""}
            </div>
          ),
        )}
        {r.approvals.map((a) => (a.kind === "question" ? <QuestionBubble key={a.id} a={a} /> : <ApprovalBubble key={a.id} a={a} />))}
        {running && <div className="mtool live">⌁ working</div>}
        {t?.error && <div className="mday err">{t.error}</div>}
      </div>
      <DrivenComposer id={id} closed={t?.status === "closed"} running={running} />
    </div>
  );
}

function DrivenComposer({ id, closed, running }: { id: string; closed: boolean; running: boolean }) {
  const orch = useOrch();
  const [text, setText] = useState("");
  const [busy, setBusy] = useState(false);
  const block = orch.blocked.get(id);
  const send = async () => {
    const t = text.trim();
    if (!t || busy) return;
    setBusy(true);
    try {
      await sendMessage(id, t);
      setText("");
    } catch (e) {
      if (!spendBlockOf(e)) showToast(e instanceof ApiError && e.status === 409 ? "Cannot steer now" : "Send failed", (e as Error).message, { kind: "alert" });
    } finally {
      setBusy(false);
    }
  };
  if (block)
    return (
      <div className="mcompose mblock" role="alert">
        <span className="mgrow">
          Spend limit reached: {usd(block.spent)} of {usd(block.limit)}.{" "}
          <Link to="/settings/limits">Limits</Link>
        </span>
        <button className="btn pri" onClick={() => allowOnce(id).catch((e) => showToast("Could not allow the turn", (e as Error).message, { kind: "alert" }))}>
          Allow once more
        </button>
      </div>
    );
  return (
    <div className="mcompose">
      <input
        value={text}
        placeholder={closed ? "This thread is closed" : running ? "Steer the running turn" : "Message"}
        aria-label="Message"
        title="Sends through agent-monitor to the driven thread"
        disabled={closed}
        onChange={(e) => setText(e.target.value)}
        onKeyDown={(e) => {
          if (e.key === "Enter" && !e.shiftKey && !e.nativeEvent.isComposing) {
            e.preventDefault();
            void send();
          }
        }}
      />
      <button className="btn pri" disabled={busy || closed || !text.trim()} onClick={send}>
        Send
      </button>
    </div>
  );
}
