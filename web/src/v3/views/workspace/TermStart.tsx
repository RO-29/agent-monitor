// Start an agent in a tmux session, wait for its first hook to link the session, then open that watched thread.
import { useCallback, useEffect, useRef, useState } from "react";
import { useNavigate } from "react-router-dom";
import { useLive } from "../../../lib/ws";
import { agentLabel, answerHost, hostInfo, startTerminal, type OHostPrompt, type OTerminalBody, type OTerminalStart, type OWhere } from "../../api/orch";
import { useV3 } from "../../live";
import { copy } from "../../../lib/clipboard";

const GIVE_UP = 120_000;
const SEEN_WAIT = 8_000;

export interface TermStartState {
  phase: "posting" | "waiting" | "slow" | "linked" | "error" | "declined";
  agent: string;
  start?: OTerminalStart;
  registered?: boolean;
  agentId?: string;
  error?: string;
  prompt?: OHostPrompt;
  screenTail?: string[];
  answer?: (option: string) => Promise<void>;
}

/** The watched-thread route of a linked session: its thread row when it has one, else the session id. */
export const sessionHref = (agentId: string, threadOf: Record<string, string>) => `/w/${encodeURIComponent(threadOf[agentId] || agentId)}`;

/** POST /api/orch/terminal, then poll GET /api/hosts/{id} each second; onLinked replaces the default navigation. */
export function useTermStart(onLinked?: (agentId: string, href: string) => void) {
  const nav = useNavigate();
  const v = useV3();
  const live = useLive();
  const [st, setSt] = useState<TermStartState | null>(null);
  const gen = useRef(0);
  const ref = useRef({ threadOf: v.threadOf, sessions: live.sessions, onLinked, nav });
  ref.current = { threadOf: v.threadOf, sessions: live.sessions, onLinked, nav };
  useEffect(() => () => void gen.current++, []);

  const follow = useCallback((agent: string, t: OTerminalStart) => {
    const g = ++gen.current;
    let t0 = Date.now();
    // A cancelled trust prompt ends the wait; a failed post throws to the card.
    const answer = async (option: string) => {
      await answerHost(t.hostId, option);
      if (g !== gen.current) return;
      if (option === "cancel") {
        gen.current++;
        setSt((s) => s && { ...s, phase: "declined", prompt: undefined });
      } else setSt((s) => s && { ...s, prompt: undefined });
    };
    setSt({ phase: "waiting", agent, start: t, answer });
    const open = (agentId: string, since: number) => {
      if (g !== gen.current) return;
      const r = ref.current;
      // The session row arrives on the live socket; waiting for it avoids a short "not found" page.
      if (!r.sessions.has(agentId) && !r.threadOf[agentId] && Date.now() - since < SEEN_WAIT) return void setTimeout(() => open(agentId, since), 400);
      const href = sessionHref(agentId, r.threadOf);
      if (r.onLinked) r.onLinked(agentId, href);
      else r.nav(href);
    };
    const tick = async () => {
      if (g !== gen.current) return;
      let h = null;
      try {
        h = await hostInfo(t.hostId);
      } catch {
        /* a daemon blip: keep polling */
      }
      if (g !== gen.current) return;
      if (h?.agentId) {
        const agentId = h.agentId;
        setSt((s) => s && { ...s, phase: "linked", registered: true, agentId });
        return open(agentId, Date.now());
      }
      const registered = !!h;
      const prompt = h?.prompt;
      const screenTail = h?.screenTail;
      if (prompt) t0 = Date.now();
      if (Date.now() - t0 > GIVE_UP) return void setSt((s) => s && { ...s, phase: "slow", registered, prompt: undefined, screenTail: screenTail || s.screenTail });
      setSt((s) =>
        s && (s.registered !== registered || !samePrompt(s.prompt, prompt) || (screenTail && screenTail.join("\n") !== s.screenTail?.join("\n")))
          ? { ...s, registered, prompt, screenTail: screenTail || s.screenTail }
          : s,
      );
      setTimeout(tick, 1000);
    };
    setTimeout(tick, 500);
  }, []);

  const start = useCallback(
    async (b: OTerminalBody) => {
      gen.current++;
      setSt({ phase: "posting", agent: b.agent });
      try {
        follow(b.agent, await startTerminal(b));
        return true;
      } catch (e) {
        setSt({ phase: "error", agent: b.agent, error: (e as Error).message });
        return false;
      }
    },
    [follow],
  );
  const reset = useCallback(() => {
    gen.current++;
    setSt(null);
  }, []);
  return { st, start, follow, reset };
}

