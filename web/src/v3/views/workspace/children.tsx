// Subagents and agents a watched session started: the side panel list and the chips on the chat rows that started them.
import { createContext, useContext, useEffect, useMemo, useState } from "react";
import { Link } from "react-router-dom";
import { fmtDuration, fmtTok, fmtUsd, modelShort, shortCwd } from "../../../lib/format";
import { isMissing, useApi, type ApiState } from "../../api/client";
import { req, type OItem, type ORun } from "../../api/orch";
import { p7, type SessionChild, type SessionChildren, type WatchedItems } from "../../api/p7";
import { agentName } from "../../live";
import { blocksOf, RunBlock, useTick } from "./DrivenChat";
import { FoldBox } from "../../components/fold";
import { CopyMenu, promptEntries, useSelectCtx } from "./MsgActions";
import { exportMarkdown } from "../../../lib/chatExport";
import { toPlain, toSlack } from "../../../lib/slack";

/** Children of one session; polled faster while the session runs. */
export function useSessionChildren(sessionId: string, running: boolean): ApiState<SessionChildren> {
  // A background subagent can run after its parent's turn ends: poll fast while any child runs.
  // The fast poll is a second timer: a changed useApi dep would clear the data and loop.
  const st = useApi(() => p7.sessionChildren(sessionId), [sessionId], { poll: 45_000, skip: !sessionId });
  const fast = running || !!st.data?.children?.some((c) => c.status === "running");
  const { reload } = st;
  useEffect(() => {
    if (!fast || !sessionId) return;
    const t = window.setInterval(() => !document.hidden && reload(), 5_000);
    return () => window.clearInterval(t);
  }, [fast, sessionId, reload]);
  return st;
}

interface ChildCtxValue {
  sid: string;
  byItem: Map<string, SessionChild[]>;
}
export const ChildCtx = createContext<ChildCtxValue | null>(null);

export function childCtxOf(sid: string, data: SessionChildren | null): ChildCtxValue | null {
  if (!data?.children?.length) return null;
  const byItem = new Map<string, SessionChild[]>();
  for (const c of data.children) {
    if (!c.parentItemId || c.parentAgentId) continue;
    const l = byItem.get(c.parentItemId) || [];
    l.push(c);
    byItem.set(c.parentItemId, l);
  }
  return { sid, byItem };
}

const statusCls = (s: SessionChild["status"]) => (s === "running" ? "run wpulse" : s === "failed" ? "err" : s === "completed" ? "done" : "");
const kindLabel = (c: SessionChild) => (c.kind === "subagent" ? c.subagentType || "subagent" : c.kind === "talk" ? "message" : agentName(c.agent));
const titleOf = (c: SessionChild) => c.description || (c.prompt || "").split("\n")[0] || c.command || c.sessionId || "Agent";
const childHref = (c: SessionChild) => (c.sessionId ? `/w/${encodeURIComponent(c.sessionId)}` : "");

function durOf(c: SessionChild, now: number) {
  if (!c.startedAt) return "";
  const end = c.status === "running" ? now : c.endedAt || 0;
  return end > c.startedAt ? fmtDuration(end - c.startedAt) : "";
}

/** Chips on a chat row (Agent call, codex or claude command) that open the child it started. */
export function ChildChips({ itemId }: { itemId: string }) {
  const ctx = useContext(ChildCtx);
  const [open, setOpen] = useState("");
  const list = ctx?.byItem.get(itemId);
  if (!ctx || !list?.length) return null;
  const shown = list.filter((c) => c.sessionId || c.hasTranscript);
  if (!shown.length) return null;
  const openKid = shown.find((c) => c.key === open);
  return (
    <>
      <div className="wkchips">
        {shown.map((c) => {
          const label = (
            <>
              <span className={`wsd ${statusCls(c.status)}`} aria-hidden />
              {c.kind === "subagent" ? "Subagent" : agentName(c.agent)}
              <span className="muted">{c.status}</span>
            </>
          );
          const tip = `${titleOf(c)}\nLinked by ${c.linkBasis}`;
          return c.sessionId ? (
            <Link key={c.key} className={`wkchip ${c.agent}`} to={childHref(c)} title={tip}>
              {label} <span aria-hidden>↗</span>
            </Link>
          ) : (
            <button key={c.key} className={`wkchip ${c.agent}`} onClick={() => setOpen(open === c.key ? "" : c.key)} aria-expanded={open === c.key} title={tip}>
              {label} <span aria-hidden>{open === c.key ? "▾" : "▸"}</span>
            </button>
          );
        })}
      </div>
      {openKid?.agentId && <SubTranscript sid={ctx.sid} c={openKid} />}
    </>
  );
}

