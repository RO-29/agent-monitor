import { AgentIcon } from "../../components/AgentIcon";
// Left column: driven and watched threads grouped as Needs you, Working, Ready, Settled, with subagents nested.
import { ListHideBtn } from "./barToggles";
import { useEffect, useLayoutEffect, useMemo, useRef, useState } from "react";
import { Link, useNavigate } from "react-router-dom";
import { showToast } from "../../../app/toast";
import { fmtAgo } from "../../../lib/format";
import { pinThread, settleThread, snoozeThread, useOrch } from "../../api/orch";
import { typing } from "../../ctx";
import { overlayOpen } from "../../keys";
import { GROUPS, type WRow, type WTree } from "./rows";
import { agentKey } from "../../components/AgentIcon";
import { useV3 } from "../../live";
import { ChipRow, groupHits, hitHref, markedTerms, SessionHitRow, typedChips, useTitleOf, useTranscriptSearch, withoutChips } from "./search";
import { UsageMini } from "../../components/Usage";
import { useFleet } from "../../api/fleet";
import { FoldHead, useFold } from "../../components/fold";

const HOUR = 3600_000;
export function tomorrowAt9(now = Date.now()): number {
  const d = new Date(now);
  d.setDate(d.getDate() + 1);
  d.setHours(9, 0, 0, 0);
  return d.getTime();
}
const fmtUntil = (ms: number) => {
  const d = new Date(ms);
  const hm = d.toLocaleTimeString([], { hour: "numeric", minute: "2-digit" });
  return d.toDateString() === new Date().toDateString() ? hm : `${d.toLocaleDateString([], { weekday: "short" })} ${hm}`;
};

export type RowAction = "pin" | "settle" | "snooze1h" | "snoozeTomorrow" | "unsnooze";
export async function rowAction(r: WRow, a: RowAction) {
  const t = r.driven;
  if (!t) return;
  const settle = settleLabel(r) === "Settle";
  const was = r.snoozedUntil || 0;
  try {
    let done: string, undo: () => Promise<unknown>;
    if (a === "pin") {
      await pinThread(t.id, !t.pinned);
      done = t.pinned ? "Unpinned" : "Pinned";
      undo = () => pinThread(t.id, !!t.pinned);
    } else if (a === "settle") {
      await settleThread(t.id, settle);
      done = settle ? "Settled" : "Unsettled";
      undo = () => settleThread(t.id, !settle);
    } else {
      const until = a === "snooze1h" ? Date.now() + HOUR : a === "snoozeTomorrow" ? tomorrowAt9() : 0;
      await snoozeThread(t.id, until);
      done = until ? `Snoozed until ${fmtUntil(until)}` : "Unsnoozed";
      undo = () => snoozeThread(t.id, was);
    }
    const run = () => void undo().catch((e: Error) => showToast("Could not undo", e.message, { kind: "alert" }));
    lastUndo = { run, until: Date.now() + 8000 };
    showToast(`${done}: ${r.title}`, undefined, { action: "Undo (u)", onClick: () => ((lastUndo = null), run()) });
  } catch (e) {
    showToast("Could not update the thread", (e as Error).message, { kind: "alert" });
  }
}
// The last row action, for the u key while its toast shows.
let lastUndo: { run: () => void; until: number } | null = null;
// Settle on a settled (not snoozed) row means unsettle.
const settleLabel = (r: WRow) => (r.group === "settled" && !r.snoozedUntil ? "Unsettle" : "Settle");

function PinIcon() {
  return (
    <svg className="wpin" viewBox="0 0 16 16" width="11" height="11" aria-label="Pinned" role="img">
      <path d="M10.5 1.5 14.5 5.5 12 6.5 9.5 9 9.8 12.2 8.6 13.4 5.8 10.2 2.5 13.5 2.5 13.5 5.8 10.2 2.6 7.4 3.8 6.2 7 6.5 9.5 4z" fill="currentColor" />
    </svg>
  );
}

