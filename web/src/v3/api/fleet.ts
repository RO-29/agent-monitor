// Fleet: this machine and its peers as one set (GET /api/fleet/{machines,threads,usage}).
import { useSyncExternalStore } from "react";
import { mreq } from "./machines";
import type { V3Thread } from "./types";
import type { AgentUsage } from "./usage";

export interface FleetMachine {
  name: string;
  self?: boolean;
  level: "full" | "control" | "view" | string;
  status: string;
}
export type PeerThread = V3Thread & { machine: string; level: string };
export type FleetUsage = AgentUsage & { machine: string; level: string };

/** Full means the composer and answers work; view means read only. */
export const canAct = (level?: string) => level === "full" || level === "control";
export const levelText = (level?: string) => (canAct(level) ? "Full" : "View only");

/** A peer thread's row id: "@<machine>:<id>"; local ids never start with "@". */
export const peerKey = (machine: string, id: string) => `@${machine}:${id}`;
export function parsePeerKey(key?: string): { machine: string; id: string } | null {
  if (!key || key[0] !== "@") return null;
  const i = key.indexOf(":");
  return i > 1 ? { machine: key.slice(1, i), id: key.slice(i + 1) } : null;
}
/** Path prefix for a route on a machine: "" for this one, "/api/m/<name>" for a peer. */
export const machineBase = (machine?: string) => (machine ? `/api/m/${encodeURIComponent(machine)}` : "");

interface FleetState {
  machines: FleetMachine[];
  threads: PeerThread[];
  at: number;
  error: string;
}
let state: FleetState = { machines: [], threads: [], at: 0, error: "" };
const subs = new Set<() => void>();
let timer = 0;

async function poll() {
  try {
    const r = await mreq<{ machines: FleetMachine[]; threads: PeerThread[] }>("GET", "/api/fleet/threads");
    state = { machines: r.machines || [], threads: r.threads || [], at: Date.now(), error: "" };
  } catch (e) {
    state = { ...state, error: (e as Error).message };
  }
  subs.forEach((f) => f());
}

function subscribe(f: () => void) {
  subs.add(f);
  if (subs.size === 1) {
    void poll();
    timer = window.setInterval(poll, 15_000);
  }
  return () => {
    subs.delete(f);
    if (!subs.size) window.clearInterval(timer);
  };
}

/** One shared poll every 15 s while any view uses it. */
export function useFleet(): FleetState {
  return useSyncExternalStore(subscribe, () => state);
}

/** The machine label to show: nothing while only this machine exists. */
export function useMachineLabels(): { many: boolean; self: string } {
  const f = useFleet();
  return { many: f.machines.length > 1, self: f.machines.find((m) => m.self)?.name || "" };
}

export async function fleetUsage(): Promise<{ machines: FleetMachine[]; agents: FleetUsage[] }> {
  return mreq("GET", "/api/fleet/usage");
}
