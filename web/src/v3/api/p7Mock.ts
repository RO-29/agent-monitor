// Dev-only phase 7 sample data (?mock=1): files, skills, uploads, review, merge, templates, schedules, watched items.
import { ApiError } from "./client";
import { applyEvent, createThread, orchSeq, orchThread, rememberThread, startTerminal, type OItem, type ORun, type OThread } from "./orch";
import { tplWhere, type OFileHit, type OMergeBody, type OMergeResult, type ORunResult, type OSchedule, type OSkill, type OTemplate, type WatchedItems, type WatchedPane } from "./p7";
import { fuzzy } from "../views/workspace/Composer";

const wait = <T,>(v: T, ms = 160) => new Promise<T>((r) => setTimeout(() => r(v), ms));
const fail = (status: number, error: string, extra: Record<string, unknown> = {}) => new Promise<never>((_, rej) => setTimeout(() => rej(new ApiError(status, error, { error, ...extra })), 300));
const H = "/Users/dev";

const FILES = [
  "auth.go", "auth/device_tokens.go", "auth/device_tokens_test.go", "auth/middleware.go", "docs/auth/device-tokens.md", "docs/", "docs/auth/",
  "server/routes.go", "server/routes_test.go", "server/", "web/src/v3/views/workspace/Composer.tsx", "web/src/v3/views/workspace/DrivenChat.tsx",
  "web/src/v3/api/orch.ts", "web/src/v3/styles/workspace.css", "web/", "orch.go", "orch_review.go", "orch_merge.go", "schedules.go", "templates.go", "go.mod", "README.md", "V3-SPEC.md",
];
const SKILLS: OSkill[] = [
  { name: "code-review", description: "Review the current diff for correctness bugs", source: "plugin" },
  { name: "simplify", description: "Review changed code for reuse and simplification", source: "user" },
  { name: "security-review", description: "Security review of the pending changes", source: "user" },
  { name: "tdd", description: "Test-driven development, red-green-refactor", source: "plugin" },
  { name: "dev-tunnel", description: "Open, check and repair internal tunnels", source: "user" },
  { name: "release-notes", description: "Draft release notes from merged PRs", source: "project" },
];
const CODEX_PROMPTS: OSkill[] = [
  { name: "review", description: "Review the working tree diff", source: "codex prompts" },
  { name: "explain", description: "Explain the selected code", source: "codex prompts" },
];

let templates: OTemplate[] = [
  { id: "builtin-fix-and-review", builtin: true, name: "Fix and review", description: "Claude fixes in a new worktree. Codex reviews when the run ends.", agent: "claude", model: "opus", permissionMode: "acceptEdits", workspace: "worktree", prompt: "Fix {{issue}} in {{cwd}}. Work on a new branch, run the tests before you stop, and say what changed.", review: true },
  { id: "builtin-read-only-investigation", builtin: true, name: "Read-only investigation", description: "Claude plans and reads. It edits nothing.", agent: "claude", permissionMode: "plan", workspace: "read-only", prompt: "Investigate {{question}}. Read only: do not edit files. Report what you found with file and line references." },
  { id: "tpl-deps", name: "Dependency check", description: "Outdated and vulnerable dependencies, security fixes first.", agent: "codex", sandbox: "read-only", workspace: "here", prompt: "List outdated dependencies in {{cwd}} on {{branch}} as of {{date}}. Flag security fixes first." },
];
const at = (h: number, m: number, daysAgo = 0) => {
  const d = new Date();
  d.setDate(d.getDate() - daysAgo);
  d.setHours(h, m, 0, 0);
  return d.getTime();
};
let schedules: OSchedule[] = [
  { id: "sch-1", templateId: "tpl-deps", cwd: `${H}/agent-monitor`, cron: "30 2 * * 1-5", enabled: true, lastRunAt: at(2, 30), lastThreadId: "dsk" },
  { id: "sch-2", templateId: "builtin-fix-and-review", cwd: `${H}/dr-router`, vars: { issue: "the flaky retry budget test" }, cron: "0 9 * * 1", enabled: false, lastRunAt: at(9, 0, 7), lastThreadId: "cq", lastCatchUp: true },
];