// The group already says Needs you or Working; a row only adds what the group does not.
function noteOf(r: WRow): { text: string; cls: string } | null {
  if (r.snoozedUntil) return { text: `until ${fmtUntil(r.snoozedUntil)}`, cls: "mute" };
  if (r.note) return { text: r.note, cls: "mute" };
  if (r.status === "Error") return { text: "Error", cls: "err" };
  if (r.status === "Subagent needs you" || r.status === "Subagents running" || r.status === "Starting") return { text: r.status, cls: "" };
  const s = r.watched?.session;
  const n = s && s.state !== "completed" && s.state !== "abandoned" ? s.cliQueued || 0 : 0;
  if (n > 0) return { text: `${n} queued`, cls: "" };
  return null;
}
const dotCls = (r: WRow) => (r.group === "need" ? "need" : r.group === "working" ? "run" : r.status === "Error" ? "err" : "");

const PLACE: Record<string, string> = { "claude-desktop": "Desktop", cli: "Terminal", "sdk-cli": "SDK", "sdk-ts": "SDK", "sdk-py": "SDK" };

/** Where a thread runs, in a word or two; a session that moved shows its path, for example "Desktop → Terminal". */
export function placeText(r: { driven?: unknown; watched?: { session?: { entrypoint?: string; places?: string[] } } }): string {
  if (r.driven) return "Thread";
  const s = r.watched?.session;
  const steps = (s?.places || []).map((p) => PLACE[p] || p);
  if (steps.length > 1) return steps.slice(-3).join(" → ");
  return PLACE[s?.entrypoint || ""] || "";
}

export function TermIcon() {
  return (
    <svg className="wtermic" viewBox="0 0 16 16" width="12" height="12" aria-label="Runs in a terminal" role="img">
      <rect x="1.5" y="2.5" width="13" height="11" rx="2" fill="none" stroke="currentColor" strokeWidth="1.3" />
      <path d="M4.5 6.2 6.8 8 4.5 9.8M8.2 10h3.3" fill="none" stroke="currentColor" strokeWidth="1.3" strokeLinecap="round" strokeLinejoin="round" />
    </svg>
  );
}

function RowBtn({ r, sel, child, onMenu }: { r: WRow; sel: boolean; child?: boolean; onMenu: (r: WRow, x: number, y: number, anchor?: HTMLElement) => void }) {
  const note = noteOf(r);
  const dc = dotCls(r);
  const menuBtn = r.driven ? (
    <button
      className="wdots"
      aria-label={`Actions for ${r.title}`}
      aria-haspopup="menu"
      onClick={(e) => {
        e.preventDefault();
        e.stopPropagation();
        const b = e.currentTarget.getBoundingClientRect();
        onMenu(r, b.right, b.bottom + 4, e.currentTarget);
      }}
    >
      ⋯
    </button>
  ) : null;
  const ctx = (e: React.MouseEvent) => {
    if (!r.driven) return;
    e.preventDefault();
    onMenu(r, e.clientX, e.clientY);
  };
  const status = r.snoozedUntil ? `Snoozed until ${fmtUntil(r.snoozedUntil)}` : r.status;
  return (
    <div className={`wth-w ${child ? "wsubrow" : ""} ${r.driven ? "has-menu" : ""}`}>
      <Link to={`/w/${encodeURIComponent(r.openId || r.id)}`} className={`wth ${child ? "wsubrow" : ""}`} role="option" aria-selected={sel} data-id={r.id} onContextMenu={ctx} title={`${r.title} · ${status}`}>
        <span className={`wsd ${dc} ${dc === "run" ? "wpulse" : ""}`} aria-hidden />
        <span className="ti">{r.title}</span>
        <span className="t">{r.at && Date.now() - r.at < 5000 ? "now" : fmtAgo(r.at, "")}</span>
        {!child && (
          <span className="r2">
            <AgentIcon agent={r.agent} size={14} />
            {r.mode === "watched" && <TermIcon />}
            {placeText(r) && placeText(r) !== "Terminal" && <span className={`wplace p-${(r.watched?.session?.entrypoint || (r.driven ? "thread" : "")).replace(/[^a-z-]/g, "")}`}>{placeText(r)}</span>}
            {r.pinned && <PinIcon />}
            {r.machine && <span className="wmach-r peer" title={`Runs on ${r.machine}`}>{r.machine}</span>}
            <span className="trunc">{r.repo || "no project"}</span>
            {r.branch && !note && <span className="trunc br">{r.branch}</span>}
            {note && <span className={`nt ${note.cls}`}>{note.text}</span>}
          </span>
        )}
        {child && <span className="r2"><AgentIcon agent={r.agent} size={13} />{note && <span className={`nt ${note.cls}`}>{note.text}</span>}</span>}
      </Link>
      {menuBtn}
    </div>
  );
}

