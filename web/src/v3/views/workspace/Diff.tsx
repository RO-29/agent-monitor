// Diff view: overlay over the chat column with a file list and a unified diff renderer (no dependency).
import { Fragment, useEffect, useMemo, useRef, useState } from "react";
import { gitApi, type ODiff, type ODiffFile } from "../../api/orch";
import { gitErrorToast, shortSha, type DiffNav } from "./git";

type Row =
  | { t: "hunk"; text: string }
  | { t: "ctx" | "add" | "del"; o?: number; n?: number; text: string }
  | { t: "note"; text: string }
  | { t: "fold"; id: number; rows: Row[] };

const CTX_KEEP = 3;
const CTX_FOLD = 8;

/** Parses the patch text of one file into rows with old/new line numbers; long unchanged runs fold. */
export function parsePatch(patch: string): Row[] {
  const out: Row[] = [];
  let o = 0;
  let n = 0;
  let inHunk = false;
  let ctx: Row[] = [];
  let fid = 0;
  const flush = (end: boolean) => {
    if (ctx.length > CTX_FOLD + (end ? 0 : CTX_KEEP)) {
      const head = out.length && out[out.length - 1].t !== "hunk" ? ctx.slice(0, CTX_KEEP) : [];
      const tail = end ? [] : ctx.slice(-CTX_KEEP);
      out.push(...head, { t: "fold", id: ++fid, rows: ctx.slice(head.length, ctx.length - tail.length) }, ...tail);
    } else out.push(...ctx);
    ctx = [];
  };
  for (const line of patch.split("\n")) {
    const h = line.match(/^@@ -(\d+)(?:,\d+)? \+(\d+)(?:,\d+)? @@(.*)$/);
    if (h) {
      if (inHunk) flush(true);
      o = +h[1];
      n = +h[2];
      inHunk = true;
      out.push({ t: "hunk", text: line });
      continue;
    }
    if (!inHunk) continue; // diff --git, index, ---, +++ headers
    if (line.startsWith("\\")) {
      if (ctx.length) flush(false);
      out.push({ t: "note", text: line.slice(2) });
    } else if (line.startsWith("+")) {
      flush(false);
      out.push({ t: "add", n: n++, text: line.slice(1) });
    } else if (line.startsWith("-")) {
      flush(false);
      out.push({ t: "del", o: o++, text: line.slice(1) });
    } else if (line.startsWith(" ") || line === "") {
      ctx.push({ t: "ctx", o: o++, n: n++, text: line.slice(1) });
    }
  }
  // A trailing empty string from the final newline is not a context line.
  const last = ctx[ctx.length - 1];
  if (last && last.t === "ctx" && last.text === "" && patch.endsWith("\n")) ctx.pop();
  flush(true);
  return out;
}

const STATUS: Record<string, { l: string; cls: string; name: string }> = {
  added: { l: "A", cls: "a", name: "added" },
  untracked: { l: "A", cls: "a", name: "new" },
  modified: { l: "M", cls: "m", name: "modified" },
  deleted: { l: "D", cls: "d", name: "deleted" },
  renamed: { l: "R", cls: "r", name: "renamed" },
  copied: { l: "C", cls: "r", name: "copied" },
  typechange: { l: "T", cls: "m", name: "type changed" },
};
const LETTER: Record<string, string> = { A: "added", "??": "untracked", M: "modified", D: "deleted", R: "renamed", C: "copied", T: "typechange" };
export function statusInfo(s: string) {
  const k = STATUS[s] ? s : LETTER[s] || LETTER[s?.[0]] || "modified";
  return STATUS[k] || STATUS.modified;
}