interface SubFeed {
  items: OItem[];
  runs: ORun[];
  more: boolean;
}

const subagentItems = (sid: string, agentId: string, after?: number) =>
  req<WatchedItems>("GET", `/api/session/${encodeURIComponent(sid)}/children/${encodeURIComponent(agentId)}/items${after != null ? `?after=${after}` : ""}`);

/** The first read loads the newest page; polls then ask only for what changed after the cursor. */
function useSubagentFeed(sid: string, agentId: string, running: boolean) {
  const [feed, setFeed] = useState<SubFeed | null>(null);
  const [failed, setFailed] = useState<"" | "missing" | "error">("");
  useEffect(() => {
    setFeed(null);
    setFailed("");
    if (!sid || !agentId) return;
    let alive = true;
    let cursor: number | undefined;
    let more = false;
    const items = new Map<string, OItem>();
    const runs = new Map<string, ORun>();
    const read = () =>
      subagentItems(sid, agentId, cursor).then(
        (r) => {
          if (!alive) return;
          if (cursor == null || r.reset) {
            items.clear();
            runs.clear();
            more = !!r.more;
          }
          for (const it of r.items || []) items.set(it.id, it);
          for (const ru of r.runs || []) runs.set(ru.id, ru);
          cursor = r.cursor;
          setFeed({ items: [...items.values()], runs: [...runs.values()], more });
          setFailed("");
        },
        (e: unknown) => {
          if (alive && cursor == null) setFailed(isMissing(e) ? "missing" : "error");
        },
      );
    void read();
    const t = running ? window.setInterval(() => !document.hidden && void read(), 6_000) : 0;
    return () => {
      alive = false;
      window.clearInterval(t);
    };
  }, [sid, agentId, running]);
  return { feed, failed };
}

/** A Claude subagent's own transcript, inline. */
export function SubTranscript({ sid, c }: { sid: string; c: SessionChild }) {
  const { feed, failed } = useSubagentFeed(sid, c.agentId || "", c.status === "running");
  const blocks = useMemo(() => (feed ? blocksOf(feed.items, feed.runs) : []), [feed]);
  const label = `subagent: ${titleOf(c).slice(0, 60)}`;
  const entries = useMemo(() => promptEntries(blocks).map((e) => ({ ...e, label })), [blocks, label]);
  const sel = useSelectCtx();
  const setExtra = sel?.setExtra;
  const sig = entries.map((e) => `${e.id}:${e.reply?.length || 0}`).join(",");
  useEffect(() => {
    if (!setExtra) return;
    setExtra(c.key, entries.length ? entries : null);
    return () => setExtra(c.key, null);
  }, [setExtra, c.key, sig]); // eslint-disable-line react-hooks/exhaustive-deps
  const doc = () => exportMarkdown(`Subagent: ${titleOf(c)}`, entries, true);
  return (
    <div className="wcx-t" aria-label={`Transcript of ${titleOf(c)}`}>
      {entries.length > 0 && (
        <div className="wcx-tb">
          <CopyMenu
            className="wcpm"
            label="Copy transcript ▾"
            title="Copy the subagent's prompts and replies"
            opts={[
              { label: "Copy", text: () => toPlain(doc()) },
              { label: "Copy as Markdown", text: doc },
              { label: "Copy for Slack", text: () => toSlack(doc()), done: "Copied for Slack" },
            ]}
          />
        </div>
      )}
      {!feed && !failed && <div className="muted sm">Loading the subagent transcript…</div>}
      {failed && <div className="muted sm">The subagent transcript is not available.</div>}
      {feed?.more && <div className="muted sm">Earlier messages are not shown.</div>}
      {feed && blocks.length === 0 && <div className="muted sm">No messages yet.</div>}
      {blocks.map((b) => (
        <RunBlock key={b.key} run={b.run} items={b.items} now={Date.now()} />
      ))}
    </div>
  );
}

