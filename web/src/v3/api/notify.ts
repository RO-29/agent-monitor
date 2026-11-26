// Phase 6 notifications: SSE /api/notify/stream, the notification list with read state, in-app toasts, Web Push.
import { useEffect, useSyncExternalStore } from "react";
import { closePushNotify, playBeep, setPushOn } from "../../app/notify";
import { showToast } from "../../app/toast";
import { ApiError, isMissing } from "./client";
import { mockWanted, toMs } from "./orch";
import type { NKind } from "./settings";

export interface ONotification {
  id: string;
  kind: NKind;
  title: string;
  body?: string;
  threadId?: string; // driven thread id, links to /w/{id}
  url?: string; // any other in-app link
  at: number;
  read?: boolean;
  count?: number; // a batch: how many events it folds
  quiet?: boolean; // held by quiet hours: listed, never toasted
  resolved?: boolean; // the approval it was about ended, wherever it was answered
}

export interface NotifyState {
  status: "loading" | "ok" | "missing" | "error";
  list: ONotification[];
  unread: number;
  connected: boolean;
  version: number;
}

const s: NotifyState = { status: "loading", list: [], unread: 0, connected: false, version: 0 };
let snap: NotifyState = { ...s };
const listeners = new Set<() => void>();
function emit() {
  s.unread = s.list.filter((n) => !n.read).length;
  s.version++;
  snap = { ...s };
  listeners.forEach((l) => l());
}

// Read ids are also kept locally so a daemon without POST /api/notify/read still remembers them.
const READ_KEY = "am-notify-read";
function localRead(): Set<string> {
  try {
    return new Set(JSON.parse(localStorage.getItem(READ_KEY) || "[]") as string[]);
  } catch {
    return new Set();
  }
}
function saveLocalRead(ids: Set<string>) {
  try {
    localStorage.setItem(READ_KEY, JSON.stringify([...ids].slice(-500)));
  } catch {
    /* storage blocked */
  }
}

const norm = (n: ONotification, read: Set<string>): ONotification => ({ ...n, id: String(n.id), at: toMs(n.at), read: !!n.read || read.has(String(n.id)) });

let navigate: ((path: string) => void) | null = null;
export const setNotifyNav = (fn: (path: string) => void) => (navigate = fn);
export const linkOf = (n: ONotification) => n.url || (n.threadId ? `/w/${encodeURIComponent(n.threadId)}` : "/notifications");

const pageStart = Date.now();
function toastFor(n: ONotification) {
  if (n.at < pageStart - 5000 || n.quiet || n.resolved) return;
  const alert = n.kind === "needs-you" || n.kind === "error" || n.kind === "spend";
  showToast(n.title, n.body, {
    kind: alert ? "alert" : "info",
    onClick: () => {
      void markRead([n.id]);
      navigate?.(linkOf(n));
    },
  });
  if (alert && document.hidden) playBeep();
}

function add(n: ONotification, live: boolean) {
  const v = norm(n, localRead());
  const i = s.list.findIndex((x) => x.id === v.id);
  if (i >= 0) {
    s.list = s.list.slice();
    s.list[i] = { ...s.list[i], ...v };
    if (v.read || v.resolved) closePushNotify(v.id);
  } else {
    s.list = [v, ...s.list].sort((a, b) => b.at - a.at).slice(0, 300);
    if (live) toastFor(v);
  }
  emit();
}

async function get<T>(method: string, url: string, body?: unknown): Promise<T> {
  let r: Response;
  try {
    r = await fetch(url, { method, cache: "no-store", headers: body !== undefined ? { "Content-Type": "application/json" } : undefined, body: body !== undefined ? JSON.stringify(body) : undefined });
  } catch (e) {
    throw new ApiError(0, `daemon unreachable (${(e as Error).message})`);
  }
  const text = await r.text();
  let j: unknown;
  try {
    j = text ? JSON.parse(text) : undefined;
  } catch {
    j = undefined;
  }
  if (!r.ok) throw new ApiError(r.status, (j as { error?: string } | undefined)?.error || text.slice(0, 200) || `HTTP ${r.status}`, j);
  if (j === undefined && text.trimStart().startsWith("<")) throw new ApiError(404, "endpoint not available");
  return (j ?? {}) as T;
}

export const notifyApi = {
  list: () => get<{ items?: ONotification[]; notifications?: ONotification[] } | ONotification[]>("GET", "/api/notify?limit=200").then((r) => (Array.isArray(r) ? r : r.items || r.notifications || [])),
  read: (b: { ids?: string[]; all?: boolean }) => get<unknown>("POST", "/api/notify/read", b),
  vapid: () => get<{ publicKey: string }>("GET", "/api/notify/vapid"),
  subscribe: (sub: PushSubscriptionJSON) => get<unknown>("POST", "/api/notify/subscribe", sub),
  unsubscribe: (endpoint: string) => get<unknown>("POST", "/api/notify/unsubscribe", { endpoint }),
  test: () => get<unknown>("POST", "/api/notify/test"),
};

/** Asks the daemon for a test notification; under ?mock=1 it is added locally. */
export async function sendTest() {
  if (mock) return add({ id: `test-${Date.now()}`, kind: "finished", title: "Test notification", body: "Notifications from agent-monitor reach this device.", at: Date.now() }, true);
  await notifyApi.test();
}

