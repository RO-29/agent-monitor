// Turns a watched session (trace + full detail) into the OItem/ORun shapes the driven chat renders.
import { useEffect, useMemo, useRef, useState } from "react";
import { api } from "../../../api/client";
import type { DetailTool, SessionDetail, Span, TraceFull } from "../../../api/types";
import { onLiveEvent } from "../../../lib/ws";
import { isMissing } from "../../api/client";
import { toMs, type OFile, type OItem, type OItemKind, type ORun } from "../../api/orch";
import { p7, type WatchedItems } from "../../api/p7";

export interface WatchedFeed {
  runs: ORun[];
  items: OItem[];
}

const parseArgs = (s: string): Record<string, unknown> => {
  try {
    const j = JSON.parse(s) as unknown;
    return j && typeof j === "object" ? (j as Record<string, unknown>) : {};
  } catch {
    return {};
  }
};
const str = (v: unknown) => (typeof v === "string" ? v : "");
const lines = (s: string) => (s ? s.split("\n").length : 0);
const short = (p: string) => p.replace(/^\/Users\/[^/]+\//, "~/");

function editFiles(name: string, a: Record<string, unknown>): OFile[] {
  const path = short(str(a.file_path) || str(a.notebook_path) || str(a.path));
  if (name === "Write") return [{ path, kind: "add", add: lines(str(a.content)), del: 0 }];
  if (name === "MultiEdit" && Array.isArray(a.edits)) {
    let add = 0;
    let del = 0;
    for (const e of a.edits as Record<string, unknown>[]) {
      add += lines(str(e.new_string));
      del += lines(str(e.old_string));
    }
    return [{ path, kind: "update", add, del }];
  }
  return [{ path, kind: "update", add: lines(str(a.new_string) || str(a.new_source)), del: lines(str(a.old_string)) }];
}

/** One tool call as a chat item; the span (when matched) adds duration and the error flag. */
function toolItem(t: DetailTool, span: Span | undefined, runId: string, threadId: string): OItem {
  const a = parseArgs(t.args);
  const name = t.name;
  const res = t.result || "";
  const exit = /^Exit code (\d+)/.exec(res);
  const failed = !!span?.err || /^<tool_use_error>/.test(res) || !!(exit && exit[1] !== "0");
  const base = { id: t.id || `${t.ts}-${name}`, threadId, runId, status: (failed ? "failed" : "completed") as OItem["status"], startedAt: t.ts, endedAt: span?.dur ? t.ts + span.dur : undefined, output: res };
  let kind: OItemKind = "other";
  let text = "";
  let command: string | undefined = name;
  if (name === "Bash" || name === "BashOutput") {
    kind = "command";
    command = str(a.command) || name;
    text = str(a.description);
    return { ...base, kind, text, command, exitCode: exit ? Number(exit[1]) : failed ? 1 : 0 };
  }
  if (name === "Edit" || name === "Write" || name === "MultiEdit" || name === "NotebookEdit") return { ...base, kind: "fileChange", text: "", files: editFiles(name, a) };
  if (name === "TodoWrite") return { ...base, kind: "plan", text: t.args, output: undefined };
  if (name.startsWith("mcp__")) {
    kind = "mcp";
    const v = Object.values(a).find((x) => typeof x === "string") as string | undefined;
    text = v || "";
  } else if (name === "WebFetch" || name === "WebSearch") {
    kind = "web";
    text = str(a.url) || str(a.query);
  } else if (name === "Read") text = short(str(a.file_path));
  else if (name === "Grep" || name === "Glob") text = `${str(a.pattern)}${a.path ? `  in ${short(str(a.path))}` : ""}`;
  else if (name === "Task" || name === "Agent") text = str(a.description) || str(a.prompt).split("\n")[0];
  else if (name === "Skill") text = str(a.skill) || str(a.command);
  else if (name === "AskUserQuestion") text = ((a.questions as { question?: string }[] | undefined)?.[0]?.question as string) || "Question";
  else if (name === "ExitPlanMode") text = "Plan ready";
  else {
    const v = Object.values(a).find((x) => typeof x === "string") as string | undefined;
    text = (v || "").split("\n")[0];
  }
  return { ...base, kind, text, command };
}

const TAG = /^\s*</;
/** A user message the person typed: plain text, a slash command, or a ! shell line. */
function userText(raw: string): { text: string; shell?: string } | null {
  const t = raw.trim();
  if (!t) return null;
  const bash = /^<bash-input>([\s\S]*?)<\/bash-input>/.exec(t);
  if (bash) return { text: "", shell: bash[1].trim() };
  const cmd = /<command-name>([^<]+)<\/command-name>/.exec(t);
  if (cmd) {
    const args = /<command-args>([\s\S]*?)<\/command-args>/.exec(t);
    return { text: `${cmd[1].trim()} ${args?.[1].trim() || ""}`.trim() };
  }
  if (TAG.test(t) || /^Caveat: /.test(t) || /^\[Request interrupted/.test(t)) return null;
  return { text: t };
}

/** Builds runs and items from the full session detail, using trace spans only for timing and errors. */
export function fromDetail(d: SessionDetail, trace: TraceFull | null, threadId: string, running: boolean): WatchedFeed {
  const spanBy = new Map<string, Span>();
  for (const s of trace?.spans || []) if (s.kind === "tool" || s.kind === "agent") spanBy.set(`${s.name}@${s.ts}`, s);
  type Ev = { ts: number; o: number; m?: SessionDetail["messages"][number]; t?: DetailTool };
  const evs: Ev[] = [];
  d.messages.forEach((m, i) => m.ts > 0 && evs.push({ ts: m.ts, o: i, m }));
  d.toolCalls.forEach((t, i) => t.ts > 0 && evs.push({ ts: t.ts, o: 1e6 + i, t }));
  evs.sort((a, b) => a.ts - b.ts || a.o - b.o);

  const runs: ORun[] = [];
  const items: OItem[] = [];
  let run: ORun | null = null;
  let pendingShell: OItem | null = null;
  const open = (ts: number, prompt: string): ORun => {
    run = { id: `w${runs.length}`, threadId, index: runs.length + 1, prompt, status: "completed", startedAt: ts };
    runs.push(run);
    return run;
  };
  for (const e of evs) {
    if (e.m) {
      const m = e.m;
      if (m.role === "user") {
        const stdout = /^<bash-stdout>([\s\S]*?)<\/bash-stdout>(?:<bash-stderr>([\s\S]*?)<\/bash-stderr>)?/.exec(m.text.trim());
        if (stdout && pendingShell) {
          pendingShell.output = [stdout[1], stdout[2]].filter((x) => x && x.trim()).join("\n");
          pendingShell.endedAt = m.ts;
          pendingShell = null;
          continue;
        }
        const u = userText(m.text);
        if (!u) continue;
        const r = open(m.ts, u.text || `! ${u.shell}`);
        if (u.shell) {
          pendingShell = { id: `u${m.ts}`, threadId, runId: r.id, kind: "command", status: "completed", text: "You ran", command: u.shell, startedAt: m.ts };
          items.push({ id: `u${m.ts}-b`, threadId, runId: r.id, kind: "user", status: "completed", text: `! ${u.shell}`, startedAt: m.ts });
          items.push(pendingShell);
        } else items.push({ id: `u${m.ts}`, threadId, runId: r.id, kind: "user", status: "completed", text: u.text, startedAt: m.ts });
        continue;
      }
      const text = (m.text || "").trim();
      if (!text || /^→ /.test(text)) continue;
      const r = run || open(m.ts, "");
      items.push({ id: `a${m.ts}-${e.o}`, threadId, runId: r.id, kind: "agent", status: "completed", text, startedAt: m.ts, endedAt: m.ts });
    } else if (e.t) {
      const r = run || open(e.t.ts, "");
      items.push(toolItem(e.t, spanBy.get(`${e.t.name}@${e.t.ts}`), r.id, threadId));
    }
  }
  finish(runs, items, running);
  return { runs, items };
}

/** Fallback while the detail loads: spans carry short text, but the shape is the same. */
export function fromTrace(tr: TraceFull, threadId: string, running: boolean): WatchedFeed {
  const runs: ORun[] = [];
  const items: OItem[] = [];
  let run: ORun | null = null;
  const spans = [...tr.spans].sort((a, b) => a.ts - b.ts);
  for (const s of spans) {
    if (s.kind === "user") {
      const u = userText(s.text || "");
      if (!u) continue;
      run = { id: `w${runs.length}`, threadId, index: runs.length + 1, prompt: u.text, status: "completed", startedAt: s.ts };
      runs.push(run);
      items.push({ id: s.id, threadId, runId: run.id, kind: "user", status: "completed", text: u.text || `! ${u.shell}`, startedAt: s.ts });
      continue;
    }
    if (!run) {
      run = { id: `w${runs.length}`, threadId, index: runs.length + 1, prompt: "", status: "completed", startedAt: s.ts };
      runs.push(run);
    }
    if (s.kind === "turn") {
      if ((s.text || "").trim()) items.push({ id: s.id, threadId, runId: run.id, kind: "agent", status: "completed", text: s.text!.trim(), startedAt: s.ts + s.dur, endedAt: s.ts + s.dur });
      continue;
    }
    const base = { id: s.id, threadId, runId: run.id, status: (s.err ? "failed" : "completed") as OItem["status"], startedAt: s.ts, endedAt: s.ts + s.dur, text: s.res || "" };
    if (s.fam === "bash") items.push({ ...base, kind: "command", command: s.res || s.name, text: "", exitCode: s.err ? 1 : 0 });
    else if (s.fam === "edit") items.push({ ...base, kind: "fileChange", files: [{ path: short(s.res || ""), kind: "update", add: 0, del: 0 }] });
    else if (s.fam === "mcp") items.push({ ...base, kind: "mcp", command: s.name });
    else if (s.fam === "web") items.push({ ...base, kind: "web", command: s.name });
    else items.push({ ...base, kind: "other", command: s.name });
  }
  finish(runs, items, running);
  return { runs, items };
}

function finish(runs: ORun[], items: OItem[], running: boolean) {
  const last = new Map<string, number>();
  for (const i of items) last.set(i.runId, Math.max(last.get(i.runId) || 0, i.endedAt || i.startedAt));
  for (const r of runs) r.endedAt = last.get(r.id) || r.startedAt;
  const tail = runs[runs.length - 1];
  if (running && tail) {
    tail.status = "running";
    tail.endedAt = undefined;
  }
}

/** Older daemons: trace first (small, fast), then the full detail; both reload when the live session gains messages. */
function useLegacyFeed(sessionId: string, threadId: string, running: boolean): { feed: WatchedFeed | null; full: boolean; error: string; detail: SessionDetail | null } {
  const [trace, setTrace] = useState<TraceFull | null>(null);
  const [detail, setDetail] = useState<SessionDetail | null>(null);
  const [error, setError] = useState("");
  const big = useRef(false);
  const timer = useRef<number>();
  const lastCount = useRef(-1);
  const lastFull = useRef(0);

  useEffect(() => {
    setTrace(null);
    setDetail(null);
    setError("");
    big.current = false;
    lastCount.current = -1;
    if (!sessionId) return;
    let alive = true;
    const loadTrace = () =>
      api
        .trace(sessionId)
        .then((t) => alive && setTrace(t))
        .catch((e: Error) => alive && setError(e.message));
    const loadFull = () => {
      lastFull.current = Date.now();
      return api
        .sessionDetail(sessionId)
        .then((d) => {
          if (!alive) return;
          big.current = d.messages.length + d.toolCalls.length > 3000;
          setDetail(d);
        })
        .catch(() => undefined);
    };
    void loadTrace().then(loadFull);
    const off = onLiveEvent((e) => {
      if (e.kind !== "upsert" || e.session.id !== sessionId || e.session.messageCount === lastCount.current) return;
      lastCount.current = e.session.messageCount;
      window.clearTimeout(timer.current);
      // Large transcripts reload the full detail at most every 10 s.
      const wait = big.current ? Math.max(1500, 10_000 - (Date.now() - lastFull.current)) : 1200;
      timer.current = window.setTimeout(() => void loadTrace().then(loadFull), wait);
    });
    return () => {
      alive = false;
      off();
      window.clearTimeout(timer.current);
    };
  }, [sessionId]);

  const feed = useMemo(() => (detail ? fromDetail(detail, trace, threadId, running) : trace ? fromTrace(trace, threadId, running) : null), [detail, trace, threadId, running]);
  return { feed, full: !!detail, error, detail };
}

export interface WatchedFeedState {
  feed: WatchedFeed | null;
  full: boolean;
  error: string;
  more: boolean; // older turns exist on the daemon (items mode only)
  loadEarlier?: () => void;
  earlierBusy: boolean;
  source: "items" | "legacy" | "";
}

const byStart = (a: { startedAt: number | string }, b: { startedAt: number | string }) => toMs(a.startedAt) - toMs(b.startedAt);

/** GET /api/session/{id}/items with cursor polling every 2 s while live; falls back to the full transcript on 404. */
export function useWatchedFeed(sessionId: string, threadId: string, running: boolean, live: boolean, machine?: string): WatchedFeedState {
  const [mode, setMode] = useState<"probe" | "items" | "legacy">("probe");
  const [ver, setVer] = useState(0);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const st = useRef({ items: new Map<string, OItem>(), runs: new Map<string, ORun>(), cursor: 0, more: false, earlier: undefined as number | undefined, loaded: false });
  const inflight = useRef(false);
  const legacy = useLegacyFeed(mode === "legacy" && !machine ? sessionId : "", threadId, running);

  const apply = (r: WatchedItems, first: boolean) => {
    const s = st.current;
    // An empty incremental poll changes nothing, so it must not re-render the transcript.
    if (!first && !r.reset && !r.items?.length && !r.runs?.length && r.cursor === s.cursor && s.loaded) return;
    if (first || r.reset) {
      s.items = new Map();
      s.runs = new Map();
      s.more = !!r.more;
      s.earlier = r.earlier;
    }
    for (const it of r.items || []) s.items.set(it.id, it);
    for (const ru of r.runs || []) s.runs.set(ru.id, ru);
    s.cursor = r.cursor;
    s.loaded = true;
    setVer((v) => v + 1);
  };

  useEffect(() => {
    st.current = { items: new Map(), runs: new Map(), cursor: 0, more: false, earlier: undefined, loaded: false };
    setMode("probe");
    setError("");
    setVer((v) => v + 1);
    if (!sessionId) return;
    let alive = true;
    let retry = 0;
    let tries = 0;
    // A new session writes its transcript only after the first turn starts, so the first read retries.
    const probe = () =>
      p7.watchedItems(sessionId, undefined, machine).then(
        (r) => {
          if (!alive) return;
          setError("");
          apply(r, true);
          setMode("items");
        },
        (e: Error) => {
          if (!alive) return;
          // The legacy transcript read is local only, so a peer 404 must show as an error, not as a blank feed.
          if (isMissing(e)) return machine ? setError(`${machine} has no transcript for this session, or it runs an older agent-monitor (${e.message})`) : setMode("legacy");
          setError(/no such file/i.test(e.message) ? "waiting" : e.message);
          retry = window.setTimeout(probe, Math.min(30_000, 2000 * 1.5 ** tries++));
        },
      );
    void probe();
    return () => {
      alive = false;
      window.clearTimeout(retry);
    };
  }, [sessionId]);

  // Incremental reads: every 2 s while live, every 5 s otherwise, plus one read on each live update.
  useEffect(() => {
    if (mode !== "items" || !sessionId) return;
    let alive = true;
    const poll = async () => {
      if (inflight.current || document.hidden) return;
      inflight.current = true;
      try {
        const r = await p7.watchedItems(sessionId, { after: st.current.cursor }, machine);
        if (alive) {
          apply(r, false);
          setError("");
        }
      } catch (e) {
        if (alive) setError((e as Error).message);
      } finally {
        inflight.current = false;
      }
    };
    // ! commands and turns that fire no hook still land in the transcript, so idle sessions poll too.
    const t = window.setInterval(poll, live ? 2000 : 5000);
    const off = onLiveEvent((e) => {
      if (e.kind === "upsert" && e.session.id === sessionId) void poll();
    });
    return () => {
      alive = false;
      window.clearInterval(t);
      off();
    };
  }, [mode, sessionId, live]);

  const loadEarlier = async () => {
    const s = st.current;
    if (busy || s.earlier == null) return;
    setBusy(true);
    try {
      const r = await p7.watchedItems(sessionId, { before: s.earlier }, machine);
      for (const it of r.items || []) if (!s.items.has(it.id)) s.items.set(it.id, it);
      for (const ru of r.runs || []) if (!s.runs.has(ru.id)) s.runs.set(ru.id, ru);
      s.more = !!r.more;
      s.earlier = r.earlier;
      setVer((v) => v + 1);
    } catch (e) {
      setError((e as Error).message);
    }
    setBusy(false);
  };

  const feed = useMemo<WatchedFeed | null>(() => {
    const s = st.current;
    if (!s.loaded) return null;
    return { runs: [...s.runs.values()].sort((a, b) => a.index - b.index), items: [...s.items.values()].sort(byStart) };
  }, [ver]); // eslint-disable-line react-hooks/exhaustive-deps

  if (mode === "legacy") return { feed: legacy.feed, full: legacy.full, error: legacy.error, more: false, earlierBusy: false, source: "legacy" };
  return { feed, full: true, error: feed ? "" : error, more: st.current.more, loadEarlier: st.current.more ? loadEarlier : undefined, earlierBusy: busy, source: mode === "items" ? "items" : "" };
}