function Subtree({ r, tree, sel, depth, open, setOpen, onMenu }: { r: WRow; tree: WTree; sel?: string; depth: number; open: Set<string>; setOpen: (id: string) => void; onMenu: (r: WRow, x: number, y: number, a?: HTMLElement) => void }) {
  const kids = depth < 3 ? tree.kids.get(r.id) || [] : [];
  const expanded = open.has(r.id) || kids.length <= 3 || r.id === sel;
  // Collapsed: still show children that need you and the open thread's branch.
  const inSel = (k: WRow): boolean => k.id === sel || (tree.kids.get(k.id) || []).some(inSel);
  const shown = expanded ? kids : kids.filter((k) => k.group === "need" || inSel(k));
  const hidden = kids.length - shown.length;
  return (
    <>
      <RowBtn r={r} sel={r.id === sel || (!!r.openId && r.openId === sel)} child={depth > 0} onMenu={onMenu} />
      {kids.length > 0 && (
        <div className="wkids" role="group" aria-label={`Subagents of ${r.title}`}>
          {shown.map((k) => (
            <Subtree key={k.id} r={k} tree={tree} sel={sel} depth={depth + 1} open={open} setOpen={setOpen} onMenu={onMenu} />
          ))}
          {kids.length > 3 && (
            <button className="wmore" onClick={() => setOpen(r.id)} aria-expanded={expanded}>
              {expanded ? "Show fewer" : `+${hidden} ${hidden === 1 ? "subagent" : "subagents"}`}
            </button>
          )}
        </div>
      )}
    </>
  );
}

function RowMenu({ r, x, y, onClose }: { r: WRow; x: number; y: number; onClose: () => void }) {
  const ref = useRef<HTMLDivElement>(null);
  const [pos, setPos] = useState({ left: x, top: y });
  useLayoutEffect(() => {
    const el = ref.current;
    if (!el) return;
    const w = el.offsetWidth;
    const h = el.offsetHeight;
    setPos({ left: Math.max(8, Math.min(x - w, window.innerWidth - w - 8)), top: Math.max(8, Math.min(y, window.innerHeight - h - 8)) });
    el.querySelector<HTMLButtonElement>("button")?.focus();
  }, [x, y]);
  useEffect(() => {
    const down = (e: MouseEvent) => {
      if (!ref.current?.contains(e.target as Node)) onClose();
    };
    const key = (e: KeyboardEvent) => {
      if (e.key === "Escape") onClose();
    };
    window.addEventListener("mousedown", down);
    window.addEventListener("keydown", key);
    window.addEventListener("resize", onClose);
    return () => {
      window.removeEventListener("mousedown", down);
      window.removeEventListener("keydown", key);
      window.removeEventListener("resize", onClose);
    };
  }, [onClose]);
  const act = (a: RowAction) => {
    onClose();
    void rowAction(r, a);
  };
  return (
    <div className="wmenu" role="menu" ref={ref} style={pos} aria-label={`Actions for ${r.title}`}>
      <button role="menuitem" onClick={() => act("pin")}>
        {r.pinned ? "Unpin" : "Pin"} <span className="kbd">p</span>
      </button>
      <button role="menuitem" onClick={() => act("settle")}>
        {settleLabel(r)} <span className="kbd">e</span>
      </button>
      <div className="sep" />
      <div className="lbl">Snooze</div>
      <button role="menuitem" onClick={() => act("snooze1h")}>
        For 1 hour <span className="kbd">z</span>
      </button>
      <button role="menuitem" onClick={() => act("snoozeTomorrow")}>
        Until tomorrow 9:00
      </button>
      {r.snoozedUntil && (
        <button role="menuitem" onClick={() => act("unsnooze")}>
          Clear snooze
        </button>
      )}
    </div>
  );
}

