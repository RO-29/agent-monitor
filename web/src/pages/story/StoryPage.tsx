import { useEffect, useMemo, useState } from "react";
import { Link, useParams } from "react-router-dom";
import { api } from "../../api/client";
import type { Segment, ThreadStoryResponse } from "../../api/types";
import { fmtDate, fmtDur, fmtTok, fmtUsd, shortCwd, stateColorVar, stateLabel, titleFor } from "../../lib/format";
import { Icon, ToolLogo } from "../../lib/icons";
import { OutputChips, SourceBadge } from "../trace/ChapterBand";
import { BOUNDARY } from "../trace/lib";
import "../trace/trace.css";
import "./story.css";

interface Row {
  sessionId: string;
  sessionTitle: string;
  seg: Segment;
  lastTs: number;
  n: number; // running index across the thread
}

const SHOW = 8;

export default function StoryPage() {
  const { id = "" } = useParams();
  const threadId = decodeURIComponent(id);
  const [data, setData] = useState<ThreadStoryResponse | null>(null);
  const [err, setErr] = useState<string | null>(null);
  const [all, setAll] = useState(false);

  useEffect(() => {
    setData(null);
    api.threadStory(threadId).then(setData).catch((e: Error) => setErr(e.message));
  }, [threadId]);

  const rows = useMemo<Row[]>(() => {
    if (!data) return [];
    const out: Row[] = [];
    let n = 0;
    for (const m of data.members) {
      for (const seg of m.segments) {
        out.push({ sessionId: m.session.id, sessionTitle: titleFor(m.session), seg, lastTs: m.trace?.lastTs || m.session.lastActivityAt, n: ++n });
      }
    }
    return out;
  }, [data]);

  if (err) return <div className="st-root"><div className="tr-error">{err}</div></div>;
  if (!data) return <div className="st-root"><div className="tr-loading">loading story…</div></div>;

  const t = data.thread;
  const segs = rows.map((r) => r.seg);
  const dropped = segs.reduce((a, s) => a + (s.boundary.droppedTokens || 0), 0);
  const kept = segs.reduce((a, s) => a + (s.boundary.postTokens || 0), 0);
  const cost = data.members.reduce((a, m) => a + (m.trace?.costUsd || 0), 0);
  const costEst = data.members.some((m) => m.trace?.costEstimated);
  const learnings = segs.reduce((a, s) => a + (s.chapter?.learnings.length || 0), 0);
  const outputs = segs.reduce((a, s) => a + (s.chapter?.outputs.length || 0), 0);
  const counts = { start: 0, clear: 0, compact: 0, resume: 0 } as Record<string, number>;
  segs.forEach((s) => (counts[s.boundary.kind] = (counts[s.boundary.kind] || 0) + 1));
  const visible = all ? rows : rows.slice(0, SHOW);
  const newest = data.members[data.members.length - 1]?.session;

  return (
    <div className="st-root">
      <div className="st-head">
        <div className="st-title">
          <ToolLogo tool={t.tools[0] || "claude"} size={16} />
          <h1>{t.title}</h1>
          <span className="mono muted" style={{ fontSize: 12 }}>{shortCwd(t.cwd)}</span>
          <span className="tr-pill" style={{ color: stateColorVar(t.state), background: "var(--bg-3)" }}>{stateLabel(t.state)}</span>
          <div style={{ flex: 1 }} />
          {newest && (
            <Link className="tr-btn" to={`/session/${encodeURIComponent(newest.id)}`}>
              <Icon name="pulse" size={13} /> trace
            </Link>
          )}
          <Link className="tr-btn" to={`/thread/${encodeURIComponent(threadId)}/learnings`}>
            <Icon name="book" size={13} /> learnings · {learnings}
          </Link>
        </div>
        <div className="st-kpis">
          <Kpi k="sessions" v={String(data.members.length)} s={t.ref ? `linked by ${t.ref}` : t.edges.map((e) => e.kind).join(" · ") || "single session"} />
          <Kpi k="segments" v={String(segs.length)} s={`${counts.clear} clear · ${counts.compact} compacts`} />
          <Kpi k="dropped" v={fmtTok(dropped)} s={`tokens across ${counts.compact} compactions`} c="var(--orange)" />
          <Kpi k="kept" v={fmtTok(kept)} s="summary tokens carried" c="var(--accent)" />
          <Kpi k="cost" v={fmtUsd(cost, costEst)} s={`${t.model ? t.model.replace(/^claude-/, "") : ""} · ${fmtDur(t.lastAt - t.startedAt)}`} />
          <Kpi k="learnings" v={String(learnings)} s={sourceBreakdown(segs)} />
          <Kpi k="outputs" v={String(outputs)} s={outputBreakdown(segs)} c="var(--green)" />
        </div>
        <div style={{ display: "flex", alignItems: "center", gap: 12 }}>
          <span className="k">timeline</span>
          <div className="tr-segstrip">
            {rows.map((r, i) => {
              const meta = BOUNDARY[r.seg.boundary.kind];
              const w = Math.max((r.seg.toTs || r.lastTs) - r.seg.fromTs, 60_000);
              return (
                <a key={r.seg.id} href={`#seg-${r.n}`} className={`tr-seg ${i === rows.length - 1 ? "cur" : ""}`} style={{ flex: `${w} 1 0` }} title={`${meta.label} · ${fmtDate(r.seg.fromTs)}`}>
                  <span className={`tr-bd sm ${meta.cls}`}>{meta.glyph}</span>
                  <span style={{ overflow: "hidden", textOverflow: "ellipsis" }}>{r.seg.boundary.kind === "compact" ? `compact` : r.seg.boundary.kind === "start" ? "start" : r.seg.boundary.kind === "clear" ? "/clear" : "resume"}</span>
                </a>
              );
            })}
          </div>
        </div>
      </div>
      <div className="st-body">
        {visible.map((r, i) => {
          const b = r.seg.boundary;
          const meta = BOUNDARY[b.kind];
          const ch = r.seg.chapter;
          const cur = i === rows.length - 1;
          const dur = (r.seg.toTs || r.lastTs) - r.seg.fromTs;
          return (
            <div key={r.seg.id} className="st-chapter" id={`seg-${r.n}`}>
              <div className="st-gutter">
                <div className="row">
                  <span className="muted" style={{ fontSize: 11.5 }}>{b.kind === "compact" ? `compact · ${b.trigger || "auto"}` : meta.label}</span>
                  <span className={`tr-bd ${meta.cls}`}>{meta.glyph}</span>
                </div>
                <div className="num muted" style={{ fontSize: 11 }}>
                  {fmtDate(r.seg.fromTs)} · {fmtDur(dur)}
                </div>
                {b.preTokens ? (
                  <div className="row" style={{ gap: 6 }}>
                    <span className="num" style={{ fontSize: 11.5 }}>{b.preTokens.toLocaleString()}</span>
                    <span className="muted">→</span>
                    <span className="num" style={{ fontSize: 11.5, color: "var(--accent)" }}>{(b.postTokens || 0).toLocaleString()}</span>
                    <span className="muted" style={{ fontSize: 11 }}>kept</span>
                  </div>
                ) : null}
                {b.droppedTokens ? <div className="num" style={{ fontSize: 11, color: "var(--orange)" }}>−{b.droppedTokens.toLocaleString()} dropped</div> : null}
              </div>
              <div className="st-rail">
                <div className="line" style={{ bottom: i === visible.length - 1 && all ? 20 : -20 }} />
                <div className={`dot ${cur ? "cur" : ""}`} />
              </div>
              <div className={`st-card ${cur ? "cur" : ""}`}>
                <div className="top">
                  <span className="num muted" style={{ fontSize: 11 }}>segment {r.n}</span>
                  <span className="t">{b.kind === "clear" || b.kind === "start" ? r.sessionTitle : ch?.point?.slice(0, 90) || r.sessionTitle}</span>
                  <div style={{ flex: 1 }} />
                  <Link className="tr-chip sm" to={`/session/${encodeURIComponent(r.sessionId)}?seg=${r.seg.index}`} style={{ color: cur ? "var(--accent)" : undefined }}>
                    open trace <Icon name="arrow" size={10} />
                  </Link>
                </div>
                <div className="st-cols">
                  <div>
                    <div className="k">The point</div>
                    <p>{ch?.point || "—"}</p>
                    {ch?.outcome && (
                      <>
                        <div className="k" style={{ marginTop: 10 }}>Outcome</div>
                        <p style={{ color: "var(--text-2)", fontSize: 12 }}>{ch.outcome}</p>
                      </>
                    )}
                  </div>
                  <div>
                    <div className="k">Intent changed · {ch?.intentChanges.length || 0}</div>
                    <div style={{ display: "flex", flexDirection: "column", gap: 3 }}>
                      {(ch?.intentChanges || []).slice(0, 6).map((c, k) => (
                        <div key={k} className="st-ic">
                          <span className="num">{new Date(c.ts).toTimeString().slice(0, 5)}</span>
                          <span style={{ overflow: "hidden", textOverflow: "ellipsis", whiteSpace: "nowrap" }}>{c.text}</span>
                        </div>
                      ))}
                      {(ch?.intentChanges.length || 0) > 6 && <span className="muted" style={{ fontSize: 11 }}>+ {ch!.intentChanges.length - 6} more</span>}
                    </div>
                  </div>
                  <div style={{ display: "flex", flexDirection: "column", gap: 10 }}>
                    <div>
                      <div className="k">Learnings · {ch?.learnings.length || 0}</div>
                      <div style={{ display: "flex", flexDirection: "column", gap: 5 }}>
                        {(ch?.learnings || []).slice(0, 5).map((l) => (
                          <div key={l.id} className="tr-lrn">
                            <SourceBadge l={l} />
                            <span>{l.text}</span>
                          </div>
                        ))}
                      </div>
                    </div>
                    {ch && ch.open.length > 0 && (
                      <div>
                        <div className="k">Open</div>
                        {ch.open.slice(0, 4).map((o, k) => (
                          <div key={k} className="tr-lrn">
                            <span style={{ width: 6, height: 6, borderRadius: "50%", background: "var(--yellow)", marginTop: 6, flex: "none" }} />
                            <span>{o}</span>
                          </div>
                        ))}
                      </div>
                    )}
                  </div>
                </div>
                <div className="st-outs">
                  <span className="k" style={{ marginRight: 4 }}>outputs</span>
                  <OutputChips outputs={ch?.outputs || []} />
                </div>
              </div>
            </div>
          );
        })}
        {!all && rows.length > SHOW && (
          <div className="st-more">
            <div style={{ textAlign: "right" }}>{rows.length - SHOW} more segments</div>
            <div style={{ position: "relative" }}><div className="dot" /></div>
            <div>
              collapsed · <a href="#" onClick={(e) => { e.preventDefault(); setAll(true); }}>expand</a>
            </div>
          </div>
        )}
      </div>
    </div>
  );
}

function Kpi({ k, v, s, c }: { k: string; v: string; s: string; c?: string }) {
  return (
    <div className="st-kpi">
      <div className="k">{k}</div>
      <div className="v" style={{ color: c }}>{v}</div>
      <div className="s">{s}</div>
    </div>
  );
}

function sourceBreakdown(segs: Segment[]) {
  const c: Record<string, number> = {};
  segs.forEach((s) => s.chapter?.learnings.forEach((l) => (c[l.source] = (c[l.source] || 0) + 1)));
  const parts = ["memory", "correction", "summary"].filter((k) => c[k]).map((k) => `${c[k]} ${k === "memory" ? "mem" : k === "correction" ? "corr" : "sum"}`);
  return parts.join(" · ") || "none yet";
}
function outputBreakdown(segs: Segment[]) {
  const c: Record<string, number> = {};
  segs.forEach((s) => s.chapter?.outputs.forEach((o) => (c[o.kind] = (c[o.kind] || 0) + 1)));
  const parts = Object.entries(c).map(([k, n]) => `${n} ${k}`);
  return parts.join(" · ") || "none";
}
