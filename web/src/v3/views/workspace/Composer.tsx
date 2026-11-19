// Composer with @ file, $ skill and / command pickers, and file attachments (picker, paste, drop, upload progress).
import { useCallback, useEffect, useId, useLayoutEffect, useMemo, useRef, useState } from "react";
import { usePromptHistory } from "./prompts";
import { showToast } from "../../../app/toast";
import { FILE_ACCEPT, fileAccepted, IMAGE_TYPES, MAX_FILE, p7, uploadImage, type OFileHit, type OSkill, type OUpload } from "../../api/p7";
import { CleanChip, MicButton, useDictation } from "../../../lib/voice";
import { HistoryButton, type CmdHistory } from "./bash";
import { copyWithToast } from "../../components/CopyBtn";

// ---- fuzzy match ----

/** Subsequence match with bonuses for runs and word starts; null when q does not match. */
export function fuzzy(q: string, s: string): { score: number; idx: number[] } | null {
  if (!q) return { score: 0, idx: [] };
  const lq = q.toLowerCase();
  const ls = s.toLowerCase();
  const idx: number[] = [];
  let score = 0;
  let j = 0;
  let prev = -2;
  for (let i = 0; i < ls.length && j < lq.length; i++) {
    if (ls[i] !== lq[j]) continue;
    const start = i === 0 || "/-_. :".includes(s[i - 1]);
    score += 1 + (i === prev + 1 ? 3 : 0) + (start ? 2 : 0);
    idx.push(i);
    prev = i;
    j++;
  }
  if (j < lq.length) return null;
  return { score: score - s.length * 0.02 - idx[0] * 0.05, idx };
}

function Hi({ text, idx }: { text: string; idx: number[] }) {
  if (!idx.length) return <>{text}</>;
  const set = new Set(idx);
  const out: React.ReactNode[] = [];
  let buf = "";
  let on = false;
  const flush = (k: number) => {
    if (buf) out.push(on ? <b key={k}>{buf}</b> : <span key={k}>{buf}</span>);
    buf = "";
  };
  for (let i = 0; i < text.length; i++) {
    const m = set.has(i);
    if (m !== on) {
      flush(i);
      on = m;
    }
    buf += text[i];
  }
  flush(text.length);
  return <>{out}</>;
}

// ---- attachments ----

export interface Att {
  key: string;
  name: string;
  url: string;
  image: boolean; // a thumbnail the browser can draw
  size: number;
  progress: number;
  upload?: OUpload;
  error?: string;
  queued?: boolean;
  file?: File; // kept for Retry
}
let attKey = 0;
const kb = (n: number) => (n >= 1024 * 1024 ? `${(n / 1024 / 1024).toFixed(1)} MB` : `${Math.max(1, Math.round(n / 1024))} KB`);
const PARALLEL = 3;

