// /approvals: the request queue that agent-monitor.app shows by itself when an agent waits for you.
// The list is GET /api/needs-you and nothing else, so it matches every other surface.
import { PeerNeedsBox, usePeerFeed } from "./peers";
import { SecretCards } from "./SecretCards";
import { settleThread } from "../api/orch";
import { useCallback, useEffect, useRef, useState } from "react";
import { setTheme, useTheme } from "../../app/prefs";
import { api } from "../../api/client";
import { fmtAgo, projectName, shortCwd } from "../../lib/format";
import { dropPerm, useLive } from "../../lib/ws";
import { ApiError } from "../api/client";
import { dropNeedsYou, refreshNeedsYou, useNeedsYou, type NeedsYouItem } from "../api/needsYou";
import { p7, type TermAnswerBody, type TermQuestion } from "../api/p7";
import type { Approval } from "../api/types";
import { AgentIcon } from "../components/AgentIcon";
import { diffLines, DiffView } from "../components/ApprovalCard";
import { agentName, startV3Live, useV3 } from "../live";
import { useTick } from "./workspace/DrivenChat";
import { PlanBody as TermPlanBody, QuestionsBody as TermQuestionsBody } from "./workspace/TermQuestion";
import { AskHeadCtx, AskKeysCtx, askFromApproval, DecideHookCtx, WsApproval, type AskInfo } from "./workspace/WsApproval";
import "../styles/v3.css";
import "../styles/workspace.css";
import "../styles/approvals.css";

type Handler = { postMessage: (v: unknown) => void };
const handler = (name: string) => (window as unknown as { webkit?: { messageHandlers?: Record<string, Handler | undefined> } }).webkit?.messageHandlers?.[name];
const native = (name: string, v: unknown): void => { void handler(name)?.postMessage(v); };
const inMac = () => !!handler("approvals");
const UNDO_MS = 3000;
// A request that just moved to the top needs a moment on screen, so a double press cannot decide it.
const SETTLE_MS = 600;

type Perm = NonNullable<NeedsYouItem["mcp"]>;
type Req =
  | { key: string; at: number; type: "appr"; it: NeedsYouItem; a: Approval; ask: AskInfo }
  | { key: string; at: number; type: "perm"; it: NeedsYouItem; p: Perm }
  | { key: string; at: number; type: "term"; it: NeedsYouItem; sid: string; q: TermQuestion }
  | { key: string; at: number; type: "prompt"; it: NeedsYouItem; sid: string }
  | { key: string; at: number; type: "error"; it: NeedsYouItem; tid: string };
type Result = "ok" | "elsewhere" | string;
type Commit = (label: string, fire: () => Promise<Result>) => void;
/** What the keyboard reaches on the request at the top. */
interface Acts {
  primary?: () => void;
  deny?: () => void;
  pick?: (n: number) => void;
}

const SHELL = new Set(["Bash", "shell", "exec_command", "local_shell", "unified_exec"]);
const str = (v: unknown) => (typeof v === "string" ? v : Array.isArray(v) ? v.map(String).join(" ") : "");
const inField = (el: EventTarget | null) => el instanceof HTMLElement && (el.isContentEditable || ["INPUT", "TEXTAREA", "SELECT"].includes(el.tagName));
// 409 means another surface answered first; 422 and the rest keep the card with the error.
const resultOf = (e: unknown): Result => (e instanceof ApiError && e.status === 409 ? "elsewhere" : (e as Error).message);
const agentOf = (r: Req) => r.it.agent || (r.type === "appr" ? r.a.agent : "claude");

async function sendPerm(p: Perm, behavior: "allow" | "deny"): Promise<Result> {
  try {
    await api.respondPerm(p.id, behavior);
    dropPerm(p.id);
    return "ok";
  } catch (e) {
    return (e as { status?: number }).status === 404 ? "elsewhere" : (e as Error).message;
  }
}
async function sendTerm(sid: string, q: TermQuestion, b: TermAnswerBody): Promise<Result> {
  try {
    await p7.answerTerm(sid, { ...b, toolUseId: q.toolUseId });
    return "ok";
  } catch (e) {
    return resultOf(e);
  }
}
async function answerPrompt(sid: string, answer: "yes" | "no" | "dismiss", key?: string): Promise<Result> {
  try {
    await api.answerPrompt(sid, answer, key);
    return "ok";
  } catch (e) {
    return resultOf(e);
  }
}