const SETTLED_SHOWN = 8;
const SETTLED_PAGE = 100;

/** Rows whose own text or any descendant matches q; typed agent: and repo: chips filter too. */
function filterTree(tree: WTree, q: string): WTree {
  const k = withoutChips(q).toLowerCase();
  const chips = typedChips(q).filter((c) => c.key === "agent" || c.key === "repo");
  if (!k && !chips.length) return tree;
  const hit = (r: WRow) =>
    (!k || `${r.title} ${r.repo} ${r.branch} ${r.agent}`.toLowerCase().includes(k)) &&
    chips.every((c) => (c.key === "agent" ? agentKey(r.agent) === agentKey(c.value) : (r.repo || "").toLowerCase() === c.value));
  const memo = new Map<string, boolean>();
  const keep = (r: WRow, d = 0): boolean => {
    if (memo.has(r.id)) return memo.get(r.id)!;
    const v = hit(r) || (d < 4 && (tree.kids.get(r.id) || []).some((c) => keep(c, d + 1)));
    memo.set(r.id, v);
    return v;
  };
  const top = new Map([...tree.top].map(([g, list]) => [g, list.filter((r) => keep(r))]));
  const kids = new Map([...tree.kids].map(([id, list]) => [id, list.filter((r) => keep(r))]));
  return { top, kids };
}

/** A watched session with no message (an empty Cursor "New Agent", for one) is noise in the list. */
const isEmptyRow = (r: WRow) => r.mode === "watched" && r.watched?.session?.messageCount === 0 && (r.watched.thread?.sessions?.length ?? 1) <= 1;

function dropEmpty(tree: WTree, keep: string | undefined): { tree: WTree; n: number } {
  let n = 0;
  const top = new Map(
    [...tree.top].map(([g, list]) => [
      g,
      list.filter((r) => {
        const drop = isEmptyRow(r) && r.id !== keep;
        if (drop) n++;
        return !drop;
      }),
    ]),
  );
  return { tree: { top, kids: tree.kids }, n };
}

/** Rows of one machine only; "" keeps all. Local rows have no machine field. */
function byMachine(tree: WTree, m: string, self: string): WTree {
  if (!m) return tree;
  const keep = (r: WRow) => (r.machine || self) === m;
  return { top: new Map([...tree.top].map(([g, list]) => [g, list.filter(keep)])), kids: tree.kids };
}

const TRANSCRIPT_SHOWN = 20;

/** A thread group whose header folds it; closed keeps the open thread in view. A filter opens every group. */
function Group({ gkey, label, count, filtering, keep, defClosed, children }: { gkey: string; label: string; count: number; filtering: boolean; keep: WRow[]; defClosed?: boolean; children: (only: WRow[] | null) => React.ReactNode }) {
  const [stored, toggle] = useFold(`threads:${gkey}`, defClosed);
  const closed = stored && !filtering;
  return (
    <div className={`wgrp-w ${closed ? "fold-c" : ""}`}>
      <div className={`wgrp ${gkey}`}>
        <FoldHead closed={closed} onToggle={toggle}>
          {label}
        </FoldHead>
        {closed && <span className="wgrp-n">{count}</span>}
      </div>
      {children(closed ? keep : null)}
    </div>
  );
}

