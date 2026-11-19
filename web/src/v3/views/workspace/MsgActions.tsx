// Copy actions on agent replies, and Select mode that exports chosen prompts as Markdown or Slack text.
import { createContext, useCallback, useContext, useEffect, useMemo, useRef, useState } from "react";
import type { OItem, ORun } from "../../api/orch";
import { toMs } from "../../api/orch";
import { copy } from "../../../lib/clipboard";
import { toPlain, toSlack } from "../../../lib/slack";
import { downloadText, exportFileName, exportMarkdown, exportSlack, type ExportEntry } from "../../../lib/chatExport";
import { showToast } from "../../../app/toast";
import { typing } from "../../ctx";
import { CopyBtn } from "../../components/ui";
import { resumeCommand } from "../../../lib/format";
import type { Tool } from "../../../api/types";

export interface CopyOpt {
  label: string;
  text: () => string;
  done?: string;
}

/** A Copy ▾ button with a menu of formats. */
export function CopyMenu({ opts, className = "", btnClass = "wmsg-b", label = "Copy ▾", title = "Copy" }: { opts: CopyOpt[]; className?: string; btnClass?: string; label?: string; title?: string }) {
  const [open, setOpen] = useState(false);
  const [done, setDone] = useState("");
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
  const take = async (o: CopyOpt) => {
    setOpen(false);
    const ok = await copy(o.text());
    if (!ok) return showToast("Copy failed", "The browser blocked the clipboard.", { kind: "alert" });
    setDone(o.done || "Copied");
    window.setTimeout(() => setDone(""), 1400);
  };
  return (
    <div className={`wmsg-a ${open || done ? "on" : ""} ${className}`} ref={ref} onClick={(e) => e.stopPropagation()}>
      <button type="button" className={`${btnClass} ${done ? "on" : ""}`} aria-haspopup="menu" aria-expanded={open} onClick={() => setOpen(!open)} title={title}>
        {done || label}
      </button>
      {open && (
        <div className="wopen-m wmsg-m" role="menu">
          {opts.map((o) => (
            <button key={o.label} role="menuitem" onClick={() => take(o)}>
              {o.label}
            </button>
          ))}
        </div>
      )}
    </div>
  );
}

/** Copy menu for prose (prompts, replies): plain text, Markdown, or Slack mrkdwn. */
export function MsgCopy({ text, className, title = "Copy this message" }: { text: string; className?: string; title?: string }) {
  return (
    <CopyMenu
      className={className}
      title={title}
      opts={[
        { label: "Copy", text: () => toPlain(text) },
        { label: "Copy as Markdown", text: () => text },
        { label: "Copy for Slack", text: () => toSlack(text), done: "Copied for Slack" },
      ]}
    />
  );
}

const fence = (body: string, lang = "") => "```" + lang + "\n" + body.replace(/\s+$/, "") + "\n```";
/** Copy menu for a command and its output. */
export function CmdCopy({ cmd, out, prompt = "$" }: { cmd: string; out?: string; prompt?: string }) {
  const o = (out || "").replace(/\s+$/, "");
  const both = o ? `${prompt} ${cmd}\n${o}` : cmd;
  const opts: CopyOpt[] = [{ label: "Copy command", text: () => cmd }];
  if (o) opts.push({ label: "Copy output", text: () => o });
  opts.push({ label: "Copy as Markdown", text: () => fence(both, o ? "console" : "bash") }, { label: "Copy for Slack", text: () => fence(both), done: "Copied for Slack" });
  return <CopyMenu opts={opts} className="wcpm" btnClass="wcpy" title="Copy the command or its output" />;
}

/** Copy menu for tool output. */
export function OutCopy({ text }: { text: string }) {
  if (!text.trim()) return null;
  return (
    <CopyMenu
      className="wcpm wout-cp"
      btnClass="wcpy"
      title="Copy the output"
      opts={[
        { label: "Copy", text: () => text },
        { label: "Copy as Markdown", text: () => fence(text) },
        { label: "Copy for Slack", text: () => fence(text), done: "Copied for Slack" },
      ]}
    />
  );
}

export interface Selection {
  on: boolean;
  picked: Set<string>;
  replies: boolean;
  /** Prompt entries of subagent transcripts open in the feed, by child key. */
  extra: Map<string, PromptEntry[]>;
  setExtra: (key: string, entries: PromptEntry[] | null) => void;
  toggle: (id: string) => void;
  set: (ids: string[]) => void;
  setReplies: (v: boolean) => void;
  start: () => void;
  stop: () => void;
}

export const SelectCtx = createContext<Selection | null>(null);
export const useSelectCtx = () => useContext(SelectCtx);