/** Attachments of one message box; scope is the upload scope: an orch thread id or "new". At most 3 upload at once. */
export function useAttachments(threadId: string, scope = threadId) {
  const [atts, setAtts] = useState<Att[]>([]);
  const live = useRef<Att[]>([]);
  live.current = atts;
  const scopeRef = useRef(scope);
  scopeRef.current = scope;
  const queue = useRef<{ key: string; file: File }[]>([]);
  const aborts = useRef(new Map<string, () => void>());
  const patch = (key: string, p: Partial<Att>) => setAtts((a) => a.map((x) => (x.key === key ? { ...x, ...p } : x)));
  const pump = useCallback(() => {
    while (aborts.current.size < PARALLEL && queue.current.length) {
      const { key, file } = queue.current.shift()!;
      const up = uploadImage(scopeRef.current, file, (p) => patch(key, { progress: p }));
      aborts.current.set(key, up.abort);
      patch(key, { queued: false, progress: 0, error: undefined });
      up.done
        .then(
          (u) => patch(key, { upload: u, progress: 1, file: undefined }),
          (e: Error) => patch(key, { error: e.message === "cancelled" ? "" : e.message || "upload failed" }),
        )
        .finally(() => {
          aborts.current.delete(key);
          pump();
        });
    }
  }, []);
  const add = useCallback(
    (files: File[]) => {
      const ok = files.filter(fileAccepted);
      if (ok.length < files.length) showToast("Some files were not attached", "Images (PNG, JPEG, WebP, GIF, HEIC), PDF, and text files (TXT, MD, CSV, JSON, HTML, XML, YAML, logs and source code) attach, up to 25 MB each.");
      const fresh: Att[] = [];
      for (const f of ok) {
        const key = `a${++attKey}`;
        const name = f.name && f.name !== "image.png" ? f.name : `pasted-${attKey}.${f.type.split("/")[1] || "png"}`;
        const a: Att = { key, name, url: URL.createObjectURL(f), image: IMAGE_TYPES.includes(f.type), size: f.size, progress: 0 };
        if (f.size > MAX_FILE) {
          fresh.push({ ...a, error: "larger than 25 MB" });
          continue;
        }
        const named = f.name === name ? f : new File([f], name, { type: f.type });
        fresh.push({ ...a, queued: true, file: named });
        queue.current.push({ key, file: named });
      }
      setAtts((x) => [...x, ...fresh]);
      pump();
    },
    [pump],
  );
  const retry = (key: string) => {
    const x = live.current.find((y) => y.key === key);
    if (!x?.file || !x.error) return;
    patch(key, { error: undefined, queued: true, progress: 0 });
    queue.current.push({ key, file: x.file });
    pump();
  };
  const stop = (key: string) => {
    queue.current = queue.current.filter((q) => q.key !== key);
    aborts.current.get(key)?.();
  };
  const remove = (key: string) =>
    setAtts((a) => {
      const x = a.find((y) => y.key === key);
      if (x) {
        stop(key);
        URL.revokeObjectURL(x.url);
      }
      return a.filter((y) => y.key !== key);
    });
  const clear = () =>
    setAtts((a) => {
      a.forEach((x) => URL.revokeObjectURL(x.url));
      return [];
    });
  const drop = () => {
    queue.current = [];
    aborts.current.forEach((f) => f());
    live.current.forEach((x) => URL.revokeObjectURL(x.url));
  };
  useEffect(() => drop, []); // eslint-disable-line react-hooks/exhaustive-deps
  useEffect(() => {
    drop();
    setAtts([]);
  }, [threadId]); // eslint-disable-line react-hooks/exhaustive-deps
  const ids = atts.filter((a) => a.upload).map((a) => a.upload!.id);
  const uploading = atts.some((a) => !a.upload && !a.error);
  const failed = atts.some((a) => !!a.error);
  return { atts, add, remove, retry, clear, ids, uploading, failed };
}
export type Attachments = ReturnType<typeof useAttachments>;

export const extOf = (name: string) => (/\.([a-z0-9]{1,5})$/i.exec(name)?.[1] || "file").toUpperCase();

/** The paperclip button and its hidden file input. */
export function AttachButton({ a, disabled }: { a: Attachments; disabled?: boolean }) {
  const inp = useRef<HTMLInputElement>(null);
  return (
    <>
      <button type="button" className="wclip" onClick={() => inp.current?.click()} disabled={disabled} aria-label="Attach files" title="Attach images, PDFs or text files (or drop or paste them)">
        <svg viewBox="0 0 16 16" width="15" height="15" aria-hidden>
          <path d="M10.5 4.5 5.8 9.2a1.3 1.3 0 0 0 1.9 1.9l5-5a2.6 2.6 0 0 0-3.7-3.7l-5.2 5.2a3.9 3.9 0 0 0 5.5 5.5L13.5 9" fill="none" stroke="currentColor" strokeWidth="1.4" strokeLinecap="round" />
        </svg>
      </button>
      <input
        ref={inp}
        type="file"
        multiple
        accept={FILE_ACCEPT}
        hidden
        onChange={(e) => {
          const files = [...(e.target.files || [])];
          e.target.value = "";
          if (files.length) a.add(files);
        }}
      />
    </>
  );
}