function jumpTo(itemId?: string) {
  if (!itemId) return;
  const el = document.querySelector(`[data-iid="${CSS.escape(itemId)}"]`);
  el?.scrollIntoView({ block: "center", behavior: "smooth" });
}

function ChildRow({ sid, c, nested, now, parentCwd }: { sid: string; c: SessionChild; nested: SessionChild[]; now: number; parentCwd?: string }) {
  const [open, setOpen] = useState(false);
  const dur = durOf(c, now);
  const meta = [
    c.by === "user" ? "you ran it" : "",
    c.model ? modelShort(c.model) : "",
    c.inTokens || c.outTokens ? `${fmtTok(c.inTokens)} in · ${fmtTok(c.outTokens)} out` : "",
    c.costUsd ? fmtUsd(c.costUsd, true) : "",
    c.pane ? `pane ${c.pane}` : c.hostId ? `host ${c.hostId.slice(0, 8)}` : "",
    c.cwd && c.cwd !== parentCwd ? shortCwd(c.cwd) : "",
    c.background ? "background" : "",
  ].filter(Boolean);
  const head = (
    <>
      <span className={`wsd ${statusCls(c.status)}`} aria-hidden />
      {kindLabel(c) !== "general-purpose" && <span className={`wcx-a ${c.agent}`}>{kindLabel(c)}</span>}
      <span className="trunc">{titleOf(c)}</span>
      <span className="tm">{c.status === "running" ? "running" : dur || c.status}</span>
    </>
  );
  const tip = [c.prompt || c.command, `Status: ${c.status}`, `Linked by ${c.linkBasis}`].filter(Boolean).join("\n\n");
  return (
    <div className="wcx">
      {c.sessionId ? (
        <Link className="wcx-h" to={childHref(c)} title={tip}>
          {head}
        </Link>
      ) : c.agentId && c.hasTranscript ? (
        <button className="wcx-h" onClick={() => setOpen(!open)} aria-expanded={open} title={tip}>
          {head}
        </button>
      ) : (
        <div className="wcx-h" title={tip}>
          {head}
        </div>
      )}
      {meta.length > 0 && (
        <div className="wcx-m" title={tip}>
          {meta.join(" · ")}
        </div>
      )}
      {open && c.agentId && <SubTranscript sid={sid} c={c} />}
      {nested.length > 0 && (
        <div className="ch">
          {nested.map((n) => (
            <ChildRow key={n.key} sid={sid} c={n} nested={[]} now={now} parentCwd={c.cwd || parentCwd} />
          ))}
        </div>
      )}
    </div>
  );
}