/** Select mode state for one thread; Esc leaves it. */
export function useSelection(threadKey: string): Selection {
  const [on, setOn] = useState(false);
  const [picked, setPicked] = useState<Set<string>>(new Set());
  const [replies, setReplies] = useState(true);
  const [extra, setExtraMap] = useState<Map<string, PromptEntry[]>>(new Map());
  const setExtra = useCallback(
    (key: string, entries: PromptEntry[] | null) =>
      setExtraMap((m) => {
        if (!entries && !m.has(key)) return m;
        const n = new Map(m);
        if (entries) n.set(key, entries);
        else n.delete(key);
        return n;
      }),
    [],
  );
  useEffect(() => {
    setOn(false);
    setPicked(new Set());
  }, [threadKey]);
  useEffect(() => {
    if (!on) return;
    const key = (e: KeyboardEvent) => {
      if (e.key !== "Escape" || e.defaultPrevented || typing(e) || document.querySelector('[role="menu"],[role="dialog"]')) return;
      setOn(false);
    };
    window.addEventListener("keydown", key);
    return () => window.removeEventListener("keydown", key);
  }, [on]);
  return useMemo(
    () => ({
      on,
      picked,
      replies,
      setReplies,
      extra,
      setExtra,
      toggle: (id: string) =>
        setPicked((p) => {
          const n = new Set(p);
          if (n.has(id)) n.delete(id);
          else n.add(id);
          return n;
        }),
      set: (ids: string[]) => setPicked(new Set(ids)),
      start: () => setOn(true),
      stop: () => {
        setOn(false);
        setPicked(new Set());
      },
    }),
    [on, picked, replies, extra, setExtra],
  );
}

type Block = { run?: ORun; items: OItem[] };
export type PromptEntry = ExportEntry & { id: string };

/** Each typed prompt with the last agent reply of its turn; a turn with two prompts gives the reply to the later one. */
export function promptEntries(blocks: Block[]): PromptEntry[] {
  const out: PromptEntry[] = [];
  for (const b of blocks) {
    const users = b.items.filter((i) => i.kind === "user" && i.text.trim());
    if (!users.length) continue;
    const reply = [...b.items].reverse().find((i) => i.kind === "agent" && i.text.trim())?.text;
    users.forEach((u, k) => out.push({ id: u.id, prompt: u.text, at: toMs(u.startedAt) || undefined, reply: k === users.length - 1 ? reply : undefined }));
  }
  return out;
}

export function SelectBar({ sel, blocks, title }: { sel: Selection; blocks: Block[]; title: string }) {
  const all = useMemo(() => {
    const main = promptEntries(blocks);
    const subs = [...sel.extra.values()].flat();
    if (!subs.length) return main;
    // Subagent prompts sit by time among the thread's own prompts.
    return [...main, ...subs].map((e, i) => ({ e, i })).sort((a, b) => (a.e.at ?? Infinity) - (b.e.at ?? Infinity) || a.i - b.i).map((x) => x.e);
  }, [blocks, sel.extra]);
  const chosen = all.filter((e) => sel.picked.has(e.id));
  const n = chosen.length;
  const [note, setNote] = useState("");
  if (!sel.on) return null;
  const slack = async () => {
    const ok = await copy(exportSlack(title, chosen, sel.replies));
    setNote(ok ? "Copied" : "Copy failed");
    window.setTimeout(() => setNote(""), 1400);
  };
  return (
    <div className="wselbar" role="toolbar" aria-label="Export prompts">
      <b>
        {n} of {all.length} {all.length === 1 ? "prompt" : "prompts"}
      </b>
      <button className="btn sm ghost" onClick={() => sel.set(n === all.length ? [] : all.map((e) => e.id))}>
        {n === all.length && n > 0 ? "None" : "All"}
      </button>
      <label className="wsel-r">
        <input type="checkbox" checked={sel.replies} onChange={(e) => sel.setReplies(e.target.checked)} />
        Include replies
      </label>
      <span className="sp" />
      <button className="btn sm" disabled={!n} onClick={() => downloadText(exportFileName(title), exportMarkdown(title, chosen, sel.replies))}>
        Download .md
      </button>
      <button className="btn sm" disabled={!n} onClick={slack}>
        {note || "Copy for Slack"}
      </button>
      <button className="btn sm ghost" onClick={sel.stop} title="Leave select mode (esc)">
        Done
      </button>
    </div>
  );
}

/** Checkbox in front of a user prompt while Select mode is on. */
export function SelectBox({ id }: { id: string }) {
  const sel = useSelectCtx();
  if (!sel?.on) return null;
  return (
    <label className="wsel" title="Include this prompt in the export" onClick={(e) => e.stopPropagation()}>
      <input type="checkbox" checked={sel.picked.has(id)} onChange={() => sel.toggle(id)} aria-label="Select this prompt" />
    </label>
  );
}

/** Composer chip that copies the terminal resume command (the same one as Resume on the trace page). */
export function ResumeChip({ tool, sessionId, cwd }: { tool: string; sessionId: string; cwd: string }) {
  const cmd = sessionId ? resumeCommand({ tool: tool as Tool, sessionId, cwd }) : null;
  if (!cmd) return null;
  return <CopyBtn text={cmd} label="Resume ⧉" className="wchip wresume" title={`Copy: ${cmd}`} />;
}
