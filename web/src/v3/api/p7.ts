// Phase 7 API: composer pickers, uploads, review, merge, templates, schedules, watched items and pane.
import { ApiError } from "./client";
import { machineBase } from "./fleet";
import { mockWanted, rememberThread, req, type OAgent, type OChildSpec, type OItem, type OPermissionMode, type ORun, type OSandbox, type OTerminalStart, type OThread, type OWhere } from "./orch";

const enc = encodeURIComponent;
const list = <T,>(r: T[] | Record<string, T[] | undefined>, key: string): T[] => (Array.isArray(r) ? r : (r as Record<string, T[]>)[key] || []);
// Mocks load only in dev; a production build leaves them out of web/dist.
const mock = () => (import.meta.env.DEV ? import("./p7Mock") : Promise.reject(new Error("no mock in this build"))).then((m) => m.p7Mock);

export interface OFileHit {
  path: string;
  kind: "file" | "dir";
}
export interface OSkill {
  name: string;
  description?: string;
  source?: string;
  insert?: string; // exact text to insert when the daemon knows the agent's syntax
}
export interface OUpload {
  id: string;
  mime: string;
  name?: string;
  size?: number;
  url?: string;
  path?: string; // only for callers on the daemon's machine
}
export type OMergeStrategy = "squash" | "merge";
export interface OMergeBody {
  into?: string;
  strategy: OMergeStrategy;
  message?: string;
}
export interface OMergeResult {
  ok: boolean;
  into?: string;
  sha?: string;
  branch?: string;
  strategy?: OMergeStrategy;
  output?: string;
  thread?: OThread;
}
/** 409 body of a refused merge. */
export interface OMergeRefusal {
  error: string;
  reason?: "dirty-worktree" | "dirty-target" | "conflicts" | string;
  conflicts?: string[];
  files?: string[];
  output?: string;
}
export type OTemplateWorkspace = "here" | "worktree" | "read-only";
export interface OTemplate {
  id: string;
  name: string;
  description?: string;
  agent: OAgent;
  model?: string;
  permissionMode?: OPermissionMode;
  sandbox?: OSandbox;
  workspace: OTemplateWorkspace | string;
  prompt: string;
  children?: OChildSpec[];
  review?: boolean;
  reviewAgent?: string;
  builtin?: boolean;
  vars?: string[]; // computed by the daemon from {{name}} in the prompts
  where?: OWhere; // missing = headless (older templates)
}
/** A template or schedule run: a driven thread, or an agent started in a tmux session. */
export interface ORunResult {
  thread?: OThread;
  terminal?: OTerminalStart;
}
export interface OSchedule {
  id: string;
  templateId: string;
  cwd: string;
  vars?: Record<string, string>;
  cron: string;
  enabled: boolean;
  lastRunAt?: number;
  lastThreadId?: string;
  lastSession?: string;
  lastHostId?: string;
  where?: "" | OWhere; // "" = the template's
  nextRunAt?: number;
  lastError?: string;
  lastErrorAt?: number;
  lastSkipAt?: number;
  lastCatchUp?: boolean; // the last run was the single catch-up run after downtime
  nextRuns?: number[];
  name?: string;
}
export interface WatchedItems {
  id: string;
  agent: string;
  items: OItem[];
  runs: ORun[];
  cursor: number;
  more: boolean;
  earlier?: number;
  reset?: boolean;
}
/** One subagent or agent a watched session started (GET /api/session/{id}/children). */
export interface SessionChild {
  key: string;
  kind: "subagent" | "agent" | "talk";
  agent: string;
  sessionId?: string;
  agentId?: string;
  parentAgentId?: string;
  toolUseId?: string;
  parentItemId?: string;
  parentRunId?: string;
  parentMessageId?: string;
  parentPrompt?: string;
  by?: string;
  name?: string;
  subagentType?: string;
  description?: string;
  prompt?: string;
  command?: string;
  cwd?: string;
  model?: string;
  pane?: string;
  hostId?: string;
  status: "running" | "completed" | "failed" | "killed" | "stopped";
  background?: boolean;
  startedAt: number;
  endedAt?: number;
  inTokens?: number;
  outTokens?: number;
  costUsd?: number;
  linkBasis: string;
  hasTranscript?: boolean;
}
export interface SessionChildren {
  id: string;
  agent: string;
  pane?: string;
  hostId?: string;
  count: number;
  children: SessionChild[];
}
export interface WatchedPane {
  registered: boolean;
  live: boolean;
  sendable: boolean;
  agentId?: string;
  agent?: string;
  paneId?: string;
  tmuxTarget?: string;
  pid?: number;
  command?: string;
}