function toReq(it: NeedsYouItem, live: Map<string, Approval>): Req | null {
  const base = { key: it.key, at: it.at, it };
  if (it.kind === "approval" && it.approval) {
    const a = live.get(it.approval.id) || it.approval;
    return { ...base, type: "appr", a, ask: askFromApproval(a) };
  }
  if (it.kind === "mcp" && it.mcp) return { ...base, type: "perm", p: it.mcp };
  if (it.kind === "question" && it.question && it.sessionId) return { ...base, type: "term", sid: it.sessionId, q: it.question };
  if (it.kind === "prompt" && it.sessionId) return { ...base, type: "prompt", sid: it.sessionId };
  if (it.kind === "error" && it.threadId) return { ...base, type: "error", tid: it.threadId };
  return null;
}

export default function ApprovalsPage() {
  const peerFeed = usePeerFeed();
  useEffect(startV3Live, []);
  const [theme, toggleTheme] = useTheme();
  useEffect(() => {
    try {
      if (localStorage.getItem("agent-monitor-theme") === null && window.matchMedia?.("(prefers-color-scheme: light)").matches) setTheme("light");
    } catch {
      /* storage off */
    }
  }, []);
  useEffect(() => { native("theme", theme); }, [theme]);
  const v = useV3();
  const live = useLive();
  useTick(true, 30_000);
  // The app pauses a hidden window, so it stops polling until it shows again.
  const [paused, setPaused] = useState(false);
  useEffect(() => {
    const w = window as unknown as { __amPause?: (on: boolean) => void };
    w.__amPause = (on) => setPaused(!!on);
    return () => void delete w.__amPause;
  }, []);
  const ny = useNeedsYou({ fast: true, paused });
  // Live approval and MCP events re-read the list at once instead of waiting for the next poll.
  useEffect(() => {
    if (!paused) void refreshNeedsYou();
  }, [v.approvals, live.perms, paused]);

  // A decided approval leaves at once; the next read drops it from the list too.
  const all: Req[] = ny.items
    .map((it) => toReq(it, v.approvals))
    .filter((r): r is Req => !!r && !(r.type === "appr" && r.a.state !== "pending"));

  // Decisions wait UNDO_MS before they are sent; a sent request stays hidden until the list drops it.
  const [hidden, setHidden] = useState<Set<string>>(new Set());
  const [pend, setPend] = useState<{ key: string; label: string; until: number } | null>(null);
  const pendRef = useRef<{ key: string; fire: () => Promise<Result>; timer: number } | null>(null);
  const [note, setNote] = useState<{ text: string; err: boolean } | null>(null);
  const [failed, setFailed] = useState<Record<string, string>>({});
  useEffect(() => {
    if (!note) return;
    const t = window.setTimeout(() => setNote(null), 4500);
    return () => window.clearTimeout(t);
  }, [note]);
  const unhide = (k: string) => setHidden((h) => (h.has(k) ? new Set([...h].filter((x) => x !== k)) : h));
  const flush = useCallback(() => {
    const p = pendRef.current;
    if (!p) return;
    pendRef.current = null;
    window.clearTimeout(p.timer);
    setPend(null);
    void p.fire().then((r) => {
      if (r === "ok") {
        dropNeedsYou(p.key);
      } else if (r === "elsewhere") {
        setNote({ text: "Answered elsewhere", err: false });
        dropNeedsYou(p.key);
      } else {
        setNote({ text: `Not sent: ${r}`, err: true });
        setFailed((f) => ({ ...f, [p.key]: r }));
        unhide(p.key);
        setSelKey(p.key);
      }
      void refreshNeedsYou();
    });
  }, []);
  useEffect(() => {
    window.addEventListener("pagehide", flush);
    return () => window.removeEventListener("pagehide", flush);
  }, [flush]);
  const [selKey, setSelKey] = useState<string | null>(null);
  const shown = all.filter((r) => !hidden.has(r.key));
  const cur = shown.find((r) => r.key === selKey) || shown[0];
  const commitFor =
    (key: string): Commit =>
    (label, fire) => {
      // The second click of a double-click lands on the next card: the same settle time as the keys.
      if (Date.now() - since.current < SETTLE_MS) return;
      flush();
      setHidden((h) => new Set(h).add(key));
      setNote(null);
      setFailed((f) => {
        const n = { ...f };
        delete n[key];
        return n;
      });
      const timer = window.setTimeout(flush, UNDO_MS);
      pendRef.current = { key, fire, timer };
      setPend({ key, label, until: Date.now() + UNDO_MS });
      setSelKey(null);
    };
  const undo = useCallback(() => {
    const p = pendRef.current;
    if (!p) return;
    pendRef.current = null;
    window.clearTimeout(p.timer);
    setPend(null);
    unhide(p.key);
    setSelKey(p.key);
  }, []);
  // Forget hidden keys once the list no longer has them.
  const keySig = all.map((r) => r.key).join("|");
  useEffect(() => {
    const keys = new Set(keySig.split("|"));
    setHidden((h) => {
      const n = new Set([...h].filter((k) => keys.has(k) || k === pendRef.current?.key));
      return n.size === h.size ? h : n;
    });
  }, [keySig]);

  // The app reads whether you type, so it does not hide the window under you, and the drawn count.
  const [typing, setTyping] = useState(false);
  useEffect(() => {
    const look = () => {
      const el = document.activeElement;
      setTyping(inField(el) && !!(el as HTMLInputElement).value);
    };
    document.addEventListener("input", look, true);
    document.addEventListener("focusin", look);
    document.addEventListener("focusout", look);
    return () => {
      document.removeEventListener("input", look, true);
      document.removeEventListener("focusin", look);
      document.removeEventListener("focusout", look);
    };
  }, []);
  useEffect(() => { native("approvals", { type: "state", typing, shown: shown.length }); }, [typing, shown.length]);

  // One keyboard map for the window.
  const acts = useRef<Acts>({});
  const since = useRef(0);
  useEffect(() => {
    since.current = Date.now();
  }, [cur?.key]);
  const move = (d: number) => {
    if (!shown.length) return;
    const i = Math.max(0, shown.findIndex((r) => r.key === cur?.key));
    setSelKey(shown[(i + d + shown.length) % shown.length].key);
  };
  const moveRef = useRef(move);
  moveRef.current = move;
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.metaKey || e.ctrlKey || e.altKey || e.repeat) return;
      if (e.key === "Escape") {
        if (inField(document.activeElement)) (document.activeElement as HTMLElement).blur();
        else native("approvals", { type: "hide" });
        return;
      }
      if (inField(e.target) || inField(document.activeElement)) return;
      const k = e.key.toLowerCase();
      if (k === "j" || e.key === "ArrowDown") return e.preventDefault(), moveRef.current(1);
      if (k === "k" || e.key === "ArrowUp") return e.preventDefault(), moveRef.current(-1);
      if (k === "u" && pendRef.current) return e.preventDefault(), undo();
      const t = e.target as HTMLElement | null;
      if (e.key === "Enter" && t && t !== document.body && (t.tagName === "BUTTON" || t.tagName === "A" || t.tagName === "SUMMARY")) return;
      const a = acts.current;
      const decide = e.key === "Enter" || k === "a" || k === "d" || /^[1-9]$/.test(e.key);
      if (!decide) return;
      e.preventDefault();
      if (Date.now() - since.current < SETTLE_MS) return;
      if (e.key === "Enter" || k === "a") a.primary?.();
      else if (k === "d") a.deny?.();
      else a.pick?.(Number(e.key));
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [undo]);

  const titleOf = (r: Req) => r.it.title || projectName(r.it.cwd || (r.type === "appr" ? r.a.cwd : r.type === "perm" ? r.p.cwd || "" : "")) || `${agentName(agentOf(r))} session`;
  const others = shown.filter((r) => r !== cur);
  const loading = ny.status === "loading";

  return (
    <div className={`v3 apv ${inMac() ? "apv-mac" : ""}`}>
      <header className="apv-top">
        <span className={`dot ${live.connected ? "d-ok" : "d-err"}`} title={live.connected ? "Live" : "Reconnecting"} />
        <b>Needs you</b>
        {shown.length > 0 && <span className="apv-count">{shown.length}</span>}
        <span className="sp" />
        <span className="apv-keys xs muted" aria-hidden>
          <span className="kbd">J</span>
          <span className="kbd">K</span> move
        </span>
        <button className="btn sm ghost" onClick={toggleTheme} title={theme === "dark" ? "Light theme" : "Dark theme"} aria-label="Switch theme">
          {theme === "dark" ? "☀" : "☾"}
        </button>
      </header>
      <main className="apv-body">
        <SecretCards />
        {ny.error && <div className="apv-warn xs">{ny.status === "ok" ? "Showing the last list; the daemon did not answer" : "Could not read the list"}: {ny.error}</div>}
        {cur ? (
          <Focus key={cur.key} req={cur} title={titleOf(cur)} pos={shown.indexOf(cur) + 1} total={shown.length} commit={commitFor(cur.key)} acts={acts} error={failed[cur.key]} />
        ) : (
          <div className="apv-empty">
            <svg viewBox="0 0 24 24" width="28" height="28" fill="none" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round" strokeLinejoin="round" aria-hidden>
              <path d="M20 6 9 17l-5-5" />
            </svg>
            <b>{loading ? "Loading…" : "Nothing needs you"}</b>
            <span className="muted sm">{loading ? "" : "Requests from your agents show here as they arrive."}</span>
          </div>
        )}
        <PeerNeedsBox feed={peerFeed} compact />
        {others.length > 0 && (
          <section className="apv-queue" aria-label="Other requests">
            <div className="apv-eb">
              Also needs you <span className="muted">{others.length}</span>
            </div>
            {others.map((r) => (
              <button key={r.key} className="apv-qrow" onClick={() => setSelKey(r.key)}>
                <AgentIcon agent={agentOf(r)} size={14} />
                <span className="apv-qt">
                  <span className="trunc">{reqLine(r)}</span>
                  <span className="trunc muted xs">{titleOf(r)}</span>
                </span>
                <span className="muted xs tab-num">{fmtAgo(r.at, "")}</span>
              </button>
            ))}
          </section>
        )}
      </main>
      {(pend || note) && (
        <div className={`apv-status ${note?.err ? "err" : ""}`} role="status">
          {pend ? <Undo label={pend.label} until={pend.until} onUndo={undo} /> : <span>{note!.text}</span>}
        </div>
      )}
    </div>
  );
}