let seq = 0;
const nextSeq = () => (seq = Math.max(seq, orchSeq()) + 1);
let mergeTries = 0;

function setReview(id: string, review: OThread["review"]) {
  const t = orchThread(id);
  if (!t) return;
  applyEvent({ seq: nextSeq(), threadId: id, at: Date.now(), type: "thread.updated", data: { thread: { ...t, review, updatedAt: Date.now() } } });
}
function reviewItem(id: string, runId: string, child: string): OItem {
  return { id: `rv-${id}-${Date.now()}`, threadId: id, runId, kind: "review", status: "completed", text: child, startedAt: Date.now() };
}

/** Seeds a finished review on k1 and a running one on cq. */
export function seedP7() {
  const now = Date.now();
  const k1 = orchThread("k1");
  if (k1) {
    applyEvent({ seq: nextSeq(), threadId: "k1", at: now, type: "item.completed", data: { item: { ...reviewItem("k1", "k1-r1", "k1-rv"), startedAt: now - 9.6 * 60_000 } } });
    setReview("k1", {
      state: "done",
      childId: "k1-rv",
      agent: "codex",
      startedAt: now - 9.9 * 60_000,
      endedAt: now - 8.2 * 60_000,
      summary: "The store is sound: tokens are hashed with SHA-256 and compared in constant time. Two problems need a fix before merge.",
      findings: [
        { severity: "high", file: "auth/device_tokens.go", line: 88, title: "Revoke does not persist when the save fails", body: "`Revoke` removes the device from the map and then calls `save()`, but ignores its error. A failed write leaves the token revoked in memory only, so it works again after a restart.\n\nReturn the error and put the device back when `save()` fails." },
        { severity: "medium", file: "auth/device_tokens.go", line: 41, title: "Issue accepts an empty device name", body: "An empty name makes two devices impossible to tell apart in Settings. Reject it with 400." },
        { severity: "low", file: "auth/device_tokens_test.go", line: 12, title: "Test uses a fixed token", body: "Use `rand.Read` so the test also covers the hash path for random input." },
        { severity: "info", title: "Consider a last-used timestamp", body: "A `lastUsedAt` per device helps find stale phones." },
      ],
    });
  }
  setReview("cq", { state: "running", childId: "cq-rv", agent: "codex", startedAt: now - 70_000 });
}

