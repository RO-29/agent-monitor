import { drivenNeedIds, useNeedsYou } from "../../api/needsYou";
// Right panel: workspace, changes, checkpoints, lineage and spend, for driven and watched threads.
import { useEffect, useMemo, useState } from "react";
import { Link } from "react-router-dom";
import { showToast } from "../../../app/toast";
import { fmtAgo, fmtDuration, fmtTok, fmtUsd, modelShort, shortCwd } from "../../../lib/format";
import { agentCls, agentLabel, allowOnce, toMs, useOrch, type ODetail, type OSpendStatus, type OThread } from "../../api/orch";
import { useApi, v3 } from "../../api/client";
import { agentName, useV3 } from "../../live";
import type { Row } from "../../model";
import { childMap, drivenThreads, permModeLabel, WORKSPACE_LABEL, workspaceOf } from "./rows";
import { KID_DOT, kidState, useTick } from "./DrivenChat";
import { Checkpoints, GitBox, GitChanges, WatchedGitBox } from "./GitBox";
import { filesOf, gitKey, useDiffNav, useGit } from "./git";
import type { WatchedFeed } from "./watched";
import { ChildrenBox } from "./children";
import { latestSession, usePane, watchedLiveness } from "./WatchedChat";
import { useLive } from "../../../lib/ws";
import type { SessionChildren } from "../../api/p7";
import { settingsApi } from "../../api/settings";
import { SpendBar } from "../../components/Spend";
import { FoldBox } from "../../components/fold";

function KV({ k, v, mono }: { k: string; v?: string; mono?: boolean }) {
  return (
    <div className="wkv">
      <span className="k">{k}</span>
      <span className={`v ${mono ? "mono" : ""}`} title={v}>
        {v || <span className="muted">none</span>}
      </span>
    </div>
  );
}

function LNode({ n, cur, kids, needIds, now, depth }: { n: OThread; cur: string; kids: Map<string, OThread[]>; needIds: Set<string>; now: number; depth: number }) {
  const st = kidState(n, needIds);
  const ch = depth < 3 ? kids.get(n.id) || [] : [];
  const live = st === "run" || st === "need";
  const a = toMs(n.createdAt);
  const b = live ? now : toMs(n.updatedAt);
  const tm = a && b > a ? fmtDuration(b - a) : "";
  return (
    <>
      <div className={`n ${n.id === cur ? "cur" : ""}`}>
        <span className={`wdot ${live ? "wpulse" : ""}`} style={{ background: KID_DOT[st] }} title={st} />
        <span className={`wag ${agentCls(n.agent)}`} title={agentLabel(n.agent)} />
        {n.id === cur ? (
          <b className="trunc" title={n.title}>
            {n.title}
          </b>
        ) : (
          <Link className="trunc" to={`/w/${encodeURIComponent(n.id)}`} title={n.title}>
            {n.title}
          </Link>
        )}
        <span className="tm">{tm}</span>
      </div>
      {ch.length > 0 && (
        <div className="ch">
          {ch.map((c) => (
            <LNode key={c.id} n={c} cur={cur} kids={kids} needIds={needIds} now={now} depth={depth + 1} />
          ))}
        </div>
      )}
    </>
  );
}

function Lineage({ detail }: { detail: ODetail }) {
  const orch = useOrch();
  const v = useV3();
  const t = detail.thread;
  const threads = useMemo(() => {
    const m = drivenThreads(orch);
    for (const c of detail.children || []) if (!m.has(c.id)) m.set(c.id, c);
    if (!m.has(t.id)) m.set(t.id, t);
    return m;
  }, [orch, detail.children, t]);
  const kids = useMemo(() => childMap(threads), [threads]);
  const ny = useNeedsYou();
  const needIds = useMemo(() => drivenNeedIds(ny.items, v.approvals.values()), [ny.version, v.approvals]); // eslint-disable-line react-hooks/exhaustive-deps
  let root = threads.get(t.id) || t;
  for (let i = 0; i < 6 && root.parentId && threads.has(root.parentId); i++) root = threads.get(root.parentId)!;
  const alone = root.id === t.id && !(kids.get(t.id) || []).length;
  const now = useTick(!alone, 5000);
  if (alone) return null;
  return (
    <FoldBox id="lineage" title="Lineage" summary={`${threads.size} threads`}>
      <div className="wtree">
        <LNode n={root} cur={t.id} kids={kids} needIds={needIds} now={now} depth={0} />
        {root.id !== t.id && root.parentId && <div className="muted sm">The parent of the root is not loaded.</div>}
      </div>
    </FoldBox>
  );
}

function SideClose({ onClose }: { onClose?: () => void }) {
  if (!onClose) return null;
  return (
    <div className="wside-x">
      <b>Details</b>
      <button className="btn sm ghost" onClick={onClose} aria-label="Close details">
        ✕
      </button>
    </div>
  );
}