function reqLine(r: Req): string {
  if (r.type === "perm") return `${r.p.toolName}: ${str(r.p.input?.command) || permSummary(r.p)}`;
  if (r.type === "term") return r.q.tool === "ExitPlanMode" ? "Plan ready" : r.q.questions?.[0]?.question || "Question";
  if (r.type === "prompt") return (r.it.message || "Asks in the terminal").split("\n")[0];
  if (r.type === "error") return `Error: ${(r.it.message || "").split("\n")[0]}`;
  const a = r.a;
  if (a.kind === "question") return a.questions?.[0]?.question || a.summary;
  if (a.kind === "plan") return "Plan ready for review";
  return `${a.toolName}: ${str(a.input?.command) || a.summary}`;
}

function Undo({ label, until, onUndo }: { label: string; until: number; onUndo: () => void }) {
  const now = useTick(true, 250);
  const left = Math.max(0, Math.ceil((until - now) / 1000));
  return (
    <>
      <span className="apv-sent">Sent</span>
      <span className="trunc">{label}</span>
      <span className="sp" />
      <button className="btn sm" onClick={onUndo}>
        Undo <span className="muted tab-num">({left} s)</span>
      </button>
    </>
  );
}

function Head({ agent, verb, title, at, pos, total, risk }: { agent: string; verb: string; title: string; at: number; pos: number; total: number; risk?: Approval["risk"] }) {
  return (
    <div className="apv-head">
      <span className="apv-glyph">
        <AgentIcon agent={agent} size={20} />
      </span>
      <span className="apv-who">
        <span className="trunc apv-title">{title}</span>
        <span className="apv-verb">
          <b>{agentName(agent)}</b> {verb}
        </span>
      </span>
      <span className="apv-meta">
        {risk && <span className={`apv-risk r-${risk}`}>{risk} risk</span>}
        <span className="muted xs tab-num">
          {total > 1 ? `${pos} of ${total} · ` : ""}
          {fmtAgo(at, " ago")}
        </span>
      </span>
    </div>
  );
}