export async function markRead(ids: string[] | "all") {
  const all = ids === "all";
  const which = new Set(all ? s.list.map((n) => n.id) : ids);
  if (![...which].some((id) => s.list.find((n) => n.id === id && !n.read))) return;
  s.list = s.list.map((n) => (which.has(n.id) ? { ...n, read: true } : n));
  const lr = localRead();
  which.forEach((id) => lr.add(id));
  saveLocalRead(lr);
  emit();
  if (mock) return;
  try {
    await notifyApi.read(all ? { all: true } : { ids: [...which] });
  } catch {
    /* kept locally */
  }
}

let es: EventSource | null = null;
let opened = false;
let retry = 0;
let timer: number | undefined;
function openStream() {
  if (typeof EventSource === "undefined") return;
  es?.close();
  // Numeric ids: resume after the newest one we hold so a reconnect replays only what we missed.
  const after = Math.max(0, ...s.list.map((n) => Number(n.id) || 0));
  const src = new EventSource(`/api/notify/stream${after ? `?after=${after}` : ""}`);
  es = src;
  src.onopen = () => {
    if (opened) void load();
    opened = true;
    retry = 0;
    s.connected = true;
    if (s.status !== "ok") s.status = "ok";
    emit();
  };
  src.onmessage = (m) => {
    try {
      const n = JSON.parse(m.data) as ONotification;
      if (n && n.id != null && n.title) add(n, true);
    } catch {
      /* heartbeat */
    }
  };
  src.onerror = () => {
    src.close();
    if (es === src) es = null;
    s.connected = false;
    emit();
    window.clearTimeout(timer);
    // Back off hard when the daemon has no stream at all.
    timer = window.setTimeout(openStream, s.status === "missing" ? 120_000 : Math.min(30_000, 1000 * 2 ** retry++));
  };
}

async function load() {
  try {
    const items = await notifyApi.list();
    const lr = localRead();
    for (const n of items) {
      const v = norm(n, lr);
      const i = s.list.findIndex((x) => x.id === v.id);
      if (i < 0) s.list.push(v);
      // A resolution made while the stream was down only shows in the list.
      else if ((v.read && !s.list[i].read) || (v.resolved && !s.list[i].resolved)) {
        s.list[i] = { ...s.list[i], read: s.list[i].read || v.read, resolved: v.resolved };
        closePushNotify(v.id);
      }
    }
    s.list = [...s.list].sort((a, b) => b.at - a.at);
    s.status = "ok";
  } catch (e) {
    s.status = isMissing(e) ? "missing" : "error";
  }
  emit();
}

let started = false;
let mock = false;
export function startNotify() {
  if (started) return;
  started = true;
  if (mockWanted()) {
    mock = true;
    void (import.meta.env.DEV ? import("./notifyMock") : Promise.reject()).then((m) => m.installNotifyMock(add, (st) => ((s.status = st), (s.connected = true), emit())));
    return;
  }
  void load().then(openStream);
}

export function useNotify(): NotifyState {
  useEffect(startNotify, []);
  return useSyncExternalStore(
    (l) => {
      listeners.add(l);
      return () => listeners.delete(l);
    },
    () => snap,
    () => snap,
  );
}

// ---- Web Push ----

export type PushState = "insecure" | "unsupported" | "denied" | "off" | "on" | "busy";
export const pushSupported = () => typeof window !== "undefined" && "serviceWorker" in navigator && "PushManager" in window && typeof Notification !== "undefined";

/** Push needs a secure origin (https or localhost); plain http over a Tailscale IP never gets it. */
export async function pushState(): Promise<PushState> {
  // ?insecure=1 in sample mode previews the plain-http explanation on localhost.
  if (!window.isSecureContext || (mock && new URLSearchParams(window.location.search).get("insecure") === "1")) return "insecure";
  if (!pushSupported()) return "unsupported";
  if (Notification.permission === "denied") return "denied";
  if (mock) return mockPush ? "on" : "off";
  const reg = await navigator.serviceWorker.getRegistration("/");
  const sub = await reg?.pushManager.getSubscription();
  return sub ? "on" : "off";
}
let mockPush = false;

const b64ToBytes = (b64: string) => {
  const pad = "=".repeat((4 - (b64.length % 4)) % 4);
  const raw = atob((b64 + pad).replace(/-/g, "+").replace(/_/g, "/"));
  return Uint8Array.from(raw, (c) => c.charCodeAt(0));
};

export async function enablePush(): Promise<PushState> {
  if (!window.isSecureContext) return "insecure";
  if (!pushSupported()) return "unsupported";
  const perm = await Notification.requestPermission();
  if (perm !== "granted") return perm === "denied" ? "denied" : "off";
  if (mock) {
    mockPush = true;
    return "on";
  }
  const { publicKey } = await notifyApi.vapid();
  const reg = await navigator.serviceWorker.register("/sw.js", { scope: "/" });
  await navigator.serviceWorker.ready;
  const sub = (await reg.pushManager.getSubscription()) || (await reg.pushManager.subscribe({ userVisibleOnly: true, applicationServerKey: b64ToBytes(publicKey) }));
  await notifyApi.subscribe(sub.toJSON());
  setPushOn(true);
  return "on";
}

export async function disablePush(): Promise<PushState> {
  if (mock) {
    mockPush = false;
    return "off";
  }
  const reg = await navigator.serviceWorker.getRegistration("/");
  const sub = await reg?.pushManager.getSubscription();
  if (sub) {
    try {
      await notifyApi.unsubscribe(sub.endpoint);
    } finally {
      await sub.unsubscribe();
    }
  }
  setPushOn(false);
  return "off";
}
