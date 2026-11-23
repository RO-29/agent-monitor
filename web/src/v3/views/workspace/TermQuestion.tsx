// A question or plan that Claude Code shows in its terminal pane, answered from here by keys sent to that pane.
import { useEffect, useRef, useState } from "react";
import { showToast } from "../../../app/toast";
import { p7, type TermAnswer, type TermAnswerBody, type TermQuestion } from "../../api/p7";
import { Markdown } from "../../components/Markdown";
import { useDraftMap } from "../../../lib/draft";
import { VoiceInput } from "../../../lib/voice";
import "../../styles/workspace.css";

export interface TermQState {
  q: TermQuestion | null;
  /** Re-reads at once, for example after a send. */
  refresh: () => void;
}

/** Polls GET /api/session/{id}/question every 3 s while on, and while a sent answer waits for its result. */
export function useTermQuestion(sessionId: string, on: boolean, machine?: string): TermQState {
  const [q, setQ] = useState<TermQuestion | null>(null);
  const [tick, setTick] = useState(0);
  const waiting = !!(q?.pending && q.sentAt);
  const live = !!sessionId && (on || waiting);
  useEffect(() => {
    if (!live) {
      setQ(null);
      return;
    }
    let alive = true;
    const look = () =>
      p7.termQuestion(sessionId, machine).then(
        (r) => alive && setQ(r),
        () => alive && setQ(null),
      );
    void look();
    const t = window.setInterval(look, 3000);
    return () => {
      alive = false;
      window.clearInterval(t);
    };
  }, [sessionId, live, tick]);
  return { q: live ? q : null, refresh: () => setTick((n) => n + 1) };
}

/** True when the card has something to show; a held question shows as the approval card instead. */
export const termQuestionShown = (q: TermQuestion | null): q is TermQuestion => !!q?.pending && !q.held && (!!q.questions?.length || q.tool === "ExitPlanMode");

export function TermQuestionCard({ sessionId, st, onOpenPane, agent = "Claude Code", machine }: { sessionId: string; st: TermQState; onOpenPane?: () => void; agent?: string; machine?: string }) {
  const q = st.q;
  const [busy, setBusy] = useState(false);
  const [sentFor, setSentFor] = useState("");
  const flight = useRef(false);
  if (!termQuestionShown(q)) return null;
  const sent = !!q.sentAt || sentFor === q.toolUseId;
  const canSend = q.sendable && q.onScreen && !sent;

  const send = async (body: TermAnswerBody) => {
    if (flight.current || !canSend) return;
    flight.current = true;
    setBusy(true);
    try {
      await p7.answerTerm(sessionId, { ...body, toolUseId: q.toolUseId }, machine);
      setSentFor(q.toolUseId || "");
    } catch (e) {
      showToast("Not sent", (e as Error).message, { kind: "alert" });
    } finally {
      flight.current = false;
      setBusy(false);
      st.refresh();
    }
  };

  const plan = q.tool === "ExitPlanMode";
  return (
    <div className="wask wtq" role="group" aria-label={plan ? "Plan in the terminal" : "Question in the terminal"}>
      <div className="hd">
        <span className="st need">● Needs you</span>
        <span className="wag cl">{agent}</span>
        <span>{plan ? "has a plan ready" : "asks"}</span>
      </div>
      {plan ? <PlanBody q={q} busy={busy} canSend={canSend} send={send} /> : <QuestionsBody q={q} busy={busy} canSend={canSend} send={send} />}
      <Status q={q} sent={sent} onOpenPane={onOpenPane} />
    </div>
  );
}

function Status({ q, sent, onOpenPane }: { q: TermQuestion; sent: boolean; onOpenPane?: () => void }) {
  const msg = sent ? "Sent. This closes when the agent takes the answer." : !q.sendable || !q.onScreen ? `Answer it in the terminal: ${q.why || "agent-monitor cannot reach this session"}.` : "";
  if (!msg && !onOpenPane) return null;
  return (
    <div className="wtq-st" role="status">
      {msg && <span className={sent ? "ok-t" : "muted"}>{msg}</span>}
      <span className="sp" />
      {onOpenPane && (
        <button className="btn sm ghost" onClick={onOpenPane}>
          Open pane
        </button>
      )}
    </div>
  );
}

export type TermBody = { q: TermQuestion; busy: boolean; canSend: boolean; send: (b: TermAnswerBody) => Promise<void> };

