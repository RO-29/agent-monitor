// Who owns a continued session: the driven thread or a terminal. Mirrors orchHandover in orch_handover.go.
import type { Session } from "../../../api/types";
import { toMs, type ORun, type OThread } from "../../api/orch";
import type { V3Thread } from "../../api/types";
import type { Row } from "../../model";

export const HANDOVER_GRACE = 15_000;

export const nativeSid = (t: OThread) => (t.nativeId ? `${t.agent}:${t.nativeId}` : "");

export interface Handover {
  sid: string;
  terminal: boolean; // a terminal used the session after the driven thread last did
  live: boolean; // a terminal process holds it now
}

const ACTIVE = new Set(["running", "awaiting-permission", "awaiting-input"]);

/** The daemon's handover field wins; older daemons fall back to the session's activity after the thread's last touch. */
export function handoverOf(t: OThread, server: V3Thread | undefined, s: Session | undefined, runs: ORun[] = []): Handover {
  const sid = nativeSid(t);
  if (!sid) return { sid, terminal: false, live: false };
  const busy = t.status === "running" || t.status === "starting" || t.status === "waiting";
  let mark = Math.max(toMs(t.createdAt), toMs(t.updatedAt));
  for (const r of runs) mark = Math.max(mark, toMs(r.startedAt), r.endedAt ? toMs(r.endedAt) : 0);
  const after = !!s && s.lastActivityAt > mark + HANDOVER_GRACE;
  const live = !busy && after && !!s && ACTIVE.has(s.state);
  if (server?.handover) return { sid, terminal: server.handover === "terminal", live };
  return { sid, terminal: !busy && after, live };
}

/** The driven row (kind "driven") the daemon sent for an orchestrator thread. */
export const serverDrivenRow = (threads: V3Thread[], id: string) => threads.find((x) => x.kind === "driven" && (x.drivenId || x.id) === id);

/** Where "Open the terminal session" goes: the watched thread that holds the session, else the session itself. */
export function terminalTarget(sid: string, threads: V3Thread[]): string {
  return threads.find((x) => x.kind !== "driven" && x.sessions?.includes(sid))?.id || sid;
}

/** A list row for a session that has no watched thread on this daemon (older daemons hide driven copies). */
export function rowFromSession(s: Session): Row {
  return {
    id: s.id,
    title: s.title || (s.firstMessage || "").split("\n")[0] || s.id,
    agent: s.tool,
    cwd: s.cwd,
    session: s,
    approvals: [],
    perms: [],
    needs: false,
    bucket: s.state === "running" ? "running" : "older",
    lastAt: s.lastActivityAt,
    sinceAt: s.lastActivityAt,
  };
}
