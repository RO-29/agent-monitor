// Phase 6 C: driven (orchestrator) threads as first-class rows in Now, Threads, Control, Messages, Timeline and Search.
import type { SessionState } from "../api/types";
import { toMs, type ODetail, type OrchState, type OThread } from "./api/orch";
import type { TimelineLane, V3Thread } from "./api/types";

const enc = encodeURIComponent;
/** The orchestrator thread id behind a v3 row, when the row is a driven thread. */
export const drivenIdOf = (t?: V3Thread | null) => (t && t.kind === "driven" ? t.drivenId || t.id : undefined);
/** Driven rows open in the Workspace, watched rows on the thread page. */
export const threadHref = (id: string, t?: V3Thread | null) => {
  const d = drivenIdOf(t);
  return d ? `/w/${enc(d)}` : `/t/${enc(id)}`;
};

function stateOf(o: OThread, d?: ODetail): SessionState {
  if (o.status === "running" || o.status === "starting") return "running";
  if (o.status === "waiting" || d?.requests.some((r) => r.state === "open")) return "awaiting-permission";
  if (o.status === "closed") return "completed";
  if (o.status === "error") return "idle";
  return d?.runs.length || o.group === "ready" || o.group === "settled" ? "completed" : "idle";
}

/** A v3 row for an orchestrator thread, built on the client when the daemon does not list driven rows itself. */
export function drivenAsV3(o: OThread, d?: ODetail): V3Thread {
  const runs = d?.runs || [];
  const lastAgent = d ? [...d.items].reverse().find((i) => i.kind === "agent" && i.text) : undefined;
  const cost = runs.reduce((n, r) => n + (r.usage?.costUsd || 0), 0);
  const created = toMs(o.createdAt);
  const updated = toMs(o.updatedAt) || created;
  return {
    id: o.id,
    kind: "driven",
    drivenId: o.id,
    status: o.status,
    title: o.title,
    display: o.title,
    cwd: o.worktree && !o.worktree.removed ? o.worktree.path : o.cwd,
    repo: o.repo,
    ref: o.worktree?.branch || o.branch,
    tools: [o.agent] as V3Thread["tools"],
    sessions: [],
    state: stateOf(o, d),
    startedAt: created,
    lastAt: updated,
    tokens: { input: runs.reduce((n, r) => n + (r.usage?.inTokens || 0), 0), output: runs.reduce((n, r) => n + (r.usage?.outTokens || 0), 0), cacheRead: 0 } as V3Thread["tokens"],
    turns: runs.length,
    attention: o.status === "error" || o.status === "waiting",
    model: o.model,
    edges: [],
    segments: 1,
    compacts: 0,
    clears: 0,
    errors: o.status === "error" ? 1 : 0,
    errorsOpen: o.status === "error" ? 1 : 0,
    costUsd: cost,
    spendUsd: cost,
    costEstimated: false,
    learnings: 0,
    outputs: 0,
    lastOutcome: o.error || lastAgent?.text || "",
    segWeights: [1],
    segKinds: [],
    sessionSegs: [1],
    parentThread: o.parentId,
    childKind: o.parentId ? "delegated" : undefined,
    childCount: o.children?.length,
  } as V3Thread;
}

/** Cheap signature so the v3 store only changes when a driven row would look different. */
export function drivenSig(orch: OrchState): string {
  const out: string[] = [];
  for (const t of orch.threads.values()) {
    const d = orch.details.get(t.id);
    out.push(`${t.id}:${t.status}:${t.updatedAt}:${t.group || ""}:${d ? `${d.runs.length}:${d.items.length}:${d.requests.filter((r) => r.state === "open").length}` : ""}`);
  }
  return out.join("|");
}
export function drivenRows(orch: OrchState): V3Thread[] {
  return [...orch.threads.values()].map((t) => drivenAsV3(t, orch.details.get(t.id)));
}

/** Timeline lanes for driven threads: one work bar per run, a wait bar while a request is open. */
export function drivenLanes(orch: OrchState, from: number, to: number): TimelineLane[] {
  const out: TimelineLane[] = [];
  const now = Date.now();
  for (const t of orch.threads.values()) {
    const d = orch.details.get(t.id);
    const bars: TimelineLane["bars"] = [];
    if (d?.runs.length)
      for (const r of d.runs) {
        const a = toMs(r.startedAt);
        const b = r.endedAt ? toMs(r.endedAt) : now;
        bars.push({ from: a, to: b, kind: r.status === "failed" ? "error" : "work", agent: t.agent });
      }
    else {
      const a = toMs(t.createdAt);
      bars.push({ from: a, to: t.status === "running" || t.status === "starting" ? now : Math.max(a + 60_000, toMs(t.updatedAt)), kind: "work", agent: t.agent });
    }
    if (t.status === "waiting" || d?.requests.some((r) => r.state === "open")) bars.push({ from: toMs(t.updatedAt) - 60_000, to: now, kind: "wait", agent: t.agent });
    if (!bars.some((b) => b.to > from && b.from < to)) continue;
    out.push({ threadId: t.id, drivenId: t.id, title: t.title, display: t.title, agent: t.agent, state: t.status, parent: t.parentId, childKind: t.parentId ? "delegated" : undefined, startTurnTs: t.parentId ? toMs(t.createdAt) : undefined, errors: t.status === "error" ? 1 : 0, bars });
  }
  return out;
}
