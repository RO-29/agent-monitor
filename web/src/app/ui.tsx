import type { CSSProperties, ReactNode } from "react";
import type { BoundaryKind, SessionState } from "../api/types";
import { stateColorVar, stateLabel } from "../lib/format";

export function StatePill({ state, style }: { state: SessionState; style?: CSSProperties }) {
  const c = stateColorVar(state);
  const bg = c.replace("var(--", "").replace(")", "");
  const bgMap: Record<string, string> = {
    green: "rgba(74,222,128,.14)",
    red: "rgba(248,113,113,.14)",
    magenta: "rgba(192,132,252,.14)",
    accent: "rgba(125,211,252,.12)",
    muted: "rgba(107,117,133,.14)",
    "muted-2": "rgba(107,117,133,.10)",
  };
  return (
    <span className={`pill s-${state}`} style={{ color: c, background: bgMap[bg] || "rgba(107,117,133,.14)", textDecoration: state === "abandoned" ? "line-through" : undefined, ...style }}>
      <span className="dot" />
      {stateLabel(state)}
    </span>
  );
}

const GLYPH: Record<BoundaryKind, string> = { start: "▶", compact: "⟲", clear: "⌫", resume: "↻" };
export function BoundaryBadge({ kind, size = 16 }: { kind: BoundaryKind | string; size?: number }) {
  return (
    <span className={`bd ${kind}`} style={{ width: size, height: size, fontSize: size * 0.6 }} title={kind}>
      {GLYPH[kind as BoundaryKind] || "•"}
    </span>
  );
}

/** Segment strip: one block per segment, width ∝ duration, last = current. */
export function SegStrip({ weights, kinds, height = 10, cur, labels }: { weights: number[]; kinds: string[]; height?: number; cur?: number; labels?: string[] }) {
  const total = weights.reduce((a, b) => a + b, 0) || 1;
  const current = cur ?? weights.length - 1;
  if (!weights.length) return <div className="seg" style={{ height, opacity: 0.4 }} />;
  return (
    <div style={{ display: "flex", gap: 3, alignItems: "center", width: "100%" }}>
      {weights.map((w, i) => (
        <div
          key={i}
          className={`seg ${i === current ? "cur" : i < current ? "prev" : ""}`}
          style={{ flex: `0 0 calc(${Math.max((w / total) * 100, 6).toFixed(1)}% - 3px)`, height }}
          title={`${kinds[i]} · segment ${i + 1}`}
        >
          <BoundaryBadge kind={kinds[i]} size={Math.min(12, height + 2)} />
          {labels?.[i] ? <span style={{ marginLeft: 4 }}>{labels[i]}</span> : null}
        </div>
      ))}
    </div>
  );
}

export function Gauge({ value, max, color, height = 6 }: { value: number; max: number; color?: string; height?: number }) {
  const p = max > 0 ? Math.min(100, (value / max) * 100) : 0;
  const c = color || (p >= 80 ? "var(--orange)" : p >= 50 ? "var(--accent)" : "var(--green)");
  return (
    <div className="gauge" style={{ height }}>
      <div style={{ width: `${p.toFixed(1)}%`, background: c }} />
    </div>
  );
}

export function Kv({ k, v, mono = true, color }: { k: string; v: ReactNode; mono?: boolean; color?: string }) {
  return (
    <div style={{ display: "flex", justifyContent: "space-between", gap: 10, padding: "5px 0", borderBottom: "1px solid var(--border)" }}>
      <span className="muted" style={{ fontSize: 11.5, flex: "none" }}>
        {k}
      </span>
      <span className={mono ? "mono ell" : "ell"} style={{ fontSize: 11.5, color: color || "var(--text)", textAlign: "right" }}>
        {v}
      </span>
    </div>
  );
}
