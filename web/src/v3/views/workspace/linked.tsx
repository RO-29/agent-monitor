// The "Linked" chip in a thread header: parent, child threads, subagents, other agents and messages in one list.
import { useEffect, useRef, useState } from "react";
import { Link } from "react-router-dom";
import type { SessionChild } from "../../api/p7";
import { agentName } from "../../live";
import { fmtTok, fmtUsd, modelShort } from "../../../lib/format";
import { SubTranscript } from "./children";

export interface LinkedEntry {
  key: string;
  group: "Parent" | "Threads" | "Subagents" | "Agents" | "Messages";
  label: string; // the counted noun: "subagent", "Codex", "message", "parent", "thread"
  title: string;
  sub?: string;
  dot?: string; // wsd class: run, err, done, need
  href?: string;
  inline?: { sid: string; c: SessionChild }; // a subagent with a transcript but no session of its own
  status?: SessionChild["status"];
  meta?: string; // model · tokens · cost, shown on the full page
}

const plural = (n: number, w: string) => (/^[a-z]/.test(w) && n !== 1 ? `${w}s` : w);

/** "3 subagents · 1 Codex": the largest groups first, at most three parts. */
export function linkedSummary(list: LinkedEntry[]): string {
  const by = new Map<string, number>();
  for (const e of list) by.set(e.label, (by.get(e.label) || 0) + 1);
  const parts = [...by.entries()].sort((a, b) => b[1] - a[1]);
  const words = parts.slice(0, parts.length > 3 ? 2 : 3).map(([w, n]) => `${n} ${plural(n, w)}`);
  const rest = parts.slice(words.length).reduce((n, [, c]) => n + c, 0);
  return rest ? `${words.join(" · ")} · ${rest} more` : words.join(" · ");
}

const dotOf = (s: SessionChild["status"]) => (s === "running" ? "run wpulse" : s === "failed" ? "err" : s === "completed" ? "done" : "");

/** Entries for the children of a watched session. */
export function sessionLinks(sid: string, kids: SessionChild[]): LinkedEntry[] {
  return kids.map((c) => {
    const title = c.description || (c.prompt || "").split("\n")[0] || c.command || c.sessionId || "Agent";
    const href = c.sessionId ? `/w/${encodeURIComponent(c.sessionId)}` : undefined;
    const meta = [c.model ? modelShort(c.model) : "", c.inTokens || c.outTokens ? `${fmtTok(c.inTokens)} in · ${fmtTok(c.outTokens)} out` : "", c.costUsd ? fmtUsd(c.costUsd, true) : ""].filter(Boolean).join(" · ");
    const base = { key: c.key, title, dot: dotOf(c.status), href, inline: !href && c.agentId && c.hasTranscript ? { sid, c } : undefined, sub: c.status, status: c.status, meta };
    if (c.kind === "subagent") return { ...base, group: "Subagents" as const, label: "subagent", sub: [c.subagentType !== "general-purpose" ? c.subagentType : "", c.status].filter(Boolean).join(" · ") };
    if (c.kind === "talk") return { ...base, group: "Messages" as const, label: "message", sub: `to ${agentName(c.agent)}` };
    return { ...base, group: "Agents" as const, label: agentName(c.agent) };
  });
}

const GROUPS: LinkedEntry["group"][] = ["Parent", "Threads", "Subagents", "Agents", "Messages"];

// Running first, then waiting on you, failed, stopped, the rest; completed rows fold away.
const rank = (e: LinkedEntry) =>
  e.dot?.startsWith("run") ? 0 : e.dot === "need" ? 1 : e.dot === "err" ? 2 : e.status === "stopped" || e.status === "killed" ? 3 : e.dot === "done" ? 5 : 4;
const matches = (e: LinkedEntry, q: string) => !q || `${e.title} ${e.sub || ""} ${e.label}`.toLowerCase().includes(q);

