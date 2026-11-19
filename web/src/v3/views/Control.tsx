import { useEffect, useMemo, useRef, useState } from "react";
import { Link, useNavigate } from "react-router-dom";
import { api } from "../../api/client";
import { fmtDur, fmtUsd, modelShort } from "../../lib/format";
import { useLive } from "../../lib/ws";
import { ApiError, useApi, v3 } from "../api/client";
import type { Approval, NowLane, Stack, TimelineLane, V3Thread, XTraceRow } from "../api/types";
import { canAlways, needsSecond, quickDecide } from "../components/ApprovalCard";
import { sentToTerminal, TermShownNote } from "../components/TermShown";
import { AgentBadge, DrivenTag, Empty, Loading } from "../components/ui";
import { drivenIdOf, threadHref } from "../driven";
import { DrivenDetail } from "./DrivenDetail";
import { typing, useShell } from "../ctx";
import { approvalKeyOk } from "../keys";
import { applyDecision, loadApprovals, useV3 } from "../live";
import { buildRows, startOfToday, type Row } from "../model";
import { showToast } from "../../app/toast";
import { plainText } from "../../lib/markdown";
import { activityOf, ago, deriveStacks, displayOf, ensurePlexFonts, outcomeOf, repoOf, verbClass } from "./control/data";
import "../styles/control.css";
import { useNeedsYou } from "../api/needsYou";

type Filter = "" | "errors" | "cost" | "codex" | `repo:${string}`;
interface Item {
  key: string;
  row?: Row;
  thread?: V3Thread;
  stack?: Stack;
  title: string;
  sub: string;
  agent: string;
  repo: string;
  lastAt: number;
  pending?: number;
  waitSince?: number;
}
interface Group {
  label: string;
  items: Item[];
  more?: number;
}

const H3 = 3 * 3600_000;
const today = () => {
  const d = new Date();
  return `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, "0")}-${String(d.getDate()).padStart(2, "0")}`;
};

