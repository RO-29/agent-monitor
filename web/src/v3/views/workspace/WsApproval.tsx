// Inline approval card for the Workspace chat: commands, file edits, questions and plans. Decides through the existing approval API.
import { createContext, useContext, useEffect, useRef, useState, useSyncExternalStore } from "react";
import { Link } from "react-router-dom";
import "../../styles/workspace.css"; // the card carries its own styles wherever it shows
import { showToast } from "../../../app/toast";
import { ApiError, v3 } from "../../api/client";
import { agentCls, agentLabel, mockHooks, useOrch, type OItem, type ORequest } from "../../api/orch";
import type { Approval, DecideBody, PermOption, Question } from "../../api/types";
import { canAlways, DenyTarget, diffLines, DiffView, type Line } from "../../components/ApprovalCard";
import { sentToTerminal, TermShownNote } from "../../components/TermShown";
import { approvalKeyOk, useApproveKeys } from "../../keys";
import { shortCwd } from "../../../lib/format";
import { useDraftMap } from "../../../lib/draft";
import { VoiceInput } from "../../../lib/voice";
import { Markdown } from "../../components/Markdown";
import { applyDecision, loadApprovals, agentName } from "../../live";

export type AskKind = "command" | "fileChange" | "permissions" | "question" | "plan";
export interface AskInfo {
  approvalId: string;
  agent: string;
  kind: AskKind;
  verb: string;
  body: string;
  questions?: Question[];
  plan?: string;
  risk?: Approval["risk"];
  riskReason?: string;
  always: boolean;
  approval?: Approval;
  from?: { id: string; title: string };
  options?: PermOption[];
  /** False while the approval record is not loaded yet; allow stays off until it is. */
  loaded: boolean;
  item?: OItem;
}

const KIND_VERB: Record<string, string> = { command: "wants to run", fileChange: "wants to edit files", permissions: "wants to use a tool", question: "asks", plan: "has a plan ready" };
const str = (v: unknown) => (typeof v === "string" ? v : "");
// ACP agents offer their own permission options; older daemons may only put them in input.
function optionsOf(a: Approval | undefined): PermOption[] | undefined {
  const raw = a?.options?.length ? a.options : Array.isArray(a?.input?.options) ? (a!.input.options as PermOption[]) : undefined;
  const ok = raw?.filter((o) => o && typeof o.optionId === "string" && typeof o.name === "string" && typeof o.kind === "string");
  return ok?.length ? ok : undefined;
}
type How = "allow" | "always" | "deny";
const howOf = (o: PermOption): How => {
  const k = typeof o.kind === "string" ? o.kind : "";
  return k === "allow_always" ? "always" : k.startsWith("reject") ? "deny" : "allow";
};

function questionsOf(a: Approval | undefined, item: OItem | undefined): Question[] {
  if (a?.questions?.length) return a.questions;
  const raw = a?.input?.questions;
  if (Array.isArray(raw)) return raw as Question[];
  const text = a?.summary || item?.text || "";
  return text ? [{ question: text, options: [] }] : [];
}

export function askFromRequest(r: ORequest, approval: Approval | undefined, item: OItem | undefined, agent: string): AskInfo {
  const kind: AskKind = r.kind;
  const plan = kind === "plan" ? approval?.plan || str(approval?.input?.plan) || item?.text || approval?.summary || "" : undefined;
  const body =
    str(approval?.input?.command) ||
    item?.command ||
    (item?.files?.length ? item.files.map((f) => f.path).join("\n") : "") ||
    approval?.summary ||
    item?.text ||
    r.kind;
  return {
    approvalId: r.approvalId,
    agent,
    kind,
    verb: KIND_VERB[r.kind] || "asks",
    body,
    questions: kind === "question" ? questionsOf(approval, item) : undefined,
    plan,
    risk: kind === "question" || kind === "plan" ? undefined : approval?.risk,
    riskReason: approval?.riskReason,
    always: kind === "question" || kind === "plan" ? false : approval ? canAlways(approval) : false,
    approval,
    options: kind === "question" || kind === "plan" ? undefined : optionsOf(approval),
    loaded: !!approval,
    item,
  };
}
export function askFromApproval(a: Approval): AskInfo {
  const cmd = str(a.input?.command);
  const kind: AskKind = a.kind === "question" ? "question" : a.kind === "plan" ? "plan" : "command";
  return {
    approvalId: a.id,
    agent: a.agent,
    kind,
    verb: a.kind === "question" ? "asks" : a.kind === "plan" ? "has a plan ready" : cmd ? "wants to run" : `wants ${a.toolName}`,
    body: a.kind === "plan" ? a.plan || a.summary : cmd || a.summary,
    questions: a.kind === "question" ? questionsOf(a, undefined) : undefined,
    plan: a.kind === "plan" ? a.plan || str(a.input?.plan) || a.summary : undefined,
    risk: a.kind === "permission" ? a.risk : undefined,
    riskReason: a.riskReason,
    always: a.kind === "permission" && canAlways(a),
    approval: a,
    options: a.kind === "permission" ? optionsOf(a) : undefined,
    loaded: true,
  };
}

