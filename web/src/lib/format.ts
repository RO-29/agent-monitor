import type { Session, SessionState, SpanFamily, Tool } from "../api/types";

export const TOOLS: Tool[] = ["claude", "codex", "opencode", "cursor", "cursor-agent"];

export const toolName = (t: Tool | string): string =>
  ({ claude: "Claude Code", codex: "Codex", opencode: "opencode", cursor: "Cursor IDE", "cursor-agent": "cursor-agent" } as Record<string, string>)[t] || t;

export const toolColorVar = (t: Tool | string) => `var(--tool-${t})`;

export const famColorVar = (f: SpanFamily | string) => `var(--fam-${f})`;
export const famLabel = (f: SpanFamily | string): string =>
  ({ bash: "Bash", read: "Read/Grep", edit: "Edit/Write", agent: "Agent", mcp: "MCP", web: "Web", other: "Other", model: "model", user: "you" } as Record<string, string>)[f] || f;

export const modelShort = (m?: string) => (m || "").replace(/^claude-/, "").replace(/-20\d{6}.*$/, "").replace("[1m]", "").trim();

export const agentTag = (s: { sessionId?: string; id: string }) =>
  (s.sessionId || s.id).replace(/[^a-z0-9]/gi, "").slice(0, 4).toLowerCase();

export const shortCwd = (p?: string) => (p || "").replace(/^\/Users\/[^/]+/, "~");
export const projectName = (p?: string) => {
  const s = (p || "").replace(/\/+$/, "");
  const last = s.split("/").pop() || "";
  return last === "" || /^\/Users\/[^/]+$/.test(s) || s === "~" ? "home" : last;
};

const NOISE_RE = /^(<|caveat:|the messages below were generated|do not respond|system-reminder|# agents?\.md|# claude\.md|instructions for |you are (an?|the) |\[image|\{|\/<slash-command>)/i;
export function firstRealLine(text?: string): string {
  if (!text) return "";
  for (let ln of text.split("\n")) {
    ln = ln.replace(/^[▎▏│┃|]+\s*/, "").trim();
    if (!ln || NOISE_RE.test(ln)) continue;
    ln = ln.replace(/^#+\s*/, "").replace(/^[-*>]\s*/, "").replace(/^["'“”]+|["'“”]+$/g, "");
    if (ln.length < 2) continue;
    return ln.length > 100 ? ln.slice(0, 100) + "…" : ln;
  }
  return "";
}
export const titleFor = (s: Session) => s.title || firstRealLine(s.firstMessage) || projectName(s.cwd);

export const isLive = (st: SessionState) => st === "running" || st === "idle" || st === "awaiting-input" || st === "awaiting-permission";
export const isAttention = (st: SessionState) => st === "awaiting-permission";
export const stateRank = (st: SessionState) =>
  ({ "awaiting-permission": 0, "awaiting-input": 1, running: 2, idle: 3, completed: 4, abandoned: 5 } as Record<string, number>)[st] ?? 9;
export const stateLabel = (st: SessionState) => st.replace("awaiting-", "");
export const stateColorVar = (st: SessionState) =>
  ({ running: "var(--green)", idle: "var(--muted)", "awaiting-input": "var(--magenta)", "awaiting-permission": "var(--red)", completed: "var(--accent)", abandoned: "var(--muted-2)" } as Record<string, string>)[st] || "var(--muted)";

export function fmtAgo(ts?: number, suffix = " ago"): string {
  if (!ts) return "—";
  const s = Math.max(0, Math.floor((Date.now() - ts) / 1000));
  if (s < 60) return `${s}s${suffix}`;
  if (s < 3600) return `${Math.floor(s / 60)}m${suffix}`;
  if (s < 86400) return `${Math.floor(s / 3600)}h${suffix}`;
  return `${Math.floor(s / 86400)}d${suffix}`;
}
export const fmtTime = (ts: number) => (ts ? new Date(ts).toTimeString().slice(0, 8) : "");
export const fmtClock = (ts: number) => (ts ? new Date(ts).toTimeString().slice(0, 5) : "");
export function fmtDate(ts: number): string {
  if (!ts) return "";
  const d = new Date(ts);
  return d.toLocaleDateString(undefined, { month: "short", day: "2-digit" }) + " · " + fmtClock(ts);
}
export function fmtDuration(ms: number): string {
  if (!ms || ms < 1000) return ms > 0 ? `${ms}ms` : "—";
  const s = Math.floor(ms / 1000);
  if (s < 60) return `${s}s`;
  const m = Math.floor(s / 60);
  if (m < 60) return `${m}m ${String(s % 60).padStart(2, "0")}s`;
  const h = Math.floor(m / 60);
  if (h < 24) return `${h}h ${String(m % 60).padStart(2, "0")}m`;
  return `${Math.floor(h / 24)}d ${h % 24}h`;
}
export function fmtDur(ms: number): string {
  // compact: 812ms · 9s · 1m 24s · 2h 20m
  if (ms < 1000) return `${Math.round(ms)}ms`;
  const s = Math.round(ms / 1000);
  if (s < 60) return `${s}s`;
  const m = Math.floor(s / 60);
  if (m < 60) return `${m}m ${String(s % 60).padStart(2, "0")}s`;
  const h = Math.floor(m / 60);
  return `${h}h ${String(m % 60).padStart(2, "0")}m`;
}
export function fmtTok(n?: number): string {
  if (!n) return "0";
  if (n < 1000) return n.toLocaleString();
  if (n < 1e6) return (n / 1000).toFixed(n < 10000 ? 1 : 0) + "k";
  if (n < 1e9) return (n / 1e6).toFixed(1) + "M";
  return (n / 1e9).toFixed(2) + "B";
}
export function fmtBytes(n?: number): string {
  if (n == null) return "";
  if (n < 1000) return n + "b";
  if (n < 1e6) return (n / 1000).toFixed(n < 10000 ? 1 : 0) + "k";
  return (n / 1e6).toFixed(1) + "M";
}
export const fmtUsd = (n?: number, est = false) => (n == null ? "—" : (est ? "≈" : "") + "$" + (n >= 100 ? n.toFixed(0) : n >= 10 ? n.toFixed(1) : n.toFixed(2)));
export const pct = (a: number, b: number) => (b > 0 ? Math.min(100, (a / b) * 100) : 0);