export default function Control() {
  ensurePlexFonts();
  const st = useV3();
  const live = useLive();
  const nav = useNavigate();
  const shell = useShell();
  const day = today();
  const all = useApi(() => v3.threads(true), [], { poll: 30_000 });
  const now = useApi(() => v3.now(), [], { poll: 20_000 });
  const tl = useApi(() => v3.timeline(day, true), [day], { poll: 60_000 });
  const errs = useApi(() => v3.errors({ since: "1h" }), [now.missing], { poll: 60_000, skip: !now.missing });
  const intel = useApi(() => v3.intel("today"), [now.missing], { poll: 120_000, skip: !now.missing });
  const [filter, setFilter] = useState<Filter>("");
  const [sel, setSel] = useState<string>("");
  const [phoneDetail, setPhoneDetail] = useState(false);
  const [denyFor, setDenyFor] = useState<string | null>(null);

  const ny = useNeedsYou();
  const rows = useMemo(() => buildRows(st, live, ny), [st, live, ny]);
  const threadsAll = all.data?.threads || st.threads;
  const stacks = useMemo(() => all.data?.stacks || deriveStacks(all.data?.threads || []), [all.data]);
  const laneOf = useMemo(() => new Map((tl.data?.lanes || []).map((l) => [l.threadId, l])), [tl.data]);
  const stackByParent = useMemo(() => {
    const m = new Map<string, Stack[]>();
    for (const s of stacks) m.set(s.parent, [...(m.get(s.parent) || []), s]);
    return m;
  }, [stacks]);

  const itemOf = (r: Row): Item => {
    const t = r.thread;
    const a = r.approvals[0];
    let sub = "";
    if (a) sub = a.kind === "question" ? `question · ${a.questions?.[0]?.question || a.summary}` : a.kind === "plan" ? `plan · ${a.summary}` : `permission · ${a.summary || a.toolName}`;
    else if (r.perms[0]) sub = `permission · ${r.perms[0].toolName}`;
    else if (r.session?.state === "awaiting-input") {
      const lm = plainText(r.session.lastMessage || "");
      sub = `ready · ${(lm && !/is waiting for your input/i.test(lm) ? lm : t ? outcomeOf(t) : "").slice(0, 120) || "for your next message"}`;
    }
    else if (t) sub = outcomeOf(t);
    return {
      key: r.id,
      row: r,
      thread: t,
      title: t ? displayOf(t, r.session?.firstMessage) : r.title,
      sub,
      agent: r.agent,
      repo: t ? repoOf(t) : repoOf({ cwd: r.cwd }),
      lastAt: r.lastAt,
      pending: r.approvals.length + r.perms.length,
      waitSince: r.needs ? r.sinceAt : undefined,
    };
  };
  const stackItem = (s: Stack): Item => ({
    key: s.id,
    stack: s,
    title: s.label,
    sub: [s.findings ? `${s.findings} with findings` : "", s.done ? `${s.done} done` : "", s.running ? `${s.running} running` : "", s.failed ? `${s.failed} failed` : "", s.abandoned ? `${s.abandoned} abandoned` : ""].filter(Boolean).join(" · "),
    agent: s.agent,
    repo: s.repo,
    lastAt: s.lastAt,
  });

  const groups: Group[] = useMemo(() => {
    const t0 = startOfToday();
    if (filter) {
      let items: Item[] = [];
      let label = "";
      if (filter.startsWith("repo:")) {
        const repo = filter.slice(5);
        label = repo;
        items = rows.filter((r) => (r.thread ? repoOf(r.thread) : repoOf({ cwd: r.cwd })) === repo).map(itemOf);
      } else if (filter === "errors") {
        label = "Errors not recovered";
        items = rows.filter((r) => r.thread && (r.thread.errorsOpen ?? (r.lastAt >= t0 ? r.thread.errors : 0)) > 0).map(itemOf);
      } else if (filter === "cost") {
        label = "Cost over $50";
        items = rows.filter((r) => (r.thread?.costUsd || 0) > 50).map(itemOf);
      } else {
        label = "Codex reviews";
        const ss = stacks.filter((s) => s.agent === "codex" && s.kind === "review").map(stackItem);
        const inStack = new Set(stacks.flatMap((s) => s.members));
        const solo = threadsAll.filter((t) => t.tools?.[0] === "codex" && verbClass(displayOf(t)) === "review" && !inStack.has(t.id));
        items = [...ss, ...solo.map((t) => ({ key: t.id, thread: t, title: displayOf(t), sub: outcomeOf(t), agent: "codex", repo: repoOf(t), lastAt: t.lastAt }))];
      }
      items.sort((a, b) => b.lastAt - a.lastAt);
      return [{ label, items: items.slice(0, 150), more: Math.max(0, items.length - 150) }];
    }
    const out: Group[] = [];
    const needs = rows.filter((r) => r.bucket === "needs-you").sort((a, b) => a.sinceAt - b.sinceAt);
    out.push({ label: "Needs you", items: needs.map(itemOf) });
    const byRepo = new Map<string, Item[]>();
    for (const r of rows.filter((x) => x.bucket === "running")) {
      const it = itemOf(r);
      byRepo.set(it.repo, [...(byRepo.get(it.repo) || []), it]);
    }
    for (const s of stacks.filter((x) => x.running > 0 && !rows.some((r) => r.id === x.parent && r.bucket === "running"))) {
      byRepo.set(s.repo, [...(byRepo.get(s.repo) || []), stackItem(s)]);
    }
    for (const [repo, items] of [...byRepo.entries()].sort((a, b) => b[1].length - a[1].length)) out.push({ label: `Running · ${repo}`, items: items.sort((a, b) => b.lastAt - a.lastAt) });
    const done = [
      ...rows.filter((r) => r.bucket === "done-today" || r.bucket === "idle-today").map(itemOf),
      ...stacks.filter((s) => s.running === 0 && s.lastAt >= t0).map(stackItem),
    ].sort((a, b) => b.lastAt - a.lastAt);
    out.push({ label: "Done today", items: done.slice(0, 60), more: Math.max(0, done.length - 60) });
    return out;
  }, [rows, stacks, filter, threadsAll]); // eslint-disable-line react-hooks/exhaustive-deps

  const flat = useMemo(() => groups.flatMap((g) => g.items), [groups]);
  const cur = flat.find((i) => i.key === sel) || flat[0];
  useEffect(() => {
    if (cur && cur.key !== sel) setSel(cur.key);
  }, [cur?.key]); // eslint-disable-line react-hooks/exhaustive-deps

  const pendingOf = (it?: Item): Approval | undefined => it?.row?.approvals[0];
  const openPane = (it?: Item) => {
    const d = drivenIdOf(it?.thread);
    if (d) return nav(`/w/${encodeURIComponent(d)}`);
    const s = it?.row?.session;
    if (s) shell.openPane(s);
    else showToast("No live pane", "This thread has no running terminal session.");
  };

  const keys = useRef({ flat, cur, pendingOf, openPane });
  keys.current = { flat, cur, pendingOf, openPane };
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (typing(e) || e.metaKey || e.ctrlKey || e.altKey || e.defaultPrevented) return;
      if (document.querySelector(".v3 .scrim, .v3 .drawer, [role=dialog]")) return;
      const k = keys.current;
      const i = k.flat.findIndex((x) => x.key === k.cur?.key);
      if (e.key === "j" || e.key === "k") {
        e.preventDefault();
        const n = k.flat[Math.min(Math.max(i + (e.key === "j" ? 1 : -1), 0), k.flat.length - 1)];
        if (n) {
          setSel(n.key);
          requestAnimationFrame(() => document.querySelector(`.ctl [data-key="${CSS.escape(n.key)}"]`)?.scrollIntoView({ block: "nearest" }));
        }
      } else if (e.key === "Enter" && k.cur) {
        const id = k.cur.stack ? k.cur.stack.parent : k.cur.key;
        nav(threadHref(id, k.cur.stack ? undefined : k.cur.thread));
      } else if (e.key === "o") k.openPane(k.cur);
      else if (e.key === "1") setFilter("");
      else if (e.key === "2") nav("/threads");
      else if (e.key === "3") nav("/timeline");
      else {
        const a = k.pendingOf(k.cur);
        if (!a || (["a", "A", "d"].includes(e.key) && !approvalKeyOk(e))) return;
        if ((e.key === "a" || e.key === "A") && needsSecond(a, e.key)) return;
        if (e.key === "a") void quickDecide(a, "allow");
        else if (e.key === "A") void quickDecide(a, "always");
        else if (e.key === "d") {
          e.preventDefault();
          setDenyFor(a.id);
        }
      }
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [nav]);

  const repos = useMemo(() => {
    const m = new Map<string, number>();
    for (const t of st.threads) m.set(repoOf(t), (m.get(repoOf(t)) || 0) + 1);
    return [...m.entries()].sort((a, b) => b[1] - a[1]).slice(0, 8);
  }, [st.threads]);

  return (
    <div className={`ctl ${phoneDetail ? "phone-detail" : ""}`}>
      <Strip rows={rows} now={now.data} nowMissing={now.missing} tl={tl.data?.lanes} threads={threadsAll} errsPerHour={errs.data?.groups.reduce((a, g) => a + g.count, 0)} spend={intel.data?.spendByRepo ? Object.values(intel.data.spendByRepo).reduce((a, b) => a + b, 0) : undefined} />
      <nav className="cnav" aria-label="Control room">
        <button className={filter === "" ? "on" : ""} onClick={() => setFilter("")}>
          Now <kbd>1</kbd>
        </button>
        <Link to="/threads">
          Threads <kbd>2</kbd>
        </Link>
        <Link to="/timeline">
          Timeline <kbd>3</kbd>
        </Link>
        <button onClick={() => shell.openPalette()}>
          Search <kbd>/</kbd>
        </button>
        <div className="h">Repos</div>
        {repos.map(([r, n]) => (
          <button key={r} className={filter === `repo:${r}` ? "on" : ""} onClick={() => setFilter(filter === `repo:${r}` ? "" : `repo:${r}`)}>
            <span className="ell">{r}</span>
            <span>{n.toLocaleString()}</span>
          </button>
        ))}
        <div className="h">Saved filters</div>
        {(
          [
            ["errors", "Errors not recovered"],
            ["cost", "Cost over $50"],
            ["codex", "Codex reviews"],
          ] as [Filter, string][]
        ).map(([f, l]) => (
          <button key={f} className={filter === f ? "on" : ""} onClick={() => setFilter(filter === f ? "" : f)}>
            {l}
          </button>
        ))}
      </nav>
      <div className="clist">
        {st.threadsStatus === "loading" && <Loading rows={8} />}
        {groups.map((g) => (
          <div key={g.label}>
            <div className="grp">
              <span>{g.label}</span>
              <span>{g.items.length + (g.more || 0)}</span>
            </div>
            {g.items.map((it) => (
              <ItemRow
                key={it.key}
                it={it}
                on={cur?.key === it.key}
                lane={laneOf.get(it.key)}
                kids={it.thread ? (stackByParent.get(it.thread.id) || []).reduce((a, s) => a + s.total, 0) || it.thread.childCount || 0 : 0}
                onClick={() => {
                  setSel(it.key);
                  setPhoneDetail(true);
                }}
              />
            ))}
            {g.items.length === 0 && <div className="none">nothing</div>}
            {!!g.more && <div className="none">+ {g.more} more</div>}
          </div>
        ))}
      </div>
      <div className="cdet">
        <button className="cback" onClick={() => setPhoneDetail(false)}>
          ‹ Back
        </button>
        {!cur ? (
          <Empty title="Nothing selected">Pick a thread on the left, or press j.</Empty>
        ) : cur.stack ? (
          <StackDetail s={cur.stack} threads={threadsAll} />
        ) : drivenIdOf(cur.thread) ? (
          <DrivenCtl key={cur.key} it={cur} denyOpen={denyFor === cur.row?.approvals[0]?.id} onDenyClosed={() => setDenyFor(null)} />
        ) : (
          <ThreadDetail key={cur.key} it={cur} denyOpen={denyFor === cur.row?.approvals[0]?.id} onDenyClosed={() => setDenyFor(null)} onPane={() => openPane(cur)} />
        )}
      </div>
    </div>
  );
}