/** Side panel box: children grouped by the user message that started them. */
export function ChildrenBox({ sid, data, cwd }: { sid: string; data: SessionChildren | null; cwd?: string }) {
  const kids = data?.children || [];
  const groups = useMemo(() => {
    const nestedOf = new Map<string, SessionChild[]>();
    const agentIds = new Set(kids.map((c) => c.agentId).filter(Boolean));
    const top: SessionChild[] = [];
    for (const c of kids) {
      if (c.parentAgentId && agentIds.has(c.parentAgentId)) {
        const l = nestedOf.get(c.parentAgentId) || [];
        l.push(c);
        nestedOf.set(c.parentAgentId, l);
      } else top.push(c);
    }
    const g = new Map<string, { msg?: string; prompt: string; list: SessionChild[] }>();
    for (const c of top) {
      const k = c.parentMessageId || "";
      if (!g.has(k)) g.set(k, { msg: c.parentMessageId, prompt: c.parentPrompt || "", list: [] });
      g.get(k)!.list.push(c);
    }
    return { list: [...g.values()].reverse(), nestedOf };
  }, [kids]);
  if (!kids.length) return null;
  const now = Date.now();
  const running = kids.filter((c) => c.status === "running").length;
  return (
    <FoldBox
      id="children"
      title="Subagents and agents"
      className="wcxs"
      summary={running > 0 ? `${running} running · ${kids.length}` : `${kids.length}`}
      right={<span className="r mono">{running > 0 ? `${running} running · ${kids.length}` : kids.length}</span>}
    >
      {groups.list.map((g) => (
        <div key={g.msg || "none"} className="wkg">
          <button className="wkg-h trunc" onClick={() => jumpTo(g.msg)} disabled={!g.msg} title={g.prompt || "Started outside a message"}>
            <span aria-hidden>“</span>
            {g.prompt || (g.msg ? "Message" : "Not tied to a message")}
            <span aria-hidden>”</span>
          </button>
          {g.list.map((c) => (
            <ChildRow key={c.key} sid={sid} c={c} nested={(c.agentId && groups.nestedOf.get(c.agentId)) || []} now={now} parentCwd={cwd} />
          ))}
        </div>
      ))}
    </FoldBox>
  );
}

const KID_ROWS = 4;
/** The subagents that run now, above the composer: the chat shows work even when the parent waits. */
export function RunningKids({ sid, data }: { sid: string; data: SessionChildren | null | undefined }) {
  const list = (data?.children || []).filter((c) => c.status === "running").sort((a, b) => a.startedAt - b.startedAt);
  const now = useTick(list.length > 0);
  const [open, setOpen] = useState("");
  const [all, setAll] = useState(false);
  // An open transcript stays after its subagent ends, so it never closes under the reader.
  const openKid = (data?.children || []).find((c) => c.key === open);
  if (!list.length && !openKid) return null;
  const shown = all ? list : list.slice(0, KID_ROWS);
  return (
    <div className="wrkids">
      <div className="wrkids-h" role="status" aria-live="polite">
        {list.length > 0 && <span className="wic run wpulse" aria-hidden>●</span>}
        {list.length === 0 ? "No subagents running" : list.length === 1 ? "1 subagent running" : `${list.length} subagents running`}
      </div>
      {shown.map((c) => {
        const body = (
          <>
            <span className="wrkids-t">{c.subagentType || (c.kind === "subagent" ? "Subagent" : agentName(c.agent))}</span>
            <span className="trunc">{titleOf(c)}</span>
            <span className="wrkids-d">{durOf(c, now)}</span>
          </>
        );
        return c.sessionId ? (
          <Link key={c.key} className="wrkids-r" to={childHref(c)} title={titleOf(c)}>
            {body}
          </Link>
        ) : (
          <button key={c.key} type="button" className="wrkids-r" onClick={() => setOpen(open === c.key ? "" : c.key)} aria-expanded={open === c.key} disabled={!c.agentId} title={titleOf(c)}>
            {body}
          </button>
        );
      })}
      {list.length > KID_ROWS && (
        <button type="button" className="wrkids-more" onClick={() => setAll(!all)}>
          {all ? "Show fewer" : `Show all ${list.length}`}
        </button>
      )}
      {openKid?.agentId && (
        <>
          {openKid.status !== "running" && (
            <button type="button" className="wrkids-more" onClick={() => setOpen("")}>
              Close the {openKid.status} subagent
            </button>
          )}
          <SubTranscript sid={sid} c={openKid} />
        </>
      )}
    </div>
  );
}
