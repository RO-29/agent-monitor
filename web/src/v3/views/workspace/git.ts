// Phase 4 git state: a small per-thread cache of GET /git, diff navigation in the URL, and error toasts.
import { useCallback, useEffect, useSyncExternalStore } from "react";
import { useSearchParams } from "react-router-dom";
import { showToast } from "../../../app/toast";
import { ApiError, isMissing } from "../../api/client";
import { gitApi, toMs, type OCheckpoint, type ODetail, type OGit, type ORun } from "../../api/orch";

export type GitStatus = "loading" | "ok" | "missing" | "nogit" | "error";
export interface GitEntry {
  status: GitStatus;
  data?: OGit;
  error?: string;
  key: string;
}

const cache = new Map<string, GitEntry>();
const inflight = new Map<string, string>();
const wanted = new Map<string, string>();
const listeners = new Set<() => void>();
let ver = 0;
const emit = () => {
  ver++;
  listeners.forEach((l) => l());
};

// Keys starting with "cwd:" read GET /api/orch/git?cwd= (watched sessions, read only).
async function load(id: string, key: string) {
  if (inflight.get(id) === key) return;
  inflight.set(id, key);
  const was = cache.get(id);
  if (!was) cache.set(id, { status: "loading", key });
  emit();
  try {
    const data = id.startsWith("cwd:") ? await gitApi().cwdGit(id.slice(4)) : await gitApi().git(id);
    cache.set(id, data.isRepo === false ? { status: "nogit", key } : { status: "ok", data, key });
  } catch (e) {
    const err = e as Error;
    // 400 means the folder is not a git repo; 404/405/501 means this daemon has no git routes yet.
    const st: GitStatus = isMissing(e) ? "missing" : e instanceof ApiError && e.status === 400 ? "nogit" : "error";
    cache.set(id, { status: st, error: err.message, key, data: st === "error" ? was?.data : undefined });
  } finally {
    if (inflight.get(id) === key) inflight.delete(id);
    emit();
  }
}

/** Reloads the git box of a thread after an action. */
export function refreshGit(id: string) {
  void load(id, `manual-${Date.now()}`);
}

/** Reloads a git box when the window gains focus and every 30 s while the page is visible. */
export function useGitPoll(id: string, on = true) {
  useEffect(() => {
    if (!on) return;
    const tick = () => {
      if (!document.hidden) refreshGit(id);
    };
    window.addEventListener("focus", tick);
    document.addEventListener("visibilitychange", tick);
    const t = window.setInterval(tick, 30_000);
    return () => {
      window.removeEventListener("focus", tick);
      document.removeEventListener("visibilitychange", tick);
      window.clearInterval(t);
    };
  }, [id, on]);
}

/** Git state of a driven thread. `key` changes when a run starts or ends or files change, which triggers a reload. */
export function useGit(id: string, key: string): GitEntry {
  useSyncExternalStore(
    (l) => {
      listeners.add(l);
      return () => listeners.delete(l);
    },
    () => ver,
  );
  useEffect(() => {
    if (wanted.get(id) === key) return;
    wanted.set(id, key);
    void load(id, key);
  }, [id, key]);
  return cache.get(id) || { status: "loading", key };
}

/** The reload key for a thread: run count, last run status, file edits and thread status. */
export function gitKey(d: ODetail): string {
  const last = d.runs[d.runs.length - 1];
  const edits = d.items.filter((i) => i.kind === "fileChange" && i.status === "completed").length;
  return `${d.runs.length}:${last?.status || ""}:${edits}:${d.thread.status}`;
}

export const runActive = (d: ODetail) => d.thread.status === "running" || d.thread.status === "starting" || d.runs.some((r) => r.status === "running");

// ---- diff navigation (?dfrom=&dto=&dfile=) so links and the browser back button work ----

export interface DiffNav {
  from: string;
  to: string;
  run?: number;
  file?: string;
  label?: string;
}
export function useDiffNav(): { cur: DiffNav | null; open: (d: DiffNav) => void; close: () => void; setFile: (f: string) => void } {
  const [sp, setSp] = useSearchParams();
  const from = sp.get("dfrom");
  const run = sp.get("drun");
  const cur = from || run ? { from: from || "", to: sp.get("dto") || "", run: run ? +run : undefined, file: sp.get("dfile") || undefined, label: sp.get("dlabel") || undefined } : null;
  const open = useCallback(
    (d: DiffNav) => {
      const n = new URLSearchParams(sp);
      for (const k of ["dfrom", "dto", "drun"]) n.delete(k);
      if (d.run != null) n.set("drun", String(d.run));
      else {
        n.set("dfrom", d.from);
        n.set("dto", d.to);
      }
      if (d.file) n.set("dfile", d.file);
      else n.delete("dfile");
      if (d.label) n.set("dlabel", d.label);
      else n.delete("dlabel");
      setSp(n);
    },
    [sp, setSp],
  );
  const close = useCallback(() => {
    const n = new URLSearchParams(sp);
    for (const k of ["dfrom", "dto", "drun", "dfile", "dlabel"]) n.delete(k);
    setSp(n);
  }, [sp, setSp]);
  const setFile = useCallback(
    (f: string) => {
      const n = new URLSearchParams(sp);
      n.set("dfile", f);
      setSp(n, { replace: true });
    },
    [sp, setSp],
  );
  return { cur, open, close, setFile };
}

/** Diff of one run through GET diff?run=, shown when the run edited files or has a checkpoint. */
export function runDiff(cps: OCheckpoint[], run: ORun, edited: boolean): DiffNav | null {
  if (!edited && !cps.some((c) => c.runIndex === run.index)) return null;
  return { from: "", to: "", run: run.index, label: `Turn ${run.index}` };
}

// ---- errors ----

/** Shows a git failure as a toast with its output (hook output, git stderr) in a scrollable block. */
export function gitErrorToast(title: string, e: unknown) {
  const err = e as Error;
  const body = e instanceof ApiError ? (e.body as { error?: string; output?: string } | undefined) : undefined;
  const output = body?.output || "";
  const msg = body?.error || err.message || "Unknown error";
  showToast(title, output && output.trim() === msg.trim() ? undefined : msg, { kind: "alert", output });
}

export const isLoopback = () => {
  const h = window.location.hostname;
  return h === "127.0.0.1" || h === "localhost" || h === "::1" || h === "[::1]";
};

export const PROTECTED = new Set(["main", "master"]);
export const pushBlocked = (g: OGit) => g.branch === g.defaultBranch || PROTECTED.has(g.branch);

/** First line of the last run's prompt, trimmed to a commit subject. */
export function commitSubject(d: ODetail): string {
  const runs = [...d.runs].sort((a, b) => a.index - b.index);
  const last = runs[runs.length - 1];
  let p = last?.prompt?.trim() || "";
  if (!p) {
    const users = d.items.filter((i) => i.kind === "user").sort((a, b) => toMs(a.startedAt) - toMs(b.startedAt));
    p = users[users.length - 1]?.text?.trim() || d.thread.title || "";
  }
  const line = p.split("\n").map((l) => l.trim()).find(Boolean) || "";
  return line.length > 72 ? `${line.slice(0, 71).replace(/\s+\S*$/, "")}` : line;
}

export function shortSha(s?: string) {
  return (s || "").slice(0, 7);
}
