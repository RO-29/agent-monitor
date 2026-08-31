import { useState } from "react";
import type { Learning, Output, Segment } from "../../api/types";
import { fmtAgo, fmtClock, fmtTok } from "../../lib/format";
import { Icon } from "../../lib/icons";
import { BOUNDARY, chapterCounts, outputHref } from "./lib";

interface CardProps {
  segment: Segment;
  onEnrich: () => void;
  enriching: boolean;
  enrichError: string | null;
  compact?: boolean; // single-column layout for the side panel
  loadMs?: number;
}

export function SourceBadge({ l }: { l: Learning }) {
  return (
    <>
      <span className={`tr-src ${l.source}`}>{l.source}</span>
      {l.heuristic && <span className="tr-src heur">heuristic</span>}
    </>
  );
}

export function OutputChips({ outputs, max = 8 }: { outputs: Output[]; max?: number }) {
  if (!outputs.length) return <span className="muted" style={{ fontSize: 11.5 }}>none</span>;
  const icon = (k: string) => (k === "pr" ? "pr" : k === "artifact" ? "frame" : k === "commit" ? "branch" : "doc");
  const color = (k: string) => (k === "pr" ? "var(--green)" : k === "artifact" ? "var(--accent)" : "var(--text-2)");
  // PRs and artifacts first; commits are the noisiest and get folded into "+N"
  const sorted = [...outputs].sort((a, b) => rank(a.kind) - rank(b.kind));
  const shown = sorted.slice(0, max);
  return (
    <div className="tr-outs">
      {sorted.length > max && <span className="tr-chip sm" style={{ cursor: "default" }}>+ {sorted.length - max} more</span>}
      {shown.map((o, i) => {
        const href = outputHref(o);
        const inner = (
          <>
            <Icon name={icon(o.kind)} size={12} color={color(o.kind)} /> {o.label}
          </>
        );
        return href ? (
          <a key={i} className="tr-chip sm" href={href} target="_blank" rel="noreferrer" title={o.ref}>
            {inner}
          </a>
        ) : (
          <span key={i} className="tr-chip sm" title={o.ref} style={{ cursor: "default" }}>
            {inner}
          </span>
        );
      })}
    </div>
  );
}

const rank = (k: string) => (k === "pr" ? 0 : k === "artifact" ? 1 : k === "doc" ? 2 : 3);

