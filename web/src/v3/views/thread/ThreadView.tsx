import { AskKeysCtx, askFromApproval, WsApproval } from "../workspace/WsApproval";
import { useEffect, useMemo, useRef, useState } from "react";
import { clearSent, useDraft } from "../../../lib/draft";
import { Link, useNavigate, useParams, useSearchParams } from "react-router-dom";
import { api } from "../../../api/client";
import type { Session, Span, SpanDetail, ThreadResponse, TraceFull } from "../../../api/types";
import { showToast } from "../../../app/toast";
import { fmtClock, fmtDate, fmtDur, fmtDuration, fmtTok, fmtUsd, modelShort, resumeCommand, shortCwd } from "../../../lib/format";
import { useLive } from "../../../lib/ws";
import { BOUNDARY, costWeight, outputHref, shortTool, stripAnsi, useTrace, type TraceState } from "../../../pages/trace/lib";
import { useApi, v3 } from "../../api/client";
import type { Approval, ChildRun, V3Thread } from "../../api/types";
import type { PermissionRequest } from "../../../api/types";
import { DiffView, diffLines, LegacyPermCard, PaneAnswerCard } from "../../components/ApprovalCard";
import { Markdown } from "../../components/Markdown";
import { AgentBadge, CopyBtn, Empty, ErrorState, famPill, Kbd, Loading, ResultBar, Ribbon, stateInfo } from "../../components/ui";
import { typing, useShell } from "../../ctx";
import { agentName, approvalsByThread, useV3 } from "../../live";
import { displayOf, firstMsgOf, isRecovered, outcomeOf, repoOf, shippedOf, stackLabel, stripMd, verbOf } from "../../model";
import { buildStory, cleanSpans, waterfall, type StoryGroup } from "./storyModel";

type Mode = "story" | "transcript" | "errors";
const PAGE = 10;

export default function ThreadPage() {
  const { threadId = "" } = useParams();
  return <ThreadView threadId={decodeURIComponent(threadId)} />;
}

const startedLabel = (ts: number) => (ts ? `started ${new Date(ts).toLocaleDateString(undefined, { weekday: "short" })} ${fmtClock(ts)}` : "");

/** Errors in a turn that no later call of the same tool fixed. */
export function openErrors(calls: Span[]): { open: Span[]; recovered: number } {
  const open: Span[] = [];
  let recovered = 0;
  calls.forEach((c, i) => {
    if (!c.err) return;
    if (isRecovered(c, calls.slice(i + 1))) recovered++;
    else open.push(c);
  });
  return { open, recovered };
}

