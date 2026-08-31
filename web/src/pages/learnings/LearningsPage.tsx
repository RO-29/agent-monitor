import { useEffect, useMemo, useState } from "react";
import { Link, useParams } from "react-router-dom";
import { api } from "../../api/client";
import type { Learning, LearningSource, ThreadLearningsResponse } from "../../api/types";
import { fmtDate, shortCwd } from "../../lib/format";
import { Icon } from "../../lib/icons";
import { copyText } from "../trace/lib";
import "../trace/trace.css";
import "./learnings.css";

const SOURCES: LearningSource[] = ["memory", "correction", "summary", "output"];
const SESSION_RE = /^(claude|codex|opencode|cursor|cursor-agent):[0-9a-f-]{8,}/i;
const promoteType: Record<LearningSource, string> = { correction: "feedback", memory: "project", summary: "project", output: "reference" };

export default function LearningsPage() {
  const { id = "" } = useParams();
  const threadId = decodeURIComponent(id);
  const [data, setData] = useState<ThreadLearningsResponse | null>(null);
  const [err, setErr] = useState<string | null>(null);
  const [source, setSource] = useState<"all" | LearningSource>("all");
  const [seg, setSeg] = useState<"all" | number>("all");
  const [sort, setSort] = useState<"time" | "source">("time");
  const [checked, setChecked] = useState<Set<string>>(new Set());
  const [busy, setBusy] = useState(false);
  const [note, setNote] = useState<string | null>(null);

  useEffect(() => {
    setData(null);
    api.threadLearnings(threadId).then(setData).catch((e: Error) => setErr(e.message));
  }, [threadId]);

  const all = data?.learnings || [];
  const counts = useMemo(() => {
    const c: Record<string, number> = {};
    all.forEach((l) => (c[l.source] = (c[l.source] || 0) + 1));
    return c;
  }, [all]);
  const segments = useMemo(() => Array.from(new Set(all.map((l) => l.seg))).sort((a, b) => a - b), [all]);
  const rows = useMemo(() => {
    let r = all.filter((l) => (source === "all" || l.source === source) && (seg === "all" || l.seg === seg));
    r = [...r].sort((a, b) => (sort === "time" ? a.ts - b.ts : a.source.localeCompare(b.source) || a.ts - b.ts));
    return r;
  }, [all, source, seg, sort]);

  const sessionOf = (l: Learning) => (l.ref && SESSION_RE.test(l.ref) ? l.ref : threadId);
  const openHref = (l: Learning) => {
    if (l.source === "output" && /^https?:/.test(l.evidence.split("·").pop()?.trim() || "")) return l.evidence.split("·").pop()!.trim();
    return `/session/${encodeURIComponent(sessionOf(l))}?seg=${l.seg}`;
  };

  const asMarkdown = () =>
    [`# Learnings · ${data?.thread.title || threadId}`, "", ...rows.map((l) => `- **${l.source}**${l.heuristic ? " (heuristic)" : ""}: ${l.text}  \n  _${l.evidence} · segment ${l.seg + 1} · ${fmtDate(l.ts)}_`)].join("\n");

  const promote = async () => {
    const picked = rows.filter((l) => checked.has(l.id));
    if (!picked.length) return;
    setBusy(true);
    setNote(null);
    let ok = 0;
    const errors: string[] = [];
    for (const l of picked) {
      try {
        await api.promote(sessionOf(l), l.text, promoteType[l.source]);
        ok++;
      } catch (e) {
        errors.push((e as Error).message);
      }
    }
    setBusy(false);
    setChecked(new Set());
    setNote(`${ok} promoted to memory${errors.length ? ` · ${errors.length} failed: ${errors[0]}` : ""}`);
  };

  if (err) return <div className="ln-root"><div className="tr-error">{err}</div></div>;
  if (!data) return <div className="ln-root"><div className="tr-loading">loading learnings…</div></div>;

  return (
    <div className="ln-root">
      <div className="ln-head">
        <div className="ln-title">
          <h1>Learnings</h1>
          <span className="mono muted" style={{ fontSize: 12 }}>
            thread · {data.thread.title} · {shortCwd(data.thread.cwd)}
          </span>
          <div style={{ flex: 1 }} />
          <Link className="tr-btn" to={`/thread/${encodeURIComponent(threadId)}`}>
            <Icon name="book" size={13} /> story
          </Link>
          <button className="tr-btn" onClick={() => { copyText(asMarkdown()); setNote("copied as markdown"); }}>
            <Icon name="copy" size={13} /> copy as markdown
          </button>
          <button className="tr-btn" style={{ background: "var(--accent)", color: "#07080b", borderColor: "var(--accent)" }} disabled={busy || checked.size === 0} onClick={promote}>
            {busy ? <span className="tr-spin" /> : <Icon name="spark" size={13} color="#07080b" />} promote {checked.size || ""} to memory
          </button>
        </div>
        <div className="ln-filters">
          <span className={`tr-chip ${source === "all" ? "on" : ""}`} onClick={() => setSource("all")}>
            All <span className="num">{all.length}</span>
          </span>
          {SOURCES.map((s) => (
            <span key={s} className={`tr-chip ${source === s ? "on" : ""}`} onClick={() => setSource(s)}>
              <span className={`tr-src ${s}`}>{s}</span> {counts[s] || 0}
            </span>
          ))}
          <span className="tr-sep" />
          <select className="tr-chip" value={String(seg)} onChange={(e) => setSeg(e.target.value === "all" ? "all" : Number(e.target.value))} style={{ background: "var(--bg-2)" }}>
            <option value="all">segment · all</option>
            {segments.map((s) => (
              <option key={s} value={s}>segment {s + 1}</option>
            ))}
          </select>
          <span className={`tr-chip`} onClick={() => setSort(sort === "time" ? "source" : "time")}>
            sort · {sort} <Icon name="chevd" size={10} />
          </span>
          <div style={{ flex: 1 }} />
          {note && <span className="muted" style={{ fontSize: 11.5 }}>{note}</span>}
        </div>
      </div>
      <div className="ln-grid head k">
        <input type="checkbox" className="ln-cb" checked={rows.length > 0 && rows.every((l) => checked.has(l.id))} onChange={(e) => setChecked(e.target.checked ? new Set(rows.map((l) => l.id)) : new Set())} />
        <span>source</span>
        <span>learning · evidence</span>
        <span>segment</span>
        <span className="c-when">when</span>
        <span className="c-open" style={{ textAlign: "right" }}>open</span>
      </div>
      <div className="ln-body">
        {rows.length === 0 && <div className="tr-empty">No learnings match.</div>}
        {rows.map((l) => {
          const href = openHref(l);
          const external = /^https?:/.test(href);
          return (
            <div key={l.id} className={`ln-grid ${checked.has(l.id) ? "ln-sel" : ""}`}>
              <input
                type="checkbox"
                className="ln-cb"
                checked={checked.has(l.id)}
                onChange={(e) =>
                  setChecked((prev) => {
                    const n = new Set(prev);
                    if (e.target.checked) n.add(l.id);
                    else n.delete(l.id);
                    return n;
                  })
                }
              />
              <span style={{ display: "flex", gap: 4, flexWrap: "wrap" }}>
                <span className={`tr-src ${l.source}`}>{l.source}</span>
                {l.heuristic && <span className="tr-src heur">heuristic</span>}
              </span>
              <div className="txt">
                <span>{l.text}</span>
                <span className="ev" title={l.evidence}>{l.evidence}</span>
              </div>
              <span className="num t2" style={{ fontSize: 11.5 }}>segment {l.seg + 1}</span>
              <span className="num muted c-when" style={{ fontSize: 11.5 }}>{fmtDate(l.ts)}</span>
              <span className="c-open" style={{ justifySelf: "end" }}>
                {external ? (
                  <a className="tr-chip sm" href={href} target="_blank" rel="noreferrer">
                    open <Icon name="external" size={10} />
                  </a>
                ) : (
                  <Link className="tr-chip sm" to={href}>
                    trace <Icon name="arrow" size={10} />
                  </Link>
                )}
              </span>
            </div>
          );
        })}
      </div>
      <div className="ln-foot">
        <Icon name="flag" size={11} color="var(--yellow)" /> corrections are heuristic until enrichment runs on the segment
        <div style={{ flex: 1 }} />
        <span className="num">{rows.length} shown · {all.length} total</span>
      </div>
    </div>
  );
}