function Chips({ a }: { a: Attachments }) {
  if (!a.atts.length) return null;
  const done = a.atts.filter((x) => x.upload).length;
  return (
    <div className="watt" aria-label="Attached files">
      {a.atts.length > 1 && a.uploading && (
        <div className="watt-n" role="status">
          {done} of {a.atts.length} uploaded
        </div>
      )}
      {a.atts.map((x) => {
        const busy = !x.upload && !x.error;
        return (
          <div key={x.key} className={`watt-c ${x.error ? "err" : ""}`} title={x.error ? `${x.name}: ${x.error}` : x.name}>
            {x.image ? <img src={x.url} alt="" /> : <span className="watt-f" aria-hidden>{extOf(x.name)}</span>}
            <span className="x">
              <span className="nm">{x.name}</span>
              <small>{x.error ? x.error : x.queued ? "Waiting" : busy ? `Uploading ${Math.round(x.progress * 100)}%` : kb(x.size)}</small>
            </span>
            {x.error && x.file && (
              <button className="watt-retry" onClick={() => a.retry(x.key)}>
                Retry
              </button>
            )}
            <button className="rm" onClick={() => a.remove(x.key)} aria-label={`Remove ${x.name}`}>
              ✕
            </button>
            {busy && !x.queued && <i className="bar" style={{ width: `${Math.round(x.progress * 100)}%` }} />}
          </div>
        );
      })}
    </div>
  );
}

// ---- pickers ----

export interface PickCommand {
  key: string;
  label: string;
  hint?: string;
  run: () => void;
}
type Kind = "@" | "$" | "/";
interface Trigger {
  kind: Kind;
  start: number;
  q: string;
}
interface Row {
  key: string;
  label: string;
  idx: number[];
  sub?: string;
  icon: string;
  pick: () => void;
}

function detect(text: string, caret: number, allow: Record<Kind, boolean>): Trigger | null {
  const before = text.slice(0, caret);
  const m = /(^|\s)([@$/])(\S*)$/.exec(before);
  if (!m) return null;
  const kind = m[2] as Kind;
  const start = before.length - m[3].length - 1;
  if (!allow[kind]) return null;
  if (kind === "/" && start !== 0) return null;
  if (kind === "$" && /^\d/.test(m[3])) return null;
  return { kind, start, q: m[3] };
}

const skillCache = new Map<string, Promise<OSkill[]>>();
const fileCache = new Map<string, { at: number; hits: OFileHit[] }>();

/** The text inserted for a skill: the daemon's `insert`, else the agent's own syntax. */
export function skillText(s: OSkill, agent: string): string {
  if (s.insert) return s.insert;
  if (agent === "codex") return `/prompts:${s.name}`;
  if (agent === "claude") return `/${s.name}`;
  return `$${s.name}`;
}

const HEAD: Record<Kind, string> = { "@": "Files", $: "Skills", "/": "Commands" };

export interface ComposerProps {
  value: string;
  onChange: (v: string) => void;
  onSubmit: () => void;
  placeholder: string;
  disabled?: boolean;
  rows?: number;
  autoFocus?: boolean;
  cwd?: string;
  agent?: string;
  files?: boolean;
  skills?: boolean;
  commands?: PickCommand[];
  attach?: Attachments;
  top?: React.ReactNode;
  className?: string;
  children?: React.ReactNode;
  /** Up arrow in an empty box (the queue selects its newest message); true when handled. */
  onEmptyUp?: () => boolean;
  /** false hides the microphone. */
  voice?: boolean;
  /** A leading "!" switches to bash mode (Claude Code terminals). */
  bash?: boolean;
  history?: CmdHistory;
  /** Up and Down outside bash mode walk your earlier prompts; this session's come first. */
  promptSession?: string;
  noPromptHistory?: boolean;
  onRunCmd?: (cmd: string) => void;
}

