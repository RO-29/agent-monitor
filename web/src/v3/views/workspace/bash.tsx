// Bash mode history: ! commands of this session, then recent ones from every session, and the History popover.
import { useCallback, useEffect, useRef, useState } from "react";
import { req } from "../../api/orch";
import { copy } from "../../../lib/clipboard";
import { fmtAgo } from "../../../lib/format";
import { showToast } from "../../../app/toast";

export interface CmdRun {
  cmd: string;
  at?: number;
  exitCode?: number;
  output?: string;
  bg?: boolean;
  sessionId?: string;
  cwd?: string;
}
const enc = encodeURIComponent;
const items = (r: { items?: CmdRun[] } | CmdRun[]) => (Array.isArray(r) ? r : r.items || []).filter((x) => x && typeof x.cmd === "string" && x.cmd.trim());
const sessionCmds = (sid: string, limit = 50) => req<{ items?: CmdRun[] }>("GET", `/api/session/${enc(sid)}/commands?limit=${limit}`).then(items);
const recentCmds = (limit = 50) => req<{ items?: CmdRun[] }>("GET", `/api/commands/recent?limit=${limit}`).then(items);

export interface CmdHistory {
  /** Newest first, unique by command text. */
  list: CmdRun[];
  missing: boolean;
  load: () => Promise<CmdRun[]>;
}

/** Loads on first use and again after 20 s; an older daemon gives an empty list. */
export function useCmdHistory(sid: string, on: boolean): CmdHistory {
  const [st, setSt] = useState<{ sid: string; list: CmdRun[]; missing: boolean; at: number }>({ sid, list: [], missing: false, at: 0 });
  const live = useRef(st);
  live.current = st;
  const load = useCallback(async () => {
    const c = live.current;
    if (c.sid === sid && c.at && Date.now() - c.at < 20_000) return c.list;
    const [mine, all] = await Promise.allSettled([sid ? sessionCmds(sid) : Promise.resolve([]), recentCmds()]);
    const a = mine.status === "fulfilled" ? mine.value : [];
    const b = all.status === "fulfilled" ? all.value : [];
    const byNew = (x: CmdRun[]) => [...x].sort((p, q) => (q.at || 0) - (p.at || 0));
    const seen = new Set<string>();
    const list = [...byNew(a), ...byNew(b)].filter((x) => {
      const k = x.cmd.trim();
      if (seen.has(k)) return false;
      seen.add(k);
      return true;
    });
    const next = { sid, list, missing: mine.status === "rejected" && all.status === "rejected", at: Date.now() };
    setSt(next);
    return list;
  }, [sid]);
  useEffect(() => {
    if (on) void load();
  }, [on, load]);
  const cur = st.sid === sid ? st : { list: [], missing: false };
  return { list: cur.list, missing: cur.missing, load };
}

/** Footer button and popover: recent commands with Copy, Edit and Run again (two taps). */
export function HistoryButton({ h, onEdit, onRun, disabled }: { h: CmdHistory; onEdit: (cmd: string) => void; onRun: (cmd: string) => void; disabled?: boolean }) {
  const [open, setOpen] = useState(false);
  const [armed, setArmed] = useState("");
  const ref = useRef<HTMLDivElement>(null);
  useEffect(() => {
    if (!open) return;
    void h.load();
    const off = (e: MouseEvent) => !ref.current?.contains(e.target as Node) && setOpen(false);
    const key = (e: KeyboardEvent) => {
      if (e.key !== "Escape") return;
      e.stopPropagation();
      setOpen(false);
    };
    window.addEventListener("mousedown", off);
    window.addEventListener("keydown", key, true);
    return () => {
      window.removeEventListener("mousedown", off);
      window.removeEventListener("keydown", key, true);
    };
  }, [open]); // eslint-disable-line react-hooks/exhaustive-deps
  useEffect(() => {
    if (!armed) return;
    const t = window.setTimeout(() => setArmed(""), 3000);
    return () => window.clearTimeout(t);
  }, [armed]);
  if (h.missing) return null;
  return (
    <div className="whist" ref={ref}>
      <button type="button" className="whist-b" aria-haspopup="dialog" aria-expanded={open} onClick={() => setOpen(!open)} title="Commands you ran with ! (↑ in an empty box recalls them)">
        History
      </button>
      {open && (
        <div className="whist-m" role="dialog" aria-label="Command history">
          <div className="whist-h">
            <span>Recent commands</span>
            <span className="sp" />
            <span>↑↓ in the box</span>
          </div>
          {!h.list.length && <div className="whist-e">No ! commands yet. Type ! to run one in the terminal.</div>}
          {h.list.slice(0, 40).map((x, i) => {
            const k = `${i}:${x.cmd}`;
            return (
              <div key={k} className="whist-r">
                <code className="whist-c" title={[x.cmd, x.cwd, x.at ? new Date(x.at).toLocaleString() : ""].filter(Boolean).join("\n")}>
                  {x.cmd}
                </code>
                {x.exitCode != null && <span className={`whist-x ${x.exitCode ? "bad" : "muted"}`}>{x.exitCode ? `exit ${x.exitCode}` : "ok"}</span>}
                {x.at ? <span className="muted xs">{fmtAgo(x.at)}</span> : null}
                <button
                  type="button"
                  className="btn sm ghost"
                  onClick={async () => {
                    if (!(await copy(x.cmd))) showToast("Copy failed", "The browser blocked the clipboard.", { kind: "alert" });
                    else showToast("Copied", x.cmd.slice(0, 80));
                  }}
                >
                  Copy
                </button>
                <button
                  type="button"
                  className="btn sm ghost"
                  onClick={() => {
                    onEdit(x.cmd);
                    setOpen(false);
                  }}
                >
                  Edit
                </button>
                <button
                  type="button"
                  className={`btn sm ${armed === k ? "pri" : "ghost"}`}
                  disabled={disabled}
                  onClick={() => {
                    if (armed !== k) return setArmed(k);
                    setArmed("");
                    setOpen(false);
                    onRun(x.cmd);
                  }}
                  title="Runs it again in the terminal of this session"
                >
                  {armed === k ? "Run it" : "Run again"}
                </button>
              </div>
            );
          })}
        </div>
      )}
    </div>
  );
}
