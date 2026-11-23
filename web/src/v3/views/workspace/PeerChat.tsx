// A thread on another machine, in the same conversation view; the grant there decides what works.
import { useEffect, useMemo, useRef, useState } from "react";
import { showToast } from "../../../app/toast";
import { useDraft, clearSent } from "../../../lib/draft";
import { canAct, levelText, machineBase } from "../../api/fleet";
import { req } from "../../api/orch";
import { agentName } from "../../live";
import { Boundary } from "../../components/Boundary";
import { blocksOf, RunBlock, useTick } from "./DrivenChat";
import { Composer } from "./Composer";
import { ListShowBtn, TopReveal } from "./barToggles";
import { TermQuestionCard, termQuestionShown, useTermQuestion } from "./TermQuestion";
import { useWatchedFeed } from "./watched";
import type { WRow } from "./rows";

const enc = encodeURIComponent;

export function PeerChat({ row, onBack }: { row: WRow; onBack?: () => void }) {
  const t = row.peer!;
  const machine = row.machine!;
  const act = canAct(row.level);
  const sid = t.sessions?.[t.sessions.length - 1] || t.id;
  const running = t.state === "running";
  const feed = useWatchedFeed(sid, t.id, running, true, machine);
  const tq = useTermQuestion(sid, true, machine);
  const now = useTick(running);
  const f = feed.feed;
  const blocks = useMemo(() => (f ? blocksOf(f.items, f.runs) : []), [f]);
  const [text, setText] = useDraft("w:" + row.id);
  const [busy, setBusy] = useState(false);
  const scroller = useRef<HTMLDivElement>(null);
  const pinned = useRef(true);
  useEffect(() => {
    const el = scroller.current;
    if (el && pinned.current) el.scrollTop = el.scrollHeight;
  }, [f?.items.length]);

  const send = async () => {
    const body = text.trim();
    if (!body || busy || !act) return;
    setBusy(true);
    try {
      await req("POST", `${machineBase(machine)}/api/session/${enc(sid)}/cli-queue/send`, { text: body });
      setText(clearSent(text));
    } catch (e) {
      showToast(`Could not send to ${machine}`, (e as Error).message, { kind: "alert" });
    } finally {
      setBusy(false);
    }
  };

  return (
    <section className="wchat" aria-label="Thread">
      <TopReveal />
      <div className="wbar">
        {onBack && (
          <button className="btn sm ghost wback" onClick={onBack} aria-label="Back to threads">
            ‹
          </button>
        )}
        <ListShowBtn />
        <div className="wcrumb">
          <b className="trunc" title={row.title}>
            {row.title}
          </b>
        </div>
        <span className="wstat">{row.status}</span>
        <span className="sp" />
        <span className="wmach" title={`This thread runs on ${machine}. This machine has ${levelText(row.level)} access there.`}>
          {machine} · {levelText(row.level)}
        </span>
      </div>
      <div
        className="wfeed"
        ref={scroller}
        onScroll={(e) => {
          const el = e.currentTarget;
          pinned.current = el.scrollHeight - el.scrollTop - el.clientHeight < 60;
        }}
      >
        {!f && !feed.error && (
          <div className="skel">
            <i style={{ width: "60%" }} />
            <i style={{ width: "85%" }} />
          </div>
        )}
        {!f && feed.error && feed.error !== "waiting" && <div className="werr">Could not read this session on {machine}: {feed.error}</div>}
        {f && blocks.length === 0 && <div className="state">No messages in this session yet.</div>}
        {feed.loadEarlier && (
          <button className="wearlier" disabled={feed.earlierBusy} onClick={feed.loadEarlier}>
            {feed.earlierBusy ? "Loading earlier turns…" : "Show earlier turns"}
          </button>
        )}
        {blocks.map((b) => (
          <RunBlock key={b.key} run={b.run} items={b.items} now={now} />
        ))}
        {termQuestionShown(tq.q) &&
          (act ? (
            <Boundary what="This question">
              <TermQuestionCard sessionId={sid} st={tq} agent={agentName(row.agent)} machine={machine} />
            </Boundary>
          ) : (
            <div className="wwait">
              <span className="wsd need" aria-hidden />
              <span>Waiting for an answer on {machine}.</span>
            </div>
          ))}
      </div>
      {act ? (
        <Composer value={text} onChange={setText} onSubmit={() => void send()} agent={row.agent} placeholder={`Message the agent on ${machine}. ⌘↵ sends`} disabled={busy}>
          <span className="wchip">{agentName(row.agent)} on {machine}</span>
          <span className="sp" />
          <button className="wsend" onClick={() => void send()} disabled={busy || !text.trim()} aria-label="Send" title="Send (⌘↵)">
            ↑
          </button>
        </Composer>
      ) : (
        <div className="wviewonly">View only on {machine}. Ask its owner for Full access to answer or send.</div>
      )}
    </section>
  );
}
