// Dev-only notifications for ?mock=1: a history list and one live event a few seconds after load.
import type { ONotification } from "./notify";

export function installNotifyMock(add: (n: ONotification, live: boolean) => void, ready: (st: "ok") => void) {
  const now = Date.now();
  const m = (min: number) => now - min * 60_000;
  const seed: ONotification[] = [
    { id: "n1", kind: "needs-you", title: "Codex wants to run a command", body: "Per-device tokens for Tailscale access: go test ./auth/... -run TestDeviceToken", threadId: "tok", at: m(1) },
    { id: "n2", kind: "needs-you", title: "Claude asks a question", body: "Split the residency router config: which region file keeps the shared defaults?", threadId: "cq", at: m(3) },
    { id: "n3", kind: "spend", title: "Thread spend limit reached", body: "Rename the stats endpoint spent $5.02 of its $5.00 limit. New turns wait for you.", threadId: "rdy", at: m(18) },
    { id: "n4", kind: "finished", title: "4 subagents finished", body: "Split the device token work: 2 done, 1 needs you, 1 running.", threadId: "lead", at: m(9), count: 4 },
    { id: "n5", kind: "error", title: "Upgrade vite to 6 stopped", body: "codex app-server exited with code 1", threadId: "bad", at: m(55), read: true },
    { id: "n6", kind: "spend", title: "80% of today's limit", body: "$20.10 of $25.00 spent today.", url: "/settings/limits", at: m(70), read: true },
    { id: "n7", kind: "finished", title: "Disk and RAM check finished", body: "Disk 61% used, RAM pressure normal. No action needed.", threadId: "dsk", at: m(290), read: true },
    { id: "n8", kind: "finished", title: "Audit stale LaunchAgents finished", body: "Found 2 agents that point at missing files.", threadId: "old", at: m(1900), read: true },
  ];
  for (const n of seed) add(n, false);
  ready("ok");
  // ?toast=1 shows one live notification as a toast, for layout checks.
  if (new URLSearchParams(window.location.search).get("toast") !== "1") return;
  window.setTimeout(() => add({ id: `n-live-${now}`, kind: "finished", title: "Comment mentions for Slack groups finished", body: "6 of 15 suites pass. The rest wait on the Slack fixture.", threadId: "cmt", at: Date.now() }, true), 2500);
}
