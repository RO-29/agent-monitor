import { useState, type ReactNode } from "react";
import type { SessionState, Span } from "../../api/types";
import { errorMarks, ribbonGradient, copyText } from "../../pages/trace/lib";
import type { ApiState } from "../api/client";

export function AgentBadge({ tool, small, title }: { tool?: string; small?: boolean; title?: string }) {
  const t = (tool || "").toLowerCase();
  const cls = t === "claude" ? "cl" : t === "codex" ? "cx" : t === "subagent" || t === "sub" ? "sb" : t === "opencode" ? "oc" : t === "gemini" ? "gm" : t === "cursor" ? "cu" : "ot";
  const ch = t === "claude" ? "C" : t === "codex" ? "X" : t === "subagent" || t === "sub" ? "S" : (t[0] || "?").toUpperCase();
  return (
    <span className={`ag ${cls}${small ? " sm" : ""}`} title={title || tool}>
      {ch}
    </span>
  );
}

export interface StateInfo {
  dot: string;
  pill: string;
  label: string;
  rank: number;
}
export function stateInfo(st: SessionState | string, pending = 0): StateInfo {
  if (pending > 0) return { dot: "d-need", pill: "p-need", label: "Needs you", rank: 0 };
  switch (st) {
    case "awaiting-permission":
      return { dot: "d-need", pill: "p-need", label: "Needs you", rank: 0 };
    // The turn ended: ready for your next message, not waiting on you.
    case "awaiting-input":
      return { dot: "d-idle", pill: "p-mute", label: "ready", rank: 3 };
    case "running":
      return { dot: "d-run", pill: "p-run", label: "working", rank: 2 };
    case "idle":
      return { dot: "d-idle", pill: "p-mute", label: "ready", rank: 3 };
    case "completed":
      return { dot: "d-ok", pill: "p-ok", label: "ended", rank: 4 };
    default:
      return { dot: "d-mute", pill: "p-mute", label: String(st || "unknown"), rank: 5 };
  }
}

export function Loading({ rows = 4 }: { rows?: number }) {
  return (
    <div className="skel" aria-busy="true" aria-label="Loading">
      {Array.from({ length: rows }, (_, i) => (
        <i key={i} style={{ width: `${90 - ((i * 17) % 40)}%` }} />
      ))}
    </div>
  );
}
export function Empty({ title, children }: { title: string; children?: ReactNode }) {
  return (
    <div className="state">
      <b>{title}</b>
      {children && <div>{children}</div>}
    </div>
  );
}
export function ErrorState({ error, onRetry }: { error: string; onRetry?: () => void }) {
  return (
    <div className="state err" role="alert">
      <b>Request failed</b>
      <div className="mono xs">{error}</div>
      {onRetry && (
        <button className="btn sm" onClick={onRetry}>
          Retry
        </button>
      )}
    </div>
  );
}
export function Missing({ endpoint, what }: { endpoint: string; what?: string }) {
  return (
    <div className="state">
      <b>{what || "Not available on this daemon yet"}</b>
      <div>
        The endpoint <code>{endpoint}</code> answered 404. This view fills in when the daemon serves it.
      </div>
    </div>
  );
}

/** Renders loading, missing, error and empty states for one ApiState, else the children. */
export function Load<T>({
  st,
  endpoint,
  isEmpty,
  empty,
  rows,
  children,
}: {
  st: ApiState<T>;
  endpoint: string;
  isEmpty?: (d: T) => boolean;
  empty?: ReactNode;
  rows?: number;
  children: (d: T) => ReactNode;
}) {
  if (st.data) {
    if (isEmpty && isEmpty(st.data)) return <>{empty ?? <Empty title="Nothing here yet" />}</>;
    return <>{children(st.data)}</>;
  }
  if (st.missing) return <Missing endpoint={endpoint} />;
  if (st.error) return <ErrorState error={st.error} onRetry={st.reload} />;
  return <Loading rows={rows} />;
}

