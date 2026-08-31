import { useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { api } from "../../api/client";
import type { Segment, Span, SpanDetail as SpanDetailT } from "../../api/types";
import { fmtBytes, fmtClock, fmtDur, fmtTime, fmtTok, fmtUsd } from "../../lib/format";
import { Icon } from "../../lib/icons";
import ChapterCard from "./ChapterBand";
import { copyText, type SpanIndex } from "./lib";

interface Props {
  sessionId: string;
  span: Span | null;
  segment: Segment;
  idx: SpanIndex;
  model: string;
  onSelect: (id: string) => void;
  onEnrich: () => void;
  enriching: boolean;
  enrichError: string | null;
}

type Tab = "overview" | "args" | "result" | "chapter";

function estimateTurnCost(model: string, t?: Span["tokens"]): number {
  if (!t) return 0;
  const m = model.toLowerCase();
  let r = [3, 15, 0.3, 3.75];
  if (m.includes("haiku")) r = [1, 5, 0.1, 1.25];
  else if (m.includes("opus") || m.includes("fable") || m.includes("mythos")) r = [15, 75, 1.5, 18.75];
  else if (m.includes("gpt") || m.includes("codex")) r = [1.25, 10, 0.125, 0];
  return (t.input * r[0] + t.output * r[1] + t.cacheRead * r[2] + t.cacheCreate * r[3]) / 1e6;
}

export default function SpanDetail({ sessionId, span, segment, idx, model, onSelect, onEnrich, enriching, enrichError }: Props) {
  const [tab, setTab] = useState<Tab>("overview");
  const [det, setDet] = useState<SpanDetailT | null>(null);
  const [err, setErr] = useState<string | null>(null);

  useEffect(() => {
    setDet(null);
    setErr(null);
    if (!span) return;
    let live = true;
    api
      .span(sessionId, span.id)
      .then((d) => live && setDet(d))
      .catch((e: Error) => live && setErr(e.message));
    return () => {
      live = false;
    };
  }, [sessionId, span?.id]);

  useEffect(() => {
    if (tab !== "chapter" && !span) setTab("chapter");
  }, [span, tab]);

  const segDur = segment.toTs - segment.fromTs || 1;
  const turn = span?.parent ? idx.byId.get(span.parent) : span?.kind === "turn" ? span : undefined;
  const parentTurn = ((): Span | undefined => {
    let cur: Span | undefined = span ?? undefined;
    while (cur && cur.kind !== "turn") cur = cur.parent ? idx.byId.get(cur.parent) : undefined;
    return cur;
  })();
  const next = det?.next || null;

  const kv = (k: string, v: React.ReactNode, color?: string) => (
    <div className="tr-kv" key={k}>
      <span className="k2">{k}</span>
      <span className="v2" style={{ color }}>{v}</span>
    </div>
  );

  const resHead = det?.result ? det.result.slice(0, 1200) : "";
  const isErr = !!span?.err;
  const exitMatch = det?.result ? /exit code[:\s]+(\d+)/i.exec(det.result.slice(0, 300)) : null;

  return (
    <div className="tr-detail">
      {!span ? (
        <>
          <div className="tr-dhead">
            <div className="tr-crumbs">segment {segment.index + 1}</div>
            <div style={{ fontWeight: 600, fontSize: 14 }}>Chapter</div>
            <div className="tr-tabs">
              <span className="on">Chapter</span>
            </div>
          </div>
          <div className="tr-dbody">
            <ChapterCard segment={segment} onEnrich={onEnrich} enriching={enriching} enrichError={enrichError} compact />
            <div className="tr-dempty" style={{ paddingLeft: 0 }}>Select a span in the flame graph or the tree to inspect it.</div>
          </div>
        </>
      ) : (
        <>
          <div className="tr-dhead">
            <div className="tr-crumbs">
              <span>segment {segment.index + 1}</span>
              {parentTurn && parentTurn.id !== span.id && (
                <>
                  <span style={{ color: "var(--muted-2)" }}>›</span>
                  <span style={{ cursor: "pointer" }} onClick={() => onSelect(parentTurn.id)}>
                    {parentTurn.name} · {fmtDur(parentTurn.dur)}
                  </span>
                </>
              )}
              <span style={{ color: "var(--muted-2)" }}>›</span>
              <span style={{ color: "var(--text-2)" }}>{span.kind === "user" ? "you" : span.name}</span>
            </div>
            <div style={{ display: "flex", alignItems: "center", gap: 8 }}>
              <span className={`sq ${isErr ? "err" : ""}`} style={{ width: 10, height: 10, borderRadius: 2, background: span.kind === "user" ? "var(--fam-user)" : `var(--fam-${span.fam})`, boxShadow: isErr ? "inset 0 0 0 1.5px var(--red)" : undefined }} />
              <span style={{ fontWeight: 600, fontSize: 14 }}>{span.kind === "user" ? "you" : span.name}</span>
              <span className="mono t2" style={{ fontSize: 12.5, overflow: "hidden", textOverflow: "ellipsis", whiteSpace: "nowrap", flex: 1 }}>{span.res || span.text}</span>
              {isErr && <span className="tr-pill err">{exitMatch ? `exit ${exitMatch[1]}` : "error"}</span>}
              {span.flag === "correction" && <span className="tr-src correction">correction</span>}
            </div>
            <div style={{ display: "flex", alignItems: "center", gap: 14 }}>
              <span className="tr-big">{span.kind === "user" ? fmtClock(span.ts) : fmtDur(span.dur)}</span>
              {span.kind !== "user" && (
                <span className="muted" style={{ fontSize: 11.5 }}>
                  {((span.dur / segDur) * 100).toFixed(1)}% of segment
                  {parentTurn && parentTurn.id !== span.id && parentTurn.dur > 0 && ` · ${((span.dur / parentTurn.dur) * 100).toFixed(0)}% of ${parentTurn.name}`}
                </span>
              )}
            </div>
            <div className="tr-tabs">
              {(["overview", "args", "result", "chapter"] as Tab[]).map((t) => (
                <span key={t} className={tab === t ? "on" : ""} onClick={() => setTab(t)}>
                  {t[0].toUpperCase() + t.slice(1)}
                </span>
              ))}
            </div>
          </div>
          <div className="tr-dbody">
            {err && <div className="tr-error" style={{ padding: "8px 0" }}>{err}</div>}
            {tab === "overview" && (
              <>
                {kv("start", `${fmtTime(span.ts)} · +${fmtDur(span.ts - segment.fromTs)}`)}
                {span.kind !== "user" && kv("end", fmtTime(span.ts + span.dur))}
                {span.kind !== "user" && kv("status", isErr ? "error" : span.dur === 0 ? "no result" : "ok", isErr ? "var(--red)" : undefined)}
                {det && kv("output size", `${fmtBytes(det.result.length)} · args ${fmtBytes(det.args.length)}`)}
                {kv("span id", span.id)}
                {span.child && kv("child session", <Link to={`/session/${encodeURIComponent(span.child)}`}>{span.child.slice(0, 22)}…</Link>)}
                {turn?.tokens && kv("turn tokens", `in ${fmtTok(turn.tokens.input)} · out ${fmtTok(turn.tokens.output)} · cache ${fmtTok(turn.tokens.cacheRead + turn.tokens.cacheCreate)}`)}
                {turn?.tokens && kv("turn cost", fmtUsd(estimateTurnCost(turn.model || model, turn.tokens), true))}
                {span.kind === "user" && span.text && (
                  <>
                    <div className="k" style={{ margin: "12px 0 6px" }}>prompt</div>
                    <pre className="tr-pre">{span.text}</pre>
                  </>
                )}
                {span.kind === "turn" && span.text && (
                  <>
                    <div className="k" style={{ margin: "12px 0 6px" }}>assistant · first prose</div>
                    <pre className="tr-pre">{span.text}</pre>
                  </>
                )}
                {resHead && (
                  <>
                    <div className="k" style={{ margin: "12px 0 6px" }}>{isErr ? "stderr / result" : "result · head"}</div>
                    <pre className={`tr-pre ${isErr ? "err" : ""}`}>{resHead}{det && det.result.length > 1200 ? "\n…" : ""}</pre>
                  </>
                )}
                {next && (
                  <>
                    <div className="k" style={{ margin: "12px 0 6px" }}>what happened next</div>
                    <div style={{ fontSize: 12, color: "var(--text-2)", lineHeight: 1.45, cursor: "pointer" }} onClick={() => onSelect(next.id)}>
                      You wrote at {fmtClock(next.ts)} ({fmtDur(next.ts - (span.ts + span.dur))} later): “{next.text}”
                      {next.flag === "correction" && (
                        <span className="tr-src correction" style={{ marginLeft: 6 }}>correction</span>
                      )}
                    </div>
                  </>
                )}
                {det?.same && det.same.length > 0 && (
                  <>
                    <div className="k" style={{ margin: "12px 0 6px" }}>same command in this session · {det.same.length}</div>
                    {det.same.slice(0, 8).map((o) => (
                      <div key={o.id} className="tr-same" onClick={() => onSelect(o.id)}>
                        <span className="num muted" style={{ width: 52 }}>seg {o.seg + 1}</span>
                        <span className="num" style={{ color: o.err ? "var(--red)" : "var(--green)" }}>
                          {o.err ? "error" : "ok"} · {fmtDur(o.dur)}
                        </span>
                        <span className="muted num" style={{ fontSize: 10.5 }}>{fmtClock(o.ts)}</span>
                      </div>
                    ))}
                  </>
                )}
              </>
            )}
            {tab === "args" && (
              <>
                <div style={{ display: "flex", justifyContent: "flex-end", marginBottom: 6 }}>
                  <button className="tr-btn" style={{ height: 22 }} onClick={() => det && copyText(det.args)}>
                    <Icon name="copy" size={11} /> copy
                  </button>
                </div>
                <pre className="tr-pre" style={{ maxHeight: "none" }}>{det ? prettyJSON(det.args) : "loading…"}</pre>
              </>
            )}
            {tab === "result" && (
              <>
                <div style={{ display: "flex", justifyContent: "flex-end", marginBottom: 6 }}>
                  <button className="tr-btn" style={{ height: 22 }} onClick={() => det && copyText(det.result)}>
                    <Icon name="copy" size={11} /> copy
                  </button>
                </div>
                <pre className={`tr-pre ${isErr ? "err" : ""}`} style={{ maxHeight: "none" }}>{det ? det.result || "(empty)" : "loading…"}</pre>
              </>
            )}
            {tab === "chapter" && <ChapterCard segment={segment} onEnrich={onEnrich} enriching={enriching} enrichError={enrichError} compact />}
          </div>
          <div className="tr-dfoot">
            <Link className="tr-btn" to={`/session/${encodeURIComponent(sessionId)}?tab=transcript&q=${encodeURIComponent((span.res || span.text || "").slice(0, 60))}`}>
              <Icon name="doc" size={12} /> transcript
            </Link>
            {span.fam === "bash" && (
              <button className="tr-btn" onClick={() => det && copyText(commandOf(det.args) || span.res || "")}>
                <Icon name="copy" size={12} /> copy command
              </button>
            )}
            <button className="tr-btn" onClick={() => copyText(`${location.origin}/session/${encodeURIComponent(sessionId)}?seg=${segment.index}&span=${span.id}`)}>
              <Icon name="link" size={12} /> permalink
            </button>
          </div>
        </>
      )}
    </div>
  );
}

function prettyJSON(s: string): string {
  try {
    return JSON.stringify(JSON.parse(s), null, 2);
  } catch {
    return s;
  }
}
function commandOf(args: string): string {
  try {
    const j = JSON.parse(args) as { command?: unknown };
    if (typeof j.command === "string") return j.command;
    if (Array.isArray(j.command)) return j.command.join(" ");
  } catch {
    /* raw args */
  }
  return "";
}
