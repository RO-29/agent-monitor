import { PeerNeedsBox, usePeerFeed } from "./peers";
import { AskKeysCtx, askFromApproval, WsApproval } from "./workspace/WsApproval";
import { useEffect, useMemo, useState } from "react";
import { Link, useNavigate } from "react-router-dom";
import { api } from "../../api/client";
import { showToast } from "../../app/toast";
import { fmtAgo, fmtUsd } from "../../lib/format";
import { useLive } from "../../lib/ws";
import { shortTool } from "../../pages/trace/lib";
import { useApi, v3 } from "../api/client";
import type { Approval, SpanRow, Stack, V3Thread } from "../api/types";
import { DenyTarget, LegacyPermCard, PaneAnswerCard, PromptCard, needsSecond, quickDecide } from "../components/ApprovalCard";
import { AgentBadge, DrivenTag, Empty, ErrorState, Loading, ResultBar, SparkBars } from "../components/ui";
import { drivenIdOf, threadHref } from "../driven";
import { sendMessage, spendBlockOf } from "../api/orch";
import { typing, useShell } from "../ctx";
import { approvalKeyOk } from "../keys";
import { agentName, useV3 } from "../live";
import { needThreadIds, refreshNeedsYou, useNeedsYou, type NeedsYouItem } from "../api/needsYou";
import { TermQuestionCard } from "./workspace/TermQuestion";
import {
  activity,
  bucketize,
  buildRows,
  displayOf,
  firstMsgOf,
  groupStacks,
  hasFindings,
  listRows,
  outcomeOf,
  repoOf,
  shippedOf,
  shippedSummary,
  stacksOf,
  stripMd,
  startOfToday,
  verbOf,
  type Row,
} from "../model";

const ASK_CAP = 8;
const WAIT_CARDS = 3;
const DIGEST_CAP = 12;

/** "Claude wants to run a command." */
function askLine(a: Approval): string {
  const who = agentName(a.agent);
  if (a.kind === "plan") return `${who} has a plan ready.`;
  if (a.kind === "question") return "";
  if (a.toolName === "Bash" || a.toolName === "exec_command" || a.toolName === "shell") return `${who} wants to run a command.`;
  if (/^(Edit|MultiEdit|Write|apply_patch|NotebookEdit)$/.test(a.toolName)) {
    const fp = String(a.input.file_path || a.input.path || "").split("/").pop();
    return `${who} wants to ${a.toolName === "Write" ? "write" : "edit"} ${fp || "a file"}.`;
  }
  return `${who} wants to use ${shortTool(a.toolName)}.`;
}

const waitAgo = (ts: number) => fmtAgo(ts, "").replace(/(\d+)([smhd])/, (_m, n, u) => `${n} ${{ s: "s", m: "min", h: "h", d: "d" }[u as "s"]}`);

type Unit =
  | { kind: "one"; t: V3Thread; at: number; repo: string }
  | { kind: "stack"; s: Stack; at: number; repo: string; latest?: V3Thread };