function DiffBody({ f, wrap }: { f: ODiffFile; wrap: boolean }) {
  const rows = useMemo(() => (f.binary ? [] : parsePatch(f.patch || "")), [f]);
  const [open, setOpen] = useState<Set<number>>(new Set());
  useEffect(() => setOpen(new Set()), [f]);
  const st = statusInfo(f.status);
  const renderRow = (r: Row, i: number): React.ReactNode => {
    if (r.t === "hunk") {
      return (
        <div key={i} className="dl hk">
          <span className="ln" />
          <span className="ln" />
          <span className="tx">{r.text}</span>
        </div>
      );
    }
    if (r.t === "note") {
      return (
        <div key={i} className="dl nt">
          <span className="ln" />
          <span className="ln" />
          <span className="tx">{r.text}</span>
        </div>
      );
    }
    if (r.t === "fold") {
      if (open.has(r.id)) return <Fragment key={i}>{r.rows.map((x, j) => renderRow(x, i * 10000 + j))}</Fragment>;
      return (
        <button key={i} className="dl fold" onClick={() => setOpen(new Set(open).add(r.id))}>
          <span className="ln">⋯</span>
          <span className="ln" />
          <span className="tx">Show {r.rows.length} unchanged lines</span>
        </button>
      );
    }
    return (
      <div key={i} className={`dl ${r.t}`}>
        <span className="ln">{r.o ?? ""}</span>
        <span className="ln">{r.n ?? ""}</span>
        <span className="tx">
          <i aria-hidden>{r.t === "add" ? "+" : r.t === "del" ? "-" : " "}</i>
          {r.text}
        </span>
      </div>
    );
  };
  return (
    <div className="dfile">
      <div className="dfile-h">
        <span className={`dst ${st.cls}`} title={st.name}>
          {st.l}
        </span>
        <span className="p mono" title={f.path}>
          {f.oldPath && f.oldPath !== f.path ? (
            <>
              <span className="muted">{f.oldPath} → </span>
              {f.path}
            </>
          ) : (
            f.path
          )}
        </span>
        <span className="n mono">
          {f.add > 0 && <span className="add">+{f.add}</span>} {f.del > 0 && <span className="del">−{f.del}</span>}
        </span>
      </div>
      {f.truncated && !f.binary && f.patch && <div className="dnote warn">This file is cut at 120 KB. The rest of its diff is not shown.</div>}
      {f.binary ? (
        <div className="dnote">Binary file. agent-monitor does not show its content.</div>
      ) : !f.patch ? (
        <div className="dnote">{f.status === "renamed" && !f.add && !f.del ? "Renamed with no content change." : "No patch text for this file. The size cap cut it."}</div>
      ) : (
        <div className={`dcode ${wrap ? "wrap" : ""}`} role="table" aria-label={`Diff of ${f.path}`}>
          {rows.map(renderRow)}
        </div>
      )}
    </div>
  );
}

