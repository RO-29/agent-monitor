// Per-device tokens (V3-SPEC phase 5). Tokens stay in memory only: never logged, never stored in the browser.
import { ApiError } from "./client";
import { mockWanted, toMs } from "./orch";

export interface Device {
  id: string;
  name: string;
  createdAt: number;
  lastUsedAt?: number;
  lastIp?: string;
  revokedAt?: number;
  current?: boolean;
  /** Approvals and admin writes from this device need a Face ID signature. */
  bio?: boolean;
  /** The key is this browser's passkey, not a phone's Face ID key. */
  passkey?: boolean;
}
export interface NewDevice {
  device: Device;
  token: string;
  pairUrl: string;
}
export interface DeviceSettings {
  requireDeviceToken: boolean;
  remoteTerminal: boolean;
}

async function req<T>(method: string, url: string, body?: unknown): Promise<T> {
  let r: Response;
  try {
    r = await fetch(url, { method, cache: "no-store", credentials: "same-origin", headers: body !== undefined ? { "Content-Type": "application/json" } : undefined, body: body !== undefined ? JSON.stringify(body) : undefined });
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

const norm = (d: Device): Device => ({ ...d, createdAt: toMs(d.createdAt), lastUsedAt: toMs(d.lastUsedAt) || undefined, revokedAt: toMs(d.revokedAt) || undefined });

const real = {
  list: () => req<Device[] | { devices: Device[] }>("GET", "/api/devices").then((r) => (Array.isArray(r) ? r : r.devices || []).map(norm)),
  add: (name: string) => req<NewDevice>("POST", "/api/devices", { name }).then((r) => ({ ...r, device: norm(r.device) })),
  revoke: (id: string) => req<unknown>("DELETE", `/api/devices/${encodeURIComponent(id)}`),
  resetBio: (id: string) => req<unknown>("DELETE", `/api/devices/${encodeURIComponent(id)}/bio-key`),
  settings: () => req<Partial<DeviceSettings>>("GET", "/api/settings").then((r) => ({ requireDeviceToken: !!r.requireDeviceToken, remoteTerminal: !!r.remoteTerminal})),
  setRequire: (on: boolean) => req<Partial<DeviceSettings>>("POST", "/api/settings", { requireDeviceToken: on }).then((r) => ({ requireDeviceToken: r.requireDeviceToken ?? on })),
  setRemoteTerminal: (on: boolean) => req<Partial<DeviceSettings>>("POST", "/api/settings", { remoteTerminal: on }).then((r) => ({ remoteTerminal: r.remoteTerminal ?? on })),
};

// ---- ?mock=1: in-memory devices, nothing reaches the daemon ----
const now = Date.now();
const H = 3600_000;
let mockList: Device[] = [
  { id: "dev-mac", name: "MacBook Pro", createdAt: now - 40 * 24 * H, lastUsedAt: now - 60_000, lastIp: "100.101.12.4", current: true },
  { id: "dev-phone", name: "iPhone 16", createdAt: now - 12 * 24 * H, lastUsedAt: now - 3 * H, lastIp: "100.88.40.17", bio: true },
  { id: "dev-ipad", name: "Old iPad", createdAt: now - 90 * 24 * H, lastUsedAt: now - 30 * 24 * H, lastIp: "100.70.2.9", revokedAt: now - 20 * 24 * H },
];
let mockRequire = false;
let mockRemoteTerm = false;
const later = <T,>(v: T) => new Promise<T>((r) => setTimeout(() => r(v), 150));
const mock: typeof real = {
  list: () => later(mockList.map((d) => ({ ...d }))),
  add: (name) => {
    const d: Device = { id: `dev-${Date.now()}`, name, createdAt: Date.now() };
    mockList = [...mockList, d];
    const token = "amd_MOCKxDoNotUse-" + Math.random().toString(36).slice(2, 12) + "QpZ7wK3rT8vN";
    return later({ device: d, token, pairUrl: `http://100.101.12.4:7777/pair#${token}` });
  },
  revoke: (id) => {
    mockList = mockList.map((d) => (d.id === id ? { ...d, revokedAt: Date.now() } : d));
    return later({});
  },
  resetBio: (id) => {
    mockList = mockList.map((d) => (d.id === id ? { ...d, bio: false } : d));
    return later({});
  },
  settings: () => later({ requireDeviceToken: mockRequire, remoteTerminal: mockRemoteTerm}),
  setRequire: (on) => {
    const others = mockList.filter((d) => !d.revokedAt && !d.current).length;
    if (on && others < 1) return Promise.reject(new ApiError(409, "Refused: no other active device. Add a device first, or you would lock this one out."));
    mockRequire = on;
    return later({ requireDeviceToken: on });
  },
  setRemoteTerminal: (on) => {
    mockRemoteTerm = on;
    return later({ remoteTerminal: on });
  },
};

export const devicesApi = () => (mockWanted() ? mock : real);