/** Sends one decision. A plan approval passes the tool input back, as ApprovalCard does. */
// The Mac app's web views carry message handlers; the daemon records where a decision came from.
// Only the Mac app registers these handlers; a plain WebKit page can expose an empty messageHandlers.
const MH = (window as unknown as { webkit?: { messageHandlers?: Record<string, unknown> } }).webkit?.messageHandlers;
const SURFACE: DecideBody["surface"] = MH?.approvals || MH?.layout ? "mac" : "web";

/** true when the daemon took the decision, else "elsewhere" (409, answered first) or the daemon's reason. */
export async function decideAskBody(ask: AskInfo, b: DecideBody, mock: boolean): Promise<true | string> {
  if (mock && mockHooks) {
    mockHooks.decide(ask.approvalId, b.behavior === "allow", b);
    return true;
  }
  const a = ask.approval;
  try {
    const r = await v3.decide(ask.approvalId, { surface: SURFACE, ...b });
    if (r && r.id) applyDecision(r);
    else if (a) applyDecision({ ...a, state: b.behavior === "deny" ? "denied" : "allowed" });
    return true;
  } catch (e) {
    const elsewhere = e instanceof ApiError && e.status === 409;
    if (elsewhere) showToast("Already answered", "Another surface answered first.");
    else showToast("Decision failed", (e as Error).message, { kind: "alert" });
    void loadApprovals();
    return elsewhere ? "elsewhere" : (e as Error).message;
  }
}

export function decideAsk(ask: AskInfo, how: How, mock: boolean, opt?: PermOption, extra?: Partial<DecideBody>): Promise<true | string> {
  const o = opt || ask.options?.find((x) => howOf(x) === how && !String(x.kind).endsWith("_always")) || ask.options?.find((x) => howOf(x) === how);
  const b: DecideBody =
    how === "deny"
      ? { behavior: "deny" }
      : ask.kind === "plan"
        ? { behavior: "allow", updatedInput: ask.approval?.input }
        : { behavior: "allow", always: how === "always" || undefined };
  if (o) b.optionId = o.optionId;
  return decideAskBody(ask, { ...b, ...extra }, mock);
}


