// Live store: one WebSocket to /ws, mirrored into React via useSyncExternalStore.
// Holds sessions, permission requests, talk inbox, connection state, and a
// small pub/sub for trace events (segment / chapter) so any view can react.
import { useSyncExternalStore } from "react";
import type { PermissionRequest, Session, Talk, WsEvent } from "../api/types";
import { isAttention } from "./format";

export interface LiveState {
  connected: boolean;
  sessions: Map<string, Session>;
  perms: Map<string, PermissionRequest>;
  talks: Map<string, Talk>;
  version: number; // bumps on every change (cheap selector invalidation)
}

type Listener = () => void;
type EventListener = (e: WsEvent) => void;

const state: LiveState = { connected: false, sessions: new Map(), perms: new Map(), talks: new Map(), version: 0 };
const listeners = new Set<Listener>();
const eventListeners = new Set<EventListener>();
let snapshot: LiveState = { ...state };
let ws: WebSocket | null = null;
let retryTimer: number | undefined;
let retryMs = 1500;

function emit() {
  state.version++;
  snapshot = { ...state, sessions: state.sessions, perms: state.perms, talks: state.talks };
  listeners.forEach((l) => l());
}

// Only a change to sessions, prompts or talks re-renders live views; other kinds go to event listeners alone.
function handle(e: WsEvent) {
  let changed = true;
  switch (e.kind) {
    case "snapshot":
      state.sessions = new Map(e.sessions.map((s) => [s.id, s]));
      break;
    case "upsert": {
      const old = state.sessions.get(e.session.id);
      if (old && JSON.stringify(old) === JSON.stringify(e.session)) {
        changed = false;
        break;
      }
      state.sessions = new Map(state.sessions);
      state.sessions.set(e.session.id, e.session);
      break;
    }
    case "remove":
      if (!state.sessions.has(e.id)) {
        changed = false;
        break;
      }
      state.sessions = new Map(state.sessions);
      state.sessions.delete(e.id);
      break;
    case "perm-add":
      state.perms = new Map(state.perms);
      state.perms.set(e.request.id, e.request);
      break;
    case "perm-remove":
      state.perms = new Map(state.perms);
      state.perms.delete(e.id);
      break;
    case "talk-request":
      state.talks = new Map(state.talks);
      state.talks.set(e.talk.id, e.talk);
      break;
    case "talk-resolved":
      state.talks = new Map(state.talks);
      state.talks.delete(e.talk.id);
      break;
    default:
      changed = false;
  }
  if (changed) emit();
  eventListeners.forEach((l) => {
    try {
      l(e);
    } catch {
      /* listener errors never break the feed */
    }
  });
}

export function connectLive() {
  if (ws && (ws.readyState === WebSocket.OPEN || ws.readyState === WebSocket.CONNECTING)) return;
  const proto = location.protocol === "https:" ? "wss:" : "ws:";
  ws = new WebSocket(`${proto}//${location.host}/ws`);
  ws.onopen = () => {
    // The daemon replays every open MCP prompt and talk on connect; drop what a missed remove left behind.
    state.perms = new Map();
    state.talks = new Map();
    state.connected = true;
    retryMs = 1500;
    emit();
  };
  ws.onmessage = (m) => {
    try {
      handle(JSON.parse(m.data) as WsEvent);
    } catch {
      /* ignore malformed frame */
    }
  };
  ws.onclose = () => {
    state.connected = false;
    emit();
    window.clearTimeout(retryTimer);
    retryTimer = window.setTimeout(connectLive, retryMs);
    retryMs = Math.min(retryMs * 2, 10_000);
  };
  ws.onerror = () => ws?.close();
}

export function useLive(): LiveState {
  return useSyncExternalStore(
    (l) => {
      listeners.add(l);
      return () => listeners.delete(l);
    },
    () => snapshot,
    () => snapshot,
  );
}

/** Subscribe to raw WS events (segment, chapter, perm-add, …). Returns unsubscribe. */
export function onLiveEvent(l: EventListener): () => void {
  eventListeners.add(l);
  return () => eventListeners.delete(l);
}

/** One definition of "needs attention", shared by the rail count, the threads
 *  filter and the side panel. A session with a pending MCP request is counted
 *  once, as the request. */
export interface AttentionSet {
  perms: PermissionRequest[];
  waiting: Session[];
  talks: Talk[];
  total: number;
}
export function attentionOf(live: LiveState): AttentionSet {
  const perms = [...live.perms.values()].sort((a, b) => a.createdAt - b.createdAt);
  const claimed = new Set(perms.map((p) => p.sessionId || ""));
  const waiting = [...live.sessions.values()]
    .filter((s) => isAttention(s.state) && !claimed.has(s.sessionId))
    .sort((a, b) => (a.state === "awaiting-permission" ? 0 : 1) - (b.state === "awaiting-permission" ? 0 : 1) || b.lastActivityAt - a.lastActivityAt);
  const talks = [...live.talks.values()].sort((a, b) => a.createdAt - b.createdAt);
  return { perms, waiting, talks, total: perms.length + waiting.length + talks.length };
}

/** Optimistic local removals (after a successful respond call). */
export function dropPerm(id: string) {
  state.perms = new Map(state.perms);
  state.perms.delete(id);
  emit();
}
export function dropTalk(id: string) {
  state.talks = new Map(state.talks);
  state.talks.delete(id);
  emit();
}