/** Thread and today's spend against the limits; re-read when a run ends. */
function SpendBox({ detail, cost }: { detail: ODetail; cost: number }) {
  const t = detail.thread;
  const orch = useOrch();
  const [st, setSt] = useState<OSpendStatus | null>(null);
  const ended = detail.runs.filter((r) => r.status !== "running").length;
  useEffect(() => {
    let off = false;
    settingsApi()
      .spend(t.id)
      .then((r) => !off && setSt(r))
      .catch(() => !off && setSt(null));
    return () => {
      off = true;
    };
  }, [t.id, ended, orch.blocked]);
  const th = st?.thread;
  const day = st?.day;
  const blocked = orch.blocked.get(t.id) || (th?.blocked ? { error: "", spent: th.spent, limit: th.limit } : undefined);
  if (!blocked && !((th?.spent ?? cost) > 0)) return null;
  return (
    <FoldBox
      id="spend"
      title="Spend"
      summary={fmtUsd(th?.spent ?? cost)}
      right={
        <Link className="r sm" to="/settings/limits">
          Limits
        </Link>
      }
    >
      <SpendBar label="This thread" spent={th?.spent ?? cost} limit={th?.limit || 0} note={th?.allowedOnce ? "One more turn is allowed past the limit." : undefined} />
      {day && <SpendBar label="Today, all threads" spent={day.spent} limit={day.limit} />}
      {blocked && (
        <div className="spb-a">
          <button
            className="btn sm pri"
            onClick={() => allowOnce(t.id).catch((e) => showToast("Could not allow the turn", (e as Error).message, { kind: "alert" }))}
          >
            Allow this thread once more
          </button>
        </div>
      )}
    </FoldBox>
  );
}

export function DrivenSide({ detail, onClose }: { detail: ODetail; onClose?: () => void }) {
  const t = detail.thread;
  const git = useGit(t.id, gitKey(detail));
  const { open: openDiff } = useDiffNav();
  const hasGit = !t.worktree?.removed && (git.status === "ok" || (git.status === "error" && !!git.data));
  const files = useMemo(() => filesOf(detail.items), [detail.items]);
  const add = files.reduce((n, f) => n + f.add, 0);
  const del = files.reduce((n, f) => n + f.del, 0);
  const costed = detail.runs.filter((r) => r.usage?.costUsd != null);
  const cost = costed.reduce((n, r) => n + (r.usage?.costUsd || 0), 0);
  return (
    <aside className="wside" aria-label="Thread details">
      <SideClose onClose={onClose} />
      <FoldBox id="workspace" title="Workspace" summary={shortCwd(t.worktree && !t.worktree.removed ? t.worktree.path : t.cwd)}>
        <KV k="Repo" v={t.repo} />
        {t.worktree ? (
          <>
            <KV k="Worktree" v={t.worktree.removed ? "removed" : shortCwd(t.worktree.path)} mono />
            <KV k="From" v={shortCwd(t.cwd)} mono />
          </>
        ) : (
          <KV k="Folder" v={shortCwd(t.cwd)} mono />
        )}
        {!hasGit && <KV k="Branch" v={t.worktree?.branch || t.branch} mono />}
        {t.parentId && <KV k="Access" v={WORKSPACE_LABEL[workspaceOf(t)]} />}
        {t.agent === "codex" ? <KV k="Sandbox" v={t.sandbox} /> : <KV k="Mode" v={permModeLabel(t.permissionMode || "manual")} />}
        <KV k="Model" v={`${agentLabel(t.agent)} · ${t.model || "default"}`} />
      </FoldBox>
      <GitBox detail={detail} git={git} />
      {hasGit ? (
        <>
          <GitChanges git={git} onDiff={(d) => { openDiff(d); onClose?.(); }} />
          <Checkpoints detail={detail} git={git} onDiff={(d) => { openDiff(d); onClose?.(); }} />
        </>
      ) : (
      <FoldBox
        id="changes"
        title="Changes"
        summary={files.length ? `${files.length} ${files.length === 1 ? "file" : "files"} · +${add} −${del}` : "none"}
        right={
          files.length > 0 && (
            <span className="r mono">
              <span className="add">+{add}</span> <span className="del">−{del}</span>
            </span>
          )
        }
      >
        {files.length === 0 ? (
          <div className="muted sm">No file changes yet.</div>
        ) : (
          files.map((f) => (
            <div key={f.path} className="f">
              <span className="p" title={f.path}>
                {f.path}
              </span>
              <span className="n">
                {f.kind === "add" && !f.add && !f.del ? <span className="muted">new</span> : null}
                {f.add > 0 && <span className="add">+{f.add}</span>} {f.del > 0 && <span className="del">−{f.del}</span>}
              </span>
            </div>
          ))
        )}
      </FoldBox>
      )}
      <SpendBox detail={detail} cost={costed.length ? cost : 0} />
      <Lineage detail={detail} />
    </aside>
  );
}

interface SessionPlace {
  kind: string;
  label: string;
  detail?: string;
  origin?: string;
  journey: { entry: string; label: string; at: number }[];
}

