// Watched (terminal) sessions in the same conversation view as driven threads.
import { ScrollJump } from "./ScrollJump";
import { SessionLifeBtn } from "./SessionLife";
import { agentCommands } from "./agentCommands";
import { FillCtx } from "../../components/Markdown";
import { insertFor } from "../../../lib/insert";
import { DetailsBtn, ListShowBtn, TopBarItem, TopReveal } from "./barToggles";
import { clearSent, useDraft } from "../../../lib/draft";
import { Fragment, useEffect, useMemo, useRef, useState } from "react";
import { Link, useNavigate } from "react-router-dom";
import { api } from "../../../api/client";
import type { Session } from "../../../api/types";
import { showToast } from "../../../app/toast";
import { modelShort } from "../../../lib/format";
import { useLive } from "../../../lib/ws";
import { continueSession, sendMessage, useOrch } from "../../api/orch";
import { useShell } from "../../ctx";
import { agentName, approvalsByThread, useV3 } from "../../live";
import type { Row } from "../../model";
import { BarMenu, blocksOf, ChatAgentCtx, FoldItem, NarrowBarItems, RunBlock, useTick } from "./DrivenChat";
import { DiffView } from "./Diff";
import { filesOf, useDiffNav } from "./git";
import { agentTerm, cwdTerm, TermDock, toggleTerm, useTermOpen } from "./TermDock";
import type { WatchedFeedState } from "./watched";
import { nativeSid } from "./handover";
import { p7 } from "../../api/p7";
import { isMissing, v3 } from "../../api/client";
import { Composer, useAttachments } from "./Composer";
import { cliApi, queueApi, useQueue } from "../../api/queue";
import { QueueList, type QueueListHandle } from "./QueueList";
import { Boundary } from "../../components/Boundary";
import { askFromApproval, AskAnnouncer, WsApproval } from "./WsApproval";
import { HitCtx, useHitFocus, type HitReq } from "./hit";
import { firstRealLine } from "../../../lib/format";
import { TermQuestionCard, termQuestionShown, useTermQuestion } from "./TermQuestion";
import { ChildCtx, childCtxOf } from "./children";
import type { SessionChildren } from "../../api/p7";
import { ResumeChip, SelectBar, SelectCtx, useSelection } from "./MsgActions";
import { LinkedChip, LinkedPage, sessionLinks, type LinkedEntry } from "./linked";
import { NewThreadBtn } from "./NewThread";
import { useCmdHistory } from "./bash";
import { NewLine, PendingBubbles, useNewMark, usePendingSends } from "./sendMarks";
import { useDocTitle } from "../../../lib/title";
import { useUsageLimits } from "../../api/usage";
import { creditsLine } from "../../components/Usage";

export const latestSession = (row: Row) => row.thread?.sessions[row.thread.sessions.length - 1] || row.session?.id || "";

export interface PaneInfo {
  target?: string; // the id the send route resolves
  paneId?: string;
  ok: boolean;
}
/** GET /api/session/{id}/pane, re-read every 15 s; an older daemon falls back to the pane status route. */
export function usePane(sessionId: string, on: boolean): PaneInfo {
  const [p, setP] = useState<PaneInfo>({ ok: false });
  useEffect(() => {
    setP({ ok: false });
    if (!sessionId || !on) return;
    let alive = true;
    let old = false;
    const look = async () => {
      try {
        if (!old) {
          const w = await p7.watchedPane(sessionId);
          if (alive) setP({ target: w.agentId || sessionId, paneId: w.paneId, ok: w.sendable });
          return;
        }
        const st = await api.paneStatus(sessionId);
        if (alive) setP({ target: sessionId, paneId: st.paneId, ok: !!st.ok });
      } catch (e) {
        if (!old && isMissing(e)) {
          old = true;
          return look();
        }
        if (alive) setP({ ok: false });
      }
    };
    void look();
    const t = window.setInterval(look, 15_000);
    return () => {
      alive = false;
      window.clearInterval(t);
    };
  }, [sessionId, on]);
  return p;
}