function Strip({ rows, now, nowMissing, tl, threads, errsPerHour, spend }: { rows: Row[]; now: import("../api/types").NowResponse | null; nowMissing: boolean; tl?: TimelineLane[]; threads: V3Thread[]; errsPerHour?: number; spend?: number }) {
  const t = Date.now();
  const byId = new Map(threads.map((x) => [x.id, x]));
  const ny = useNeedsYou();
  const needs = ny.status === "loading" ? (now?.needsYou ?? rows.filter((r) => r.bucket === "needs-you").length) : ny.count;
  const running = now?.running ?? rows.filter((r) => r.bucket === "running").length;
  const eph = now?.errorsPerHour ?? errsPerHour;
  const usd = now?.spendToday ?? spend;
  const from = now?.from ?? t - H3;
  const to = now?.to ?? t;
  let lanes: NowLane[] = now?.lanes || [];
  if (!now && nowMissing && tl) {
    lanes = tl
      .filter((l) => l.kind !== "stack" && l.bars.some((b) => b.from > 0 && b.to > from && b.kind !== "idle"))
      .map((l) => ({
        threadId: l.threadId,
        display: byId.get(l.threadId) ? displayOf(byId.get(l.threadId)!) : l.display || l.title,
        agent: l.agent,
        bars: l.bars.filter((b) => b.from > 0 && b.to > from && b.kind !== "idle").map((b) => ({ from: Math.max(b.from, from), to: Math.min(b.to, to), kind: b.kind as "work" | "wait" | "error" })),
      }))
      .sort((a, b) => Math.max(...b.bars.map((x) => x.to)) - Math.max(...a.bars.map((x) => x.to)));
  }
  lanes = lanes.slice(0, 6);
  const x = (v: number) => ((Math.min(Math.max(v, from), to) - from) / Math.max(to - from, 1)) * 100;
  return (
    <div className="strip">
      <div>
        <span className="k">Needs you</span>
        <span className="v need">{needs}</span>
      </div>
      <div>
        <span className="k">Running</span>
        <span className="v run">{running}</span>
      </div>
      <div>
        <span className="k">Errors / h</span>
        <span className="v">{eph ?? "…"}</span>
      </div>
      <div>
        <span className="k">Spend today</span>
        <span className="v">{usd == null ? "…" : "$" + Math.round(usd).toLocaleString()}</span>
      </div>
      <div className="lanes" title="Last 3 hours">
        {lanes.length === 0 && <span className="none">No agent activity in the last 3 hours</span>}
        {lanes.map((l) => (
          <Link key={l.threadId} className="lane" to={`/t/${encodeURIComponent(l.threadId)}`}>
            <span className="ell">{l.display}</span>
            <div className="bar">
              {l.bars.map((b, i) => (
                <i key={i} className={b.kind} style={{ left: `${x(b.from)}%`, width: `${Math.max(x(b.to) - x(b.from), b.kind === "error" ? 0.6 : 0.3)}%` }} />
              ))}
            </div>
          </Link>
        ))}
      </div>
    </div>
  );
}