/** A second key press confirms a high-risk allow or a plan; clicks act at once. */
function useArm() {
  const [armed, setArmed] = useState(false);
  useEffect(() => {
    if (!armed) return;
    const t = window.setTimeout(() => setArmed(false), 3000);
    return () => window.clearTimeout(t);
  }, [armed]);
  return { armed, guard: (fn: () => void) => (armed ? (setArmed(false), fn()) : setArmed(true)) };
}

function Focus({ req, title, pos, total, commit, acts, error }: { req: Req; title: string; pos: number; total: number; commit: Commit; acts: React.MutableRefObject<Acts>; error?: string }) {
  const p = { title, pos, total, commit, acts };
  return (
    <article className="apv-focus" aria-label="Request">
      {req.type === "term" ? <TermFocus req={req} {...p} /> : req.type === "prompt" ? <PromptFocus req={req} {...p} /> : req.type === "error" ? <ErrorFocus req={req} {...p} /> : req.type === "perm" ? <PermFocus req={req} {...p} /> : <ApprFocus req={req} {...p} />}
      {error && <div className="apv-warn xs">Not sent: {error}</div>}
    </article>
  );
}

type FocusProps<T> = { req: T; title: string; pos: number; total: number; commit: Commit; acts: React.MutableRefObject<Acts> };