/** Copy and Clear for the box's text; Clear turns into Undo for 5 s. */
function BoxTools({ value, onChange, disabled }: { value: string; onChange: (v: string) => void; disabled?: boolean }) {
  const [undo, setUndo] = useState<string | null>(null);
  useEffect(() => {
    if (undo === null) return;
    const t = window.setTimeout(() => setUndo(null), 5000);
    return () => window.clearTimeout(t);
  }, [undo]);
  if (undo !== null && !value)
    return (
      <button type="button" className="btn sm ghost" onClick={() => (onChange(undo), setUndo(null))}>
        Undo clear
      </button>
    );
  if (!value.trim()) return null;
  return (
    <>
      <button type="button" className="btn sm ghost" title="Copy the text in the box" onClick={() => void copyWithToast(value)}>
        Copy
      </button>
      <button type="button" className="btn sm ghost" title="Clear the box" disabled={disabled} onClick={() => (setUndo(value), onChange(""))}>
        Clear
      </button>
    </>
  );
}

export function Composer(p: ComposerProps) {
  const ta = useRef<HTMLTextAreaElement>(null);
  const box = useRef<HTMLDivElement>(null);
  const [caret, setCaret] = useState(0);
  const [sel, setSel] = useState(0);
  const [dismissed, setDismissed] = useState("");
  const [hits, setHits] = useState<{ q: string; list: OFileHit[]; err?: string } | null>(null);
  const [skills, setSkills] = useState<{ list: OSkill[]; err?: string } | null>(null);
  const [drag, setDrag] = useState(false);
  const inBash = !!p.bash && p.value.startsWith("!");
  const shown = inBash ? p.value.slice(1) : p.value;
  const setShown = (v: string) => p.onChange(inBash ? "!" + v : v);
  const hist = useRef({ i: -1, stash: "", bash: false });
  const prompts = usePromptHistory(p.promptSession);
  const dict = useDictation(p.value, p.onChange, inBash ? "command" : "prompt");
  const allow = { "@": !inBash && !!p.files && !!p.cwd, $: !inBash && !!p.skills, "/": !inBash && !!p.commands?.length };
  const trig = p.disabled ? null : detect(p.value, caret, allow);
  const tkey = trig ? `${trig.kind}${trig.start}` : "";
  const open = !!trig && tkey !== dismissed;

  // @ files: debounced server search, cached per folder and query for 30 s.
  const fq = open && trig?.kind === "@" ? trig.q : null;
  useEffect(() => {
    if (fq === null || !p.cwd) return;
    const k = `${p.cwd}\n${fq}`;
    const c = fileCache.get(k);
    if (c && Date.now() - c.at < 30_000) return setHits({ q: fq, list: c.hits });
    let alive = true;
    const t = window.setTimeout(() => {
      p7.files(p.cwd!, fq).then(
        (list) => {
          fileCache.set(k, { at: Date.now(), hits: list });
          if (alive) setHits({ q: fq, list });
        },
        (e: Error) => alive && setHits({ q: fq, list: [], err: e.message }),
      );
    }, 110);
    return () => {
      alive = false;
      window.clearTimeout(t);
    };
  }, [fq, p.cwd]);

  const wantSkills = open && trig?.kind === "$";
  useEffect(() => {
    if (!wantSkills) return;
    const k = `${p.agent}\n${p.cwd || ""}`;
    let pr = skillCache.get(k);
    if (!pr) {
      pr = p7.skills(p.agent || "", p.cwd || "");
      skillCache.set(k, pr);
      pr.catch(() => skillCache.delete(k));
    }
    let alive = true;
    pr.then(
      (list) => alive && setSkills({ list }),
      (e: Error) => alive && setSkills({ list: [], err: e.message }),
    );
    return () => {
      alive = false;
    };
  }, [wantSkills, p.agent, p.cwd]);

  const replace = (t: Trigger, text: string, keepOpen = false) => {
    const end = t.start + 1 + t.q.length;
    const next = p.value.slice(0, t.start) + text + p.value.slice(end);
    p.onChange(next);
    const at = t.start + text.length;
    setCaret(at);
    if (!keepOpen) setDismissed("");
    requestAnimationFrame(() => {
      ta.current?.focus();
      ta.current?.setSelectionRange(at, at);
    });
  };

  const rows: Row[] = useMemo(() => {
    if (!open || !trig) return [];
    if (trig.kind === "@") {
      return (hits?.list || []).slice(0, 50).map((h) => {
        const m = fuzzy(trig.q, h.path);
        return { key: h.path, label: h.path, idx: m?.idx || [], icon: h.kind === "dir" ? "▸" : "·", sub: h.kind === "dir" ? "folder" : undefined, pick: () => replace(trig, `@${h.path.replace(/\/$/, "")}${h.kind === "dir" ? "/" : " "}`, h.kind === "dir") };
      });
    }
    if (trig.kind === "$") {
      return (skills?.list || [])
        .map((s) => ({ s, m: fuzzy(trig.q, s.name) }))
        .filter((x) => x.m)
        .sort((a, b) => b.m!.score - a.m!.score)
        .slice(0, 40)
        .map(({ s, m }) => ({ key: `${s.source || ""}:${s.name}`, label: s.name, idx: m!.idx, icon: "$", sub: s.description, pick: () => replace(trig, `${skillText(s, p.agent || "")} `) }));
    }
    return (p.commands || [])
      .map((c) => ({ c, m: fuzzy(trig.q, c.key) || fuzzy(trig.q, c.label) }))
      .filter((x) => x.m)
      .sort((a, b) => (trig.q ? b.m!.score - a.m!.score : 0))
      .map(({ c, m }) => ({
        key: c.key,
        label: `/${c.key}`,
        idx: fuzzy(trig.q, c.key) ? m!.idx.map((i) => i + 1) : [],
        icon: "/",
        sub: c.label,
        pick: () => {
          replace(trig, "");
          c.run();
        },
      }));
  }, [open, tkey, trig?.q, hits, skills, p.commands, p.agent, p.value]); // eslint-disable-line react-hooks/exhaustive-deps

  useEffect(() => setSel(0), [tkey, trig?.q]);
  const list = useRef<HTMLDivElement>(null);
  const lid = useId();
  useLayoutEffect(() => {
    list.current?.querySelector<HTMLElement>(`[data-i="${sel}"]`)?.scrollIntoView({ block: "nearest" });
  }, [sel]);

  // Drop files anywhere on the conversation; the Mac app also dispatches a drop on the box itself.
  const add = p.attach?.add;
  useEffect(() => {
    if (!add || p.disabled) return;
    const own = box.current;
    const area = own?.closest(".wchat") || own;
    if (!area || !own) return;
    const seen = new WeakSet<Event>();
    let depth = 0;
    const hasFiles = (e: DragEvent) => !!e.dataTransfer && ([...e.dataTransfer.types].includes("Files") || e.dataTransfer.files.length > 0);
    const enter = (e: Event) => {
      if (!hasFiles(e as DragEvent)) return;
      e.preventDefault();
      depth++;
      setDrag(true);
    };
    const over = (e: Event) => hasFiles(e as DragEvent) && e.preventDefault();
    const leave = () => {
      depth = Math.max(0, depth - 1);
      if (!depth) setDrag(false);
    };
    const drop = (e: Event) => {
      const de = e as DragEvent;
      if (seen.has(e) || !hasFiles(de)) return;
      seen.add(e);
      e.preventDefault();
      depth = 0;
      setDrag(false);
      add([...(de.dataTransfer?.files || [])]);
      ta.current?.focus();
    };
    area.addEventListener("dragenter", enter);
    area.addEventListener("dragover", over);
    area.addEventListener("dragleave", leave);
    area.addEventListener("drop", drop);
    if (own !== area) own.addEventListener("drop", drop);
    return () => {
      own.removeEventListener("drop", drop);
      area.removeEventListener("dragenter", enter);
      area.removeEventListener("dragover", over);
      area.removeEventListener("dragleave", leave);
      area.removeEventListener("drop", drop);
    };
  }, [add, p.disabled]);

  const syncCaret = () => setCaret((ta.current?.selectionStart ?? shown.length) + (inBash ? 1 : 0));
  // Up and Down walk the ! history like a shell; Down past the newest restores what was typed.
  const walk = (dir: 1 | -1): boolean => {
    const h = hist.current;
    if (h.i < 0) h.bash = inBash;
    const list = h.bash ? (p.history?.list || []).map((c) => "!" + c.cmd) : p.noPromptHistory ? [] : prompts.list;
    if (h.i >= 0 && p.value !== list[h.i]) h.i = -1;
    if (dir === -1 && h.i < 0) return false;
    if (dir === 1 && h.i < 0) {
      if (!list.length) return false;
      h.stash = p.value;
    }
    const n = Math.min(list.length - 1, h.i + dir);
    if (n === h.i) return true;
    h.i = n;
    const v = n < 0 ? h.stash : list[n];
    p.onChange(v);
    requestAnimationFrame(() => {
      const end = ta.current?.value.length ?? 0;
      ta.current?.setSelectionRange(end, end);
    });
    return true;
  };
  const loading = open && ((trig?.kind === "@" && hits?.q !== trig.q && !rows.length) || (trig?.kind === "$" && !skills));
  const err = trig?.kind === "@" ? hits?.err : trig?.kind === "$" ? skills?.err : undefined;
  const empty = open && !loading && !rows.length;

  return (
    <div ref={box} data-vox className={`wcomp ${p.disabled ? "off" : ""} ${drag ? "drag" : ""} ${inBash ? "wbash" : ""} ${p.className || ""}`}>
      {open && trig && (
        <div className="wpop" id={`${lid}-l`} role="listbox" aria-label={HEAD[trig.kind]} ref={list}>
          <div className="wpop-h">
            <span>{HEAD[trig.kind]}</span>
            {trig.kind === "@" && p.cwd && <span className="trunc">in {p.cwd.replace(/^\/Users\/[^/]+/, "~")}</span>}
            <span className="sp" />
            <span className="hidephone">↑↓ ↵ esc</span>
          </div>
          {loading && <div className="wpop-e">{trig.kind === "@" ? "Searching…" : "Loading skills…"}</div>}
          {empty && <div className="wpop-e">{err ? `Could not load: ${err}` : trig.kind === "@" ? "No file matches." : trig.kind === "$" ? "No skill matches." : "No command matches."}</div>}
          {rows.map((r, i) => (
            <button
              key={r.key}
              id={`${lid}-o${i}`}
              data-i={i}
              tabIndex={-1}
              role="option"
              aria-selected={i === sel}
              className="wpop-r"
              onMouseEnter={() => setSel(i)}
              onMouseDown={(e) => {
                e.preventDefault();
                r.pick();
              }}
            >
              <span className="ic" aria-hidden>
                {r.icon}
              </span>
              <span className="l">
                <Hi text={r.label} idx={r.idx} />
              </span>
              {r.sub && <span className="s">{r.sub}</span>}
            </button>
          ))}
        </div>
      )}
      {p.top}
      {p.attach && <Chips a={p.attach} />}
      <div className="wbash-in">
      {inBash && (
        <span className="wbash-p" aria-hidden>
          !
        </span>
      )}
      <textarea
        ref={ta}
        rows={p.rows || 2}
        value={shown}
        readOnly={p.disabled}
        aria-disabled={p.disabled || undefined}
        aria-label={inBash ? "Bash command" : undefined}
        autoFocus={p.autoFocus}
        spellCheck={inBash ? false : undefined}
        autoCapitalize={inBash ? "off" : undefined}
        autoCorrect={inBash ? "off" : undefined}
        placeholder={inBash ? "Run a command in the terminal" : p.placeholder}
        onFocus={() => {
          void p.history?.load().catch(() => undefined);
          if (!p.noPromptHistory) void prompts.load().catch(() => undefined);
        }}
        role="combobox"
        aria-multiline="true"
        aria-autocomplete="list"
        aria-expanded={!!(open && trig)}
        aria-controls={open && trig ? `${lid}-l` : undefined}
        aria-activedescendant={open && trig && rows.length ? `${lid}-o${Math.min(sel, rows.length - 1)}` : undefined}
        onChange={(e) => {
          hist.current.i = -1;
          setShown(e.target.value);
          setCaret(e.target.selectionStart + (inBash ? 1 : 0));
        }}
        onSelect={syncCaret}
        onClick={syncCaret}
        onPaste={(e) => {
          if (!add || p.disabled) return;
          const files = [...e.clipboardData.files];
          if (!files.length) return;
          if (!e.clipboardData.getData("text/plain")) e.preventDefault();
          add(files);
        }}
        onKeyDown={(e) => {
          if (open && trig) {
            if (e.key === "Escape") {
              e.preventDefault();
              e.stopPropagation();
              setDismissed(tkey);
              return;
            }
            if (rows.length && (e.key === "ArrowDown" || e.key === "ArrowUp")) {
              e.preventDefault();
              setSel((s) => (s + (e.key === "ArrowDown" ? 1 : rows.length - 1)) % rows.length);
              return;
            }
            if (rows.length && (e.key === "Tab" || (e.key === "Enter" && !e.shiftKey))) {
              e.preventDefault();
              rows[Math.min(sel, rows.length - 1)].pick();
              return;
            }
          }
          const plain = !e.shiftKey && !e.metaKey && !e.ctrlKey && !e.altKey;
          if (e.key === "Backspace" && inBash && !shown && plain) {
            e.preventDefault();
            p.onChange("");
            return;
          }
          if (e.key === "ArrowUp" && !p.value && plain && hist.current.i < 0 && p.onEmptyUp?.()) {
            e.preventDefault();
            return;
          }
          if (plain && (p.history || !p.noPromptHistory) && (e.key === "ArrowUp" || e.key === "ArrowDown")) {
            const el = e.currentTarget;
            const edge = e.key === "ArrowUp" ? !el.value.slice(0, el.selectionStart).includes("\n") : !el.value.slice(el.selectionEnd).includes("\n");
            if ((!p.value || inBash || hist.current.i >= 0) && edge && walk(e.key === "ArrowUp" ? 1 : -1)) {
              e.preventDefault();
              return;
            }
          }
          if (e.key === "Enter" && (e.metaKey || e.ctrlKey)) {
            e.preventDefault();
            if (!p.disabled) {
              dict.stop();
              hist.current.i = -1;
              p.onSubmit();
            }
          }
        }}
      />
      </div>
      {(p.children || p.attach || p.voice !== false) && (
        <div className="wcomp-f">
          {p.attach && <AttachButton a={p.attach} disabled={p.disabled} />}
          {p.voice !== false && <MicButton d={dict} disabled={p.disabled} />}
          {inBash && <span className="wbash-tag">Bash mode</span>}
          {p.bash && p.history && (
            <HistoryButton
              h={p.history}
              disabled={p.disabled || !p.onRunCmd}
              onEdit={(cmd) => {
                p.onChange("!" + cmd);
                requestAnimationFrame(() => ta.current?.focus());
              }}
              onRun={(cmd) => p.onRunCmd?.(cmd)}
            />
          )}
          <CleanChip d={dict} />
          <BoxTools value={p.value} onChange={p.onChange} disabled={p.disabled} />
          {p.children}
        </div>
      )}
      {drag && <div className="wdrop">Drop files to attach them</div>}
    </div>
  );
}