function ItemRow({ it, on, lane, kids, onClick }: { it: Item; on: boolean; lane?: TimelineLane; kids: number; onClick: () => void }) {
  const act = it.stack || it.waitSince ? null : activityOf(it.thread, lane);
  const max = act ? Math.max(...act.calls, 1) : 1;
  const right = it.waitSince ? (
    <span className="pill p-need">{ago(it.waitSince) === "now" ? "now" : ago(it.waitSince)}</span>
  ) : (
    <span className="s">{agoShort(it.lastAt)}</span>
  );
  return (
    <div className={`it ${on ? "on" : ""} ${it.waitSince ? "need" : ""}`} data-key={it.key} onClick={onClick} role="button" tabIndex={-1}>
      <AgentBadge tool={it.agent} />
      <div style={{ minWidth: 0 }}>
        <div className="t ell">
          {drivenIdOf(it.thread) && <DrivenTag />}
          {it.title}
          {kids > 1 && <span className="pill p-sub">{kids}</span>}
        </div>
        {act ? (
          <div className="m" aria-label="tool calls per 5 minutes, last hour">
            {act.calls.map((c, i) => (
              <i key={i} className={act.errs[i] ? "e" : c ? "" : "z"} style={{ height: act.errs[i] ? 16 : c ? Math.max(3, Math.round((c / max) * 16)) : 1 }} />
            ))}
          </div>
        ) : (
          it.sub && <div className="s ell">{it.sub}</div>
        )}
      </div>
      {right}
    </div>
  );
}
const agoShort = (ts: number) => {
  const s = Math.max(0, Math.floor((Date.now() - ts) / 1000));
  return s < 60 ? `${s}s` : ago(ts);
};

