// Machines: peers this Mac can see (GET/POST/DELETE /api/peers, /api/m/<peer>/api/...) and grants on this Mac (/api/devices).
import { ApiError } from "./client";

export interface PeerInfo {
  name: string;
  url: string;
  level: string;
  addedAt: number;
  status: string; // online | offline | revoked | error ...
}
export interface Grant {
  id: string;
  name: string;
  kind: "device" | "machine";
  level: "view" | "control" | "admin";
  createdAt: number;
  lastUsedAt?: number;
  lastIp?: string;
  boundIp?: string;
  revokedAt?: number;
  current?: boolean;
}

export async function mreq<T>(method: string, url: string, body?: unknown): Promise<T> {
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
  if (!r.ok) throw new ApiError(r.status, (j as { error?: string } | undefined)?.error || `HTTP ${r.status}`, j);
  return (j ?? {}) as T;
}

const enc = encodeURIComponent;
export const machinesApi = {
  peers: () => mreq<{ peers: PeerInfo[] }>("GET", "/api/peers").then((r) => r.peers || []),
  addPeer: (url: string, code: string, name: string) => mreq<{ peer: PeerInfo }>("POST", "/api/peers", { url, code, name }),
  removePeer: (name: string) => mreq("DELETE", `/api/peers/${enc(name)}`),
  grants: () => mreq<Grant[]>("GET", "/api/devices"),
  setLevel: (id: string, level: string) => mreq<{ device: Grant }>("PATCH", `/api/devices/${enc(id)}`, { level }),
  revoke: (id: string) => mreq("DELETE", `/api/devices/${enc(id)}`),
  machineCode: (name: string, level: string) => mreq<{ device: Grant; pairUrl: string }>("POST", "/api/devices", { name, kind: "machine", level }),
  /** One peer route through this Mac: /api/m/<peer>/api/<route>. */
  peer: <T>(peer: string, method: string, route: string, body?: unknown) => mreq<T>(method, `/api/m/${enc(peer)}/api/${route}`, body),
};