/** A session the terminal no longer owns can be continued here. */
export function watchedLiveness(row: Row, connected: boolean) {
  const t = row.thread;
  const ss = row.session?.state;
  const live = ss === "running" || row.bucket === "running" || row.bucket === "needs-you";
  const owned = connected ? !!row.session && ss !== "completed" && ss !== "abandoned" : live || t?.state === "awaiting-input" || t?.state === "awaiting-permission";
  return { owned, running: ss === "running" || (!row.session && row.bucket === "running") };
}

const MAX_RUNS = 40;

interface TabInfo {
  id: string;
  startedAt: number;
  prompt: string;
  turns?: number;
}

/** Session tabs of a thread (after /clear or a resume), newest first. */
export function useSessionTabs(row: Row): TabInfo[] {
  const ids = row.thread?.sessions || [];
  const key = ids.join(",");
  const [meta, setMeta] = useState<{ sessions: Session[]; turns: Record<string, number> } | null>(null);
  useEffect(() => {
    setMeta(null);
    if (ids.length < 2) return;
    let alive = true;
    const fill = async () => {
      const r = await api.thread(row.id).catch(() => null);
      const turns = { ...(r?.turns || {}) };
      // Older daemons do not send turns: count the prompts in the search index instead.
      if (!r?.turns)
        await Promise.all(
          ids.slice(-8).map((id) =>
            v3.search({ q: `session:${id} kind:prompt`, limit: 1 }).then(
              (x) => (turns[id] = x.total),
              () => undefined,
            ),
          ),
        );
      if (alive) setMeta({ sessions: r?.sessions || [], turns });
    };
    void fill();
    return () => {
      alive = false;
    };
  }, [row.id, key]); // eslint-disable-line react-hooks/exhaustive-deps
  return useMemo(() => {
    if (ids.length < 2) return [];
    const by = new Map((meta?.sessions || []).map((s) => [s.id, s]));
    return [...ids].reverse().map((id) => {
      const s = by.get(id);
      return { id, startedAt: s?.startedAt || 0, prompt: firstRealLine(s?.firstMessage) || s?.title || "", turns: meta?.turns[id] };
    });
  }, [key, meta]); // eslint-disable-line react-hooks/exhaustive-deps
}

const fmtStart = (ms: number) => {
  if (!ms) return "";
  const d = new Date(ms);
  const t = d.toLocaleTimeString([], { hour: "numeric", minute: "2-digit" });
  return d.toDateString() === new Date().toDateString() ? t : `${d.toLocaleDateString([], { month: "short", day: "numeric" })} ${t}`;
};

function SessionTabs({ tabs, cur, onTab }: { tabs: TabInfo[]; cur: string; onTab: (id: string) => void }) {
  const ref = useRef<HTMLDivElement>(null);
  useEffect(() => {
    ref.current?.querySelector('[aria-selected="true"]')?.scrollIntoView({ block: "nearest", inline: "nearest" });
  }, [cur]);
  return (
    <div className="wtabs" role="tablist" aria-label="Sessions in this thread" ref={ref}>
      {tabs.map((t, i) => (
        <button key={t.id} role="tab" aria-selected={t.id === cur} className="wtab" onClick={() => onTab(t.id)} title={`${t.prompt || t.id}${i === 0 ? " (newest)" : ""}`}>
          <span className="wtab-t">{fmtStart(t.startedAt) || (i === 0 ? "Newest" : `Session ${tabs.length - i}`)}</span>
          <span className="wtab-p trunc">{t.prompt || t.id.split(":").pop()?.slice(0, 8)}</span>
          {t.turns != null && <span className="wtab-n">{t.turns} {t.turns === 1 ? "turn" : "turns"}</span>}
        </button>
      ))}
    </div>
  );
}