// One keyboard dispatcher for every approval card on the page. a/d act on the focused card, else on
// the newest card in view, which carries a focus ring. Cards register themselves while mounted.
interface Slot {
  el: HTMLElement | null;
  act: (k: "a" | "d") => void;
}
const slots = new Map<string, Slot>();
let cur: string | null = null;
let curSince = 0;
const curSubs = new Set<() => void>();
const setCur = (id: string | null) => {
  if (id === cur) return;
  cur = id;
  curSince = Date.now();
  curSubs.forEach((f) => f());
};
function inView(el: HTMLElement) {
  const r = el.getBoundingClientRect();
  if (!r.height || r.bottom <= 0 || r.top >= window.innerHeight) return false;
  const box = el.closest(".wfeed")?.getBoundingClientRect();
  return !box || (r.bottom > box.top + 8 && r.top < box.bottom - 8);
}
function pickCur() {
  const act = document.activeElement;
  const list = [...slots.entries()].filter(([, s]) => s.el?.isConnected);
  const foc = list.find(([, s]) => act && s.el!.contains(act));
  if (foc) return setCur(foc[0]);
  const vis = list.filter(([, s]) => inView(s.el!)).sort(([, a], [, b]) => (a.el!.compareDocumentPosition(b.el!) & Node.DOCUMENT_POSITION_FOLLOWING ? -1 : 1));
  setCur(vis.length ? vis[vis.length - 1][0] : null);
}
let raf = 0;
const schedule = () => {
  if (!raf) raf = requestAnimationFrame(() => ((raf = 0), pickCur()));
};
// A card that just became the target needs a moment on screen, so a double press cannot decide the next one.
const SETTLE_MS = 600;
function onKey(e: KeyboardEvent) {
  if (e.key !== "a" && e.key !== "d") return;
  if (!approvalKeyOk(e)) return;
  pickCur();
  const s = cur ? slots.get(cur) : undefined;
  if (!s) return;
  e.preventDefault();
  if (Date.now() - curSince < SETTLE_MS) return;
  s.act(e.key);
}
function attach(on: boolean) {
  if (on) {
    window.addEventListener("keydown", onKey);
    document.addEventListener("scroll", schedule, true);
    document.addEventListener("focusin", schedule);
    document.addEventListener("focusout", schedule);
    window.addEventListener("resize", schedule);
  } else {
    window.removeEventListener("keydown", onKey);
    document.removeEventListener("scroll", schedule, true);
    document.removeEventListener("focusin", schedule);
    document.removeEventListener("focusout", schedule);
    window.removeEventListener("resize", schedule);
  }
}

/** False in the floating panel: no single-key approval there, and high risk needs a second click. */
export const AskKeysCtx = createContext(true);
/** A page that holds decisions (the Mac panel's 3 s Undo) passes its queue here; null sends at once. */
/** False hides the card's own header, for a page that draws its own (the Mac panel). */
export const AskHeadCtx = createContext(true);
export const DecideHookCtx = createContext<((label: string, fire: () => Promise<true | string>) => void) | null>(null);

function useAskSlot(id: string, act: (k: "a" | "d") => void) {
  const on = useContext(AskKeysCtx);
  const actRef = useRef(act);
  actRef.current = act;
  const slot = useRef<Slot>({ el: null, act: (k) => actRef.current(k) });
  useEffect(() => {
    if (!on) return;
    slots.set(id, slot.current);
    if (slots.size === 1) attach(true);
    schedule();
    return () => {
      slots.delete(id);
      if (!slots.size) attach(false);
      if (cur === id) setCur(null);
      schedule();
    };
  }, [id, on]);
  const ref = (el: HTMLDivElement | null) => {
    slot.current.el = el;
  };
  const isCur = useSyncExternalStore(
    (f) => (curSubs.add(f), () => curSubs.delete(f)),
    () => on && cur === id,
  );
  return { ref, isCur };
}

/** Per card: one decision in flight, and a second press for high risk and plans. */
function useDecide() {
  const hook = useContext(DecideHookCtx);
  const [busy, setBusy] = useState(false);
  const flight = useRef(false);
  const [armed, setArmed] = useState<"a" | "d" | null>(null);
  useEffect(() => {
    if (!armed) return;
    const t = window.setTimeout(() => setArmed(null), 4000);
    return () => window.clearTimeout(t);
  }, [armed]);
  const run = async (fn: () => Promise<true | string>, label = "Sent") => {
    if (hook) return hook(label, fn);
    if (flight.current) return;
    flight.current = true;
    setBusy(true);
    setArmed(null);
    try {
      await fn();
    } finally {
      flight.current = false;
      setBusy(false);
    }
  };
  /** Keyboard path: returns true when this press only arms the confirmation. */
  const arm = (k: "a" | "d") => {
    if (armed === k) return false;
    setArmed(k);
    return true;
  };
  return { busy, flight, run, armed, arm };
}

