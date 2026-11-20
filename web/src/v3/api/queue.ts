// Per-thread message queue (GET/POST/PATCH/DELETE /api/queue/{thread}); live through queue.updated on the orch stream.
import { useEffect, useSyncExternalStore } from "react";
import { mockWanted, queueSink, req, sendMessage } from "./orch";

export type QState = "queued" | "sending" | "sent" | "failed";
export interface QItem {
  id: string;
  threadId: string;
  text: string;
  attachments?: string[];
  session?: string;
  createdAt: number;
  state: QState;
  error?: string;
  mode?: "turn" | "steer" | "pane";
}
/** A message waiting in the agent CLI's own queue, read from its transcript. */
export interface CliItem {
  id: string;
  text: string;
  at: number;
  kind: "user" | "system" | "bash";
  /** A ! command agent-monitor sent: "waiting" or "running" until its result is in the transcript. */
  status?: "waiting" | "running";
  /** A running ! command's output as the terminal shows it now. */
  live?: string;
}
export interface CliView {
  agent: string;
  supported: boolean;
  items: CliItem[];
  hidden?: number;
  reachable: boolean;
  note?: string;
  canRemove: boolean;
  removeWhy?: string;
  /** Text typed in the terminal's input box and not sent yet. */
  draft?: string;
}
export interface QView {
  items: QItem[];
  paused?: "terminal" | string;
  terminal?: string[];
  cli?: CliView;
  /** The agent terminal's footer (model, context, mode), read from its pane. */
  statusLine?: string[];
  status: "loading" | "ok" | "missing" | "error";
}

const enc = encodeURIComponent;
const EMPTY: QView = { items: [], status: "loading" };
const views = new Map<string, QView>();
const subs = new Set<() => void>();
const emit = () => subs.forEach((f) => f());
const set = (id: string, v: Partial<QView>) => {
  views.set(id, { ...(views.get(id) || EMPTY), ...v });
  emit();
};

/** Called by the orch stream for queue.updated events. */
export function applyQueueEvent(threadId: string, items: QItem[]) {
  set(threadId, { items, status: "ok" });
}

queueSink.fn = (id, items) => applyQueueEvent(id, items as QItem[]);

// ---- dev mock (?mock=1): an in-memory queue with three messages on the running "cmt" thread ----
let mockSeeded = false;
function mockSeed() {
  if (mockSeeded) return;
  mockSeeded = true;
  const now = Date.now();
  const mk = (n: number, text: string): QItem => ({ id: `q_m${n}`, threadId: "cmt", text, createdAt: now - (4 - n) * 40_000, state: "queued" });
  views.set("cmt", {
    status: "ok",
    items: [
      mk(1, "Also cover the group mention in thread replies, not only top-level comments."),
      mk(2, "Run the worker tests after that and paste the failures, if any."),
      { ...mk(3, "Then open a draft PR against main."), state: "failed", error: "The day spend limit is reached. Allow one more turn, then send it again." },
    ],
  });
}
let mockN = 10;
function mockItems(id: string) {
  return (views.get(id)?.items || []).slice();
}

async function call(method: string, url: string, body?: unknown) {
  const r = await req<{ items?: QItem[]; item?: QItem; mode?: string }>(method, url, body);
  return r;
}

export const queueApi = {
  async load(threadId: string, session?: string, cliAll = false) {
    if (mockWanted()) {
      mockSeed();
      if (!views.has(threadId)) set(threadId, { items: [], status: "ok" });
      return;
    }
    try {
      const qs = new URLSearchParams();
      if (session) qs.set("session", session);
      if (cliAll) qs.set("cliAll", "1");
      const r = await req<{ items?: QItem[]; paused?: string; terminal?: string[]; cli?: CliView; statusLine?: string[] }>("GET", `/api/queue/${enc(threadId)}${qs.toString() ? `?${qs}` : ""}`);
      set(threadId, { items: r.items || [], paused: r.paused, terminal: r.terminal, cli: r.cli, statusLine: r.statusLine, status: "ok" });
    } catch (e) {
      const st = (e as { status?: number }).status;
      set(threadId, { status: st === 404 || st === 405 ? "missing" : "error" });
    }
  },
  async add(threadId: string, text: string, opts: { session?: string; attachments?: string[] } = {}) {
    if (mockWanted()) {
      set(threadId, { items: [...mockItems(threadId), { id: `q_m${++mockN}`, threadId, text, createdAt: Date.now(), state: "queued", attachments: opts.attachments }], status: "ok" });
      return;
    }
    const r = await call("POST", `/api/queue/${enc(threadId)}`, { text, session: opts.session, attachments: opts.attachments?.length ? opts.attachments : undefined });
    if (r.items) set(threadId, { items: r.items, status: "ok" });
  },
  async remove(threadId: string, id: string) {
    if (mockWanted()) return set(threadId, { items: mockItems(threadId).filter((x) => x.id !== id) });
    const r = await call("DELETE", `/api/queue/${enc(threadId)}/${enc(id)}`);
    if (r.items) set(threadId, { items: r.items });
  },
  async patch(threadId: string, id: string, p: { text?: string; index?: number }) {
    if (mockWanted()) {
      const items = mockItems(threadId);
      const i = items.findIndex((x) => x.id === id);
      if (i < 0) return;
      const [it] = items.splice(i, 1);
      const n = p.text != null ? { ...it, text: p.text, state: "queued" as QState, error: undefined } : it;
      items.splice(p.index != null ? Math.max(0, Math.min(items.length, p.index)) : i, 0, n);
      return set(threadId, { items });
    }
    const r = await call("PATCH", `/api/queue/${enc(threadId)}/${enc(id)}`, p);
    if (r.items) set(threadId, { items: r.items });
  },
  async sendNow(threadId: string, id: string) {
    if (mockWanted()) {
      const it = mockItems(threadId).find((x) => x.id === id);
      if (!it) return;
      set(threadId, { items: mockItems(threadId).filter((x) => x.id !== id) });
      if (threadId.startsWith("claude:") || threadId.startsWith("codex:")) return;
      await sendMessage(threadId, it.text, it.attachments);
      return;
    }
    const r = await call("POST", `/api/queue/${enc(threadId)}/${enc(id)}/send-now`);
    if (r.items) set(threadId, { items: r.items });
  },
};