export default function Now() {
  const peerFeed = usePeerFeed();
  const st = useV3();
  const live = useLive();
  const nav = useNavigate();
  const ny = useNeedsYou();
  const rows = useMemo(() => buildRows(st, live, ny), [st.version, live.version, ny.version]); // eslint-disable-line react-hooks/exhaustive-deps
  const now = useApi(() => v3.now(), [], { poll: 30_000 });
  const all = useApi(() => v3.threads(true), [], { poll: 60_000 });
  const spans = useApi(() => v3.spans({ groupBy: "none", since: "1h", limit: 1000 }), [], { poll: 60_000 });
  const fm = (t: V3Thread) => firstMsgOf(t, live);

  const asks = listRows(rows, "needs-you");
  // Sessions that finished their turn: ready for your next message, never counted as needing you.
  const waits = rows.filter((r) => !r.needs && r.session?.state === "awaiting-input").sort((a, b) => b.lastAt - a.lastAt);
  const running = listRows(rows, "running");
  const today = startOfToday();
  const finished = rows.filter((r) => r.thread && !r.needs && r.bucket !== "running" && r.lastAt >= today && !waits.includes(r));

  const allThreads = all.data?.threads || [];
  const byId = useMemo(() => new Map(allThreads.map((t) => [t.id, t])), [allThreads]);
  const stacks = useMemo(() => stacksOf(allThreads, all.data?.stacks, fm), [allThreads, all.data?.stacks]); // eslint-disable-line react-hooks/exhaustive-deps
  const stacksByParent = useMemo(() => {
    const m = new Map<string, Stack[]>();
    for (const s of stacks) if (s.parent) m.set(s.parent, [...(m.get(s.parent) || []), s]);
    return m;
  }, [stacks]);
  const spansBySession = useMemo(() => {
    const m = new Map<string, SpanRow[]>();
    for (const s of spans.data?.spans || []) m.set(s.session, [...(m.get(s.session) || []), s]);
    return m;
  }, [spans.data]);

  const sparkOf = (t?: V3Thread) => {
    if (!t) return null;
    if (t.activity) return { calls: t.activity, errs: t.activityErr || [] };
    if (!spans.data) return null;
    return bucketize(t.sessions.flatMap((sid) => spansBySession.get(sid) || []));
  };

  // Finished today: one row per thread, review-like siblings in one repo fold into a stack row.
  const units = useMemo((): Unit[] => {
    const tops = finished.map((r) => r.thread!);
    const grouped = groupStacks(tops, (t) => {
      const v = verbOf(displayOf(t, fm(t)));
      return v === "other" ? null : `top|${repoOf(t)}|${t.tools?.[0]}|${v}`;
    }, fm);
    const inGroup = new Set(grouped.flatMap((g) => g.members));
    const out: Unit[] = grouped.map((s) => ({ kind: "stack", s, at: s.lastAt, repo: s.repo, latest: tops.find((t) => t.id === s.members[0]) }));
    for (const t of tops) if (!inGroup.has(t.id)) out.push({ kind: "one", t, at: t.lastAt, repo: repoOf(t) });
    const busy = new Set([...asks, ...running].map((r) => r.id));
    for (const s of stacks)
      if (s.lastAt >= today && !busy.has(s.parent) && s.running + s.waiting === 0) out.push({ kind: "stack", s, at: s.lastAt, repo: s.repo, latest: byId.get(s.members[0]) });
    const repoAt = new Map<string, number>();
    for (const u of out) repoAt.set(u.repo, Math.max(repoAt.get(u.repo) || 0, u.at));
    return out.sort((a, b) => (repoAt.get(b.repo)! - repoAt.get(a.repo)!) || a.repo.localeCompare(b.repo) || b.at - a.at);
  }, [finished.map((r) => r.id + r.lastAt).join(), stacks, byId]); // eslint-disable-line react-hooks/exhaustive-deps

  const [showAll, setShowAll] = useState(false);
  const [sel, setSel] = useState(0);
  const [denyId, setDenyId] = useState<string | null>(null);
  const queue = asks.flatMap((r) => r.approvals.map((a) => ({ r, a })));
  const cur = queue[Math.min(sel, queue.length - 1)];

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (typing(e) || e.metaKey || e.ctrlKey || e.altKey || !queue.length) return;
      if (document.querySelector(".scrim, .drawer")) return;
      if (e.key === "j") setSel((i) => Math.min(i + 1, queue.length - 1));
      else if (e.key === "k") setSel((i) => Math.max(i - 1, 0));
      else if ((e.key === "a" || e.key === "A") && cur) {
        if (!approvalKeyOk(e)) return;
        e.preventDefault();
        if (needsSecond(cur.a, e.key)) return;
        void quickDecide(cur.a, e.key === "A" ? "always" : "allow");
      } else if (e.key === "d" && cur) {
        if (!approvalKeyOk(e)) return;
        e.preventDefault();
        setDenyId(cur.a.id);
      } else if (e.key === "Enter" && cur) nav(threadHref(cur.r.id, cur.r.thread));
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [queue, cur, nav]);

  const loading = st.threadsStatus === "loading" && rows.length === 0;
  const spend = now.data ? now.data.spendToday : finished.concat(running, asks, waits).reduce((a, r) => a + (r.thread && r.thread.lastAt >= today ? r.thread.costUsd : 0), 0);
  const meta = [
    running.length ? `${running.length} running` : "",
    finished.length ? `${finished.length} finished today` : "",
    spend > 0 ? `${now.data ? "" : "≈"}${fmtUsd(spend)} ${now.data ? "spent today" : "in today's threads"}` : "",
  ].filter(Boolean);
  const nNeed = ny.status === "loading" ? asks.length : ny.count;
  const shownUnits = showAll ? units : units.slice(0, DIGEST_CAP);

  return (
    <DenyTarget.Provider value={{ id: denyId, clear: () => setDenyId(null) }}>
      <div className="desk-page">
        <div className="desk-col">
          <div className="hello">
            <h2>{loading ? "Loading…" : nNeed ? `${nNeed} need${nNeed > 1 ? "" : "s"} you` : "Nothing needs you"}</h2>
            {meta.length > 0 && <span className="muted">{meta.join(" · ")}</span>}
          </div>
          <PeerNeedsBox feed={peerFeed} />
          {loading && <Loading rows={6} />}
          {st.threadsStatus === "error" && rows.length === 0 && <ErrorState error={st.threadsError} />}

          {asks.slice(0, ASK_CAP).map((r) => (
            <AskCard key={r.id} r={r} selId={cur?.a.id} title={r.thread ? displayOf(r.thread, fm(r.thread)) : r.title} items={ny.items.filter((i) => needThreadIds([i], st.threadOf, st.drivenRowOf).has(r.id))} />
          ))}
          {asks.length > ASK_CAP && (
            <Link className="btn more" to="/threads">
              {asks.length - ASK_CAP} more requests in Threads
            </Link>
          )}
          {waits.slice(0, WAIT_CARDS).map((r) => (
            <AskCard key={r.id} r={r} title={r.thread ? displayOf(r.thread, fm(r.thread)) : r.title} />
          ))}
          {waits.length > WAIT_CARDS && (
            <div className="sec">
              <div className="cap">Also ready · {waits.length - WAIT_CARDS}</div>
              <div className="flight">
                {waits.slice(WAIT_CARDS).map((r) => (
                  <WaitRow key={r.id} r={r} title={r.thread ? displayOf(r.thread, fm(r.thread)) : r.title} />
                ))}
              </div>
            </div>
          )}

          {!loading && (
            <div className="sec">
              <div className="cap">In flight{running.length > 3 ? ` · ${running.length}` : ""}</div>
              {running.length === 0 ? (
                <div className="quiet">No agent is running right now.</div>
              ) : (
                <div className="flight">
                  {running.map((r) => (
                    <FlightRow key={r.id} r={r} title={r.thread ? displayOf(r.thread, fm(r.thread)) : r.title} stacks={stacksByParent.get(r.id)} spark={sparkOf(r.thread)} />
                  ))}
                </div>
              )}
            </div>
          )}

          {!loading && (
            <div className="sec">
              <div className="row">
                <span className="cap grow1">Finished today</span>
                {units.length > 1 && <span className="muted small">grouped by repo</span>}
              </div>
              {units.length === 0 ? (
                <div className="quiet">Nothing finished yet today.</div>
              ) : (
                <div className="digest">
                  {shownUnits.map((u) => (u.kind === "one" ? <DigestOne key={u.t.id} t={u.t} title={displayOf(u.t, fm(u.t))} /> : <DigestStack key={u.s.id} u={u} byId={byId} />))}
                </div>
              )}
              {units.length > DIGEST_CAP && (
                <button className="btn sm more" onClick={() => setShowAll(!showAll)}>
                  {showAll ? "Show fewer" : `Show all ${units.length}`}
                </button>
              )}
            </div>
          )}
          {!loading && !nNeed && !running.length && !units.length && st.threadsStatus === "ok" && <Empty title="A quiet day">No agent waits, runs or finished today. Search with ⌘K to open an older thread.</Empty>}
        </div>
      </div>
    </DenyTarget.Provider>
  );
}