const samePrompt = (a?: OHostPrompt, b?: OHostPrompt) => JSON.stringify(a ?? null) === JSON.stringify(b ?? null);

/** The dialog text without the line that repeats the question, cut to two sentences. */
const shortExplanation = (text: string) => {
  const s = text
    .split(". ")
    .filter((p) => !/quick safety check/i.test(p))
    .slice(0, 2)
    .join(". ");
  return s.length > 220 ? s.slice(0, 217) + "..." : s;
};

/** The agent's folder trust question, answered from here instead of the terminal. */
function TrustCard({ st, prompt }: { st: TermStartState; prompt: OHostPrompt }) {
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState("");
  const folder = prompt.folder || "";
  const base = folder.split("/").filter(Boolean).pop() || folder || "this folder";
  // The agent's own wording for each choice, as its terminal dialog shows it.
  const labelOf = (id: string, def: string) => prompt.options?.find((o) => o.id === id)?.label?.trim() || def;
  const send = async (option: string) => {
    if (!st.answer) return;
    setBusy(true);
    setErr("");
    try {
      await st.answer(option);
    } catch (e) {
      setErr(`Could not answer: ${(e as Error).message}`);
    } finally {
      setBusy(false);
    }
  };
  if (prompt.kind !== "trust")
    return (
      <div className="wtrust" role="group" aria-label="Question before start">
        <b>
          {agentLabel(prompt.agent || st.agent)}: {prompt.title || "a question before it starts"}
        </b>
        {prompt.text && <span className="wtstart-n">{shortExplanation(prompt.text)}</span>}
        {err && <span className="wtrust-e">{err}</span>}
        <div className="wtrust-b" style={{ flexWrap: "wrap" }}>
          {prompt.options.map((o) => (
            <button key={o.id} className={`btn sm ${o.id === "deny" ? "ghost" : "pri"}`} disabled={busy} onClick={() => send(o.id)} title={o.label}>
              {o.label}
            </button>
          ))}
        </div>
      </div>
    );
  return (
    <div className="wtrust" role="group" aria-label="Folder trust">
      <b>
        {agentLabel(prompt.agent || st.agent)} asks whether you trust {base}
      </b>
      {folder && (
        <code className="mono trunc" title={folder}>
          {folder}
        </code>
      )}
      <span className="wtstart-n">{shortExplanation(prompt.text)}</span>
      {err && <span className="wtrust-e">{err}</span>}
      <div className="wtrust-b">
        <button className="btn sm ghost" disabled={busy} onClick={() => send("cancel")} title={labelOf("deny", "Cancel")}>
          {labelOf("deny", "Cancel")}
        </button>
        <button className="btn sm pri" disabled={busy} onClick={() => send("trust")} title={labelOf("trust", "Trust")}>
          {labelOf("trust", "Trust")}
        </button>
      </div>
    </div>
  );
}

export function AttachCmd({ cmd }: { cmd: string }) {
  const [done, setDone] = useState(false);
  return (
    <div className="wattach">
      <code className="mono trunc" title={cmd}>
        {cmd}
      </code>
      <button
        className="btn sm"
        onClick={async () => {
          if (await copy(cmd)) {
            setDone(true);
            setTimeout(() => setDone(false), 1500);
          }
        }}
        aria-label="Copy the attach command"
      >
        {done ? "Copied" : "Copy"}
      </button>
    </div>
  );
}