export const p7Mock = {
  files: async (_cwd: string, q: string): Promise<OFileHit[]> => {
    const hits = FILES.map((p) => ({ p, m: fuzzy(q, p) }))
      .filter((x) => x.m)
      .sort((a, b) => b.m!.score - a.m!.score)
      .slice(0, 50)
      .map(({ p }) => ({ path: p.replace(/\/$/, ""), kind: (p.endsWith("/") ? "dir" : "file") as OFileHit["kind"] }));
    return wait(hits, 90);
  },
  skills: async (agent: string): Promise<OSkill[]> => wait(agent === "codex" ? [...CODEX_PROMPTS, ...SKILLS.slice(0, 2)] : SKILLS, 120),
  review: async (id: string, b: { agent?: string; focus?: string }): Promise<OThread | undefined> => {
    const t = orchThread(id);
    if (!t) return fail(404, "thread not found");
    if (t.review?.state === "running") return fail(409, "a review is already running");
    const child = `${id}-rv${Date.now() % 1000}`;
    const now = Date.now();
    const runId = `${id}-r9`;
    applyEvent({ seq: nextSeq(), threadId: id, at: now, type: "item.completed", data: { item: reviewItem(id, runId, child) } });
    setReview(id, { state: "running", childId: child, agent: b.agent || "codex", startedAt: now });
    window.setTimeout(() => {
      setReview(id, {
        state: "done",
        childId: child,
        agent: b.agent || "codex",
        startedAt: now,
        endedAt: Date.now(),
        summary: b.focus ? `Checked the diff with a focus on ${b.focus}. One issue.` : "The change does what the prompt asked. One issue.",
        findings: [{ severity: "medium", file: "server/routes.go", line: 88, title: "Unreachable code after the redirect", body: "`return` on line 87 makes the log call on line 88 dead." }],
      });
    }, 4000);
    return orchThread(id);
  },
  merge: async (id: string, b: OMergeBody): Promise<OMergeResult> => {
    console.info("mock merge", id, b);
    if (id === "k1" && mergeTries++ === 0)
      return fail(409, "merge conflicts", { files: ["auth.go", "auth/middleware.go", "docs/auth/device-tokens.md"], output: "CONFLICT (content): Merge conflict in auth.go" });
    return wait({ ok: true, into: b.into || "main", sha: "5e1d9a07c4b2", output: `${b.strategy === "squash" ? "Squash commit" : "Merge made by the 'ort' strategy."}\n 3 files changed, 158 insertions(+), 7 deletions(-)` }, 500);
  },
  templates: async () => wait(structuredClone(templates)),
  saveTemplate: async (t: OTemplate): Promise<OTemplate> => {
    if (t.builtin) return fail(403, "built-in templates cannot change; copy it");
    const n = { ...t, id: t.id || `tpl-${Date.now()}` };
    templates = templates.some((x) => x.id === n.id) ? templates.map((x) => (x.id === n.id ? n : x)) : [...templates, n];
    return wait(n);
  },
  deleteTemplate: async (id: string) => {
    if (templates.find((x) => x.id === id)?.builtin) return fail(403, "built-in templates cannot be deleted");
    templates = templates.filter((x) => x.id !== id);
    return wait({ ok: true });
  },
  runTemplate: async (id: string, cwd: string, vars?: Record<string, string>, where?: "" | "terminal" | "headless"): Promise<ORunResult> => {
    const t = templates.find((x) => x.id === id);
    if (!t) return fail(404, "template not found");
    const all: Record<string, string> = { cwd, branch: "main", date: new Date().toISOString().slice(0, 10), ...(vars || {}) };
    const prompt = t.prompt.replace(/\{\{\s*([\w-]+)\s*\}\}/g, (m, k: string) => all[k] ?? m);
    if (tplWhere({ ...t, where: where || t.where }) === "terminal")
      return { terminal: await startTerminal({ agent: t.agent as "claude" | "codex", cwd, prompt, model: t.model, permissionMode: t.permissionMode, sandbox: t.sandbox }) };
    const thread = await createThread({ agent: t.agent, cwd, prompt, model: t.model, permissionMode: t.permissionMode, sandbox: t.sandbox, workspace: t.workspace === "worktree" ? "worktree" : undefined, children: t.children });
    const tagged = { ...thread, templateId: id };
    rememberThread(tagged);
    return { thread: tagged };
  },
  schedules: async () => wait(structuredClone(schedules)),
  saveSchedule: async (sc: OSchedule): Promise<OSchedule> => {
    const n = { ...sc, id: sc.id || `sch-${Date.now()}` };
    schedules = schedules.some((x) => x.id === n.id) ? schedules.map((x) => (x.id === n.id ? n : x)) : [...schedules, n];
    return wait(n);
  },
  deleteSchedule: async (id: string) => {
    schedules = schedules.filter((x) => x.id !== id);
    return wait({ ok: true });
  },
  runSchedule: async (id: string): Promise<ORunResult> => {
    const sc = schedules.find((x) => x.id === id);
    if (!sc) return fail(404, "schedule not found");
    const r = await p7Mock.runTemplate(sc.templateId, sc.cwd, sc.vars, sc.where);
    if (r.terminal) {
      schedules = schedules.map((x) => (x.id === id ? { ...x, lastRunAt: Date.now(), lastSession: r.terminal!.session, lastHostId: r.terminal!.hostId } : x));
      return r;
    }
    const thread = { ...r.thread!, scheduleId: id };
    rememberThread(thread);
    schedules = schedules.map((x) => (x.id === id ? { ...x, lastRunAt: Date.now(), lastThreadId: thread.id } : x));
    return { thread };
  },
  watchedItems: async (id: string, q?: { after?: number; before?: number }): Promise<WatchedItems> => {
    const all = watchedTranscript(id);
    const PAGE = 6;
    const turns = all.runs.length;
    let from = Math.max(0, turns - PAGE);
    let to = turns;
    if (q?.after != null) return wait({ id, agent: "claude", items: [], runs: [], cursor: q.after, more: false });
    if (q?.before != null) {
      to = q.before;
      from = Math.max(0, to - PAGE);
    }
    const runs = all.runs.slice(from, to);
    const ids = new Set(runs.map((r) => r.id));
    return wait({ id, agent: "claude", runs, items: all.items.filter((i) => ids.has(i.runId)), cursor: turns, more: from > 0, earlier: from > 0 ? from : undefined }, 220);
  },
  watchedPane: async (id?: string): Promise<WatchedPane> =>
    wait(id?.includes("mock-pane") ? { registered: true, live: true, sendable: true, agentId: id, agent: "claude", paneId: "%42" } : { registered: false, live: false, sendable: false }),
};