function ApprFocus({ req, title, pos, total, commit, acts }: FocusProps<Extract<Req, { type: "appr" }>>) {
  const { a, ask } = req;
  const box = useRef<HTMLDivElement>(null);
  const verb = a.kind === "question" ? "asks" : a.kind === "plan" ? "has a plan ready" : SHELL.has(a.toolName) ? "wants to run" : diffLines(a.toolName, a.input || {}) ? "wants to edit a file" : `wants to use ${a.toolName}`;
  // The one approval card; each decision goes through this panel's Undo, and the panel's keys press its buttons.
  // The daemon's own reason reaches the panel: 409 shows "Answered elsewhere", a 423 refusal its instruction.
  const hook = useCallback((label: string, fire: () => Promise<true | string>) => commit(label, async () => {
    const r = await fire();
    return r === true ? "ok" : r;
  }), [commit]);
  // The card marks what each key presses (data-k, data-n); a plan needs a second A, as the card's own "a a".
  const arm = useArm();
  const press = (sel: string) => box.current?.querySelector<HTMLButtonElement>(sel + ":not(:disabled)")?.click();
  acts.current = {
    primary: () => (a.kind === "plan" ? arm.guard(() => press('[data-k="a"]')) : press('[data-k="a"]')),
    deny: () => press('[data-k="d"]'),
    pick: (n: number) => {
        // A digit answers the first question without an answer, then the next one.
        const q = box.current?.querySelector('.wq[data-done="0"]') ?? [...(box.current?.querySelectorAll(".wq") ?? [])].pop();
        (q?.querySelectorAll<HTMLButtonElement>(".wq-o")[n - 1] ?? box.current?.querySelector<HTMLButtonElement>(`[data-n="${n}"]`))?.click();
      },
  };
  return (
    <>
      <Head agent={a.agent} verb={verb} title={title} at={a.createdAt} pos={pos} total={total} risk={a.kind === "permission" ? a.risk : undefined} />
      {arm.armed && (
        <div className="apv-armed" role="status">
          Press <span className="kbd">A</span> again to approve the plan.
        </div>
      )}
      <div ref={box} className="apv-card">
        <DecideHookCtx.Provider value={hook}>
          <AskHeadCtx.Provider value={false}>
            <AskKeysCtx.Provider value={false}>
              <WsApproval ask={ask} />
            </AskKeysCtx.Provider>
          </AskHeadCtx.Provider>
        </DecideHookCtx.Provider>
      </div>
    </>
  );
}