/** Where a watched session runs now, who started it, and each place it ran in (GET /api/session/{id}/place). */
function TermKV({ row }: { row: Row }) {
  const sid = latestSession(row);
  const [p, setP] = useState<SessionPlace | null>(null);
  useEffect(() => {
    if (!sid) return;
    let live = true;
    const load = () =>
      fetch(`/api/session/${encodeURIComponent(sid)}/place`, { credentials: "same-origin" })
        .then((r) => (r.ok ? r.json() : null))
        .then((j) => live && j && setP(j as SessionPlace))
        .catch(() => {});
    void load();
    const t = window.setInterval(load, 10000);
    return () => ((live = false), window.clearInterval(t));
  }, [sid]);
  if (!p) return <KV k="Runs in" v="…" />;
  return (
    <>
      <KV k="Runs in" v={p.detail ? `${p.label} · ${p.detail}` : p.label} />
      {p.origin && <KV k="Started from" v={p.origin} />}
      {(p.journey?.length ?? 0) > 1 && (
        <div className="wjourney" aria-label="Places this session ran in">
          <span className="wjourney-k">Journey</span>
          <ol>
            {p.journey.map((j, i) => (
              <li key={i}>
                <b>{j.label}</b>
                {j.at > 0 && <span className="muted"> · {new Date(j.at).toLocaleString([], { month: "short", day: "numeric", hour: "2-digit", minute: "2-digit" })}</span>}
              </li>
            ))}
          </ol>
        </div>
      )}
    </>
  );
}

export function WatchedSide({ row, feed, onClose, sid, kids: sessionKids, kidsMissing }: { row: Row; feed: WatchedFeed | null; onClose?: () => void; sid?: string; kids?: SessionChildren | null; kidsMissing?: boolean }) {
  const t = row.thread;
  const { open: openDiff } = useDiffNav();
  // An older daemon has no per-session children route; the thread lineage stands in.
  const kids = useApi(() => v3.children(row.id), [row.id, kidsMissing], { poll: 60_000, skip: !kidsMissing });
  const files = useMemo(() => filesOf(feed?.items || []), [feed]);
  const add = files.reduce((n, f) => n + f.add, 0);
  const del = files.reduce((n, f) => n + f.del, 0);
  const list = kids.data?.children || [];
  return (
    <aside className="wside" aria-label="Thread details">
      <SideClose onClose={onClose} />
      <FoldBox id="workspace" title="Workspace" summary={shortCwd(row.cwd)}>
        <KV k="Repo" v={t?.repo} />
        <KV k="Folder" v={shortCwd(row.cwd)} mono />
        <TermKV row={row} />
        <KV k="Model" v={modelShort(t?.model) ? `${agentName(row.agent)} · ${modelShort(t?.model)}` : agentName(row.agent)} />
      </FoldBox>
      <WatchedGitBox
        t={t}
        cwd={row.cwd}
        onDiff={(d) => {
          openDiff(d);
          onClose?.();
        }}
      />
      {files.length > 0 && (
        <FoldBox
          id="edited"
          title="Edited in this session"
          summary={`${files.length} ${files.length === 1 ? "file" : "files"} · +${add} −${del}`}
          right={
            <span className="r mono">
              <span className="add">+{add}</span> <span className="del">−{del}</span>
            </span>
          }
        >
          {files.slice(0, 12).map((f) => (
            <div key={f.path} className="f">
              <span className="p" title={f.path}>
                {f.path.split("/").slice(-2).join("/")}
              </span>
              <span className="n">
                {f.add > 0 && <span className="add">+{f.add}</span>} {f.del > 0 && <span className="del">−{f.del}</span>}
              </span>
            </div>
          ))}
          {files.length > 12 && <div className="muted sm">and {files.length - 12} more</div>}
        </FoldBox>
      )}
      {sid && <ChildrenBox sid={sid} data={sessionKids || null} cwd={row.cwd} />}
      {kidsMissing && list.length > 0 && (
        <FoldBox id="lineage" title="Lineage" summary={`${list.length}`}>
          <div className="wtree">
            {list.slice(0, 12).map((c) => (
              <div key={c.sessionId} className="n">
                <span className={`wsd ${c.attention ? "need" : c.state === "running" ? "run wpulse" : c.errors ? "err" : ""}`} aria-hidden />
                <Link className="trunc" to={`/w/${encodeURIComponent(c.threadId)}`} title={c.title}>
                  {c.title || c.kind}
                </Link>
                <span className="tm">{fmtAgo(c.ts, "")}</span>
              </div>
            ))}
            {list.length > 12 && <div className="muted sm">and {list.length - 12} more</div>}
          </div>
        </FoldBox>
      )}
      {t && t.costUsd > 0 && (
        <FoldBox id="spend" title="Spend" summary={fmtUsd(t.costUsd, t.costEstimated)}>
          <KV k="Session" v={fmtUsd(t.costUsd, t.costEstimated)} mono />
          <KV k="Tokens" v={t.tokens?.input || t.tokens?.output ? `${fmtTok(t.tokens.input)} in · ${fmtTok(t.tokens.output)} out` : ""} mono />
        </FoldBox>
      )}
    </aside>
  );
}