/** Full chapter card (the point, intent changes, outcome, learnings, open, outputs, footer). */
export default function ChapterCard({ segment, onEnrich, enriching, enrichError, compact, loadMs }: CardProps) {
  const ch = segment.chapter;
  if (!ch) return <div className="muted" style={{ padding: "8px 0", fontSize: 12 }}>No chapter for this segment yet.</div>;
  const cols = compact ? "minmax(0,1fr)" : "minmax(0,1.3fr) minmax(0,1fr) minmax(0,1fr)";
  return (
    <div className="tr-card" style={{ gridTemplateColumns: cols, padding: compact ? "8px 0 4px" : undefined, borderTop: compact ? 0 : undefined }}>
      <div>
        <div className="k">The point</div>
        <p>{ch.point || "—"}</p>
        {ch.outcome && (
          <>
            <div className="k" style={{ marginTop: 10 }}>Outcome so far</div>
            <p style={{ color: "var(--text-2)", fontSize: 12 }}>{ch.outcome}</p>
          </>
        )}
      </div>
      <div>
        <div className="k">Intent changed · {ch.intentChanges.length}</div>
        <div style={{ display: "flex", flexDirection: "column", gap: 4 }}>
          {ch.intentChanges.slice(0, 8).map((c, i) => (
            <div key={i} className="tr-ic">
              <span className="num">{fmtClock(c.ts)}</span>
              <span style={{ overflow: "hidden", textOverflow: "ellipsis", whiteSpace: "nowrap" }}>{c.text}</span>
            </div>
          ))}
          {ch.intentChanges.length > 8 && <span className="muted" style={{ fontSize: 11 }}>+ {ch.intentChanges.length - 8} more</span>}
          {ch.intentChanges.length === 0 && <span className="muted" style={{ fontSize: 11.5 }}>none</span>}
        </div>
      </div>
      <div style={{ display: "flex", flexDirection: "column", gap: 10 }}>
        <div>
          <div className="k">Learnings · {ch.learnings.length}</div>
          <div style={{ display: "flex", flexDirection: "column", gap: 6 }}>
            {ch.learnings.slice(0, 8).map((l) => (
              <div key={l.id} className="tr-lrn">
                <SourceBadge l={l} />
                <span style={{ flex: 1 }}>
                  {l.text} <span className="num muted" style={{ fontSize: 10.5 }}>· {fmtClock(l.ts)}</span>
                </span>
              </div>
            ))}
            {ch.learnings.length === 0 && <span className="muted" style={{ fontSize: 11.5 }}>none</span>}
          </div>
        </div>
        {ch.open.length > 0 && (
          <div>
            <div className="k">Open · {ch.open.length}</div>
            {ch.open.map((o, i) => (
              <div key={i} className="tr-lrn">
                <span style={{ width: 6, height: 6, borderRadius: "50%", background: "var(--yellow)", marginTop: 6, flex: "none" }} />
                <span>{o}</span>
              </div>
            ))}
          </div>
        )}
        <div>
          <div className="k">Outputs · {ch.outputs.length}</div>
          <OutputChips outputs={ch.outputs} />
        </div>
      </div>
      <div className="tr-card-foot">
        {ch.source === "enriched" ? (
          <>
            <Icon name="spark" size={11} color="var(--magenta)" /> enrichment · {ch.model} · {ch.enrichedAt ? fmtAgo(ch.enrichedAt) : ""}
          </>
        ) : (
          <>
            <span className="dot" /> deterministic{loadMs ? ` · ${loadMs} ms` : ""}
          </>
        )}
        <div style={{ flex: 1 }} />
        {enrichError && <span style={{ color: "var(--red)" }}>{enrichError}</span>}
        <button className="tr-chip sm" onClick={onEnrich} disabled={enriching}>
          {enriching ? <span className="tr-spin" /> : <Icon name="spark" size={11} />} {enriching ? "running…" : ch.source === "enriched" ? "re-run enrichment" : "run enrichment"}
        </button>
      </div>
    </div>
  );
}

interface BandProps extends CardProps {
  loadMs: number;
}

/** One-line band under the header; expands into the full card. */
export function ChapterBand(props: BandProps) {
  const { segment } = props;
  const [open, setOpen] = useState(false);
  const b = segment.boundary;
  const meta = BOUNDARY[b.kind] || BOUNDARY.start;
  const c = chapterCounts(segment.chapter);
  return (
    <div className={`tr-chapter ${b.kind}`}>
      <div className="tr-band">
        <span className={`tr-bd ${meta.cls}`}>{meta.glyph}</span>
        <span style={{ fontWeight: 600, fontSize: 12.5, whiteSpace: "nowrap" }}>
          {b.kind === "compact" ? `compact · ${b.trigger || "auto"}` : meta.label}
        </span>
        {b.kind === "compact" && b.preTokens ? (
          <span className="num muted" style={{ fontSize: 11, whiteSpace: "nowrap" }}>
            {fmtTok(b.preTokens)} → {fmtTok(b.postTokens)} tok
          </span>
        ) : null}
        <span className="tr-sep" />
        <span className="k">the point</span>
        <span className="point">{segment.chapter?.point || "—"}</span>
        <span className="tr-chip sm" style={{ cursor: "default" }}>
          intent changed <span className="num">{c.intent}</span>
        </span>
        <span className="tr-chip sm" style={{ cursor: "default" }}>
          <span className="tr-src correction">correction</span> {c.corrections}
        </span>
        <span className="tr-chip sm" style={{ cursor: "default" }}>
          learnings <span className="num">{c.learnings}</span>
        </span>
        <span className="tr-chip sm warn" style={{ cursor: "default" }}>
          open <span className="num">{c.open}</span>
        </span>
        <span className={`tr-chip sm ${open ? "on" : ""}`} onClick={() => setOpen(!open)}>
          chapter <Icon name={open ? "chevd" : "chev"} size={10} />
        </span>
      </div>
      {open && <ChapterCard {...props} />}
    </div>
  );
}