function AskCard({ r, selId, title, items = [] }: { r: Row; selId?: string; title: string; items?: NeedsYouItem[] }) {
  const shell = useShell();
  const repo = r.thread ? repoOf(r.thread) : repoOf({ cwd: r.cwd });
  const head = (
    <div className="row askh">
      <AgentBadge tool={r.agent} />
      <Link className="askt" to={threadHref(r.id, r.thread)} title={title}>
        {title}
      </Link>
      {drivenIdOf(r.thread) && <DrivenTag />}
      <span className="muted small" style={{ flex: "none" }}>
        {repo} · {r.needs ? `waiting ${waitAgo(r.sinceAt)}` : `ready ${waitAgo(r.lastAt)}`}
      </span>
    </div>
  );
  if (r.approvals.length)
    return (
      <>
        {r.approvals.map((a) => (
          <div key={a.id} className={`ask ${selId === a.id ? "sel" : ""}`}>
            {head}
            {askLine(a) && <p className="q">{askLine(a)}</p>}
            <AskKeysCtx.Provider value={false}><WsApproval ask={askFromApproval(a)} /></AskKeysCtx.Provider>
          </div>
        ))}
      </>
    );
  if (r.perms.length)
    return (
      <div className="ask">
        {head}
        {r.perms.map((p) => (
          <LegacyPermCard key={p.id} p={p} />
        ))}
      </div>
    );
  const q = items.find((i) => i.kind === "question");
  if (q?.question && q.sessionId)
    return (
      <div className="ask">
        {head}
        <TermQuestionCard sessionId={q.sessionId} st={{ q: q.question, refresh: () => void refreshNeedsYou() }} agent={agentName(q.agent || r.agent)} />
      </div>
    );
  const p = items.find((i) => i.kind === "prompt");
  if (p)
    return (
      <div className="ask">
        {head}
        <PromptCard item={p} onOpenPane={r.session ? () => shell.openPane(r.session!) : undefined} />
      </div>
    );
  if (r.session?.state === "awaiting-permission")
    return (
      <div className="ask">
        {head}
        <PaneAnswerCard s={r.session} onOpenPane={() => shell.openPane(r.session!)} />
      </div>
    );
  return <ReplyCard r={r} head={head} />;
}