const refLabel = (r: string) => {
  if (r.startsWith("since:")) return "session start";
  if (r === "base") return "thread start";
  if (r === "worktree") return "working tree";
  if (r === "head") return "HEAD";
  if (!r) return "?";
  const m = r.match(/refs\/am\/cp\/[^/]+\/(\d+)-(start|end|restore)(?:-\d+)?$/) || r.match(/^(\d+)-(start|end|restore)(?:-\d+)?$/);
  if (m) return `turn ${m[1]} ${m[2]}`;
  return /^[0-9a-f]{12,}$/.test(r) ? shortSha(r) : r.replace(/^refs\//, "");
};

// "cwd:/path" is a watched session's folder: GET /api/orch/git/diff?cwd= (head to worktree only).
const loadDiff = (id: string, q: { from?: string; to?: string; path?: string; run?: number }) => (id.startsWith("cwd:") ? gitApi().cwdDiff(id.slice(4), q) : gitApi().diff(id, q));

const FOCUSABLE = 'button:not([disabled]), [href], input:not([disabled]), select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex="-1"])';
/** Keeps Tab and Shift+Tab inside the dialog. */
export function trapTab(e: KeyboardEvent, box: HTMLElement) {
  const els = [...box.querySelectorAll<HTMLElement>(FOCUSABLE)].filter((x) => x.offsetParent !== null);
  if (!els.length) {
    e.preventDefault();
    box.focus();
    return;
  }
  const first = els[0];
  const last = els[els.length - 1];
  const a = document.activeElement;
  if (!box.contains(a)) {
    e.preventDefault();
    first.focus();
  } else if (e.shiftKey && (a === first || a === box)) {
    e.preventDefault();
    last.focus();
  } else if (!e.shiftKey && a === last) {
    e.preventDefault();
    first.focus();
  }
}

export function DiffView({ threadId, nav, onClose, onFile }: { threadId: string; nav: DiffNav; onClose: () => void; onFile: (f: string) => void }) {
  const [diff, setDiff] = useState<ODiff | null>(null);
  const [err, setErr] = useState("");
  const [loading, setLoading] = useState(true);
  const [wrap, setWrap] = useState(() => {
    try {
      return localStorage.getItem("am-diff-wrap") === "1";
    } catch {
      return false;
    }
  });
  const [single, setSingle] = useState<ODiffFile | null>(null);
  const right = useRef<HTMLDivElement>(null);
  const box = useRef<HTMLDivElement>(null);
  const pathKey = nav.paths?.join("\n") || "";

  useEffect(() => {
    let off = false;
    setLoading(true);
    setErr("");
    setDiff(null);
    setSingle(null);
    loadDiff(threadId, nav.run != null ? { run: nav.run } : { from: nav.from, to: nav.to, path: pathKey || undefined })
      .then((d) => !off && setDiff(d))
      .catch((e) => {
        if (off) return;
        setErr((e as Error).message);
        gitErrorToast("Could not load the diff", e);
      })
      .finally(() => !off && setLoading(false));
    return () => {
      off = true;
    };
  }, [threadId, nav.from, nav.to, nav.run, pathKey]);

  const files = diff?.files || [];
  const cur = single && single.path === nav.file ? single : files.find((f) => f.path === nav.file) || (nav.file ? undefined : files[0]);
  const curPath = cur?.path || nav.file;
  const idx = files.findIndex((f) => f.path === curPath);
  const add = files.reduce((n, f) => n + Math.max(0, f.add || 0), 0);
  const del = files.reduce((n, f) => n + Math.max(0, f.del || 0), 0);

  const loadOne = async () => {
    if (!nav.file) return;
    try {
      const d = await loadDiff(threadId, nav.run != null ? { run: nav.run, path: nav.file } : { from: nav.from, to: nav.to, path: nav.file });
      const f = d.files.find((x) => x.path === nav.file) || d.files[0];
      if (f) setSingle(f);
      else setErr(`${nav.file} has no changes in this range.`);
    } catch (e) {
      gitErrorToast("Could not load the file", e);
    }
  };

  useEffect(() => {
    right.current?.scrollTo({ top: 0 });
  }, [curPath]);
  useEffect(() => {
    const back = document.activeElement as HTMLElement | null;
    box.current?.focus();
    return () => {
      if (back && back.isConnected) back.focus();
    };
  }, []);

  // Capture phase so the thread list's j/k does not also move.
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Tab" && box.current) {
        trapTab(e, box.current);
        return;
      }
      const tg = e.target as HTMLElement | null;
      if (tg && (tg.tagName === "INPUT" || tg.tagName === "TEXTAREA" || tg.tagName === "SELECT" || tg.isContentEditable)) return;
      if (e.metaKey || e.ctrlKey || e.altKey) return;
      if (e.key === "Escape") {
        e.preventDefault();
        e.stopPropagation();
        onClose();
      } else if (e.key === "j" || e.key === "k") {
        e.preventDefault();
        e.stopPropagation();
        if (!files.length) return;
        const i = Math.max(0, Math.min(files.length - 1, idx < 0 ? 0 : idx + (e.key === "j" ? 1 : -1)));
        onFile(files[i].path);
      } else if (e.key === "w") {
        e.stopPropagation();
        toggleWrap();
      }
    };
    window.addEventListener("keydown", onKey, true);
    return () => window.removeEventListener("keydown", onKey, true);
  });
  const toggleWrap = () => {
    setWrap((w) => {
      try {
        localStorage.setItem("am-diff-wrap", w ? "0" : "1");
      } catch {
        /* storage off */
      }
      return !w;
    });
  };

  return (
    <div className="wdiff" role="dialog" aria-modal="true" aria-label="Diff" tabIndex={-1} ref={box}>
      <div className="wdiff-h">
        <button className="btn sm ghost" onClick={onClose} aria-label="Close diff" title="Close (Esc)">
          ✕
        </button>
        <b className="trunc">
          {nav.label ? `${nav.label} · ` : ""}
          <span className="mono rng">{nav.run != null ? `turn ${nav.run} changes` : `${refLabel(nav.from)} → ${refLabel(nav.to)}`}</span>
        </b>
        {diff && (
          <span className="mono sm nowrap hidephone">
            {files.length} {files.length === 1 ? "file" : "files"} <span className="add">+{add}</span> <span className="del">−{del}</span>
          </span>
        )}
        <span className="sp" />
        <label className="wwrap" title="Wrap long lines (w)">
          <input type="checkbox" checked={wrap} onChange={toggleWrap} /> Wrap
        </label>
        <span className="kbdhint hidephone">j/k file · Esc close</span>
      </div>
      {diff?.truncated && <div className="dnote warn top">The diff is cut at 400 KB. Some files show no patch. Open a single file to see more of it.</div>}
      {err && !diff ? (
        <div className="state">
          <b>Could not load the diff</b>
          <div>{err}</div>
        </div>
      ) : loading || !diff ? (
        <div className="state">Loading the diff…</div>
      ) : files.length === 0 ? (
        <div className="state">
          <b>No changes</b>
          <div>{nav.run != null ? `Turn ${nav.run} changed no files.` : `Nothing differs between ${refLabel(nav.from)} and ${refLabel(nav.to)}.`}</div>
        </div>
      ) : (
        <div className="wdiff-b">
          <select className="inp dsel" value={curPath} onChange={(e) => onFile(e.target.value)} aria-label="File">
            {files.map((f) => (
              <option key={f.path} value={f.path}>
                {statusInfo(f.status).l} {f.path} {f.binary ? "(binary)" : `(+${Math.max(0, f.add)} −${Math.max(0, f.del)})`}
              </option>
            ))}
          </select>
          <nav className="dlist" aria-label="Changed files">
            {files.map((f) => {
              const st = statusInfo(f.status);
              const base = f.path.split("/").pop();
              const dir = f.path.slice(0, f.path.length - (base?.length || 0)).replace(/\/$/, "");
              return (
                <button key={f.path} className={`dli ${f.path === curPath ? "on" : ""}`} onClick={() => onFile(f.path)} title={f.oldPath ? `${f.oldPath} → ${f.path}` : f.path} aria-current={f.path === curPath}>
                  <span className={`dst ${st.cls}`}>{st.l}</span>
                  <span className="nm">
                    <b>{base}</b>
                    {dir && <small>{dir}</small>}
                  </span>
                  <span className="n mono">{f.binary ? <span className="muted">bin</span> : <>{f.add > 0 && <span className="add">+{f.add}</span>} {f.del > 0 && <span className="del">−{f.del}</span>}</>}</span>
                </button>
              );
            })}
          </nav>
          <div className="dright" ref={right}>
            {cur ? (
              <DiffBody f={cur} wrap={wrap} />
            ) : (
              <div className="state">
                <b>{nav.file} is not in this diff</b>
                <div>The size cap may have cut it, or it has no changes in this range.</div>
                <button className="btn sm" onClick={loadOne}>
                  Load this file only
                </button>
              </div>
            )}
          </div>
        </div>
      )}
    </div>
  );
}