function Head({ ask }: { ask: AskInfo }) {
  const riskCls = ask.risk === "high" ? "p-err" : ask.risk === "low" ? "p-ok" : "p-need";
  return (
    <div className="hd">
      <span className="st need">● Needs you</span>
      {ask.from && (
        <Link className="wfrom trunc" to={`/w/${encodeURIComponent(ask.from.id)}`}>
          {ask.from.title}
        </Link>
      )}
      <span className={`wag ${agentCls(ask.agent)}`}>{agentLabel(ask.agent)}</span>
      <span>{ask.verb}</span>
      {ask.risk && <span className={`pill ${riskCls}`}>{ask.risk} risk</span>}
    </div>
  );
}

function RiskLine({ ask }: { ask: AskInfo }) {
  if (!ask.risk || ask.risk === "low" || !ask.riskReason) return null;
  return (
    <div className={`wrisk ${ask.risk}`}>
      <b>Why {ask.risk} risk:</b> {ask.riskReason}
    </div>
  );
}

/** Full text with a visible cut marker and a Show all toggle; never cuts in silence. */
export function FullText({ text, label, lines = 14, chars = 1500 }: { text: string; label: string; lines?: number; chars?: number }) {
  const [all, setAll] = useState(false);
  const ls = text.split("\n");
  const long = ls.length > lines || text.length > chars;
  let shown = text;
  if (long && !all) {
    shown = ls.slice(0, lines).join("\n");
    if (shown.length > chars) shown = shown.slice(0, chars);
  }
  const hidden = text.length - shown.length;
  return (
    <div className="wfull">
      <pre aria-label={label} className={all ? "all" : ""}>
        {shown}
        {long && !all && <span className="wcut">{`\n… ${hidden.toLocaleString()} more characters not shown`}</span>}
      </pre>
      {long && (
        <button className="btn sm ghost" onClick={() => setAll(!all)} aria-expanded={all}>
          {all ? "Show less" : `Show all (${ls.length.toLocaleString()} ${ls.length === 1 ? "line" : "lines"}, ${text.length.toLocaleString()} characters)`}
        </button>
      )}
    </div>
  );
}

function DiffLines({ lines }: { lines: Line[] }) {
  const [all, setAll] = useState(false);
  const MAX = 60;
  const long = lines.length > MAX;
  return (
    <div className="wfull">
      <DiffView lines={long && !all ? [...lines.slice(0, MAX), { k: "h", t: `… ${lines.length - MAX} more lines not shown` }] : lines} />
      {long && (
        <button className="btn sm ghost" onClick={() => setAll(!all)} aria-expanded={all}>
          {all ? "Show less" : `Show all ${lines.length.toLocaleString()} lines`}
        </button>
      )}
    </div>
  );
}

const SHELL = new Set(["Bash", "shell", "exec_command", "local_shell", "unified_exec"]);
const FILE_WRITE = new Set(["Write", "Edit", "MultiEdit", "NotebookEdit", "apply_patch", "ApplyPatch"]);
const cmdText = (v: unknown) => (typeof v === "string" ? v : Array.isArray(v) ? v.map(String).join(" ") : "");

/** The exact thing the user approves: the full command, the diff of an edit, or the full tool input. */
function AskBody({ ask }: { ask: AskInfo }) {
  const a = ask.approval;
  if (!a) {
    const files = ask.item?.files;
    return (
      <>
        {ask.item?.command ? <FullText text={ask.item.command} label="Command" /> : files?.length ? (
          <ul className="wfiles mono">
            {files.map((f) => (
              <li key={f.path}>
                {f.path} <span className="add">+{f.add}</span> <span className="del">−{f.del}</span>
              </li>
            ))}
          </ul>
        ) : (
          <FullText text={ask.body} label="Request" />
        )}
        <div className="wnote-s">Loading the full request from the daemon. Allow stays off until it loads.</div>
      </>
    );
  }
  const input = a.input || {};
  const cmd = cmdText(input.command) || cmdText(input.cmd);
  const diff = diffLines(a.toolName, input, Infinity);
  const json = Object.keys(input).length ? JSON.stringify(input, null, 2) : "";
  return (
    <>
      <div className="wtool">
        <span className="mono">{a.toolName}</span>
        {a.cwd && <span className="muted"> in {shortCwd(a.cwd)}</span>}
      </div>
      {cmd && (SHELL.has(a.toolName) || !diff) ? (
        <>
          <FullText text={cmd} label="Command" />
          {!SHELL.has(a.toolName) && json && <FullText text={json} label="Tool input" lines={10} />}
        </>
      ) : diff ? (
        <DiffLines lines={diff} />
      ) : json ? (
        <FullText text={json} label="Tool input" lines={24} />
      ) : (
        <FullText text={a.summary || ask.body} label="Request" />
      )}
    </>
  );
}