export function QuestionsBody({ q, busy, canSend, send }: TermBody) {
  const qs = q.questions || [];
  const [picked, setPicked] = useState<Record<number, number[]>>({});
  const [otherOf, setOtherOf, clearOther] = useDraftMap(`apvt:${q.toolUseId || qs[0]?.question || ""}`);
  const other = Object.fromEntries(qs.map((_, i) => [i, otherOf(i)])) as Record<number, string>;
  useEffect(() => {
    setPicked({});
  }, [q.toolUseId]);
  const step = Math.min(q.step || 0, Math.max(qs.length - 1, 0));
  const rest = qs.length - step;
  const single = rest === 1 && !qs[qs.length - 1]?.multiSelect;
  const answerOf = (i: number, p = picked): TermAnswer | null => {
    const text = other[i]?.trim();
    if (qs[i].multiSelect) return p[i]?.length ? { options: p[i] } : null;
    if (text) return { text };
    return p[i]?.length ? { option: p[i][0] } : null;
  };
  // Only the questions from the terminal's current step on; earlier ones were answered there.
  const all = (p = picked) => qs.map((_, i) => answerOf(i, p)).slice(step);
  const answered = all().filter(Boolean).length;
  const submit = (p = picked) => {
    const a = all(p);
    if (!a.every(Boolean)) return;
    clearOther();
    void send({ answers: a as TermAnswer[] });
  };
  const off = busy || !canSend;
  return (
    <>
      {qs.map((x, i) => (
        <div key={i} className="wq">
          <div className="wq-t">
            {x.header && <span className="pill p-mute">{x.header}</span>}
            <span>{x.question}</span>
            {x.multiSelect && <span className="muted xs">pick any</span>}
          </div>
          {i < step ? (
            <div className="muted xs">Answered in the terminal.</div>
          ) : (
          <>
          <div className="wq-opts" role={x.multiSelect ? "group" : "radiogroup"}>
            {x.options.map((o, j) => {
              const on = (picked[i] || []).includes(j);
              return (
                <button
                  key={j}
                  className={`wq-o ${on ? "on" : ""}`}
                  role={x.multiSelect ? "checkbox" : "radio"}
                  aria-checked={on}
                  disabled={off}
                  onClick={() => {
                    const cur = picked[i] || [];
                    const next = x.multiSelect ? (on ? cur.filter((k) => k !== j) : [...cur, j].sort((a, b) => a - b)) : [j];
                    const p = { ...picked, [i]: next };
                    setPicked(p);
                    if (!x.multiSelect) setOtherOf(i, "");
                    if (single) submit(p);
                  }}
                >
                  <span className={`wq-box ${x.multiSelect ? "sq" : ""}`} aria-hidden />
                  <span className="wq-l">
                    <b>{o.label}</b>
                    {o.description && <small>{o.description}</small>}
                  </span>
                </button>
              );
            })}
          </div>
          {x.multiSelect ? (
            <div className="muted xs">To type your own answer to this question, use the terminal.</div>
          ) : (
            <VoiceInput
              className="inp wq-other"
              placeholder="Other: type an answer"
              value={other[i] || ""}
              disabled={off}
              onChange={(v) => {
                setOtherOf(i, v);
                if (v.trim()) setPicked({ ...picked, [i]: [] });
              }}
              onKeyDown={(e) => {
                e.stopPropagation();
                if (e.key === "Enter") submit();
              }}
            />
          )}
          </>
          )}
        </div>
      ))}
      {(!single || Object.values(other).some((v) => v.trim())) && (
        <div className="btns">
          <button className="btn allow" disabled={off || answered < rest} onClick={() => submit()}>
            {busy ? "Sending…" : `Send${rest > 1 ? ` (${answered}/${rest})` : ""}`}
          </button>
        </div>
      )}
    </>
  );
}

export function PlanBody({ q, busy, canSend, send }: TermBody) {
  const opts = q.planOptions || [];
  const fb = opts.find((o) => o.feedback);
  const [msg, setMsg] = useState("");
  const off = busy || !canSend;
  return (
    <>
      <div className="wplan">
        <Markdown text={q.plan || "(empty plan)"} collapseAt={40} />
      </div>
      {fb && (
        <textarea
          className="inp wkeep"
          rows={2}
          value={msg}
          disabled={off}
          placeholder="What should change in the plan?"
          onChange={(e) => setMsg(e.target.value)}
          onKeyDown={(e) => {
            e.stopPropagation();
            if (e.key === "Enter" && (e.metaKey || e.ctrlKey) && msg.trim()) void send({ planOption: fb.n, text: msg });
          }}
        />
      )}
      {opts.length > 0 && (
        <div className="btns">
          {opts.map((o) =>
            o.feedback ? (
              <button key={o.n} className="btn" disabled={off || !msg.trim()} onClick={() => void send({ planOption: o.n, text: msg })}>
                {o.label}
              </button>
            ) : (
              <button key={o.n} className={`btn ${o.n === 1 ? "allow" : ""}`} disabled={off} onClick={() => void send({ planOption: o.n })}>
                {o.label}
              </button>
            ),
          )}
        </div>
      )}
    </>
  );
}