/** The grouped list with its filter: running first, completed rows folded behind "N completed". */
function LinkedList({ list, onPick, autoFocus, meta }: { list: LinkedEntry[]; onPick?: () => void; autoFocus?: boolean; meta?: boolean }) {
  const [inl, setInl] = useState("");
  const [q, setQ] = useState("");
  const [showDone, setShowDone] = useState<Record<string, boolean>>({});
  const needle = q.trim().toLowerCase();
  const row = (e: LinkedEntry) => {
    const body = (
      <>
        <span className={`wsd ${e.dot || ""}`} aria-hidden />
        {meta && e.meta ? (
          <span className="wlnk-tt">
            <span className="trunc">{e.title}</span>
            <small className="wlnk-m">{e.meta}</small>
          </span>
        ) : (
          <span className="trunc">{e.title}</span>
        )}
        {e.sub && <small>{e.sub}</small>}
        <span aria-hidden className="wlnk-go">{e.href ? "↗" : e.inline ? (inl === e.key ? "▾" : "▸") : ""}</span>
      </>
    );
    return (
      <div key={e.key}>
        {e.href ? (
          <Link className="wlnk-r" to={e.href} onClick={onPick}>
            {body}
          </Link>
        ) : e.inline ? (
          <button className="wlnk-r" aria-expanded={inl === e.key} onClick={() => setInl(inl === e.key ? "" : e.key)}>
            {body}
          </button>
        ) : (
          <div className="wlnk-r">{body}</div>
        )}
        {inl === e.key && e.inline && <SubTranscript sid={e.inline.sid} c={e.inline.c} />}
      </div>
    );
  };
  let shown = 0;
  const groups = GROUPS.map((g) => {
    const rows = list.filter((e) => e.group === g && matches(e, needle)).sort((a, b) => rank(a) - rank(b));
    if (!rows.length) return null;
    shown += rows.length;
    const live = rows.filter((e) => e.dot !== "done");
    const done = rows.filter((e) => e.dot === "done");
    // A search shows every match; otherwise completed rows sit behind "N completed".
    const fold = !needle && done.length > 1 && live.length > 0 && !showDone[g];
    return (
      <div key={g} className="wlnk-g">
        <h5>
          {g} <span className="wlnk-gn">{rows.length}</span>
        </h5>
        {(fold ? live : rows).map(row)}
        {fold && (
          <button className="wlnk-r wlnk-more" aria-expanded={false} onClick={() => setShowDone({ ...showDone, [g]: true })}>
            <span className="wsd done" aria-hidden />
            <span className="trunc">{done.length} completed</span>
            <span aria-hidden className="wlnk-go">▸</span>
          </button>
        )}
      </div>
    );
  });
  return (
    <>
      {list.length > 4 && (
        <input className="wlnk-q" type="search" value={q} autoFocus={autoFocus} onChange={(e) => setQ(e.target.value)} placeholder={`Filter ${list.length} linked`} aria-label="Filter linked threads and agents" />
      )}
      {groups}
      {needle && !shown && <p className="wlnk-none">Nothing matches “{q.trim()}”.</p>}
    </>
  );
}

export function LinkedChip({ list, page }: { list: LinkedEntry[]; page?: string }) {
  const [open, setOpen] = useState(false);
  const ref = useRef<HTMLDivElement>(null);
  useEffect(() => {
    if (!open) return;
    const off = (e: MouseEvent) => !ref.current?.contains(e.target as Node) && setOpen(false);
    const key = (e: KeyboardEvent) => e.key === "Escape" && setOpen(false);
    window.addEventListener("mousedown", off);
    window.addEventListener("keydown", key);
    return () => {
      window.removeEventListener("mousedown", off);
      window.removeEventListener("keydown", key);
    };
  }, [open]);
  if (!list.length) return null;
  const running = list.some((e) => e.dot?.startsWith("run"));
  const summary = linkedSummary(list);
  return (
    <div className="wlnk" ref={ref}>
      <button className="wlnk-b" aria-haspopup="dialog" aria-expanded={open} onClick={() => setOpen(!open)} title={`Linked: ${summary}`}>
        {running && <span className="wsd run wpulse" aria-hidden />}
        <span className="trunc wlnk-full">{summary}</span>
        <span className="wlnk-n" aria-hidden>
          <svg viewBox="0 0 16 16" width="13" height="13">
            <path d="M6.5 9.5 9.5 6.5M7 4.5l1-1a2.5 2.5 0 0 1 3.5 3.5l-1 1M9 11.5l-1 1A2.5 2.5 0 0 1 4.5 9l1-1" fill="none" stroke="currentColor" strokeWidth="1.4" strokeLinecap="round" />
          </svg>
          {list.length} {list.every((e) => e.group === "Subagents" || e.group === "Agents") ? (list.length === 1 ? "agent" : "agents") : "linked"}
        </span>
        <span aria-hidden className="wchev">▾</span>
      </button>
      {open && (
        <div className="wlnk-p" role="dialog" aria-label="Linked threads and agents">
          <div className="wlnk-h">
            <b>Linked agents</b>
            {page && (
              <span className="wlnk-acts">
                <Link className="btn sm ghost" to={page} onClick={() => setOpen(false)} title="Show the full list in place of the conversation">
                  Full page
                </Link>
                <a className="btn sm ghost" href={page} target="_blank" rel="noopener" onClick={() => setOpen(false)} title="Open the full list in a new tab">
                  New tab ↗
                </a>
              </span>
            )}
            <button className="btn sm ghost" onClick={() => setOpen(false)} aria-label="Close">
              ✕
            </button>
          </div>
          <LinkedList list={list} onPick={() => setOpen(false)} />
        </div>
      )}
    </div>
  );
}

/** /w/<thread>/linked: everything linked to the thread, as a page in place of the conversation. */
export function LinkedPage({ list, back }: { list: LinkedEntry[]; back: string }) {
  const running = list.filter((e) => e.dot?.startsWith("run")).length;
  return (
    <div className="wlnk-page" role="region" aria-label="Linked threads and agents">
      <div className="wlnk-ph">
        <div>
          <h2>Linked agents</h2>
          <p className="muted sm">{list.length ? `${linkedSummary(list)}${running ? ` · ${running} running` : ""}` : "Nothing is linked to this thread yet."}</p>
        </div>
        <Link className="btn sm" to={back}>
          Back to the conversation
        </Link>
      </div>
      <LinkedList list={list} autoFocus meta />
    </div>
  );
}