function permSummary(p: Perm): string {
  const i = p.input || {};
  return str(i.file_path) || str(i.path) || str(i.url) || str(i.pattern) || str(i.query) || JSON.stringify(i).slice(0, 160);
}

function PermFocus({ req, title, pos, total, commit, acts }: FocusProps<Extract<Req, { type: "perm" }>>) {
  const { p } = req;
  const high = p.risk === "high";
  const arm = useArm();
  const short = (str(p.input?.command) || permSummary(p)).slice(0, 80);
  const send = (b: "allow" | "deny") => commit(`${b === "allow" ? "Allowed" : "Denied"}: ${short}`, () => sendPerm(p, b));
  const allow = () => (high ? arm.guard(() => send("allow")) : send("allow"));
  const deny = () => send("deny");
  acts.current = { primary: allow, deny };
  const verb = SHELL.has(p.toolName) ? "wants to run" : diffLines(p.toolName, p.input || {}) ? "wants to edit a file" : `wants to use ${p.toolName}`;
  return (
    <>
      <Head agent={agentOf(req)} verb={verb} title={title} at={p.createdAt} pos={pos} total={total} risk={p.risk} />
      <What a={{ toolName: p.toolName, input: p.input || {}, cwd: p.cwd || "", summary: permSummary(p) }} />
      {p.riskReason && p.risk && p.risk !== "low" && (
        <div className={`apv-why r-${p.risk}`}>
          <b>Why {p.risk} risk:</b> {p.riskReason}
        </div>
      )}
      {arm.armed && (
        <div className="apv-armed" role="status">
          High risk: press <span className="kbd">A</span> or click again within 3 s to allow.
        </div>
      )}
      <div className="apv-acts">
        <button className={`apv-btn yes ${arm.armed ? "armed" : ""}`} onClick={allow}>
          {arm.armed ? "Confirm allow" : high ? "Allow once" : "Allow"} <span className="kbd">↵</span>
        </button>
        <button className="apv-btn no" onClick={deny}>
          Deny <span className="kbd">D</span>
        </button>
      </div>
      <div className="apv-note xs">The session waits until you answer.</div>
    </>
  );
}

function What({ a }: { a: Pick<Approval, "toolName" | "input" | "cwd" | "summary"> }) {
  const input = a.input || {};
  const cmd = str(input.command) || str(input.cmd);
  const diff = diffLines(a.toolName, input, 400);
  const url = str(input.url);
  const file = str(input.file_path) || str(input.path) || str(input.notebook_path);
  const json = Object.keys(input).length ? JSON.stringify(input, null, 2) : "";
  return (
    <div className="apv-what">
      <div className="apv-tool xs">
        <span className="mono">{a.toolName}</span>
        {a.cwd && <span className="muted"> in {shortCwd(a.cwd)}</span>}
      </div>
      {cmd ? (
        <pre className="apv-cmd">{cmd}</pre>
      ) : diff ? (
        <>
          {file && <div className="apv-path mono">{shortCwd(file)}</div>}
          <DiffView lines={diff} />
        </>
      ) : url ? (
        <div className="apv-path mono">{url}</div>
      ) : file ? (
        <div className="apv-path mono">{shortCwd(file)}</div>
      ) : (
        <pre className="apv-cmd soft">{json || a.summary}</pre>
      )}
      {(url || (file && !diff)) && json && (
        <details className="apv-raw">
          <summary className="xs muted">Tool input</summary>
          <pre>{json}</pre>
        </details>
      )}
    </div>
  );
}