/** The starting card: progress, the attach command, and what to do when the session does not link. */
export function TermStarting({ st, onClose, compact }: { st: TermStartState; onClose?: () => void; compact?: boolean }) {
  const name = agentLabel(st.agent);
  const sess = st.start?.session;
  const head =
    st.phase === "posting"
      ? `Starting ${name} in tmux`
      : st.phase === "error"
        ? `Could not start ${name}`
        : st.phase === "linked"
          ? `${name} is running. Opening the session.`
          : st.phase === "slow"
            ? `${name} has not linked yet`
            : st.phase === "declined"
              ? `Cancelled. ${name} did not start in this folder.`
              : st.prompt
                ? `${name} is waiting for an answer`
                : `Starting ${name} in tmux session ${sess}`;
  const note =
    st.phase === "error"
      ? st.error
      : st.phase === "slow"
        ? `agent-monitor stopped waiting after 2 minutes. ${st.registered ? "The host is running but the agent sent no hook yet." : "The host did not register."} Attach to see the terminal.`
        : st.phase === "waiting" && !st.prompt
          ? st.registered
            ? `The host is up. Waiting for ${name} to link its session.`
            : "Waiting for the host to register."
          : st.phase === "posting"
            ? "Asking the daemon to open a tmux session."
            : st.phase === "declined"
              ? "The agent exited at the trust question. The tmux session closes with it."
              : "";
  return (
    <div className={`wtstart ${st.phase} ${compact ? "compact" : ""}`} role="status" aria-live="polite">
      <div className="wtstart-h">
        {(st.phase === "posting" || (st.phase === "waiting" && !st.prompt) || st.phase === "linked") && <span className="wspin" aria-hidden />}
        <b>{head}</b>
      </div>
      {note && <span className="wtstart-n">{note}</span>}
      {st.phase === "waiting" && st.prompt && <TrustCard st={st} prompt={st.prompt} />}
      {st.phase === "slow" && !!st.screenTail?.length && (
        <>
          <span className="wtstart-l">Terminal screen</span>
          <pre className="wtail mono">{st.screenTail.join("\n")}</pre>
        </>
      )}
      {st.start && (
        <>
          <span className="wtstart-l">Attach from any terminal on this Mac</span>
          <AttachCmd cmd={st.start.attach} />
        </>
      )}
      {onClose && (st.phase === "slow" || st.phase === "error" || st.phase === "declined") && (
        <div className="wtstart-f">
          <button className="btn sm ghost" onClick={onClose}>
            {st.phase === "error" ? "Back to the form" : "Close"}
          </button>
        </div>
      )}
    </div>
  );
}

export const TERM_AGENTS = new Set(["claude", "codex"]);

/** The Where choice; the terminal option is off for ACP agents and while fan-out rows exist. */
export function WherePick({ where, onChange, termOff, compact }: { where: OWhere; onChange: (w: OWhere) => void; termOff?: string; compact?: boolean }) {
  return (
    <div className={`wpick two wwhere ${compact ? "compact" : ""}`} role="radiogroup" aria-label="Where">
      <button className={`wopt ${termOff ? "off" : ""}`} role="radio" aria-checked={where === "terminal"} aria-pressed={where === "terminal"} disabled={!!termOff} title={termOff || undefined} onClick={() => onChange("terminal")}>
        <b>In a terminal</b>
        <span>{termOff || "A new tmux session. Attach any time, answer here or there."}</span>
      </button>
      <button className="wopt" role="radio" aria-checked={where === "headless"} aria-pressed={where === "headless"} onClick={() => onChange("headless")}>
        <b>Headless</b>
        <span>agent-monitor drives the agent. No terminal.</span>
      </button>
    </div>
  );
}
