import { useEffect, useMemo, useState } from "react";
import { Link } from "react-router-dom";
import { api } from "../../api/client";
import type { FeedLearning, LearningSource } from "../../api/types";
import { fmtAgo, projectName } from "../../lib/format";
import { Icon, ToolLogo } from "../../lib/icons";
import { Markdown } from "../../lib/markdown";
import { useSearch } from "../../app/search";
import { copyText } from "../trace/lib";
import "../trace/trace.css";
import "./learnings.css";

const SOURCES: LearningSource[] = ["memory", "correction", "summary", "output"];
const promoteType: Record<LearningSource, string> = { correction: "feedback", memory: "project", summary: "project", output: "reference" };

/** Global ledger: the newest learnings across every recently active thread. */
export default function LearningsFeed() {
  const [rows, setRows] = useState<FeedLearning[] | null>(null);
  const [meta, setMeta] = useState<{ total: number; threadsScanned: number; truncated: boolean; counts: Record<string, number> } | null>(null);
  const [err, setErr] = useState("");
  const [source, setSource] = useState<"all" | LearningSource>("all");
  const [depth, setDepth] = useState(60);
  const [checked, setChecked] = useState<Set<string>>(new Set());
  const [busy, setBusy] = useState(false);
  const [note, setNote] = useState<string | null>(null);
  const search = useSearch().trim().toLowerCase();

  useEffect(() => {
    let alive = true;
    setRows(null);
    api
      .learningsFeed({ limit: 400, threads: depth })
      .then((r) => {
        if (!alive) return;
        setRows(r.learnings);
        setMeta({ total: r.total, threadsScanned: r.threadsScanned, truncated: r.truncated, counts: r.counts });
        setErr("");
      })
      .catch((e: Error) => alive && setErr(e.message));
    return () => {
      alive = false;
    };
  }, [depth]);

  const key = (l: FeedLearning) => `${l.threadId}|${l.id}`;
  const shown = useMemo(() => {
    let r = rows || [];
    if (source !== "all") r = r.filter((l) => l.source === source);
    if (search) r = r.filter((l) => `${l.text} ${l.evidence} ${l.threadTitle} ${l.cwd}`.toLowerCase().includes(search));
    return r;
  }, [rows, source, search]);

  const promote = async () => {
    const picked = shown.filter((l) => checked.has(key(l)));
    if (!picked.length) return;
    setBusy(true);
    setNote(null);
    let ok = 0;
    let firstErr = "";
    for (const l of picked) {
      try {
        await api.promote(l.sessionId, l.text, promoteType[l.source]);
        ok++;
      } catch (e) {
        firstErr = firstErr || (e as Error).message;
      }
    }
    setBusy(false);
    setChecked(new Set());
    setNote(`${ok} promoted to memory${firstErr ? ` · failed: ${firstErr}` : ""}`);
  };

  const asMarkdown = () => ["# Learnings", "", ...shown.map((l) => `- **${l.source}**: ${l.text}  \n  _${l.threadTitle} · ${projectName(l.cwd)} · ${fmtAgo(l.ts)}_`)].join("\n");

  return (
    <div className="ln-root">
      <div className="ln-head">
        <div className="ln-title">
          <h1>Learnings</h1>
          <span className="num muted">{shown.length}</span>
          <div style={{ flex: 1 }} />
          <button className="tr-btn" onClick={() => { copyText(asMarkdown()); setNote("copied as markdown"); }}>
            <Icon name="copy" size={13} /> copy as markdown
          </button>
          <button
            className="tr-btn"
            style={{ background: "var(--accent)", color: "#07080b", borderColor: "var(--accent)" }}
            disabled={busy || checked.size === 0}
            onClick={promote}
          >
            {busy ? <span className="tr-spin" /> : <Icon name="spark" size={13} color="#07080b" />} promote {checked.size || ""} to memory
          </button>
        </div>
        <div className="ln-filters">
          <span className={`tr-chip ${source === "all" ? "on" : ""}`} onClick={() => setSource("all")}>
            All <span className="num">{rows?.length ?? 0}</span>
          </span>
          {SOURCES.map((s) => (
            <span key={s} className={`tr-chip ${source === s ? "on" : ""}`} onClick={() => setSource(s)}>
              <span className={`tr-src ${s}`}>{s}</span> {meta?.counts[s] || 0}
            </span>
          ))}
          <span className="tr-sep" />
          <select className="tr-chip" value={String(depth)} onChange={(e) => setDepth(Number(e.target.value))} style={{ background: "var(--bg-2)" }}>
            <option value="25">last 25 threads</option>
            <option value="60">last 60 threads</option>
            <option value="150">last 150 threads</option>
            <option value="400">every thread</option>
          </select>
          <div style={{ flex: 1 }} />
          {note && <span className="muted" style={{ fontSize: 11.5 }}>{note}</span>}
        </div>
      </div>
      {err && <div className="tr-error">learnings unavailable: {err}</div>}
      {!err && rows === null && <div className="tr-loading">reading {depth} threads…</div>}
      {rows !== null && (
        <>
          <div className="ln-grid feed head k">
            <input
              type="checkbox"
              className="ln-cb"
              checked={shown.length > 0 && shown.every((l) => checked.has(key(l)))}
              onChange={(e) => setChecked(e.target.checked ? new Set(shown.map(key)) : new Set())}
            />
            <span>source</span>
            <span>learning · evidence</span>
            <span>thread</span>
            <span className="c-when">when</span>
            <span className="c-open" style={{ textAlign: "right" }}>open</span>
          </div>
          <div className="ln-body">
            {shown.length === 0 && <div className="tr-empty">No learnings match.</div>}
            {shown.map((l) => {
              const href = l.source === "output" && /^https?:/.test(l.evidence.split("·").pop()?.trim() || "") ? l.evidence.split("·").pop()!.trim() : "";
              const k = key(l);
              return (
                <div key={k} className={`ln-grid feed ${checked.has(k) ? "ln-sel" : ""}`}>
                  <input
                    type="checkbox"
                    className="ln-cb"
                    checked={checked.has(k)}
                    onChange={(e) =>
                      setChecked((prev) => {
                        const n = new Set(prev);
                        if (e.target.checked) n.add(k);
                        else n.delete(k);
                        return n;
                      })
                    }
                  />
                  <span style={{ display: "flex", gap: 4, flexWrap: "wrap" }}>
                    <span className={`tr-src ${l.source}`}>{l.source}</span>
                    {l.heuristic && <span className="tr-src heur">heuristic</span>}
                  </span>
                  <div className="txt">
                    <Markdown text={l.text} inlineOnly />
                    <span className="ev" title={l.evidence}>{l.evidence}</span>
                  </div>
                  <Link className="txt" to={`/thread/${encodeURIComponent(l.threadId)}/learnings`} style={{ color: "inherit", minWidth: 0 }}>
                    <span className="ell" style={{ display: "flex", gap: 5, alignItems: "center" }}>
                      <ToolLogo tool={l.tool} size={11} /> {l.threadTitle || "Untitled"}
                    </span>
                    <span className="ev">{projectName(l.cwd)}</span>
                  </Link>
                  <span className="num muted c-when" style={{ fontSize: 11.5 }}>{fmtAgo(l.ts)}</span>
                  <span className="c-open" style={{ justifySelf: "end" }}>
                    {href ? (
                      <a className="tr-chip sm" href={href} target="_blank" rel="noreferrer">
                        open <Icon name="external" size={10} />
                      </a>
                    ) : (
                      <Link className="tr-chip sm" to={`/session/${encodeURIComponent(l.sessionId)}?seg=${l.seg}`}>
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
            <span className="num">
              {shown.length} shown · {meta?.total ?? 0} across {meta?.threadsScanned ?? 0} threads{meta?.truncated ? " (more remain)" : ""}
            </span>
          </div>
        </>
      )}
    </div>
  );
}