export function Spark({ points, w = 80, h = 20, color = "var(--accent)" }: { points: number[]; w?: number; h?: number; color?: string }) {
  if (!points || points.length < 2) return <span className="muted xs">·</span>;
  const max = Math.max(...points, 1);
  const pts = points.map((v, i) => `${((i / (points.length - 1)) * w).toFixed(1)},${(h - 2 - (v / max) * (h - 4)).toFixed(1)}`).join(" ");
  return (
    <svg width={w} height={h} viewBox={`0 0 ${w} ${h}`} style={{ display: "block" }} aria-hidden>
      <polyline points={pts} fill="none" stroke={color} strokeWidth="1.5" />
    </svg>
  );
}

export function Ribbon({ calls, style }: { calls: Span[]; style?: React.CSSProperties }) {
  const marks = errorMarks(calls);
  return (
    <div className="ribbon" style={style}>
      <div className="fill" style={{ background: ribbonGradient(calls) }} />
      {marks.map((m, i) => (
        <span key={i} className="em" style={{ left: `${m}%` }} />
      ))}
    </div>
  );
}

export function CopyBtn({ text, label = "Copy", className = "btn sm", title }: { text: string; label?: string; className?: string; title?: string }) {
  const [done, setDone] = useState(false);
  return (
    <button
      className={className}
      title={title}
      onClick={(e) => {
        e.stopPropagation();
        copyText(text);
        setDone(true);
        window.setTimeout(() => setDone(false), 1400);
      }}
    >
      {done ? "Copied" : label}
    </button>
  );
}

export function Kbd({ children }: { children: ReactNode }) {
  return <span className="kbd">{children}</span>;
}

/** Duration where zero is a real reading ("0m"), not a missing one. */
export const durOr0 = (ms: number | undefined, fmt: (n: number) => string) => (ms && ms > 0 ? fmt(ms) : "0m");

/** Pill class per span family. */
export const famPill: Record<string, string> = { bash: "p-ok", read: "p-acc", edit: "p-need", agent: "p-sub", mcp: "p-run", web: "p-mute", other: "p-mute" };
// Some transcripts carry spans without a start time; their dur is then epoch-sized garbage.
export const spanDur = (s: { ts: number; dur: number }) => (s.ts > 0 && s.dur > 0 && s.dur < 7 * 864e5 ? s.dur : 0);

/** 12-bucket activity bars; buckets with errors draw red, empty ones faint. */
export function SparkBars({ calls, errs, h = 20 }: { calls: number[]; errs?: number[]; h?: number }) {
  const max = Math.max(...calls, 1);
  return (
    <span className="sbars" aria-label={`${calls.reduce((a, b) => a + b, 0)} tool calls in the last hour`} role="img">
      {calls.map((c, i) => (
        <i key={i} className={errs?.[i] ? "e" : c ? "" : "z"} style={{ height: c ? Math.max(3, Math.round((c / max) * h)) : 2 }} />
      ))}
    </span>
  );
}

/** One cell per member (f = findings, e = failed, x = still open), proportional past 24 members. */
export function ResultBar({ f = 0, e = 0, x = 0, ok = 0 }: { f?: number; e?: number; x?: number; ok?: number }) {
  const total = f + e + x + ok;
  if (!total) return null;
  const parts: [string, number][] = [["f", f], ["e", e], ["", ok], ["x", x]];
  const label = `${f} with findings, ${ok} clean${e ? `, ${e} failed` : ""}${x ? `, ${x} open` : ""}`;
  if (total > 24)
    return (
      <span className="rbar prop" role="img" aria-label={label}>
        {parts.filter(([, n]) => n > 0).map(([c, n]) => (
          <i key={c || "ok"} className={c} style={{ flex: n }} />
        ))}
      </span>
    );
  return (
    <span className="rbar" role="img" aria-label={label}>
      {parts.flatMap(([c, n]) => Array.from({ length: n }, (_, i) => <i key={`${c}${i}`} className={c} />))}
    </span>
  );
}

/** Marks a row that agent-monitor drives (orchestrator thread), as opposed to a watched terminal session. */
export function DrivenTag() {
  return (
    <span className="drv" title="Driven from agent-monitor. Opens in the Workspace.">
      driven
    </span>
  );
}