function TermFocus({ req, title, pos, total, commit, acts }: FocusProps<Extract<Req, { type: "term" }>>) {
  const { q, sid, it } = req;
  acts.current = {};
  const plan = q.tool === "ExitPlanMode";
  const canSend = it.answerable && !q.sentAt;
  // The shared bodies send at once; here each answer goes through the undo delay.
  const send = (b: TermAnswerBody) => {
    commit(plan ? "Plan answer sent" : "Answer sent", () => sendTerm(sid, q, b));
    return Promise.resolve();
  };
  return (
    <>
      <Head agent={agentOf(req)} verb={plan ? "has a plan ready" : "asks"} title={title} at={req.at} pos={pos} total={total} />
      {plan ? <TermPlanBody q={q} busy={false} canSend={canSend} send={send} /> : <TermQuestionsBody q={q} busy={false} canSend={canSend} send={send} />}
      {q.sentAt ? <div className="apv-note xs">Sent. This closes when the agent takes the answer.</div> : !it.answerable && <div className="apv-note xs">Answer it in the terminal: {it.why || "agent-monitor cannot reach this session"}.</div>}
    </>
  );
}

// Claude Code and Codex both take "1" for yes and Escape for no on their permission prompts.
function ErrorFocus({ req, title, pos, total, commit, acts }: FocusProps<Extract<Req, { type: "error" }>>) {
  const { it, tid } = req;
  const [cause, ...more] = (it.message || "").split("\n");
  const rest = more.join("\n").trim();
  const open = () => {
    const path = `/w/${encodeURIComponent(tid)}`;
    if (handler("open")) native("open", path);
    else window.open(path, "_blank");
  };
  const dismiss = () =>
    commit("Dismissed", async () => {
      try {
        await settleThread(tid, true);
        return "ok";
      } catch (e) {
        return resultOf(e);
      }
    });
  acts.current = { primary: open, deny: dismiss };
  return (
    <>
      <Head agent={agentOf(req)} verb="stopped with an error" title={title} at={req.at} pos={pos} total={total} />
      <div className="apv-why r-high">{cause}</div>
      {rest && (
        <details className="apv-more">
          <summary className="xs muted">Details</summary>
          <pre className="apv-cmd soft">{rest.slice(0, 2000)}</pre>
        </details>
      )}
      <div className="apv-acts">
        <button className="apv-btn yes" onClick={open}>
          Open <span className="kbd">↵</span>
        </button>
        <button className="apv-btn no" onClick={dismiss}>
          Dismiss <span className="kbd">D</span>
        </button>
      </div>
    </>
  );
}

function PromptFocus({ req, title, pos, total, commit, acts }: FocusProps<Extract<Req, { type: "prompt" }>>) {
  const { it, sid } = req;
  const on = it.answerable;
  const yes = () => commit("Yes", () => answerPrompt(sid, "yes", it.promptKey));
  const no = () => commit("No", () => answerPrompt(sid, "no", it.promptKey));
  const dismiss = () => commit("Dismissed", () => answerPrompt(sid, "dismiss"));
  acts.current = on ? { primary: yes, deny: no } : { deny: dismiss };
  return (
    <>
      <Head agent={agentOf(req)} verb="asks in the terminal" title={title} at={req.at} pos={pos} total={total} />
      {it.message && <pre className="apv-cmd soft">{it.message.slice(0, 1200)}</pre>}
      <div className="apv-acts">
        <button className="apv-btn yes" disabled={!on} onClick={yes}>
          Yes <span className="kbd">↵</span>
        </button>
        <button className="apv-btn no" disabled={!on} onClick={no}>
          No <span className="kbd">D</span>
        </button>
      </div>
      {!on && <div className="apv-note xs">{it.why || "Answer it in the terminal."}</div>}
      {!on && (
        <div className="apv-acts">
          <button className="apv-btn no" onClick={dismiss}>
            Dismiss <span className="kbd">D</span>
          </button>
        </div>
      )}
    </>
  );
}
