import type {
  ContextRow,
  Health,
  PaneRegistration,
  PaneStatus,
  PaneView,
  Segment,
  Session,
  SessionDetail,
  Settings,
  Span,
  SpanDetail,
  Talk,
  ThreadLearningsResponse,
  ThreadResponse,
  ThreadStoryResponse,
  ThreadsResponse,
  TmuxPane,
  TraceFull,
  TraceMeta,
  TraceSummary,
} from "./types";

async function get<T>(url: string): Promise<T> {
  const r = await fetch(url, { cache: "no-store" });
  if (!r.ok) throw new Error(`${r.status} ${url}: ${(await r.text()).slice(0, 200)}`);
  return (await r.json()) as T;
}
async function send<T>(method: string, url: string, body?: unknown): Promise<T> {
  const r = await fetch(url, {
    method,
    headers: body !== undefined ? { "Content-Type": "application/json" } : undefined,
    body: body !== undefined ? JSON.stringify(body) : undefined,
  });
  const text = await r.text();
  let j: unknown = {};
  try {
    j = text ? JSON.parse(text) : {};
  } catch {
    j = { error: text };
  }
  if (!r.ok) {
    const err = (j as { error?: string }).error || `${r.status}`;
    throw Object.assign(new Error(err), { status: r.status, body: j });
  }
  return j as T;
}
const enc = encodeURIComponent;

export const api = {
  // sessions + legacy detail
  sessions: () => get<{ sessions: Session[] }>("/api/sessions"),
  sessionDetail: (id: string) => get<SessionDetail>(`/api/session/${enc(id)}/full`),
  health: () => get<Health>("/api/health"),
  summaries: () => get<{ summaries: Record<string, TraceSummary> }>("/api/summaries"),
  contextLive: () => get<{ context: ContextRow[] }>("/api/context/live"),

  // threads
  threads: (q: { tool?: string; state?: string; project?: string } = {}) => {
    const p = new URLSearchParams();
    if (q.tool) p.set("tool", q.tool);
    if (q.state) p.set("state", q.state);
    if (q.project) p.set("project", q.project);
    const s = p.toString();
    return get<ThreadsResponse>(`/api/threads${s ? "?" + s : ""}`);
  },
  thread: (id: string) => get<ThreadResponse>(`/api/thread/${enc(id)}`),
  threadStory: (id: string) => get<ThreadStoryResponse>(`/api/thread/${enc(id)}/story`),
  threadLearnings: (id: string) => get<ThreadLearningsResponse>(`/api/thread/${enc(id)}/learnings`),

  // trace
  trace: (id: string, q: { seg?: number; from?: number; to?: number; minDur?: number } = {}) => {
    const p = new URLSearchParams();
    if (q.seg !== undefined) p.set("seg", String(q.seg));
    if (q.from) p.set("from", String(q.from));
    if (q.to) p.set("to", String(q.to));
    if (q.minDur) p.set("minDur", String(q.minDur));
    const s = p.toString();
    return get<TraceFull>(`/api/trace/${enc(id)}/full${s ? "?" + s : ""}`);
  },
  segments: (id: string) => get<{ segments: Segment[]; meta: TraceMeta }>(`/api/trace/${enc(id)}/segments`),
  spans: (id: string, q: { seg?: number; from?: number; to?: number; minDur?: number } = {}) => {
    const p = new URLSearchParams();
    if (q.seg !== undefined) p.set("seg", String(q.seg));
    if (q.from) p.set("from", String(q.from));
    if (q.to) p.set("to", String(q.to));
    if (q.minDur) p.set("minDur", String(q.minDur));
    const s = p.toString();
    return get<{ spans: Span[]; total: number }>(`/api/trace/${enc(id)}/spans${s ? "?" + s : ""}`);
  },
  span: (id: string, spanId: string) => get<SpanDetail>(`/api/trace/${enc(id)}/span/${enc(spanId)}`),
  chapter: (id: string, seg: number) =>
    get<{ deterministic: Segment["chapter"]; enriched: Segment["chapter"] | null; segment: Segment }>(
      `/api/trace/${enc(id)}/chapter/${seg}`,
    ),
  enrich: (id: string, seg: number, force = false) =>
    send<{ ok: boolean; chapter: Segment["chapter"] }>("POST", `/api/trace/${enc(id)}/enrich/${seg}${force ? "?force=1" : ""}`),
  promote: (id: string, text: string, type = "project") =>
    send<{ ok: boolean; path: string }>("POST", `/api/trace/${enc(id)}/promote`, { text, type }),

  // settings
  settings: () => get<Settings>("/api/settings"),
  saveSettings: (s: Partial<Settings>) => send<Settings>("POST", "/api/settings", s),

  // pane bridge
  paneStatus: (sessionId: string) => get<PaneStatus>(`/api/session/pane/${enc(sessionId)}`),
  paneView: (sessionId: string, lines = 200) => get<PaneView>(`/api/session/pane-view/${enc(sessionId)}?lines=${lines}`),
  tmuxPanes: () => get<{ ok: boolean; panes: TmuxPane[]; error?: string }>("/api/tmux/panes"),
  panes: () => get<{ panes: PaneRegistration[] }>("/api/panes"),
  registerPane: (b: { tool: string; sessionId: string; paneId: string; cwd: string; source: string }) =>
    send<{ ok: boolean; agentId: string }>("POST", "/api/pane/register", b),
  forgetPane: (sessionId: string) => send<{ ok: boolean }>("DELETE", `/api/pane/registration/${enc(sessionId)}`),
  namePane: (sessionId: string, alias: string) => send<{ ok: boolean }>("POST", `/api/pane/name/${enc(sessionId)}`, { alias }),
  sendText: (sessionId: string, text: string, enter: boolean) =>
    send<{ ok: boolean; pane: string; session: string }>("POST", `/api/session/send/${enc(sessionId)}`, { text, enter }),
  sendKeys: (sessionId: string, keys: string[]) =>
    send<{ ok: boolean; pane: string; keys: string[] }>("POST", `/api/session/keys/${enc(sessionId)}`, { keys }),
  cancel: (sessionId: string) => send<{ ok: boolean; pane: string }>("POST", `/api/session/cancel/${enc(sessionId)}`),
  focus: (sessionId: string) =>
    send<{ ok: boolean; paneId?: string; sessionName?: string; terminal?: string; reason?: string }>(
      "POST",
      `/api/session/focus/${enc(sessionId)}`,
    ),

  // permissions + talks
  permissions: () => get<{ requests: unknown[] }>("/api/permissions"),
  respondPerm: (id: string, behavior: "allow" | "deny") =>
    send<{ ok: boolean }>("POST", `/api/permission/${enc(id)}/respond`, { behavior }),
  talks: () => get<{ talks: Talk[] }>("/api/talks"),
  talkRequest: (fromAgent: string, toAgent: string, message: string) =>
    send<{ ok: boolean; id: string; toAgent: string }>("POST", "/api/talk/request", { fromAgent, toAgent, message }),
  respondTalk: (id: string, behavior: "allow" | "deny") =>
    send<{ ok: boolean; status: string; pane?: string }>("POST", `/api/talk/${enc(id)}/respond`, { behavior }),
};

export type Api = typeof api;