function DrivenCtl({ it, denyOpen, onDenyClosed }: { it: Item; denyOpen: boolean; onDenyClosed: () => void }) {
  const nav = useNavigate();
  const id = drivenIdOf(it.thread)!;
  const a = it.row?.approvals[0];
  const st = it.thread?.status || "";
  return (
    <>
      <div className="dh">
        <AgentBadge tool={it.agent} />
        <h3>{it.title}</h3>
        <DrivenTag />
        {it.waitSince ? <span className="pill p-need">waiting {ago(it.waitSince)}</span> : <span className={`pill ${st === "running" ? "p-run" : st === "error" ? "p-err" : "p-mut"}`}>{st === "idle" ? "ready" : st}</span>}
        <span className="pill p-mut">{it.repo}</span>
        <Link className="bbtn ml-auto" to={`/w/${encodeURIComponent(id)}`}>
          Workspace <kbd>↵</kbd>
        </Link>
      </div>
      {a && <ApprovalBox a={a} denyOpen={denyOpen} onDenyClosed={onDenyClosed} onPane={() => nav(`/w/${encodeURIComponent(id)}`)} paneLabel="Workspace" />}
      <DrivenDetail id={id} row={it.row} compact />
    </>
  );
}

function ThreadDetail({ it, denyOpen, onDenyClosed, onPane }: { it: Item; denyOpen: boolean; onDenyClosed: () => void; onPane: () => void }) {
  const t = it.thread;
  const r = it.row;
  const sid = r?.session?.id || (t ? t.sessions[t.sessions.length - 1] : r?.approvals[0]?.sessionId) || "";
  const xt = useApi(() => v3.xtrace(sid, ""), [sid, t?.lastAt], { skip: !sid });
  const seg = useApi(() => api.segments(sid), [sid, t?.lastAt], { skip: !sid });
  const state = r?.session?.state || t?.state || "idle";
  const a = r?.approvals[0];
  const statePill = it.waitSince ? (
    <span className="pill p-need">waiting {ago(it.waitSince)}</span>
  ) : state === "running" ? (
    <span className="pill p-run">running</span>
  ) : state === "completed" ? (
    <span className="pill p-ok">done</span>
  ) : (
    <span className="pill p-mut">{state}</span>
  );
  const rows = useMemo(() => wfRows(xt.data?.rows || []), [xt.data]);
  const toolDurs = (xt.data?.rows || []).filter((x) => x.depth >= 1 && x.kind !== "turn" && x.ts > 0 && x.dur > 0 && x.dur < 864e5).map((x) => x.dur).sort((p, q) => p - q);
  const p50 = toolDurs.length ? toolDurs[Math.floor(toolDurs.length / 2)] : 0;
  const meta = seg.data?.meta;
  const ctx = meta && meta.contextWindow ? Math.round((meta.contextUsed / meta.contextWindow) * 100) : null;
  const errs = t ? t.errorsOpen ?? t.errors : 0;
  const model = r?.session?.model || t?.model;
  return (
    <>
      <div className="dh">
        <AgentBadge tool={it.agent} />
        <h3>{it.title}</h3>
        {statePill}
        <span className="pill p-mut">{it.repo}</span>
        {model && <span className="pill p-mut">{modelShort(model)}</span>}
        <Link className="bbtn ml-auto" to={`/t/${encodeURIComponent(it.key)}`}>
          Open <kbd>↵</kbd>
        </Link>
      </div>
      {a ? (
        <ApprovalBox a={a} denyOpen={denyOpen} onDenyClosed={onDenyClosed} onPane={onPane} />
      ) : r && (r.perms.length > 0 || r.session?.state === "awaiting-permission" || r.session?.state === "awaiting-input") ? (
        <div className="cappr">
          <div className="row">
            <b className="grow1">{r.perms[0] ? `Permission · ${r.perms[0].toolName}` : r.session?.state === "awaiting-input" ? "Ready for your next message" : "Needs you in the terminal"}</b>
          </div>
          {(r.session?.permissionMessage || r.session?.lastMessage) && <pre>{plainText(r.session.permissionMessage || r.session.lastMessage || "").slice(0, 400)}</pre>}
          <div className="row" style={{ gap: 6 }}>
            <button className="bbtn" onClick={onPane}>
              Open pane<kbd>o</kbd>
            </button>
          </div>
        </div>
      ) : null}
      {t && outcomeOf(t) && <p className="outcome">{outcomeOf(t)}</p>}
      <div className="kv">
        <div>
          <div className="k">Turns</div>
          <div className="v">{t ? t.turns.toLocaleString() : "—"}</div>
        </div>
        <div>
          <div className="k">Cost</div>
          <div className="v">{t ? fmtUsd(t.costUsd, t.costEstimated) : "—"}</div>
        </div>
        <div>
          <div className="k">p50 tool</div>
          <div className="v">{p50 ? fmtDur(p50) : "—"}</div>
        </div>
        <div>
          <div className="k">Errors</div>
          <div className={`v ${errs ? "err" : ""}`}>{errs}</div>
        </div>
        <div>
          <div className="k">Context</div>
          <div className="v">{ctx == null ? "—" : `${ctx}%`}</div>
        </div>
      </div>
      {sid && (
        <div className="cwf">
          <div>
            <span>Span · {(xt.data?.rows || []).find((x) => x.depth === 0)?.name || (xt.data?.turn != null ? `turn ${xt.data.turn + 1}` : "latest turn")}</span>
            <span>0 s {"─".repeat(12)} {rows.span ? Math.round(rows.span / 1000) : 0} s</span>
            <span>dur</span>
          </div>
          {xt.loading && !xt.data && <div className="none">loading the latest turn…</div>}
          {xt.missing && <div className="none">The trace endpoint is not available on this daemon.</div>}
          {xt.data && rows.rows.length === 0 && <div className="none">No tool calls in the latest turn.</div>}
          {rows.rows.map((w, i) => (
            <div key={i} className={w.err ? "er" : ""} title={w.label}>
              <span className="ell">{w.label}</span>
              <span className="tr">
                <i className={`f-${w.fam}`} style={{ left: `${w.l}%`, width: `${Math.max(w.w, 0.6)}%` }} />
              </span>
              <span>{fmtDur(w.dur)}</span>
            </div>
          ))}
          {xt.data && (
            <Link className="wfmore" to={`/traces/${encodeURIComponent(sid)}/${xt.data.turn ?? 0}`}>
              Open the full trace ›
            </Link>
          )}
        </div>
      )}
    </>
  );
}