export const p7 = {
  files: async (cwd: string, q: string): Promise<OFileHit[]> =>
    mockWanted() ? (await mock()).files(cwd, q) : list(await req<OFileHit[] | { files: OFileHit[] }>("GET", `/api/orch/files?cwd=${enc(cwd)}&q=${enc(q)}`), "files"),
  skills: async (agent: string, cwd: string): Promise<OSkill[]> =>
    mockWanted() ? (await mock()).skills(agent) : list(await req<OSkill[] | { skills: OSkill[] }>("GET", `/api/orch/skills?agent=${enc(agent)}&cwd=${enc(cwd)}`), "skills"),
  review: async (id: string, b: { agent?: OAgent; model?: string; focus?: string }): Promise<OThread | undefined> => {
    if (mockWanted()) return (await mock()).review(id, b);
    // 201 {threadId, thread: child, parent}
    const r = await req<{ threadId?: string; thread?: OThread; parent?: OThread }>("POST", `/api/orch/threads/${enc(id)}/review`, b);
    if (r.thread) rememberThread(r.thread);
    if (r.parent) rememberThread(r.parent);
    return r.parent;
  },
  merge: async (id: string, b: OMergeBody): Promise<OMergeResult> => {
    const r = mockWanted() ? await (await mock()).merge(id, b) : await req<OMergeResult>("POST", `/api/orch/threads/${enc(id)}/merge`, b);
    if (r.thread) rememberThread(r.thread);
    return r;
  },
  templates: async (): Promise<OTemplate[]> => (mockWanted() ? (await mock()).templates() : list(await req<OTemplate[] | { templates: OTemplate[] }>("GET", "/api/orch/templates"), "templates")),
  saveTemplate: async (t: OTemplate): Promise<OTemplate> => {
    if (mockWanted()) return (await mock()).saveTemplate(t);
    const r = await req<OTemplate | { template: OTemplate }>(t.id ? "PUT" : "POST", t.id ? `/api/orch/templates/${enc(t.id)}` : "/api/orch/templates", t);
    return "template" in r ? r.template : r;
  },
  deleteTemplate: async (id: string) => (mockWanted() ? (await mock()).deleteTemplate(id) : req<unknown>("DELETE", `/api/orch/templates/${enc(id)}`)),
  runTemplate: async (id: string, cwd: string, vars?: Record<string, string>): Promise<ORunResult> => {
    const r = mockWanted() ? await (await mock()).runTemplate(id, cwd, vars) : await req<ORunResult>("POST", `/api/orch/templates/${enc(id)}/run`, { cwd, vars });
    if (r.thread) rememberThread(r.thread);
    return r;
  },
  schedules: async (): Promise<OSchedule[]> => (mockWanted() ? (await mock()).schedules() : list(await req<OSchedule[] | { schedules: OSchedule[] }>("GET", "/api/orch/schedules"), "schedules")),
  saveSchedule: async (sc: OSchedule): Promise<OSchedule> => {
    if (mockWanted()) return (await mock()).saveSchedule(sc);
    const r = await req<OSchedule | { schedule: OSchedule }>(sc.id ? "PUT" : "POST", sc.id ? `/api/orch/schedules/${enc(sc.id)}` : "/api/orch/schedules", sc);
    return "schedule" in r ? r.schedule : r;
  },
  deleteSchedule: async (id: string) => (mockWanted() ? (await mock()).deleteSchedule(id) : req<unknown>("DELETE", `/api/orch/schedules/${enc(id)}`)),
  runSchedule: async (id: string): Promise<ORunResult> => {
    const r = mockWanted() ? await (await mock()).runSchedule(id) : await req<ORunResult>("POST", `/api/orch/schedules/${enc(id)}/run-now`);
    if (r.thread) rememberThread(r.thread);
    return r;
  },
  watchedItems: async (id: string, q?: { after?: number; before?: number }, machine?: string): Promise<WatchedItems> => {
    if (mockWanted()) return (await mock()).watchedItems(id, q);
    const p = q?.after != null ? `?after=${q.after}` : q?.before != null ? `?before=${q.before}` : "";
    return req<WatchedItems>("GET", `${machineBase(machine)}/api/session/${enc(id)}/items${p}`);
  },
  sessionFiles: (id: string): Promise<{ paths: string[] }> => req<{ paths: string[] }>("GET", `/api/session/${enc(id)}/files`),
  sessionChildren: async (id: string): Promise<SessionChildren> => {
    if (mockWanted()) return { id, agent: "", count: 0, children: [] };
    return req<SessionChildren>("GET", `/api/session/${enc(id)}/children`);
  },
  subagentItems: (id: string, agentId: string): Promise<WatchedItems> => req<WatchedItems>("GET", `/api/session/${enc(id)}/children/${enc(agentId)}/items`),
  /** The daemon validates and computes the next runs in the Mac's local time. */
  cronPreview: async (expr: string): Promise<{ valid: boolean; error?: string; next: number[] }> => {
    if (mockWanted()) throw new Error("sample data mode");
    return req("GET", `/api/orch/cron?expr=${enc(expr)}`);
  },
  watchedPane: async (id: string): Promise<WatchedPane> => (mockWanted() ? (await mock()).watchedPane(id) : req<WatchedPane>("GET", `/api/session/${enc(id)}/pane`)),
  termQuestion: (id: string, machine?: string): Promise<TermQuestion> => (mockWanted() ? Promise.resolve({ pending: false, sendable: false, onScreen: false }) : req<TermQuestion>("GET", `${machineBase(machine)}/api/session/${enc(id)}/question`)),
  answerTerm: (id: string, body: TermAnswerBody, machine?: string): Promise<{ ok: boolean; sentAt: number }> => {
    if (mockWanted()) return Promise.reject(new Error("sample data mode"));
    return req("POST", `${machineBase(machine)}/api/session/${enc(id)}/answer`, body);
  },
};