/** The agent-monitor rule that Always adds, in the same shape as the daemon's alwaysPattern. */
function alwaysRule(a: Approval): string {
  let pat = "";
  if (SHELL.has(a.toolName)) {
    const keep: string[] = [];
    for (const t of a.summary.split(/\s+/).filter(Boolean)) {
      if (keep.length === 3 || t.startsWith("-") || /[/=$'"&|;]/.test(t)) break;
      keep.push(t);
    }
    pat = keep.length ? `${keep.join(" ")}*` : a.summary;
  } else if (FILE_WRITE.has(a.toolName) && a.summary.startsWith("/") && !a.summary.includes(" ")) {
    pat = `${a.summary.slice(0, a.summary.lastIndexOf("/")) || "/"}/*`;
  }
  return `allow ${agentName(a.agent)} ${a.toolName}${pat ? ` when the input matches "${pat}"` : " with any input"}, in ${shortCwd(a.cwd)} and below`;
}

function KeyHint({ k, show }: { k: string; show: boolean }) {
  const on = useContext(AskKeysCtx);
  return show && on ? <span className="kbd">{k}</span> : null;
}

function Armed({ armed, what }: { armed: "a" | "d" | null; what: string }) {
  const on = useContext(AskKeysCtx);
  if (!armed) return null;
  return (
    <div className="warmed" role="status">
      {on ? (
        <>
          Press <span className="kbd">{armed}</span> again to {what}, or use the button.
        </>
      ) : (
        <>Click Allow again to {what}.</>
      )}
    </div>
  );
}

function Card({ ask, label, isCur, slotRef, children }: { ask: AskInfo; label: string; isCur: boolean; slotRef: (el: HTMLDivElement | null) => void; children: React.ReactNode }) {
  const keysOn = useApproveKeys();
  const head = useContext(AskHeadCtx);
  return (
    <div ref={slotRef} className={`wask ${isCur && keysOn ? "cur" : ""}`} role="group" aria-label={`${label}: ${agentLabel(ask.agent)} ${ask.verb}`} tabIndex={0} data-ask={ask.approvalId}>
      {head && <Head ask={ask} />}
      <TermShownNote a={ask.approval} />
      {sentToTerminal(ask.approval) ? <pre className="wsent">{ask.body}</pre> : children}
    </div>
  );
}

function QuestionAsk({ ask }: { ask: AskInfo }) {
  const { mock } = useOrch();
  const keysOn = useApproveKeys();
  const qs = ask.questions || [];
  const [picked, setPicked] = useState<Record<string, string[]>>({});
  const [otherOf, setOtherOf, clearOther] = useDraftMap(`apv:${ask.approvalId}`);
  const other = Object.fromEntries(qs.map((q, i) => [q.question, otherOf(i)])) as Record<string, string>;
  const d = useDecide();
  const single = qs.length === 1 && !qs[0].multiSelect && qs[0].options.length > 0;

  // Same answer shape as ApprovalCard: question text to the picked labels joined by ", ".
  const submit = (over?: Record<string, string[]>) => {
    const src = over || picked;
    const missing = qs.filter((q) => !(src[q.question] || []).length && !other[q.question]?.trim()).length;
    if (missing) return showToast("Answer every question", `${missing} of ${qs.length} still need an answer.`);
    const out: Record<string, string> = {};
    for (const q of qs) {
      const p = [...(src[q.question] || [])];
      if (other[q.question]?.trim()) p.push(other[q.question].trim());
      if (p.length) out[q.question] = p.join(", ");
    }
    if (!Object.keys(out).length) return;
    clearOther();
    void d.run(() => decideAskBody(ask, { behavior: "allow", answers: out }, mock), "Answered: " + Object.values(out).join(", "));
  };
  const decline = () => void d.run(() => decideAskBody(ask, { behavior: "deny", message: "The user declined to answer." }, mock), "Declined");
  const { ref, isCur } = useAskSlot(ask.approvalId, (k) => {
    if (k === "a") showToast("Pick an answer", "Questions need an option or a typed answer.");
    else decline();
  });
  const answered = qs.filter((q) => (picked[q.question] || []).length || other[q.question]?.trim()).length;

  return (
    <Card ask={ask} label="Question" isCur={isCur} slotRef={ref}>
      {qs.map((q) => (
        <div key={q.question} className="wq" data-done={(picked[q.question] || []).length || other[q.question]?.trim() ? 1 : 0}>
          <div className="wq-t">
            {q.header && <span className="pill p-mute">{q.header}</span>}
            <span>{q.question}</span>
            {q.multiSelect && <span className="muted xs">pick any</span>}
          </div>
          {q.options.length > 0 && (
            <div className="wq-opts" role={q.multiSelect ? "group" : "radiogroup"}>
              {q.options.map((o) => {
                const on = (picked[q.question] || []).includes(o.label);
                return (
                  <button
                    key={o.label}
                    className={`wq-o ${on ? "on" : ""}`}
                    role={q.multiSelect ? "checkbox" : "radio"}
                    aria-checked={on}
                    disabled={d.busy}
                    onClick={() => {
                      const cur = picked[q.question] || [];
                      const next = q.multiSelect ? (on ? cur.filter((x) => x !== o.label) : [...cur, o.label]) : [o.label];
                      const all = { ...picked, [q.question]: next };
                      setPicked(all);
                      if (single && !other[q.question]?.trim()) submit(all);
                    }}
                  >
                    <span className={`wq-box ${q.multiSelect ? "sq" : ""}`} aria-hidden />
                    <span className="wq-l">
                      <b>{o.label}</b>
                      {o.description && <small>{o.description}</small>}
                    </span>
                  </button>
                );
              })}
            </div>
          )}
          <VoiceInput
            className="inp wq-other"
            placeholder={q.options.length ? "Other: type an answer" : "Type an answer"}
            value={other[q.question] || ""}
            disabled={d.busy}
            onChange={(v) => setOtherOf(qs.indexOf(q), v)}
            onKeyDown={(e) => {
              e.stopPropagation();
              if (e.key === "Enter") submit();
            }}
          />
        </div>
      ))}
      <div className="btns">
        {(!single || Object.values(other).some((v) => v.trim())) && (
          <button className="btn allow" data-k="a" disabled={d.busy || answered < qs.length} onClick={() => submit()}>
            Send answer{qs.length > 1 ? ` (${answered}/${qs.length})` : ""}
          </button>
        )}
        <button className="btn deny" data-k="d" disabled={d.busy} onClick={decline}>
          Decline <KeyHint k="d" show={isCur && keysOn} />
        </button>
      </div>
    </Card>
  );
}

function PlanAsk({ ask }: { ask: AskInfo }) {
  const { mock } = useOrch();
  const keysOn = useApproveKeys();
  const d = useDecide();
  const [keep, setKeep] = useState(false);
  const [msg, setMsg] = useState("");
  const approve = () => void d.run(() => decideAskBody(ask, { behavior: "allow", updatedInput: ask.approval?.input }, mock), "Plan approved");
  const keepPlanning = (m?: string) => void d.run(() => decideAskBody(ask, { behavior: "deny", message: (m ?? msg).trim() || "Keep planning. Do not start yet." }, mock), "Keep planning");
  const { ref, isCur } = useAskSlot(ask.approvalId, (k) => {
    if (d.flight.current) return;
    if (k === "a") {
      if (!ask.loaded) return showToast("Still loading", "Wait for the plan to load, then approve it.");
      if (!d.arm("a")) approve();
    } else keepPlanning("");
  });
  return (
    <Card ask={ask} label="Plan approval" isCur={isCur} slotRef={ref}>
      <div className="wplan">
        <Markdown text={ask.plan || ask.body} collapseAt={40} />
      </div>
      <Armed armed={d.armed === "a" ? "a" : null} what="approve the plan" />
      {keep && (
        <textarea
          className="inp wkeep"
          rows={2}
          autoFocus
          value={msg}
          placeholder="What should change in the plan? (optional)"
          onChange={(e) => setMsg(e.target.value)}
          onKeyDown={(e) => {
            e.stopPropagation();
            if (e.key === "Enter" && (e.metaKey || e.ctrlKey)) keepPlanning();
            if (e.key === "Escape") setKeep(false);
          }}
        />
      )}
      <div className="btns">
        <button className="btn allow" data-k="a" disabled={d.busy || !ask.loaded} onClick={approve}>
          Approve plan <KeyHint k="a a" show={isCur && keysOn} />
        </button>
        {keep ? (
          <button className="btn" data-k="d" disabled={d.busy} onClick={() => keepPlanning()}>
            Send and keep planning
          </button>
        ) : (
          <button className="btn" data-k="d" disabled={d.busy} onClick={() => setKeep(true)}>
            Keep planning <KeyHint k="d" show={isCur && keysOn} />
          </button>
        )}
      </div>
    </Card>
  );
}

function PermAsk({ ask }: { ask: AskInfo }) {
  const { mock } = useOrch();
  // Deny with a reason: the button, or a page key (Desk, Threads) that points DenyTarget at this card.
  const target = useContext(DenyTarget);
  const [denyOpen, setDenyOpen] = useState(false);
  const [why, setWhy] = useState("");
  useEffect(() => {
    if (target.id === ask.approvalId) setDenyOpen(true);
  }, [target.id, ask.approvalId]);
  const closeDeny = () => {
    setDenyOpen(false);
    setWhy("");
    if (target.id === ask.approvalId) target.clear();
  };
  const keysOn = useApproveKeys();
  const d = useDecide();
  const high = ask.risk === "high";
  const keys = useContext(AskKeysCtx);
  const go = (how: How, o?: PermOption, extra?: Partial<DecideBody>) => {
    // Without keys, the first click on a high-risk allow only arms the confirmation.
    if (!keys && high && how !== "deny" && d.arm("a")) return;
    void d.run(() => decideAsk(ask, how, mock, o, extra), o ? o.name : how === "deny" ? "Denied" : how === "always" ? "Always allowed" : "Allowed");
  };
  const denyWith = (stop: boolean) => {
    go("deny", undefined, { message: why.trim() || undefined, interrupt: stop || undefined });
    closeDeny();
  };
  const firstAllow = ask.options?.find((o) => howOf(o) === "allow");
  const firstDeny = ask.options?.find((o) => howOf(o) === "deny");
  const { ref, isCur } = useAskSlot(ask.approvalId, (k) => {
    if (d.flight.current) return;
    if (k === "d") return ask.options ? (firstDeny ? go("deny", firstDeny) : undefined) : go("deny");
    if (!ask.loaded) return showToast("Still loading", "Wait for the full request to load, then allow it.");
    if (ask.options && !firstAllow) return;
    if (high && d.arm("a")) return;
    go("allow", ask.options ? firstAllow : undefined);
  });
  const hint = (k: string) => <KeyHint k={high && k === "a" ? "a a" : k} show={isCur && keysOn} />;
  // A Claude request that carries permission_suggestions can also write the CLI's own rule.
  const claudeRule = ask.always && ask.agent === "claude" && !!ask.approval?.suggestions?.length;
  const offersAlways = ask.options ? ask.options.some((o) => o.kind === "allow_always") && !!ask.approval?.alwaysOk : ask.always;
  return (
    <Card ask={ask} label="Approval request" isCur={isCur} slotRef={ref}>
      <AskBody ask={ask} />
      <RiskLine ask={ask} />
      {offersAlways && ask.approval && (
        <div className="wrule">
          <b>Always</b> adds this rule: {alwaysRule(ask.approval)}.
          {claudeRule && (
            <>
              {" "}
              <b>Always (Claude rule)</b> also writes Claude's own rule: <span className="mono">{suggestionText(ask.approval.suggestions!)}</span>.
            </>
          )}
        </div>
      )}
      <Armed armed={d.armed === "a" ? "a" : null} what="allow this high-risk request" />
      <div className="btns">
        {ask.options ? (
          ask.options.map((o, i) => {
            const how = howOf(o);
            return (
              <button key={o.optionId} className={`btn ${how === "allow" ? "allow" : how === "deny" ? "deny" : ""}`} data-k={o === firstAllow ? "a" : o === firstDeny ? "d" : undefined} data-n={i + 1} disabled={d.busy || (how !== "deny" && !ask.loaded)} onClick={() => go(how, o)}>
                {o.name} {o === firstAllow && hint("a")}
                {o === firstDeny && hint("d")}
              </button>
            );
          })
        ) : (
          <>
            <button className="btn allow" data-k="a" disabled={d.busy || !ask.loaded} onClick={() => go("allow")}>
              {high ? "Allow once" : "Allow"} {hint("a")}
            </button>
            {ask.always && (
              <button className="btn" disabled={d.busy} onClick={() => go("always")}>
                Always
              </button>
            )}
            {claudeRule && (
              <button className="btn cr" disabled={d.busy} onClick={() => go("always", undefined, { applySuggestions: true })}>
                Always (Claude rule)
              </button>
            )}
            <button className="btn deny" data-k="d" disabled={d.busy} onClick={() => go("deny")}>
              Deny {hint("d")}
            </button>
            {!denyOpen && (
              <button className="btn ghost" disabled={d.busy} onClick={() => setDenyOpen(true)}>
                Deny with reason…
              </button>
            )}
          </>
        )}
      </div>
      {denyOpen && !ask.options && (
        <div className="wdeny">
          <textarea
            className="inp"
            rows={2}
            autoFocus
            value={why}
            placeholder="Tell the agent why (optional)"
            onChange={(e) => setWhy(e.target.value)}
            onKeyDown={(e) => {
              e.stopPropagation();
              if (e.key === "Escape") closeDeny();
              if (e.key === "Enter" && (e.metaKey || e.ctrlKey)) denyWith(false);
            }}
          />
          <div className="btns">
            <button className="btn deny" disabled={d.busy} onClick={() => denyWith(false)}>
              Deny
            </button>
            {ask.agent === "claude" && (
              <button className="btn deny" disabled={d.busy} onClick={() => denyWith(true)}>
                Deny and stop
              </button>
            )}
            <button className="btn ghost" onClick={closeDeny}>
              Cancel
            </button>
          </div>
        </div>
      )}
    </Card>
  );
}

export function WsApproval({ ask }: { ask: AskInfo }) {
  if (ask.kind === "question") return <QuestionAsk ask={ask} />;
  if (ask.kind === "plan") return <PlanAsk ask={ask} />;
  return <PermAsk ask={ask} />;
}

/** "Bash(rg:*)" from Claude's permission_suggestions. */
function suggestionText(sug: object[]): string {
  const out: string[] = [];
  for (const x of sug as { rules?: { toolName?: string; ruleContent?: string }[]; directories?: string[]; mode?: string }[]) {
    for (const r of x.rules || []) out.push(r.ruleContent ? `${r.toolName}(${r.ruleContent})` : r.toolName || "");
    for (const d of x.directories || []) out.push(`directory ${d}`);
    if (x.mode) out.push(`mode ${x.mode}`);
  }
  return out.filter(Boolean).join(", ") || "the CLI's suggestion";
}

/** Screen-reader announcement when a new approval appears in this view. */
export function AskAnnouncer({ asks }: { asks: AskInfo[] }) {
  const seen = useRef<Set<string> | null>(null);
  const [msg, setMsg] = useState("");
  useEffect(() => {
    const ids = new Set(asks.map((a) => a.approvalId));
    if (seen.current) {
      const fresh = asks.filter((a) => !seen.current!.has(a.approvalId));
      if (fresh.length) {
        const a = fresh[fresh.length - 1];
        setMsg(`${agentLabel(a.agent)} ${a.verb}${a.risk ? `, ${a.risk} risk` : ""}. ${asks.length} ${asks.length === 1 ? "request waits" : "requests wait"} for you.`);
      }
    }
    seen.current = ids;
  }, [asks]);
  return (
    <div className="sr-only" aria-live="polite" aria-atomic="true">
      {msg}
    </div>
  );
}