const famOf = (r: XTraceRow) => {
  if (r.kind === "agent" || r.child) return "agent";
  const n = r.name.toLowerCase();
  if (n === "bash" || n === "exec_command" || n === "shell") return "bash";
  if (n === "read" || n === "grep" || n === "glob" || n === "ls") return "read";
  if (n === "edit" || n === "write" || n === "multiedit" || n === "apply_patch") return "edit";
  if (n.startsWith("mcp__")) return "mcp";
  if (n.startsWith("web")) return "web";
  return "other";
};

/** Tool rows of the turn plus synthetic "model" rows for gaps of a second or more. */
function wfRows(src: XTraceRow[]) {
  const sane = (x: XTraceRow) => x.ts > 0 && x.dur >= 0 && x.dur < 864e5;
  const root = src.find((x) => x.depth === 0 && sane(x));
  const calls = src.filter((x) => x.depth >= 1 && x.kind !== "turn" && sane(x)).sort((p, q) => p.ts - q.ts);
  if (!calls.length) return { rows: [], span: 0 };
  const t0 = Math.min(root?.ts ?? Infinity, calls[0].ts);
  const t1 = Math.max(root ? root.ts + root.dur : 0, ...calls.map((c) => c.ts + c.dur));
  const span = Math.max(t1 - t0, 1);
  const out: { label: string; fam: string; l: number; w: number; dur: number; err: boolean }[] = [];
  const pct = (v: number) => ((v - t0) / span) * 100;
  let cursor = t0;
  for (const c of calls) {
    if (c.depth === 1 && c.ts - cursor >= 1000) out.push({ label: "model", fam: "model", l: pct(cursor), w: pct(c.ts) - pct(cursor), dur: c.ts - cursor, err: false });
    const fam = famOf(c);
    const short = c.name.startsWith("mcp__") ? c.name.split("__").slice(-1)[0] : c.name;
    const lead = fam === "other" ? short : fam === "mcp" || (fam === "agent" && short.toLowerCase() !== "agent") ? `${fam}  ${short}` : fam;
    out.push({ label: `${"  ".repeat(Math.max(c.depth - 1, 0))}${lead}  ${(c.res || "").replace(/\s+/g, " ").slice(0, 60)}`.trimEnd(), fam, l: pct(c.ts), w: (c.dur / span) * 100, dur: c.dur, err: !!c.err });
    if (c.depth === 1) cursor = Math.max(cursor, c.ts + c.dur);
  }
  if (t1 - cursor >= 1000) out.push({ label: "model", fam: "model", l: pct(cursor), w: pct(t1) - pct(cursor), dur: t1 - cursor, err: false });
  return { rows: out.slice(-40), span };
}