/** GET /api/session/{id}/question: an AskUserQuestion or ExitPlanMode call that waits in the terminal. */
export interface TermQuestion {
  pending: boolean;
  tool?: "AskUserQuestion" | "ExitPlanMode";
  toolUseId?: string;
  questions?: { question: string; header?: string; multiSelect?: boolean; options: { label: string; description?: string }[] }[];
  plan?: string;
  planOptions?: { n: number; label: string; feedback?: boolean }[];
  askedAt?: number;
  held?: boolean;
  sendable: boolean;
  onScreen: boolean;
  why?: string;
  sentAt?: number;
  /** The question the terminal shows now; earlier ones were answered there. */
  step?: number;
}
export interface TermAnswer {
  option?: number;
  options?: number[];
  text?: string;
}
export interface TermAnswerBody {
  toolUseId?: string;
  answers?: TermAnswer[];
  planOption?: number;
  text?: string;
}

/** Uploads one file with progress. Mock mode never posts. */
export function uploadImage(threadId: string, file: File, onProgress: (f: number) => void): { done: Promise<OUpload>; abort: () => void } {
  if (mockWanted()) {
    let t = 0;
    let stop = false;
    const done = new Promise<OUpload>((res, rej) => {
      const tick = () => {
        if (stop) return rej(new ApiError(0, "cancelled"));
        t += 0.12 + Math.random() * 0.1;
        onProgress(Math.min(1, t));
        if (t >= 1) {
          if (file.size > MAX_FILE) return rej(new ApiError(413, `file is larger than ${MAX_FILE_LABEL}`));
          return res({ id: `up-${Date.now()}-${file.name}`, path: `~/.agent-monitor/uploads/${threadId}/${file.name}`, mime: file.type });
        }
        window.setTimeout(tick, 160);
      };
      window.setTimeout(tick, 120);
    });
    return { done, abort: () => (stop = true) };
  }
  const xhr = new XMLHttpRequest();
  const done = new Promise<OUpload>((res, rej) => {
    const fd = new FormData();
    fd.append("threadId", threadId);
    fd.append("file", file, file.name);
    xhr.open("POST", "/api/orch/uploads");
    xhr.upload.onprogress = (e) => e.lengthComputable && onProgress(e.loaded / e.total);
    xhr.onload = () => {
      let j: unknown;
      try {
        j = JSON.parse(xhr.responseText);
      } catch {
        j = undefined;
      }
      if (xhr.status >= 200 && xhr.status < 300 && j) res(((j as { upload?: OUpload }).upload || j) as OUpload);
      else rej(new ApiError(xhr.status, (j as { error?: string } | undefined)?.error || `upload failed (HTTP ${xhr.status})`, j));
    };
    xhr.onerror = () => rej(new ApiError(0, "daemon unreachable"));
    xhr.onabort = () => rej(new ApiError(0, "cancelled"));
    xhr.send(fd);
  });
  return { done, abort: () => xhr.abort() };
}

export const IMAGE_TYPES = ["image/png", "image/jpeg", "image/webp", "image/gif"];
/** POST /api/orch/uploads takes any file; images, pdf and text are read by type, everything else is kept as it is. */
export const MAX_FILE = 100 * 1024 * 1024;
export const MAX_FILE_LABEL = "100 MB";
export const PLAYABLE = (mime: string) => mime.startsWith("video/") || mime.startsWith("audio/");

/** Template prompt variables the run body must fill: everything except the built-ins. */
export const BUILTIN_VARS = new Set(["cwd", "branch", "date"]);
export function freeVars(prompts: string[]): string[] {
  const out = new Set<string>();
  for (const p of prompts) for (const m of (p || "").matchAll(/\{\{\s*([a-zA-Z_][\w-]*)\s*\}\}/g)) if (!BUILTIN_VARS.has(m[1])) out.add(m[1]);
  return [...out];
}

/** Variables a template run must fill: the daemon's list when present, else parsed from the prompts. */
/** Where a template runs: children force headless, and only Claude and Codex run in a terminal. */
export const tplWhere = (t: Pick<OTemplate, "where" | "children" | "agent">): OWhere =>
  t.where === "terminal" && !t.children?.length && (t.agent === "claude" || t.agent === "codex") ? "terminal" : "headless";
export const tplVars = (t: OTemplate): string[] =>
  t.vars ? t.vars.filter((v) => !BUILTIN_VARS.has(v)) : freeVars([t.prompt, ...(t.children || []).flatMap((c) => [c.title, c.prompt])]);