export function WatchedChat({ row, feed, onBack, onSide, tab, onTab, hit, kids, linkedPage }: { row: Row; feed: WatchedFeedState; onBack?: () => void; onSide?: () => void; tab?: string; onTab?: (id: string) => void; hit?: HitReq | null; kids?: SessionChildren | null; linkedPage?: boolean }) {
  const v = useV3();
  const lv = useLive();
  const orch = useOrch();
  const nav = useNavigate();
  const shell = useShell();
  const dnav = useDiffNav();
  const [statusOpen, setStatusOpen] = useState(false);
  // The header Diff shows what this session changed, even after a commit; Uncommitted stays in the Git box.
  const openSessionDiff = async () => {
    const start = row.thread?.startedAt || 0;
    let paths = filesOf(feed.feed?.items || []).map((x) => x.path);
    let all = !feed.loadEarlier;
    try {
      paths = (await p7.sessionFiles(latestSession(row))).paths;
      all = true;
    } catch {
      // older daemons have no /files route: keep the loaded turns
    }
    // No start or no edited file: a repo-wide range would show other people's commits.
    if (!start || !paths.length) return dnav.open({ from: "head", to: "worktree", label: "Uncommitted" });
    dnav.open({ from: `since:${start}:${latestSession(row)}`, to: "worktree", label: all ? "Files this session edited" : "Files edited in the loaded turns", paths });
  };
  const t = row.thread;
  const sid = latestSession(row);
  const session: Session | undefined = row.session || lv.sessions.get(sid);
  const live = watchedLiveness(row, lv.connected);
  const running = live.running;
  // A live registered pane owns the session even after an hour idle marks it completed.
  const pane = usePane(sid, !!sid);
  const owned = live.owned || pane.ok;
  const asks = useMemo(() => (approvalsByThread(v).get(row.id) || []).filter((a) => a.state === "pending").map(askFromApproval), [v, row.id]);
  const now = useTick(running);
  const [more, setMore] = useState(false);
  const f = feed.feed;
  const blocks = useMemo(() => (f ? blocksOf(f.items, f.runs) : []), [f]);
  const kidCtx = useMemo(() => childCtxOf(tab || latestSession(row), kids || null), [tab, row, kids]);
  // The items endpoint pages on the daemon; the legacy path holds the whole transcript and slices here.
  const shownBlocks = more || feed.source === "items" ? blocks : blocks.slice(-MAX_RUNS);
  const hidden = blocks.length - shownBlocks.length;
  const [atBottom, setAtBottom] = useState(true);
  const sent = usePendingSends(f?.items, sid);
  const newMark = useNewMark(sid, f?.items, atBottom);

  const scroller = useRef<HTMLDivElement>(null);
  const pinned = useRef(true);
  const lastText = f?.items.length ? f.items[f.items.length - 1].text?.length || 0 : 0;
  const size = `${f?.items.length || 0}:${asks.length}:${sent.list.length}:${lastText}`;
  useEffect(() => {
    const el = scroller.current;
    if (el && pinned.current) el.scrollTop = el.scrollHeight;
  }, [size, row.id, tab]);
  useEffect(() => {
    pinned.current = true;
    setMore(false);
  }, [row.id, tab]);

  const tabs = useSessionTabs(row);
  const curTab = tab || sid;
  const oldTab = curTab !== sid;
  const sel = useSelection(row.id + "|" + curTab);
  const hlId = useHitFocus({ hit: hit || null, items: f?.items || [], runs: f?.runs || [], scroller, pinned, loadEarlier: feed.loadEarlier, earlierBusy: feed.earlierBusy, onReveal: () => setMore(true) });

  const drivable = row.agent === "claude" || row.agent === "codex";
  const continued = useMemo(() => [...orch.threads.values()].find((x) => x.nativeId && nativeSid(x) === sid), [orch.threads, sid]);
  const mode: "tmux" | "continue" | "off" = owned ? (pane.ok && pane.target ? "tmux" : "off") : drivable && orch.status !== "missing" ? "continue" : "off";
  const offWhy = owned
    ? "Runs in a terminal agent-monitor cannot reach. Type in that terminal."
    : !drivable
      ? `${agentName(row.agent)} sessions cannot be continued here yet.`
      : "This daemon cannot continue sessions.";
  const termOpen = useTermOpen();
  const [text, setText] = useDraft("w:" + row.id);
  useDocTitle(row.title);
  const [busy, setBusy] = useState(false);
  const awaiting = session?.state === "awaiting-permission" || session?.state === "awaiting-input";
  const cliName = row.agent === "claude" ? "Claude Code" : row.agent === "codex" ? "Codex" : agentName(row.agent);
  const [cliAll, setCliAll] = useState(false);
  const q = useQueue(row.id, sid, owned, { cliAll, bump: session?.cliQueued || 0 });
  const usage = useUsageLimits(60_000);
  const credits = creditsLine(usage.data?.agents.find((a) => a.agent === row.agent));
  const qref = useRef<QueueListHandle>(null);
  // A reachable terminal gets the message at once, as if typed: a busy CLI queues it itself.
  const busyNow = running || session?.state === "awaiting-permission";
  const cliQueues = mode === "tmux" && busyNow;
  // Esc in the terminal stops the turn (or closes an open prompt), as in Claude Code and Codex.
  const pressEsc = async () => {
    try {
      await api.sendKeys(sid, ["Escape"]);
      showToast("Esc sent to the terminal");
    } catch (e) {
      showToast("Esc not sent", e instanceof Error ? e.message : undefined, { kind: "alert" });
    }
  };
  const amQueue = async (body: string) => {
    await queueApi.add(row.id, body, { session: pane.target || sid });
  };
  const att = useAttachments(row.id, "new");
  const bashOk = mode === "tmux" && (row.agent === "claude" || row.agent === "codex");
  const cmdHist = useCmdHistory(sid, bashOk);
  const send = async (again?: string) => {
    const body = again ? "!" + again : text.trim();
    const ids = again ? [] : att.ids;
    if (busy || mode === "off" || att.uploading || (mode === "tmux" && !body && !ids.length)) return;
    setBusy(true);
    try {
      if (mode === "tmux") {
        try {
          const r = await cliApi.sendFull(sid, body, ids);
          if (!again) sent.add(body, busyNow);
          if (r.panel) showToast(body.split(/\s/)[0], r.menu ? "A menu is open in the terminal. Answer it there." : undefined, { output: r.panel });
        } catch (e) {
          const st = (e as { status?: number }).status;
          // Files need the direct route: the fallbacks below type only text.
          if (ids.length) throw e;
          // An older daemon has no direct route; a prompt on screen (409) leaves it for agent-monitor's queue.
          if (isMissing(e)) {
            if (busyNow && q.status !== "missing") await amQueue(body);
            else await api.sendText(pane.target!, body, true);
          } else if (st === 409 && busyNow && q.status !== "missing") {
            await amQueue(body);
            showToast("Queued in agent-monitor", `${(e as Error).message}. It goes when the turn ends.`);
          } else throw e;
        }
        if (!again) {
          setText(clearSent(body));
          att.clear();
        }
        pinned.current = true;
      } else {
        const from = window.location.pathname;
        const d = await continueSession(row.id, ids.length ? undefined : body);
        if (!d) {
          if (!ids.length) setText(clearSent(body));
          showToast("Resumed in a terminal", ids.length ? "This session uses Code Mode, which runs only in a terminal. Send the message with its files there." : "This session uses Code Mode, which runs only in a terminal. Your message goes there.");
          return;
        }
        if (ids.length) await sendMessage(d.id, body, ids);
        setText(clearSent(body));
        att.clear();
        if (window.location.pathname === from) nav(`/w/${encodeURIComponent(d.id)}`); // not when the user went elsewhere meanwhile
      }
    } catch (e) {
      showToast(mode === "tmux" ? "Send failed" : "Could not continue here", (e as Error).message, { kind: "alert" });
    } finally {
      setBusy(false);
    }
  };

  const tq = useTermQuestion(sid, owned && row.agent === "claude" && !asks.length && awaiting);
  const tqShown = termQuestionShown(tq.q) && !oldTab;
  const waitingInTerm = !asks.length && session?.state === "awaiting-permission" && !tqShown;
  useEffect(() => {
    const el = scroller.current;
    if (el && tqShown && pinned.current) el.scrollTop = el.scrollHeight;
  }, [tqShown]);
  // A turn with no output for 2 min may be stuck: say how long it has been quiet.
  const quietMin = running && session?.lastActivityAt ? Math.floor((now - session.lastActivityAt) / 60_000) : 0;
  const status = asks.length || waitingInTerm || tqShown ? { cls: "need", l: "Needs you" } : running ? { cls: "run", l: quietMin >= 2 ? `Working · quiet ${quietMin} m` : "Working" } : null;
  const linked = useMemo<LinkedEntry[]>(() => {
    const out = sessionLinks(curTab, kids?.children || []);
    if (continued) out.unshift({ key: "cont:" + continued.id, group: "Threads", label: "thread", title: continued.title, sub: "continued in agent-monitor", href: `/w/${encodeURIComponent(continued.id)}` });
    return out;
  }, [kids, curTab, continued]);

  const fillBox = (raw: string, lang: string) => {
    setText((cur: string) => insertFor(raw, lang, bashOk, cur));
    requestAnimationFrame(() => document.querySelector<HTMLTextAreaElement>(".wchat textarea")?.focus());
  };
  return (
    <FillCtx.Provider value={fillBox}>
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
        {status && (quietMin >= 2 && session && owned ? (
          <button className={`wstat ${status.cls} wstat-btn`} onClick={() => shell.openPane(session)} title={`No output since ${new Date(session.lastActivityAt).toLocaleTimeString()}. Open the pane to look.`}>
            {status.l}
          </button>
        ) : (
          <span className={`wstat ${status.cls}`}>{status.l}</span>
        ))}
        <LinkedChip list={linked} page={`/w/${encodeURIComponent(row.id)}/linked`} />
        <span className="sp" />
        <div className="wbar-a">
          {row.cwd && (
            <button className="btn sm ghost wbar-diff" onClick={() => void openSessionDiff()} title="Changes to the files this session edited, since it started (committed or not)">
              Diff
            </button>
          )}
          <NewThreadBtn />
          <BarMenu>
            <TopBarItem />
            <NarrowBarItems status={status} onSide={onSide} />
            {row.cwd && (
              <button role="menuitem" className="wmenu-narrow" onClick={() => void openSessionDiff()}>
                Diff
              </button>
            )}
            {session && owned && (
              <button role="menuitem" onClick={() => shell.openPane(session)}>
                Open pane
              </button>
            )}
            <button role="menuitem" onClick={() => nav(`/t/${encodeURIComponent(row.id)}`)}>
              Trace
            </button>
            <button role="menuitem" onClick={toggleTerm} disabled={!row.cwd}>
              {termOpen ? "Hide terminal" : mode === "tmux" && pane.target ? "Agent terminal (live)" : "Terminal"}
              <kbd className="wkbd hidephone">ctrl+`</kbd>
            </button>
            <FoldItem />
            <button role="menuitem" onClick={sel.start} disabled={!blocks.length}>
              Select prompts to export
            </button>
          </BarMenu>
          {onSide && <DetailsBtn onSide={onSide} />}
        </div>
      </div>
      {tabs.length > 1 && onTab && <SessionTabs tabs={tabs} cur={curTab} onTab={onTab} />}
      {linkedPage ? (
        <LinkedPage list={linked} back={`/w/${encodeURIComponent(row.id)}`} />
      ) : (
      <>
      <ChildCtx.Provider value={kidCtx}>
      <SelectCtx.Provider value={sel}>
      <HitCtx.Provider value={hlId}>
      <ChatAgentCtx.Provider value={row.agent}>
      <div
        className={`wfeed ${sel.on ? "wselecting" : ""}`}
        ref={scroller}
        onScroll={(e) => {
          const el = e.currentTarget;
          pinned.current = el.scrollHeight - el.scrollTop - el.clientHeight < 60;
          if (pinned.current !== atBottom) setAtBottom(pinned.current);
        }}
      >
        {!f && !feed.error && (
          <div className="skel">
            <i style={{ width: "60%" }} />
            <i style={{ width: "85%" }} />
            <i style={{ width: "40%" }} />
          </div>
        )}
        {!f && feed.error === "waiting" && <div className="state">Waiting for the session to write its transcript…</div>}
        {!f && feed.error && feed.error !== "waiting" && <div className="werr">Could not read this session: {feed.error}</div>}
        {continued && !oldTab && (
          <div className="wnote-tab">
            This session was also continued in agent-monitor. <Link to={`/w/${encodeURIComponent(continued.id)}`}>Open that thread</Link>
          </div>
        )}
        {oldTab && <div className="wnote-tab">Earlier session of this thread. Messages you send go to the newest session.</div>}
        {f && blocks.length === 0 && <div className="state">No messages in this session yet.</div>}
        {hidden > 0 && (
          <button className="wearlier" onClick={() => setMore(true)}>
            Show {hidden} earlier {hidden === 1 ? "turn" : "turns"}
          </button>
        )}
        {feed.loadEarlier && (
          <button className="wearlier" disabled={feed.earlierBusy} onClick={feed.loadEarlier}>
            {feed.earlierBusy ? "Loading earlier turns…" : "Show earlier turns"}
          </button>
        )}
        {shownBlocks.map((b) => (
          <Fragment key={b.key}>
            {b.key === newMark && b !== shownBlocks[0] && <NewLine />}
            <RunBlock run={b.run} items={b.items} now={now} />
          </Fragment>
        ))}
        {!oldTab && <PendingBubbles list={sent.list} drop={sent.drop} />}
        {!oldTab && <AskAnnouncer asks={asks} />}
        {!oldTab && asks.map((a) => (
          <Boundary key={a.approvalId} what="This request">
            <WsApproval ask={a} />
          </Boundary>
        ))}
        {tqShown && (
          <Boundary what="This question">
            <TermQuestionCard sessionId={sid} st={tq} onOpenPane={session ? () => shell.openPane(session) : undefined} />
          </Boundary>
        )}
        {waitingInTerm && !oldTab && (
          <div className="wwait">
            <span className="wsd need" aria-hidden />
            <span>Waiting for an answer in the terminal.</span>
            {session && (
              <button className="btn sm ghost" onClick={() => shell.openPane(session)}>
                Open pane
              </button>
            )}
          </div>
        )}
      </div>
      <ScrollJump el={scroller} />
      </ChatAgentCtx.Provider>
      </HitCtx.Provider>
      </SelectCtx.Provider>
      </ChildCtx.Provider>
      <SelectBar sel={sel} blocks={blocks} title={row.title} />
      {q.cli?.draft && mode === "tmux" && (
        <div className="wdraft" role="status">
          <span className="wdraft-k">In the terminal box</span>
          <pre className="wdraft-t">{q.cli.draft}</pre>
          <div className="wdraft-a">
            <button
              className="btn sm ghost"
              title="Move this text into the message box here and clear it in the terminal"
              onClick={() => {
                const d = q.cli?.draft || "";
                void cliApi
                  .clearDraft(row.id, sid)
                  .then(() => setText((cur: string) => (cur.trim() ? `${cur}\n${d}` : d)))
                  .catch((e: Error) => showToast("Not moved", e.message, { kind: "alert" }));
              }}
            >
              Edit here
            </button>
            <button className="btn sm ghost" title="Delete this text in the terminal box" onClick={() => void cliApi.clearDraft(row.id, sid).catch((e: Error) => showToast("Not cleared", e.message, { kind: "alert" }))}>
              Clear
            </button>
          </div>
        </div>
      )}
      <QueueList
        ref={qref}
        threadId={row.id}
        q={q}
        sendLabel="Send now"
        onEdit={(body) => setText((cur) => (cur.trim() ? `${cur}\n${body}` : body))}
        cli={row.agent === "claude" && owned ? { session: sid, name: cliName, showAll: cliAll, onShowAll: setCliAll } : undefined}
      />
      {(!!q.statusLine?.length || !!credits) && (
        <pre className={`wstatus ${statusOpen ? "open" : ""}`} aria-label="Agent status line" title={statusOpen ? "Show one line" : "Show the full status line"} onClick={() => setStatusOpen((o) => !o)}>
          {[...(q.statusLine || []), ...(credits ? [credits] : [])].slice(0, statusOpen ? undefined : 1).join("\n")}
        </pre>
      )}
      <Composer
        value={text}
        onChange={setText}
        onSubmit={() => void send()}
        bash={bashOk}
        history={bashOk ? cmdHist : undefined}
        promptSession={sid}
        agent={row.agent}
        skills={bashOk}
        commands={bashOk ? agentCommands(row.agent).map(([key, label]) => ({ key, label, run: () => setText(`/${key} `) })) : undefined}
        onRunCmd={bashOk ? (cmd) => void send(cmd) : undefined}
        onEmptyUp={() => qref.current?.selectNewest() ?? false}
        disabled={mode === "off"}
        placeholder={cliQueues ? `${cliName} is working. ⌘↵ adds the message to its own queue. @ inserts a file` : mode === "tmux" ? (bashOk ? "Message the agent · ! command · / commands · @ file" : "Message the agent · @ file") : mode === "continue" ? "Continue this session here (message optional). ⌘↵" : offWhy}
        cwd={row.cwd}
        files
        attach={mode === "off" ? undefined : att}
      >
          <span className="wchip">
            <span className={`wag ${row.agent === "codex" ? "cx" : row.agent === "claude" ? "cl" : ""}`} />
            {agentName(row.agent)}
            {t?.model && !q.statusLine?.length ? ` · ${modelShort(t.model)}` : ""}
          </span>
          <ResumeChip tool={session?.tool || row.agent} sessionId={session?.sessionId || sid.slice(sid.indexOf(":") + 1)} cwd={session?.cwd || row.cwd} />
          <span className="sp" />
          {(row.agent === "claude" || row.agent === "codex") && (
            <SessionLifeBtn sid={sid} live={mode === "tmux"} owned={owned} prompt={text} files={att.ids} onResumed={() => (setText(clearSent(text)), att.clear())} />
          )}
          {mode === "continue" ? (
            <button className="btn sm pri" onClick={() => void send()} disabled={busy || att.uploading} title="agent-monitor resumes this session in its folder">
              {busy ? "Continuing…" : "Continue here"}
            </button>
          ) : (
            <>
            {mode === "tmux" && busyNow && (
              <button className="btn sm ghost" onClick={() => void pressEsc()} title={`Press Esc in the terminal: ${cliName} stops its turn`} aria-label="Stop with Esc">
                Esc
              </button>
            )}
            <button className="wsend" onClick={() => void send()} disabled={busy || mode === "off" || att.uploading || (!text.trim() && !att.ids.length)} aria-label={cliQueues ? "Queue" : "Send"} title={cliQueues ? `Add to the ${cliName} queue (⌘↵)` : "Send (⌘↵)"}>
              ↑
            </button>
            </>
          )}
      </Composer>
      </>
      )}
      <TermDock target={mode === "tmux" && pane.target ? agentTerm(sid, row.cwd) : cwdTerm(row.cwd)} />
      {dnav.cur && row.cwd && <DiffView threadId={`cwd:${row.cwd}`} nav={dnav.cur} onClose={dnav.close} onFile={dnav.setFile} />}
    </section>
    </FillCtx.Provider>
  );
}