function ApprovalBox({ a, denyOpen, onDenyClosed, onPane, paneLabel = "Open pane" }: { a: Approval; denyOpen: boolean; onDenyClosed: () => void; onPane: () => void; paneLabel?: string }) {
  const [busy0, setBusy] = useState(false);
  const busy = busy0 || sentToTerminal(a);
  const [msg, setMsg] = useState("");
  const ref = useRef<HTMLInputElement>(null);
  useEffect(() => {
    if (denyOpen) ref.current?.focus();
  }, [denyOpen]);
  const decide = async (b: import("../api/types").DecideBody) => {
    setBusy(true);
    try {
      const r = await v3.decide(a.id, b);
      applyDecision(r && r.id ? r : { ...a, state: b.behavior === "allow" ? "allowed" : "denied" });
      onDenyClosed();
    } catch (e) {
      if (e instanceof ApiError && e.status === 409) showToast("Already answered", "Another surface or the terminal answered first.");
      else showToast("Decision failed", (e as Error).message, { kind: "alert" });
      void loadApprovals();
    } finally {
      setBusy(false);
    }
  };
  const cmd = typeof a.input?.command === "string" ? (a.input.command as string) : typeof a.input?.cmd === "string" ? (a.input.cmd as string) : a.summary;
  const head = a.kind === "question" ? "Question" : a.kind === "plan" ? "Plan" : `Permission · ${a.toolName}`;
  return (
    <div className="cappr">
      <div className="row">
        <b className="grow1">{head}</b>
        {a.kind === "permission" && (
          <span className={`pill ${a.risk === "high" ? "p-err" : a.risk === "low" ? "p-ok" : "p-need"}`}>
            {a.risk}
            {a.riskReason ? ` · ${a.riskReason}` : ""}
          </span>
        )}
      </div>
      <TermShownNote a={a} />
      {a.kind === "question" ? (
        (a.questions || []).map((q) => (
          <div key={q.question} className="q">
            <div>{q.question}</div>
            <div className="row" style={{ gap: 6, flexWrap: "wrap" }}>
              {q.options.map((o) => (
                <button key={o.label} className="bbtn" disabled={busy} title={o.description} onClick={() => decide({ behavior: "allow", answers: { [q.question]: o.label } })}>
                  {o.label}
                </button>
              ))}
            </div>
          </div>
        ))
      ) : (
        <pre>{(a.kind === "plan" ? a.plan || a.summary : cmd).slice(0, 1200)}</pre>
      )}
      {denyOpen ? (
        <div className="row" style={{ gap: 6 }}>
          <input
            ref={ref}
            className="deny"
            placeholder="Why deny? (optional) · Enter sends · Esc cancels"
            value={msg}
            onChange={(e) => setMsg(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === "Enter") void decide({ behavior: "deny", message: msg.trim() || undefined });
              else if (e.key === "Escape") onDenyClosed();
            }}
          />
          <button className="bbtn" disabled={busy} onClick={() => decide({ behavior: "deny", message: msg.trim() || undefined })}>
            Deny
          </button>
        </div>
      ) : (
        <div className="row" style={{ gap: 6, flexWrap: "wrap" }}>
          {a.kind !== "question" && (
            <button className="bbtn ok" disabled={busy} onClick={() => quickDecide(a, "allow")}>
              Allow<kbd>a</kbd>
            </button>
          )}
          <button className="bbtn" disabled={busy} onClick={() => decide({ behavior: "deny" })}>
            Deny<kbd>d</kbd>
          </button>
          {canAlways(a) && (
            <button className="bbtn" disabled={busy} onClick={() => quickDecide(a, "always")}>
              Always<kbd>⇧a</kbd>
            </button>
          )}
          <button className="bbtn" onClick={onPane}>
            {paneLabel}<kbd>o</kbd>
          </button>
        </div>
      )}
    </div>
  );
}