/** An agent that finished its turn and waits for a reply: its last words and a reply box. */
function ReplyCard({ r, head }: { r: Row; head: React.ReactNode }) {
  const shell = useShell();
  const [text, setText] = useState("");
  const [busy, setBusy] = useState(false);
  const lm = r.session?.lastMessage || "";
  const generic = !lm || /^[←→]/.test(lm) || /is waiting for your (input|reply)|needs your (attention|permission)/i.test(lm);
  const last = (r.thread ? outcomeOf(r.thread) : "") || (generic ? "" : stripMd(lm));
  const sid = r.session?.id || r.thread?.sessions[r.thread.sessions.length - 1] || "";
  const did = drivenIdOf(r.thread);
  const send = async () => {
    const t = text.trim();
    if (!t || (!sid && !did)) return;
    setBusy(true);
    try {
      if (did) {
        await sendMessage(did, t);
        setText("");
        showToast("Sent", "The driven thread got your message.");
        return;
      }
      await api.sendText(sid, t, true);
      setText("");
      showToast("Sent", "Typed into the agent's pane.");
    } catch (e) {
      const b = spendBlockOf(e);
      if (b) {
        showToast("Spend limit reached", `${b.error}. Open the thread to allow one more turn.`, { kind: "alert" });
        return;
      }
      showToast("Send failed", `${(e as Error).message}. Open the pane to register it.`, { kind: "alert", onClick: r.session ? () => shell.openPane(r.session!) : undefined });
    } finally {
      setBusy(false);
    }
  };
  return (
    <div className="ask">
      {head}
      {last && <p className="quote">{last.trim().slice(0, 600)}</p>}
      <div className="reply">
        <input
          value={text}
          placeholder={`Reply to ${agentName(r.agent)}…`}
          aria-label="Reply"
          onChange={(e) => setText(e.target.value)}
          onKeyDown={(e) => {
            e.stopPropagation();
            if (e.key === "Enter") void send();
          }}
        />
        <button className="btn pri" disabled={busy || !text.trim()} onClick={send}>
          Send
        </button>
        {r.session && (
          <button className="btn ghost" onClick={() => shell.openPane(r.session!)}>
            Pane
          </button>
        )}
      </div>
    </div>
  );
}

function FlightRow({ r, title, stacks, spark }: { r: Row; title: string; stacks?: Stack[]; spark: { calls: number[]; errs: number[] } | null }) {
  const line = activity(r);
  const since = r.session?.lastActivityAt || r.lastAt;
  const big = stacks?.slice().sort((a, b) => b.total - a.total)[0];
  return (
    <Link className="fl" to={threadHref(r.id, r.thread)}>
      <AgentBadge tool={r.agent} />
      <div className="grow1" style={{ minWidth: 0 }}>
        <div className="row">
          <b className="ell">{title}</b>
          {drivenIdOf(r.thread) && <DrivenTag />}
          {big ? (
            <span className="pill p-sub" style={{ flex: "none" }}>
              {big.label}
              {big.running ? ` · ${big.running} running` : big.waiting ? ` · ${big.waiting} waiting` : ""}
            </span>
          ) : (
            <span className="muted small" style={{ flex: "none" }}>
              {r.thread ? repoOf(r.thread) : repoOf({ cwd: r.cwd })}
            </span>
          )}
        </div>
        <div className="nowl ell">
          {line || "working"}
          {since ? ` · ${fmtAgo(since, "")}` : ""}
        </div>
      </div>
      {spark ? <SparkBars calls={spark.calls} errs={spark.errs} /> : <span />}
    </Link>
  );
}