/** The CLI's own queue: typing goes to the CLI as if typed; the other actions press its keys. */
export const cliApi = {
  /** Types the message into the CLI; a 409 means the terminal cannot take it now. Answers whether the CLI queued it. */
  async send(session: string, text: string, attachments?: string[]) {
    return (await cliApi.sendFull(session, text, attachments)).queued;
  },
  /** As send, plus the panel a Claude slash command drew (/usage); menu means it waits for an answer in the terminal. */
  async sendFull(session: string, text: string, attachments?: string[]) {
    const r = await req<{ queued?: boolean; panel?: string; menu?: boolean }>("POST", `/api/session/${enc(session)}/cli-queue/send`, attachments?.length ? { text, attachments } : { text });
    return { queued: !!r.queued, panel: r.panel || "", menu: !!r.menu };
  },
  /** Deletes the text in the terminal's input box; refused while a prompt is open. */
  async clearDraft(threadId: string, session: string) {
    await req("POST", `/api/session/${enc(session)}/cli-queue/clear-draft`, {});
    const cur = views.get(threadId);
    if (cur?.cli) set(threadId, { cli: { ...cur.cli, draft: undefined } });
  },
  /** Forgets a sent ! command whose result never reached the transcript; nothing is typed into the terminal. */
  async forget(threadId: string, session: string, id: string) {
    await req("POST", `/api/session/${enc(session)}/cli-queue/forget`, { id });
    const cur = views.get(threadId);
    if (cur?.cli) set(threadId, { cli: { ...cur.cli, items: (cur.cli.items || []).filter((x) => x.id !== id) } });
  },
  async act(threadId: string, session: string, action: "send-now" | "edit" | "remove", id?: string) {
    const r = await req<{ text?: string; items?: CliItem[] }>("POST", `/api/session/${enc(session)}/cli-queue/${action}`, { id });
    const cur = views.get(threadId);
    if (cur?.cli && r.items) set(threadId, { cli: { ...cur.cli, items: r.items } });
    return r.text || "";
  },
};

/** The queue of one thread; loads on open and on every reconnect, then follows the stream. */
export function useQueue(threadId: string, session?: string, poll = false, opts: { cliAll?: boolean; bump?: number } = {}): QView {
  const { cliAll = false, bump = 0 } = opts;
  useEffect(() => {
    if (!threadId) return;
    void queueApi.load(threadId, session, cliAll);
    // The orch stream carries changes; a watched thread also polls for the terminal's own queue.
    if (!poll) return;
    const t = window.setInterval(() => void queueApi.load(threadId, session, cliAll), 5000);
    return () => window.clearInterval(t);
  }, [threadId, session, poll, cliAll, bump]);
  const view = useSyncExternalStore(
    (f) => (subs.add(f), () => subs.delete(f)),
    () => views.get(threadId) || EMPTY,
  );
  // While a ! command runs, its output on the terminal refreshes every 1.5 s.
  const running = !!view.cli?.items?.some((i) => i.kind === "bash" && i.status === "running");
  useEffect(() => {
    if (!threadId || !poll || !running) return;
    const t = window.setInterval(() => void queueApi.load(threadId, session, cliAll), 1500);
    return () => window.clearInterval(t);
  }, [threadId, session, poll, cliAll, running]);
  return view;
}