function StackDetail({ s, threads }: { s: Stack; threads: V3Thread[] }) {
  const byId = new Map(threads.map((t) => [t.id, t]));
  const parent = byId.get(s.parent);
  const ms = s.members.map((id) => byId.get(id)).filter(Boolean).sort((a, b) => b!.lastAt - a!.lastAt) as V3Thread[];
  const w = (n: number) => `${(n / Math.max(s.total, 1)) * 100}%`;
  return (
    <>
      <div className="dh">
        <AgentBadge tool={s.agent} />
        <h3>{s.label}</h3>
        <span className="pill p-mut">{s.repo}</span>
        {parent && (
          <Link className="pill p-mut" to={`/t/${encodeURIComponent(parent.id)}`}>
            from {displayOf(parent)}
          </Link>
        )}
      </div>
      <div className="kv">
        <div>
          <div className="k">Total</div>
          <div className="v">{s.total}</div>
        </div>
        <div>
          <div className="k">Done</div>
          <div className="v">{s.done}</div>
        </div>
        <div>
          <div className="k">Running</div>
          <div className="v run">{s.running}</div>
        </div>
        <div>
          <div className="k">Findings</div>
          <div className="v need">{s.findings}</div>
        </div>
        <div>
          <div className="k">Failed</div>
          <div className={`v ${s.failed ? "err" : ""}`}>{s.failed}</div>
        </div>
      </div>
      <div className="sbar" aria-label="result bar">
        <i className="ok" style={{ width: w(s.done) }} />
        <i className="run" style={{ width: w(s.running) }} />
        <i className="err" style={{ width: w(s.failed) }} />
      </div>
      <div className="cwf members">
        <div>
          <span>Member</span>
          <span>Outcome</span>
          <span>last</span>
        </div>
        {ms.map((m) => (
          <Link key={m.id} to={`/t/${encodeURIComponent(m.id)}`} className={(m.errorsOpen ?? 0) > 0 ? "er" : ""}>
            <span className="ell">{displayOf(m)}</span>
            <span className="ell">{outcomeOf(m) || m.state}</span>
            <span>{ago(m.lastAt)}</span>
          </Link>
        ))}
      </div>
    </>
  );
}