export function ThreadList({ rows, tree, sel, notice, liveLabel }: { rows: WRow[]; tree: WTree; sel?: string; notice?: React.ReactNode; liveLabel: string }) {
  const [menu, setMenu] = useState<{ r: WRow; x: number; y: number; anchor?: HTMLElement } | null>(null);
  const [open, setOpenSet] = useState<Set<string>>(new Set());
  const [q, setQ] = useState("");
  const [settledShown, setSettledShown] = useState(SETTLED_SHOWN);
  const fleet = useFleet();
  const [onlyM, setOnlyM] = useState(() => {
    try {
      return localStorage.getItem("threads:machine") || "";
    } catch {
      return "";
    }
  });
  const pickM = (m: string) => {
    setOnlyM(m);
    try {
      localStorage.setItem("threads:machine", m);
    } catch {
      /* private mode */
    }
  };
  const selfName = fleet.machines.find((m) => m.self)?.name || "";
  const mFilter = fleet.machines.length > 1 && fleet.machines.some((m) => m.name === onlyM) ? onlyM : "";
  const [showEmpty, setShowEmpty] = useState(() => {
    try {
      return localStorage.getItem("threads:empty") === "1";
    } catch {
      return false;
    }
  });
  const flipEmpty = () => {
    setShowEmpty(!showEmpty);
    try {
      localStorage.setItem("threads:empty", showEmpty ? "0" : "1");
    } catch {
      /* private mode */
    }
  };
  const lean = useMemo(() => (showEmpty ? { tree, n: 0 } : dropEmpty(tree, sel)), [tree, showEmpty, sel]);
  const emptyN = useMemo(() => (showEmpty ? dropEmpty(tree, sel).n : lean.n), [tree, showEmpty, sel, lean.n]);
  const shown = useMemo(() => byMachine(filterTree(lean.tree, q), mFilter, selfName), [lean.tree, q, mFilter, selfName]);
  const active = (shown.top.get("need")?.length || 0) + (shown.top.get("working")?.length || 0) > 0;
  const nav = useNavigate();
  const v = useV3();
  const orchStatus = useOrch().status;
  const ts = useTranscriptSearch(q);
  const titleOf = useTitleOf();
  const groups = useMemo(() => groupHits(ts.res?.hits || [], v.threadOf), [ts.res, v.threadOf]);
  const hrefOf = (g: (typeof groups)[number]) => hitHref(g.hits[0], markedTerms(g.hits), v.threadOf);
  const toggle = (id: string) =>
    setOpenSet((s) => {
      const n = new Set(s);
      if (n.has(id)) n.delete(id);
      else n.add(id);
      return n;
    });
  const closeMenu = () => {
    menu?.anchor?.focus();
    setMenu(null);
  };

  // e settles, p pins, z snoozes the selected driven row for 1 hour (z again clears it).
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "u" && lastUndo && Date.now() < lastUndo.until && !typing(e) && !overlayOpen() && !e.metaKey && !e.ctrlKey && !e.altKey) {
        e.preventDefault();
        const u = lastUndo;
        lastUndo = null;
        u.run();
        return;
      }
      if (typing(e) || overlayOpen() || e.repeat || e.metaKey || e.ctrlKey || e.altKey || !["e", "p", "z"].includes(e.key)) return;
      const r = rows.find((x) => x.id === sel);
      if (!r?.driven) return;
      e.preventDefault();
      void rowAction(r, e.key === "p" ? "pin" : e.key === "e" ? "settle" : r.snoozedUntil ? "unsnooze" : "snooze1h");
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [rows, sel]);

  const total = GROUPS.reduce((n, g) => n + (shown.top.get(g.key) || []).length, 0);
  return (
    <aside className="wlist-col" aria-label="Threads">
      <div className="wlist-top">
        <div className="wbrand">
          <span>Threads</span>
          {liveLabel && <small>{liveLabel}</small>}
          <ListHideBtn />
          <Link to="/w/new" className="wnew" title="New thread" aria-label="New thread">
            <svg viewBox="0 0 16 16" width="14" height="14" aria-hidden>
              <path d="M8 3v10M3 8h10" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round" />
            </svg>
          </Link>
        </div>
        <input
          className="wfind"
          type="search"
          value={q}
          placeholder="Filter threads and transcripts"
          aria-label="Filter threads and search transcripts"
          onChange={(e) => setQ(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === "Escape") {
              setQ("");
              e.currentTarget.blur();
            } else if (e.key === "Enter" && q.trim()) {
              e.preventDefault();
              const first = GROUPS.map((g) => (shown.top.get(g.key) || [])[0]).find(Boolean);
              if (first && withoutChips(q)) nav(`/w/${encodeURIComponent(first.id)}`);
              else if (groups[0] && ts.q === q.trim() && !ts.loading) nav(hrefOf(groups[0]));
              else if (first) nav(`/w/${encodeURIComponent(first.id)}`);
            }
          }}
        />
        {fleet.machines.length > 1 && (
          <div className="wmachs" role="radiogroup" aria-label="Machine">
            {[{ name: "", label: "All machines" }, ...fleet.machines.map((m) => ({ name: m.name, label: m.self ? "This Mac" : m.name, off: m.status !== "online" }))].map((m) => (
              <button key={m.name || "all"} role="radio" aria-checked={mFilter === m.name} className={`wmach-f ${mFilter === m.name ? "on" : ""} ${"off" in m && m.off ? "off" : ""}`} onClick={() => pickM(m.name)} title={"off" in m && m.off ? `${m.name} does not answer` : m.name || undefined}>
                {m.label}
              </button>
            ))}
          </div>
        )}
      </div>
      {notice}
      <div className="wlist" role="listbox" aria-label="Threads">
        {total === 0 && !q && <div className="wnone">{orchStatus === "loading" ? "Loading threads…" : "No threads yet. Start one with the + button."}</div>}
        {total === 0 && q && <div className="wnone">No thread title matches.</div>}
        {GROUPS.map((g) => {
          const all = shown.top.get(g.key) || [];
          if (!all.length) return null;
          const cut = g.key === "settled" && !q && all.length > settledShown + 2;
          const list = cut ? all.filter((r, i) => i < settledShown || r.id === sel) : all;
          return (
            <Group key={g.key} gkey={g.key} label={g.label} count={all.length} filtering={!!q} defClosed={g.key === "settled" && active} keep={list.filter((r) => r.id === sel || r.openId === sel)}>
              {(rowsIn) => (
                <>
                  {(rowsIn || list).map((r) => (
                    <Subtree key={r.id} r={r} tree={shown} sel={sel} depth={0} open={open} setOpen={toggle} onMenu={(r, x, y, anchor) => setMenu({ r, x, y, anchor })} />
                  ))}
                  {cut && !rowsIn && (
                    <button className="wmore" onClick={() => setSettledShown((n) => n + SETTLED_PAGE)}>
                      Show {Math.min(SETTLED_PAGE, all.length - list.length)} more of {all.length - list.length}
                    </button>
                  )}
                </>
              )}
            </Group>
          );
        })}
        {emptyN > 0 && !q && (
          <button className="wmore" onClick={flipEmpty}>
            {showEmpty ? `Hide ${emptyN} empty sessions` : `Show ${emptyN} empty sessions`}
          </button>
        )}
        {q.trim().length >= 2 && (
          <div className="wgrp-w wtrans" aria-busy={ts.loading}>
            <div className="wgrp">
              <span>In transcripts</span>
              {ts.res && <span className="wgrp-n">{groups.length}{ts.res.total > ts.res.hits.length ? "+" : ""}</span>}
            </div>
            {ts.res && <ChipRow chips={ts.res.query.chips} />}
            {ts.loading && !ts.res && <div className="wnone">Searching transcripts…</div>}
            {ts.missing && <div className="wnone">Transcript search is not available on this daemon.</div>}
            {ts.err && !ts.missing && <div className="wnone err">Search failed: {ts.err}</div>}
            {ts.res && !groups.length && <div className="wnone">No transcript matches.</div>}
            {groups.slice(0, TRANSCRIPT_SHOWN).map((g) => (
              <SessionHitRow key={g.session} g={g} title={titleOf(g)} href={hrefOf(g)} compact />
            ))}
            {ts.res && (
              <Link className="wmore" to={`/w/search?q=${encodeURIComponent(q.trim())}`}>
                {groups.length > TRANSCRIPT_SHOWN ? `All ${groups.length} sessions in Search` : "Open in Search"}
              </Link>
            )}
          </div>
        )}
      </div>
      <UsageMini focus={rows.find((r) => r.id === sel || (!!r.openId && r.openId === sel))?.agent} />
      {menu && <RowMenu r={rows.find((x) => x.id === menu.r.id) || menu.r} x={menu.x} y={menu.y} onClose={closeMenu} />}
    </aside>
  );
}