function DigestOne({ t, title }: { t: V3Thread; title: string }) {
  const o = outcomeOf(t);
  const out = o.toLowerCase().includes(title.replace(/…$/, "").toLowerCase()) ? "" : o;
  const shipped = shippedSummary(shippedOf(t));
  const open = t.errorsOpen || 0;
  return (
    <Link className="dg" to={threadHref(t.id, t)}>
      <div className="row">
        <AgentBadge tool={t.tools?.[0]} />
        <b className="grow1 ell">{title}</b>
        {drivenIdOf(t) && <DrivenTag />}
        <span className="muted small" style={{ flex: "none" }}>
          {repoOf(t)}
          {t.turns > 1 ? ` · ${t.turns.toLocaleString()} turns` : ""}
          {t.costUsd >= 0.5 ? ` · ${fmtUsd(t.costUsd, t.costEstimated)}` : ""}
        </span>
      </div>
      {(out || shipped) && (
        <span className="fg2">
          {shipped ? `Shipped: ${shipped}. ` : ""}
          {out}
        </span>
      )}
      {open > 0 && <span className="err-t small">{open} error{open > 1 ? "s" : ""} not recovered</span>}
    </Link>
  );
}

function DigestStack({ u, byId }: { u: Extract<Unit, { kind: "stack" }>; byId: Map<string, V3Thread> }) {
  const s = u.s;
  const members = s.members.map((id) => byId.get(id)).filter(Boolean) as V3Thread[];
  const latestFinding = members.find(hasFindings) || (u.latest && hasFindings(u.latest) ? u.latest : undefined);
  const open = s.running + s.waiting + s.abandoned;
  const clean = Math.max(0, s.total - s.findings - s.failed - open);
  const word = s.kind === "review" ? "found defects" : "had findings";
  const text =
    s.findings > 0
      ? `${s.findings} ${word}${latestFinding ? ` (latest: "${outcomeOf(latestFinding).slice(0, 90)}")` : ""}, ${clean} ${clean === 1 ? "was" : "were"} clean.`
      : `All ${s.total} were clean.`;
  const parent = s.parent ? byId.get(s.parent) : undefined;
  return (
    <Link className="dg" to={`/t/${encodeURIComponent(s.parent || s.members[0])}`}>
      <div className="row">
        <b className="grow1 ell">
          {s.repo} · {s.label}
          {parent ? <span className="muted" style={{ fontWeight: 400 }}> · from {displayOf(parent)}</span> : null}
        </b>
        <span className="muted small" style={{ flex: "none" }}>
          {fmtAgo(s.lastAt)}
        </span>
      </div>
      <span className="fg2">
        {text}
        {s.abandoned ? ` ${s.abandoned} abandoned.` : ""}
        {s.failed ? ` ${s.failed} failed.` : ""}
      </span>
      <ResultBar f={s.findings} e={s.failed} x={open} ok={clean} />
    </Link>
  );
}

function WaitRow({ r, title }: { r: Row; title: string }) {
  const out = r.thread ? outcomeOf(r.thread) : "";
  return (
    <Link className="fl" to={threadHref(r.id, r.thread)}>
      <AgentBadge tool={r.agent} />
      <div className="grow1" style={{ minWidth: 0 }}>
        <div className="row">
          <b className="ell">{title}</b>
          {drivenIdOf(r.thread) && <DrivenTag />}
          <span className="muted small" style={{ flex: "none" }}>
            {r.thread ? repoOf(r.thread) : repoOf({ cwd: r.cwd })}
          </span>
        </div>
        {out && <div className="fg2 ell small">{out}</div>}
      </div>
      <span className="muted small">{waitAgo(r.sinceAt)}</span>
    </Link>
  );
}