export function ThreadView({ threadId, embedded }: { threadId: string; embedded?: boolean }) {
  const [sp, setSp] = useSearchParams();
  const nav = useNavigate();
  const live = useLive();
  const st = useV3();
  const shell = useShell();
  const th = useApi<ThreadResponse>(() => api.thread(threadId), [threadId], { poll: 30_000 });
  const children = useApi(() => v3.children(threadId), [threadId], { poll: 30_000 });
  const thread = th.data?.thread;
  const sessions = thread?.sessions || [];
  const wanted = embedded ? undefined : sp.get("session") || undefined;
  const sessionId = wanted && sessions.includes(wanted) ? wanted : sessions[sessions.length - 1] || "";
  const session: Session | undefined = live.sessions.get(sessionId) || th.data?.sessions.find((s) => s.id === sessionId);
  const mode: Mode = embedded ? "story" : ((sp.get("mode") as Mode) || "story");
  const pending = approvalsByThread(st).get(threadId) || [];
  const perms = [...live.perms.values()].filter((p) => p.sessionId && sessionId.endsWith(p.sessionId));
  const v3t = st.threads.find((x) => x.id === threadId);
  const merged: V3Thread | undefined = thread ? { ...thread, ...(v3t || {}) } : undefined;
  const repo = merged ? repoOf(merged) : "";

  useEffect(() => {
    if (thread) shell.setScope(repo);
    return () => shell.setScope(undefined);
  }, [thread?.cwd]); // eslint-disable-line react-hooks/exhaustive-deps

  // Esc goes back to the Workspace thread unless a field, dialog, menu or popover has it.
  useEffect(() => {
    if (embedded) return;
    const onKey = (e: KeyboardEvent) => {
      if (e.key !== "Escape" || e.defaultPrevented || e.metaKey || e.ctrlKey || e.altKey || typing(e)) return;
      if (document.querySelector('[role="dialog"],[aria-modal="true"],dialog[open],[role="menu"],.scrim,.palette')) return;
      nav(`/w/${encodeURIComponent(threadId)}`);
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [embedded, threadId, nav]);

  if (th.missing) return <Empty title="Thread not found">No thread with id <code>{threadId}</code>.</Empty>;
  if (th.error && !thread) return <ErrorState error={th.error} onRetry={th.reload} />;
  if (!thread || !merged) return <Loading rows={8} />;
  if (!sessionId) return <Empty title="No sessions in this thread" />;

  const info = stateInfo(session?.phase || thread.phase || session?.state || thread.state, pending.length);
  const setMode = (m: Mode) => {
    const n = new URLSearchParams(sp);
    if (m === "story") n.delete("mode");
    else n.set("mode", m);
    setSp(n, { replace: true });
  };
  const resume = session ? resumeCommand(session) : null;
  const sIdx = sessions.indexOf(sessionId);
  const title = displayOf(merged, firstMsgOf(merged, live));
  const kids = children.data?.children || [];
  const pillLabel = pending.length || info.rank <= 1 ? "waiting for you" : info.label;

  const head = (
    <div className="thead">
      <div className="row" style={{ flexWrap: "wrap" }}>
        <AgentBadge tool={thread.tools[0]} />
        <span className="muted small trunc" style={{ maxWidth: "70%" }}>
          {[repo, session?.branch, modelShort(thread.model), startedLabel(thread.startedAt), sessions.length > 1 ? `session ${sIdx + 1} of ${sessions.length}` : ""].filter(Boolean).join(" · ")}
        </span>
        <span className="ml-auto" />
        <span className={`pill ${info.pill}`}>
          <span className={`dot ${info.dot}`} />
          {pillLabel}
        </span>
      </div>
      <h1 className="ttl" title={thread.title}>
        {title}
      </h1>
      <div className="tacts">
        {!embedded && (
          <span className="seg">
            {(["story", "transcript", "errors"] as Mode[]).map((m) => (
              <button key={m} className={mode === m ? "on" : ""} onClick={() => setMode(m)}>
                {m === "story" ? "Story" : m === "transcript" ? "Transcript" : `Errors${thread.errors ? " " + thread.errors : ""}`}
              </button>
            ))}
          </span>
        )}
        {!embedded && sessions.length > 1 && (
          <select
            className="inp"
            style={{ padding: "3px 6px", fontSize: 12 }}
            value={sessionId}
            aria-label="Session"
            onChange={(e) => {
              const n = new URLSearchParams(sp);
              n.set("session", e.target.value);
              setSp(n, { replace: true });
            }}
          >
            {sessions.map((s, i) => (
              <option key={s} value={s}>
                session {i + 1}
              </option>
            ))}
          </select>
        )}
        {embedded && (
          <Link className="btn sm" to={`/t/${encodeURIComponent(threadId)}`}>
            Open ↵
          </Link>
        )}
        <button className="btn sm ghost hidephone" onClick={() => shell.openPalette({ scope: repo })}>
          ⌕ in repo
        </button>
        {session && (
          <button className="btn sm ghost" onClick={() => shell.openPane(session)}>
            Open pane
          </button>
        )}
        {resume && <CopyBtn text={resume} label="Resume ⧉" className="btn sm ghost" />}
      </div>
    </div>
  );

  return (
    <WithTrace key={sessionId} sessionId={sessionId}>
      {(tr) => (
        <div className={`tdesk ${embedded ? "emb" : ""}`}>
          <div className="tdoc-wrap">
            <SessionBody tr={tr} sessionId={sessionId} session={session} mode={mode} embedded={embedded} pending={pending} perms={perms} kids={kids} parent={children.data?.parent} head={head} thread={merged} />
            <Composer session={session} sessionId={sessionId} />
          </div>
          {!embedded && <SidePanel tr={tr} threadId={threadId} th={th.data!} thread={merged} kids={kids} kidsMissing={children.missing} onOpenChild={(t) => nav(`/t/${encodeURIComponent(t)}`)} />}
        </div>
      )}
    </WithTrace>
  );
}

function WithTrace({ sessionId, children }: { sessionId: string; children: (tr: TraceState) => React.ReactNode }) {
  const tr = useTrace(sessionId);
  return <>{children(tr)}</>;
}

function SessionBody({
  tr,
  sessionId,
  session,
  mode,
  embedded,
  pending,
  perms,
  kids,
  parent,
  head,
  thread,
}: {
  tr: TraceState;
  sessionId: string;
  session?: Session;
  mode: Mode;
  embedded?: boolean;
  pending: Approval[];
  perms: PermissionRequest[];
  kids: ChildRun[];
  parent?: { threadId: string; sessionId: string; spanId: string; ts: number };
  head: React.ReactNode;
  thread: V3Thread;
}) {
  const spans = useMemo(() => cleanSpans(tr.trace), [tr.trace]);
  const story = useMemo(() => buildStory(tr.trace, spans), [tr.trace, spans]);
  const shell = useShell();
  const st = useV3();

  const liveEnd = (
    <>
      {pending.map((a) => (
        <div key={a.id} className="turn">
          <AskKeysCtx.Provider value={false}><WsApproval ask={askFromApproval(a)} /></AskKeysCtx.Provider>
        </div>
      ))}
      {perms.map((p) => (
        <div key={p.id} className="turn">
          <LegacyPermCard p={p} />
        </div>
      ))}
      {pending.length === 0 && perms.length === 0 && session?.state === "awaiting-permission" && (
        <div className="turn">
          <PaneAnswerCard s={session} onOpenPane={() => shell.openPane(session)} />
        </div>
      )}
      {session?.state === "running" && (
        <div className="livew">
          <span className="dot d-run" />
          <span className="trunc">{/^[←→]/.test(session.lastMessage || "") ? "working" : (session.lastMessage || "working").slice(0, 160)}</span>
        </div>
      )}
    </>
  );

  const parentLine = parent && (
    <div className="kidl" style={{ paddingLeft: 0 }}>
      <span className="muted">Started by</span>
      <Link className="trunc" to={`/t/${encodeURIComponent(parent.threadId)}`}>
        {titleOfThread(st, parent.threadId)}
      </Link>
      <span className="muted small">· {fmtClock(parent.ts)}</span>
    </div>
  );

  if (mode !== "story")
    return (
      <>
        <div className="tdoc" style={{ paddingBottom: 12 }}>
          {head}
        </div>
        {tr.error && !tr.trace ? <ErrorState error={tr.error} onRetry={tr.reload} /> : !tr.trace ? <Loading rows={10} /> : <Transcript sessionId={sessionId} spans={spans} errorsOnly={mode === "errors"} />}
      </>
    );

  return (
    <Doc
      sessionId={sessionId}
      head={head}
      parentLine={parentLine}
      groups={story.groups}
      kids={kids}
      liveEnd={liveEnd}
      trace={tr.trace}
      error={tr.error}
      reload={tr.reload}
      thread={thread}
      embedded={embedded}
    />
  );
}

const titleOfThread = (st: ReturnType<typeof useV3>, id: string) => {
  const t = st.threads.find((x) => x.id === id);
  return t ? displayOf(t) : id.split(":").pop()?.slice(0, 8) || id;
};

const normKey = (t: string) => t.replace(/\s+/g, " ").trim().slice(0, 100);

/** Trace spans keep a short, flattened excerpt; the session detail has the full text with its line breaks. */
function useFullTexts(sessionId: string, version: number): Map<string, string> {
  const [m, setM] = useState<Map<string, string>>(new Map());
  useEffect(() => {
    if (!version) return;
    let alive = true;
    const t = window.setTimeout(() => {
      api
        .sessionDetail(sessionId)
        .then((d) => {
          if (!alive) return;
          const out = new Map<string, string>();
          for (const msg of d.messages || []) if (msg.text && msg.text.length > 60) out.set(normKey(msg.text), msg.text);
          setM(out);
        })
        .catch(() => undefined);
    }, 300);
    return () => {
      alive = false;
      window.clearTimeout(t);
    };
  }, [sessionId, version]);
  return m;
}

function Doc({
  sessionId,
  head,
  parentLine,
  groups,
  kids,
  liveEnd,
  trace,
  error,
  reload,
  thread,
  embedded,
}: {
  sessionId: string;
  head: React.ReactNode;
  parentLine: React.ReactNode;
  groups: StoryGroup[];
  kids: ChildRun[];
  liveEnd: React.ReactNode;
  trace: TraceFull | null;
  error: string | null;
  reload: () => void;
  thread: V3Thread;
  embedded?: boolean;
}) {
  const [sp] = useSearchParams();
  const [shown, setShown] = useState(PAGE);
  const ref = useRef<HTMLDivElement>(null);
  const jumpSpan = sp.get("span");
  const jumpTurn = sp.get("turn");
  const target = useMemo(() => {
    if (jumpSpan) {
      const i = groups.findIndex((g) => g.g.calls.some((c) => c.id === jumpSpan) || g.g.prompt?.id === jumpSpan || g.g.turns.some((t) => t.id === jumpSpan));
      if (i >= 0) return i;
    }
    if (jumpTurn) {
      const n = Number(jumpTurn);
      const i = groups.findIndex((g) => g.turnNo === n || g.g.turns.some((t) => t.name.includes(`turn ${n}`)));
      if (i >= 0) return i;
    }
    return -1;
  }, [groups, jumpSpan, jumpTurn]);
  useEffect(() => {
    if (target >= 0 && groups.length - target > shown) setShown(groups.length - target + 2);
  }, [target]); // eslint-disable-line react-hooks/exhaustive-deps
  const stuck = useRef(false);
  useEffect(() => {
    const el = ref.current;
    if (!el) return;
    if (target >= 0) requestAnimationFrame(() => el.querySelector(`[data-g="${target}"]`)?.scrollIntoView({ block: "start" }));
    else if (stuck.current) el.scrollTop = el.scrollHeight;
  }, [groups.length, target]);

  const first = Math.max(0, groups.length - shown);
  const vis = groups.slice(first);
  const kidBySpan = new Map(kids.map((k) => [k.spanId, k]));
  const kidBySession = new Map(kids.map((k) => [k.sessionId, k]));
  const fullTexts = useFullTexts(sessionId, groups.length);
  const full = (t?: string) => (t ? fullTexts.get(normKey(t)) || t : "");

  const facts = useMemo(() => {
    let open = 0;
    let recovered = 0;
    for (const g of groups) {
      const e = openErrors(g.g.calls);
      open += e.open.length;
      recovered += e.recovered;
    }
    return { open: thread.errorsOpen ?? open, recovered };
  }, [groups, thread.errorsOpen]);
  const shipped = shippedOf(thread, trace?.outputs);
  const lastReply = [...groups].reverse().find((g) => g.reply)?.reply?.text || "";
  const summary = outcomeOf(thread) || (lastReply ? stripMd(lastReply).slice(0, 220) : "");
  const n = (k: string) => shipped.filter((s) => s.kind === k).length;
  const cost = Math.max(thread.costUsd, trace?.costUsd || 0);
  const factList: [string, string][] = [
    [thread.turns > 0 ? thread.turns.toLocaleString() : "", thread.turns === 1 ? "turn" : "turns"],
    [cost > 0 ? fmtUsd(cost, thread.costUsd ? thread.costEstimated : !!trace?.costEstimated) : "", ""],
    [n("pr") ? String(n("pr")) : "", n("pr") === 1 ? "PR" : "PRs"],
    [n("commit") ? String(n("commit")) : "", n("commit") === 1 ? "commit" : "commits"],
    [kids.length ? String(kids.length) : "", kids.length === 1 ? "child run" : "child runs"],
    [thread.compacts ? String(thread.compacts) : "", thread.compacts === 1 ? "compaction" : "compactions"],
  ];

  return (
    <div
      className="tdoc-scroll"
      ref={ref}
      onScroll={(e) => {
        const el = e.currentTarget;
        stuck.current = el.scrollHeight - el.scrollTop - el.clientHeight < 80;
      }}
    >
      <div className="tdoc">
        {head}
        {parentLine}
        {(summary || factList.some(([v]) => v)) && (
          <div className="summary">
            <div className="cap">So far</div>
            {summary && <p>{summary}</p>}
            <div className="facts">
              {factList
                .filter(([v]) => v)
                .map(([v, l]) => (
                  <span key={l || v}>
                    <b>{v}</b> {l}
                  </span>
                ))}
              {facts.open > 0 && (
                <span className="err-t">
                  <b className="err-t">{facts.open}</b> {facts.open === 1 ? "error" : "errors"} not recovered
                </span>
              )}
            </div>
          </div>
        )}
        {error && !trace && <ErrorState error={error} onRetry={reload} />}
        {!trace && !error && <Loading rows={8} />}
        {trace && first > 0 && (
          <button className="btn sm" style={{ justifySelf: "center" }} onClick={() => setShown(shown + PAGE * 2)}>
            Show {Math.min(PAGE * 2, first)} earlier turns · {first} hidden
          </button>
        )}
        {trace && groups.length === 0 && <Empty title="No turns parsed yet">The transcript has no prompt or reply the trace parser recognises.</Empty>}
        {vis.map((sg, j) => (
          <Turn key={sg.g.id} i={first + j} sg={sg} full={full} sessionId={sessionId} target={first + j === target} jump={first + j === target ? jumpSpan : null} kidBySpan={kidBySpan} kidBySession={kidBySession} agent={trace?.tool || "claude"} />
        ))}
        {liveEnd}
        {embedded && null}
      </div>
    </div>
  );
}

function Turn({
  i,
  sg,
  sessionId,
  target,
  jump,
  kidBySpan,
  kidBySession,
  agent,
  full,
}: {
  i: number;
  sg: StoryGroup;
  full: (t?: string) => string;
  sessionId: string;
  target: boolean;
  jump: string | null;
  kidBySpan: Map<string, ChildRun>;
  kidBySession: Map<string, ChildRun>;
  agent: string;
}) {
  const [wide, setWide] = useState(false);
  const b = sg.seg.boundary;
  const errs = useMemo(() => openErrors(sg.g.calls), [sg.g.calls]);
  const prompt = full(sg.g.prompt?.text).trim();
  const sys = prompt.startsWith("<");
  const kids = sg.children.map((c) => ({ c, k: kidBySpan.get(c.id) || (c.child ? kidBySession.get(c.child) : undefined) }));
  if (!prompt && !sg.reply && !sg.g.calls.length) return null;
  return (
    <>
      {sg.first && b.kind !== "start" && (
        <div className="divider">
          {BOUNDARY[b.kind]?.label || b.kind}
          {b.kind === "compact" && b.droppedTokens ? ` · dropped ${fmtTok(b.droppedTokens)} tokens` : ""} · {fmtDate(b.at)}
        </div>
      )}
      <div className={`turn ${target ? "flash" : ""}`} data-g={i}>
        {prompt &&
          (sys ? (
            <p className="you muted small" style={{ fontWeight: 400 }}>
              System note · {prompt.replace(/<[^>]*>/g, " ").replace(/\s+/g, " ").trim().slice(0, 140)}
            </p>
          ) : (
            <p className={`you ${wide ? "" : "clamp"}`} onClick={() => setWide(!wide)} title={wide ? "" : "Show the whole prompt"}>
              {prompt.slice(0, wide ? 8000 : 1200)}
              <span className="ts">{fmtClock(sg.g.prompt!.ts)}</span>
            </p>
          ))}
        {sg.g.calls.length > 0 && <WorkLine sessionId={sessionId} sg={sg} index={i} kids={kidBySpan} jump={jump} recovered={errs.recovered} open={errs.open.length} />}
        {errs.open.slice(0, 3).map((c) => (
          <ErrorInline key={c.id} sessionId={sessionId} span={c} />
        ))}
        {errs.open.length > 3 && <div className="small err-t">+ {errs.open.length - 3} more errors not recovered (the Errors tab lists all)</div>}
        <KidLines kids={kids} />
        {sg.reply && (
          <div className="ans">
            <Markdown text={full(sg.reply.text).slice(0, 20000)} />
            <div className="who" style={{ marginTop: 4 }}>
              {agentName(agent)} · {fmtClock(sg.reply.ts + sg.reply.dur)}
            </div>
          </div>
        )}
      </div>
    </>
  );
}

function WorkLine({ sessionId, sg, index, kids, jump, recovered, open: nOpen }: { sessionId: string; sg: StoryGroup; index: number; kids: Map<string, ChildRun>; jump: string | null; recovered: number; open: number }) {
  const [open, setOpen] = useState(!!jump);
  const g = sg.g;
  const rows = open ? waterfall(g.calls, 60) : [];
  const t0 = g.calls[0]?.ts || g.fromTs;
  const span = Math.max(g.toTs - t0, 1);
  return (
    <div className="workbox">
      <div className="workl" onClick={() => setOpen(!open)} role="button" aria-expanded={open} title={open ? "Hide the calls" : "Show the calls"}>
        <span>Worked {fmtDuration(g.dur)}</span>
        <Ribbon calls={g.calls} />
        <span>
          {g.calls.length} call{g.calls.length === 1 ? "" : "s"}
          {recovered > 0 && ` · ${recovered} error${recovered > 1 ? "s" : ""}, recovered`}
          {nOpen > 0 && (
            <>
              {" · "}
              <span className="err-t">
                {nOpen} error{nOpen > 1 ? "s" : ""}
              </span>
            </>
          )}
        </span>
        <Link to={`/traces/${encodeURIComponent(sessionId)}/${index}`} onClick={(e) => e.stopPropagation()}>
          open trace
        </Link>
      </div>
      {open && (
        <div className="wf">
          {rows.map((r) => (
            <WfLine key={r.first.id} r={r} t0={t0} span={span} kid={kids.get(r.first.id)} />
          ))}
          {rows.length >= 60 && <div className="xs muted" style={{ gridColumn: "1/4" }}>The first 60 rows. The Transcript tab lists every call.</div>}
        </div>
      )}
    </div>
  );
}

function KidLines({ kids }: { kids: { c: Span; k?: ChildRun }[] }) {
  const [all, setAll] = useState(false);
  if (!kids.length) return null;
  const line = ({ c, k }: { c: Span; k?: ChildRun }) => {
    const to = k ? `/t/${encodeURIComponent(k.threadId)}` : c.child ? `/t/${encodeURIComponent(c.child)}` : null;
    const s = k ? stateInfo(k.state) : null;
    const label = k?.title || c.res || c.name;
    return (
      <div key={c.id} className="kidl">
        <AgentBadge tool={k?.kind === "delegated" ? "codex" : "subagent"} small />
        {to ? (
          <Link className="trunc" to={to}>
            {label}
          </Link>
        ) : (
          <span className="trunc">{label}</span>
        )}
        {k?.findings ? <span className="pill p-ok">{k.findings} findings</span> : null}
        {k && k.errors > 0 && <span className="pill p-err">{k.errors} errors</span>}
        {k && s && s.rank <= 2 && <span className={`pill ${s.pill}`}>{k.attention ? "needs you" : s.label}</span>}
        <span className="muted small" style={{ flex: "none" }}>
          {fmtDur(c.dur)}
        </span>
      </div>
    );
  };
  if (kids.length <= 3 || all) return <>{kids.map(line)}</>;
  const done = kids.filter((x) => x.k && x.k.state !== "running" && !x.k.attention).length;
  return (
    <div className="kidl">
      <AgentBadge tool={kids[0].k?.kind === "delegated" ? "codex" : "subagent"} small />
      <span>
        {kids.length} child runs{done ? ` · ${done} done` : ""}
      </span>
      <button className="btn sm ghost" onClick={() => setAll(true)}>
        Show all
      </button>
    </div>
  );
}

function ErrorInline({ sessionId, span }: { sessionId: string; span: Span }) {
  const [want, setWant] = useState(false);
  const { d, err } = useSpanDetail(sessionId, span.id, want);
  return (
    <div className="errin">
      <div className="eh">
        <b>Error, not recovered</b>
        <span className="small">
          {shortTool(span.name)} · {fmtClock(span.ts)}
        </span>
        {!want && (
          <button className="btn sm ghost ml-auto" onClick={() => setWant(true)}>
            Show output
          </button>
        )}
      </div>
      <code>
        {span.fam === "bash" ? "$ " : ""}
        {(span.res || span.text || "").slice(0, 400)}
      </code>
      {want && !d && !err && <div className="xs muted">loading output…</div>}
      {err && <div className="xs err-t">output unavailable: {err}</div>}
      {d && <OutputPre text={d.result} max={1600} />}
    </div>
  );
}

function SidePanel({ tr, threadId, th, thread, kids, kidsMissing, onOpenChild }: { tr: TraceState; threadId: string; th: ThreadResponse; thread: V3Thread; kids: ChildRun[]; kidsMissing: boolean; onOpenChild: (t: string) => void }) {
  const t = th.thread;
  const spans = useMemo(() => cleanSpans(tr.trace), [tr.trace]);
  const hot = useMemo(() => {
    const m = new Map<string, number>();
    for (const s of spans) if (s.fam === "edit" && s.res) m.set(s.res, (m.get(s.res) || 0) + 1);
    return [...m.entries()].sort((a, b) => b[1] - a[1]).slice(0, 5);
  }, [spans]);
  const shipped = shippedOf(thread, tr.trace?.outputs);
  const bare = shipped.filter((o) => o.kind === "commit" && !o.label).length;
  const curve = useMemo(() => {
    const turns = spans.filter((s) => s.kind === "turn" && s.ts > 0 && s.tokens).sort((a, b) => a.ts - b.ts);
    if (turns.length < 2) return null;
    const from = turns[0].ts;
    const to = turns[turns.length - 1].ts;
    const N = 24;
    const b = new Array(N).fill(0);
    for (const s of turns) b[Math.min(N - 1, Math.floor(((s.ts - from) / Math.max(to - from, 1)) * N))] += costWeight(s.model || t.model || "", s.tokens);
    let acc = 0;
    const cum = b.map((v) => (acc += v));
    const max = cum[N - 1] || 1;
    return { pts: cum.map((v, i) => `${((i / (N - 1)) * 240).toFixed(1)},${(46 - (v / max) * 40).toFixed(1)}`).join(" "), from, to };
  }, [spans, t.model]);
  const groups = useMemo(() => {
    const m = new Map<string, ChildRun[]>();
    for (const k of kids) {
      const key = `${k.kind}|${verbOf(k.title || "")}`;
      m.set(key, [...(m.get(key) || []), k]);
    }
    return [...m.entries()].sort((a, b) => b[1].length - a[1].length);
  }, [kids]);
  const cost = Math.max(t.costUsd, tr.trace?.costUsd || 0);
  return (
    <aside className="tside">
      <div>
        <div className="cap">Outcome</div>
        <ul>
          {shipped.length === 0 && <li className="muted">Nothing shipped yet.</li>}
          {shipped.filter((o) => o.label || o.kind !== "commit").map((o, i) => {
            const href = o.ref && /^https?:/.test(o.ref) ? o.ref : o.kind === "page" ? undefined : outputHref({ kind: o.kind, ref: o.ref || "" });
            const label = o.label || (o.kind === "commit" ? "commit, message not parsed" : o.kind);
            return (
              <li key={i}>
                <span className={`pill ${o.kind === "pr" || o.kind === "merge" ? "p-ok" : "p-line"}`}>{o.kind === "pr" ? "PR" : o.kind}</span>
                {href ? (
                  <a className="ell" href={href} target="_blank" rel="noreferrer" title={label}>
                    {label}
                  </a>
                ) : (
                  <span className={`ell ${o.label ? "" : "muted"}`} title={label}>
                    {o.ref && o.kind === "commit" ? `${o.ref.slice(0, 7)} ` : ""}
                    {label}
                  </span>
                )}
              </li>
            );
          })}
          {bare > 0 && (
            <li>
              <span className="pill p-line">commit</span>
              <span className="ell muted">
                {bare} commit{bare > 1 ? "s" : ""}, message not parsed
              </span>
            </li>
          )}
        </ul>
      </div>
      {hot.length > 0 && (
        <div>
          <div className="cap">Hot files</div>
          <ul className="mono">
            {hot.map(([f, n], i) => (
              <li key={f} title={f}>
                <span className="ell">
                  {shortCwd(f).split("/").slice(-2).join("/")} · {n}
                  {i === 0 ? ` edit${n === 1 ? "" : "s"}` : ""}
                </span>
              </li>
            ))}
          </ul>
        </div>
      )}
      {(kids.length > 0 || kidsMissing) && (
        <div>
          <div className="cap">Children</div>
          <ul>
            {kidsMissing && <li className="muted small">Child links need /api/v3/thread/:id/children.</li>}
            {groups.map(([key, ks]) => {
              if (ks.length < 2)
                return ks.map((k) => (
                  <li key={k.threadId + k.spanId}>
                    <AgentBadge tool={k.kind === "delegated" ? "codex" : "subagent"} small />
                    <a className="ell" href={`/t/${encodeURIComponent(k.threadId)}`} onClick={(e) => (e.preventDefault(), onOpenChild(k.threadId))}>
                      {k.title}
                    </a>
                    {k.errors > 0 ? <span className="pill p-err">{k.errors} err</span> : k.findings ? <span className="pill p-ok">{k.findings} findings</span> : k.attention ? <span className="pill p-need">needs you</span> : null}
                  </li>
                ));
              const [kind, verb] = key.split("|");
              const f = ks.filter((k) => (k.findings || 0) > 0).length;
              const e = ks.filter((k) => k.errors > 0 && !k.findings).length;
              const x = ks.filter((k) => k.state === "running" || k.attention).length;
              return (
                <li key={key} style={{ display: "grid", gap: 2 }}>
                  <span className="row">
                    <AgentBadge tool={kind === "delegated" ? "codex" : "subagent"} small />
                    <a className="ell" href={`/t/${encodeURIComponent(ks[0].threadId)}`} onClick={(ev) => (ev.preventDefault(), onOpenChild(ks[0].threadId))}>
                      {stackLabel(ks.length, kind === "delegated" ? "codex" : "claude", verb as ReturnType<typeof verbOf>, kind)}
                    </a>
                    <span className="muted small" style={{ flex: "none" }}>
                      {f ? `${f} with findings` : `${ks.length - x} done`}
                    </span>
                  </span>
                  <ResultBar f={f} e={e} x={x} ok={Math.max(0, ks.length - f - e - x)} />
                </li>
              );
            })}
          </ul>
        </div>
      )}
      {curve && (
        <div>
          <div className="row">
            <span className="cap grow1">Cost over time</span>
            {cost > 0 && <span className="muted small">{fmtUsd(cost, t.costEstimated)}</span>}
          </div>
          <svg viewBox="0 0 240 50" width="100%" height="50" role="img" aria-label="cumulative cost over the session" style={{ display: "block", marginTop: 6 }}>
            <polyline fill="none" stroke="var(--accent)" strokeWidth="2" strokeLinejoin="round" points={curve.pts} />
          </svg>
          <div className="row muted small">
            <span>{fmtClock(curve.from)}</span>
            <span className="ml-auto">{fmtClock(curve.to)}</span>
          </div>
        </div>
      )}
      <div className="xs muted" style={{ marginTop: "auto" }}>
        {fmtTok(t.tokens.input + t.tokens.output + t.tokens.cacheRead + t.tokens.cacheCreate)} tokens · {t.turns.toLocaleString()} turns
        <div className="mono" style={{ marginTop: 4, wordBreak: "break-all" }}>
          {threadId}
        </div>
      </div>
    </aside>
  );
}

function WfLine({ r, t0, span, kid }: { r: ReturnType<typeof waterfall>[number]; t0: number; span: number; kid?: ChildRun }) {
  const left = ((r.from - t0) / span) * 100;
  const width = Math.max(((r.to - r.from) / span) * 100, 0.4);
  const color = r.err ? "var(--err)" : r.child || r.first.kind === "agent" ? "var(--sub)" : `var(--fam-${r.fam})`;
  return (
    <>
      <div className={`n ${r.depth > 1 ? "ind" : ""} ${r.err ? "e" : ""}`} title={r.first.res || r.name} style={r.child || r.first.kind === "agent" ? { color: "var(--sub)" } : undefined}>
        {shortTool(r.name)}
        {r.count > 1 ? ` × ${r.count}` : ""}
        {kid ? ` · ${kid.title}` : ""}
      </div>
      <div className="bar">
        <i style={{ left: `${Math.min(left, 99.5)}%`, width: `${Math.min(width, 100 - left)}%`, background: color }} />
      </div>
      <div className="d">{fmtDur(r.dur)}</div>
    </>
  );
}

function useSpanDetail(sessionId: string, spanId: string, enabled: boolean) {
  const [d, setD] = useState<SpanDetail | null>(null);
  const [err, setErr] = useState("");
  useEffect(() => {
    if (!enabled || d) return;
    let alive = true;
    api
      .span(sessionId, spanId)
      .then((x) => alive && setD(x))
      .catch((e) => alive && setErr((e as Error).message));
    return () => {
      alive = false;
    };
  }, [enabled, sessionId, spanId]); // eslint-disable-line react-hooks/exhaustive-deps
  return { d, err };
}

const failLine = (s: string) => /(✕|✗|FAIL|Error|error:|panic|Traceback|exit code|failed)/i.test(s);
function OutputPre({ text, max = 4000 }: { text: string; max?: number }) {
  const t = stripAnsi(text || "");
  const clipped = t.length > max ? t.slice(0, max) : t;
  return (
    <pre className="out" style={{ marginTop: 8 }}>
      {clipped.split("\n").map((ln, i) => (
        <span key={i} style={failLine(ln) ? { color: "var(--err)" } : undefined}>
          {ln}
          {"\n"}
        </span>
      ))}
      {t.length > max && <span className="muted">… output {(t.length / 1000).toFixed(1)} KB, first {max / 1000} KB shown</span>}
    </pre>
  );
}


function Transcript({ sessionId, spans, errorsOnly }: { sessionId: string; spans: Span[]; errorsOnly: boolean }) {
  const [filter, setFilter] = useState<string>(errorsOnly ? "errors" : "all");
  const [shown, setShown] = useState(200);
  const [focus, setFocus] = useState<string | null>(null);
  const [openSet, setOpenSet] = useState<Set<string>>(new Set());
  const ref = useRef<HTMLDivElement>(null);
  useEffect(() => setFilter(errorsOnly ? "errors" : "all"), [errorsOnly]);
  const atEnd = useRef(true);
  useEffect(() => {
    if (atEnd.current && ref.current) ref.current.scrollTop = ref.current.scrollHeight;
  }, [spans.length, filter]);

  const items = useMemo(() => spans.filter((s) => s.kind === "user" || s.kind === "tool" || s.kind === "agent").sort((a, b) => a.ts - b.ts), [spans]);
  const calls = items.filter((s) => s.kind !== "user");
  const byTool = useMemo(() => {
    const m: Record<string, number> = {};
    for (const c of calls) m[shortTool(c.name)] = (m[shortTool(c.name)] || 0) + 1;
    return Object.entries(m).sort((a, b) => b[1] - a[1]).slice(0, 6);
  }, [calls]);
  const nErr = calls.filter((c) => c.err).length;
  const nKid = calls.filter((c) => c.child || c.kind === "agent").length;
  const keep = (s: Span) => {
    if (filter === "all") return true;
    if (s.kind === "user") return false;
    if (filter === "errors") return !!s.err;
    if (filter === "children") return !!s.child || s.kind === "agent";
    return shortTool(s.name) === filter;
  };
  const list = items.filter(keep);
  const first = Math.max(0, list.length - shown);
  const vis = list.slice(first);

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (typing(e) || e.metaKey || e.ctrlKey) return;
      if (e.key === "e") {
        const errs = list.filter((s) => s.err);
        if (!errs.length) return;
        const cur = focus ? list.findIndex((s) => s.id === focus) : -1;
        const next = errs.find((s) => list.indexOf(s) > cur) || errs[0];
        const idx = list.indexOf(next);
        if (idx < first) setShown(list.length - idx + 10);
        setFocus(next.id);
        requestAnimationFrame(() => ref.current?.querySelector(`[data-id="${CSS.escape(next.id)}"]`)?.scrollIntoView({ block: "center" }));
      } else if (e.key === "o" && focus) {
        setOpenSet((o) => {
          const n = new Set(o);
          if (n.has(focus)) n.delete(focus);
          else n.add(focus);
          return n;
        });
      }
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [list, focus, first]);

  return (
    <div
      className="stream"
      ref={ref}
      style={{ gap: 6 }}
      onScroll={(e) => {
        const el = e.currentTarget;
        atEnd.current = el.scrollHeight - el.scrollTop - el.clientHeight < 80;
      }}
    >
      <div className="filters">
        <span>Filter</span>
        <button className={`chip ${filter === "all" ? "on" : "t"}`} onClick={() => setFilter("all")}>
          all calls {calls.length}
        </button>
        {byTool.map(([t, n]) => (
          <button key={t} className={`chip ${filter === t ? "on" : "t"}`} onClick={() => setFilter(t)}>
            {t} {n}
          </button>
        ))}
        <button className={`chip ${filter === "errors" ? "on" : "er"}`} onClick={() => setFilter("errors")}>
          errors {nErr}
        </button>
        {nKid > 0 && (
          <button className={`chip ${filter === "children" ? "on" : "t"}`} onClick={() => setFilter("children")}>
            children {nKid}
          </button>
        )}
        <span className="ml-auto">
          <Kbd>e</Kbd> next error · <Kbd>o</Kbd> output
        </span>
      </div>
      {first > 0 && (
        <button className="btn sm" style={{ alignSelf: "center" }} onClick={() => setShown(shown + 200)}>
          Show earlier · {first} hidden
        </button>
      )}
      {list.length === 0 && <Empty title={filter === "errors" ? "No failed calls in this session" : "No calls match this filter"} />}
      {vis.map((s) =>
        s.kind === "user" ? (
          <div key={s.id} className="card you" data-id={s.id}>
            <div className="ch">You · {new Date(s.ts).toTimeString().slice(0, 8)}</div>
            <div className="body">{(s.text || "").slice(0, 1200)}</div>
          </div>
        ) : (
          <CallRow
            key={s.id}
            sessionId={sessionId}
            s={s}
            focused={focus === s.id}
            open={openSet.has(s.id) || (errorsOnly && !!s.err)}
            onToggle={() => {
              setFocus(s.id);
              setOpenSet((o) => {
                const n = new Set(o);
                if (n.has(s.id)) n.delete(s.id);
                else n.add(s.id);
                return n;
              });
            }}
          />
        ),
      )}
    </div>
  );
}


function CallRow({ sessionId, s, open, focused, onToggle }: { sessionId: string; s: Span; open: boolean; focused: boolean; onToggle: () => void }) {
  const { d, err } = useSpanDetail(sessionId, s.id, open);
  let input: Record<string, unknown> | null = null;
  if (d?.args) {
    try {
      input = JSON.parse(d.args);
    } catch {
      input = null;
    }
  }
  const diff = input ? diffLines(s.name, input) : null;
  return (
    <div className={`card tcall ${s.err ? "errb" : ""} ${focused ? "flash" : ""}`} data-id={s.id}>
      <div className="row clk" onClick={onToggle}>
        <span className={`pill ${famPill[s.fam] || "p-mute"}`}>{shortTool(s.name)}</span>
        <code className="grow1">{s.res || s.text || ""}</code>
        {s.err && <span className="pill p-err">failed</span>}
        {(s.child || s.kind === "agent") && <span className="pill p-sub">child</span>}
        <span className="muted xs tab-num" style={{ flex: "none" }}>
          {fmtClock(s.ts)} · {fmtDur(s.dur)}
        </span>
      </div>
      {open && !d && !err && <div className="xs muted" style={{ marginTop: 6 }}>loading call…</div>}
      {open && err && <div className="xs err-t" style={{ marginTop: 6 }}>call detail unavailable: {err}</div>}
      {open && d && (
        <>
          {diff ? (
            <div style={{ marginTop: 8 }}>
              <DiffView lines={diff} />
            </div>
          ) : (
            d.args && (
              <pre className="out" style={{ marginTop: 8, color: "var(--fg-2)" }}>
                {d.args.length > 3000 ? d.args.slice(0, 3000) + "…" : d.args}
              </pre>
            )
          )}
          {d.result && <OutputPre text={d.result} />}
          {s.child && (
            <div className="childnote">
              started child run · <Link to={`/t/${encodeURIComponent(s.child)}`}>open child</Link>
            </div>
          )}
        </>
      )}
    </div>
  );
}

function Composer({ session, sessionId }: { session?: Session; sessionId: string }) {
  const [text, setText] = useDraft("w:" + sessionId);
  const [busy, setBusy] = useState(false);
  const shell = useShell();
  const send = async () => {
    const t = text.trim();
    if (!t) return;
    setBusy(true);
    try {
      await api.sendText(sessionId, t, true);
      setText(clearSent(t));
      showToast("Sent", "Typed into the agent's pane.");
    } catch (e) {
      showToast("Send failed", `${(e as Error).message}. Open the pane to register it.`, { kind: "alert", onClick: session ? () => shell.openPane(session) : undefined });
    } finally {
      setBusy(false);
    }
  };
  return (
    <div className="composer">
      <textarea
        rows={1}
        value={text}
        placeholder="Message this agent…"
        title="Types the message into the agent's tmux pane and presses Enter"
        onChange={(e) => setText(e.target.value)}
        onKeyDown={(e) => {
          if (e.key === "Enter" && !e.shiftKey) {
            e.preventDefault();
            void send();
          }
        }}
        aria-label="Message"
      />
      {session?.state === "running" && (
        <button
          className="btn"
          onClick={() =>
            api
              .cancel(sessionId)
              .then(() => showToast("Interrupt sent"))
              .catch((e) => showToast("Interrupt failed", (e as Error).message, { kind: "alert" }))
          }
        >
          Interrupt
        </button>
      )}
      <button className="btn pri" disabled={busy || !text.trim()} onClick={send}>
        Send
      </button>
    </div>
  );
}