// A 14-turn transcript; turn offsets stand in for byte cursors.
function watchedTranscript(id: string): { runs: ORun[]; items: OItem[] } {
  const start = Date.now() - 190 * 60_000;
  const runs: ORun[] = [];
  const items: OItem[] = [];
  const asks = [
    "Read the AU tracker filters and tell me how state is kept.",
    "Move the filter state into the URL query.",
    "Keep the old localStorage key as a fallback for one release.",
    "Add a test for the URL round trip.",
    "The empty state flashes on load. Fix it.",
    "Rename the query keys to region and status.",
    "Run the type check and the unit tests.",
    "Make the clear button reset the page to 1.",
    "Show a count next to each region chip.",
    "Remove the old localStorage key now.",
    "Check the phone layout at 400 px.",
    "Update the README section on filters.",
    "Run the tests again.",
    "Summarize what changed for the PR description.",
  ];
  asks.forEach((p, i) => {
    const t0 = start + i * 12 * 60_000;
    const rid = `${id}@${i}`;
    runs.push({ id: rid, threadId: id, index: i + 1, prompt: p, status: "completed", startedAt: t0, endedAt: t0 + 6 * 60_000, usage: { inTokens: 40_000 + i * 3000, outTokens: 1800, costUsd: 0.12 + i * 0.01 } });
    items.push({ id: `${rid}.u`, threadId: id, runId: rid, kind: "user", status: "completed", text: p, startedAt: t0 });
    items.push({ id: `${rid}.c`, threadId: id, runId: rid, kind: "command", status: "completed", text: "", command: i % 2 ? "npm run type-check" : "rg -n 'useFilters' src/tracker", exitCode: 0, output: "src/tracker/Filters.vue:12", startedAt: t0 + 60_000, endedAt: t0 + 64_000 });
    if (i % 3 === 1) items.push({ id: `${rid}.f`, threadId: id, runId: rid, kind: "fileChange", status: "completed", text: "", files: [{ path: "src/tracker/Filters.vue", kind: "update", add: 12 + i, del: 4 }], startedAt: t0 + 2 * 60_000, endedAt: t0 + 2.1 * 60_000 });
    items.push({ id: `${rid}.a`, threadId: id, runId: rid, kind: "agent", status: "completed", text: i === asks.length - 1 ? "Filters now keep their state in the URL (`region`, `status`). The empty state waits for the first load. Tests and the type check pass." : `Done with step ${i + 1}.`, startedAt: t0 + 5.5 * 60_000 });
  });
  return { runs, items };
}
