import { useContext, useState, type ReactNode } from "react";
import { fmtDuration } from "../../../lib/format";
import { toMs, type OItem } from "../../api/orch";
import { HitCtx } from "./hit";

// Tool calls that fold into a trail; what the agent says stays outside it.
const TRAIL_KINDS = new Set(["command", "fileChange", "mcp", "web", "other", "review"]);
export const TRAIL_MIN = 4;

// A question to the user is conversation, so it stays out of the folded steps.
export const isAsk = (it: OItem) => it.kind === "other" && it.command === "AskUserQuestion";
export const inTrail = (it: OItem) => TRAIL_KINDS.has(it.kind) && it.by !== "user" && !isAsk(it);

const failedOf = (it: OItem) => it.status === "failed" || (it.exitCode !== undefined && it.exitCode !== 0);

function kindOf(it: OItem, kids?: OItem[]): { cls: string; label: string } {
  const name = it.command || "";
  if (kids?.length) return { cls: "scr", label: "Script" };
  if (it.kind === "command") return { cls: "sh", label: "Bash" };
  if (it.kind === "fileChange") return { cls: "ed", label: "Edit" };
  if (it.kind === "mcp") return { cls: "mcp", label: name.match(/^mcp__(.+?)__/)?.[1] || "MCP" };
  if (it.kind === "web") return { cls: "web", label: "Web" };
  if (name === "Task" || name === "Agent") return { cls: "sub", label: "Subagent" };
  if (/^(Read|Grep|Glob|LS)$/.test(name)) return { cls: "rd", label: name === "Read" ? "Read" : "Search" };
  return { cls: "ot", label: name || "Step" };
}

const durOf = (it: OItem, now: number) => {
  const s = toMs(it.startedAt);
  const e = it.endedAt ? toMs(it.endedAt) : it.status === "started" ? now : s;
  return s && e > s ? e - s : 0;
};

/** A run of tool calls as one strip: one segment per call, width by time, color by tool. */
export function StepTrail({ items, kids, now, live, row }: { items: OItem[]; kids: Map<string, OItem[]>; now: number; live: boolean; row: (it: OItem) => ReactNode }) {
  const hl = useContext(HitCtx);
  const [all, setAll] = useState<boolean | null>(null);
  const [pick, setPick] = useState<string | null>(null);
  const counts = new Map<string, number>();
  let failed = 0;
  let ms = 0;
  for (const it of items) {
    const k = kindOf(it, kids.get(it.id)).label;
    counts.set(k, (counts.get(k) || 0) + 1);
    if (failedOf(it) || kids.get(it.id)?.some(failedOf)) failed++;
    ms += durOf(it, now);
  }
  const top = [...counts.entries()].sort((a, b) => b[1] - a[1]);
  const calls = items.length + items.reduce((n, it) => n + (kids.get(it.id)?.length || 0), 0);
  const last = items[items.length - 1];
  const hit = hl ? items.find((i) => i.id === hl || kids.get(i.id)?.some((k) => k.id === hl)) : undefined;
  const shownAll = all ?? !!hit;
  const picked = !shownAll ? items.find((i) => i.id === pick) || (live && last.status === "started" ? last : undefined) : undefined;
  return (
    <div className="wtrail">
      <div className="wtrail-h">
        <div className="wtrail-bar" role="list" aria-label={`${calls} tool calls`}>
          {items.map((it) => {
            const k = kindOf(it, kids.get(it.id));
            const d = durOf(it, now);
            const bad = failedOf(it) || kids.get(it.id)?.some(failedOf);
            const first = (it.text || "").trim().split("\n")[0].slice(0, 140);
            return (
              <button
                key={it.id}
                role="listitem"
                className={`wtseg t-${k.cls} ${bad ? "bad" : ""} ${it.status === "started" ? "wpulse" : ""} ${pick === it.id ? "on" : ""}`}
                style={{ flexGrow: 1 + Math.log2(1 + d / 1000) }}
                title={`${k.label}${d ? ` · ${fmtDuration(d)}` : ""}${bad ? " · failed" : ""}\n${first}`}
                aria-label={`${k.label}${bad ? ", failed" : ""}: ${first}`}
                onClick={() => (setAll(false), setPick(pick === it.id ? null : it.id))}
              />
            );
          })}
        </div>
        <button className="wtrail-sum" onClick={() => (setPick(null), setAll(!shownAll))} aria-expanded={shownAll}>
          <span className="trunc">
            {calls} calls
            {top.slice(0, 3).map(([l, n]) => ` · ${l} ${n}`).join("")}
            {ms > 0 ? ` · ${fmtDuration(ms)}` : ""}
          </span>
          {failed > 0 && <span className="wtrail-bad"> · {failed} failed</span>}
          <span className="wchev">{shownAll ? "▾" : "▸"}</span>
        </button>
      </div>
      {picked && <div className="wtrail-one">{row(picked)}</div>}
      {shownAll && <div className="wcmdg-b">{items.map((it) => <div key={it.id}>{row(it)}</div>)}</div>}
    </div>
  );
}

/** Splits steps into trails (TRAIL_MIN or more tool calls in a row) and the items between them. */
export function trailSegments(items: OItem[]): (OItem | OItem[])[] {
  const out: (OItem | OItem[])[] = [];
  let run: OItem[] = [];
  const flush = () => {
    if (run.length >= TRAIL_MIN) out.push(run);
    else out.push(...run);
    run = [];
  };
  for (const it of items) {
    if (inTrail(it)) run.push(it);
    else {
      flush();
      out.push(it);
    }
  }
  flush();
  return out;
}

/** The trail of a folded turn as a small bar in its summary row; the row opens the steps. */
export function TrailBar({ items, now }: { items: OItem[]; now: number }) {
  const calls = items.filter(inTrail);
  if (calls.length < 2) return null;
  return (
    <span className="wtrail-mini" aria-hidden>
      {calls.slice(-60).map((it) => (
        <span key={it.id} className={`wtseg t-${kindOf(it).cls} ${failedOf(it) ? "bad" : ""}`} style={{ flexGrow: 1 + Math.log2(1 + durOf(it, now) / 1000) }} />
      ))}
    </span>
  );
}
