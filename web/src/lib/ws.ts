// Live store: one WebSocket to /ws, mirrored into React via useSyncExternalStore.
// Holds sessions, permission requests, talk inbox, connection state, and a
// small pub/sub for trace events (segment / chapter) so any view can react.
import { useSyncExternalStore } from "react";
import type { PermissionRequest, Session, Talk, WsEvent } from "../api/types";

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

function emit() {
  state.version++;
  snapshot = { ...state, sessions: state.sessions, perms: state.perms, talks: state.talks };
  listeners.forEach((l) => l());
}

function handle(e: WsEvent) {
  switch (e.kind) {
    case "snapshot":
      state.sessions = new Map(e.sessions.map((s) => [s.id, s]));
      break;
    case "upsert":
      state.sessions = new Map(state.sessions);
      state.sessions.set(e.session.id, e.session);
      break;
    case "remove":
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
      break;
  }
  emit();
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
    state.connected = true;
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
    retryTimer = window.setTimeout(connectLive, 1500);
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
