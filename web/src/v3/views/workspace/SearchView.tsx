// Search page (/w/search): full-text search over every transcript, grouped by session, with facets and Ask.
import { useEffect, useMemo, useRef, useState } from "react";
import { Link, useNavigate, useSearchParams } from "react-router-dom";
import { api } from "../../../api/client";
import type { Settings } from "../../../api/types";
import { fmtUsd } from "../../../lib/format";
import { showToast } from "../../../app/toast";
import { ApiError, v3 } from "../../api/client";
import type { AskResponse, Chip, Hit } from "../../api/types";
import { AgentIcon } from "../../components/AgentIcon";
import { removeChip } from "../../components/Palette";
import { useV3 } from "../../live";
import { ChipRow, groupHits, hitHref, markedTerms, SessionHitRow, setChip, Snip, useTitleOf, useTranscriptSearch, type SessionHits } from "./search";

const KIND: Record<string, string> = { prompt: "Prompt", reply: "Reply", command: "Command", output: "Output", error: "Error", tool: "Tool", file: "File", summary: "Summary" };
const TIMES: { v: string; l: string }[] = [
  { v: "", l: "Any time" },
  { v: "today", l: "Today" },
  { v: "24h", l: "24 hours" },
  { v: "7d", l: "7 days" },
  { v: "30d", l: "30 days" },
];
const fmtWhen = (ms: number) => {
  const d = new Date(ms);
  const t = d.toLocaleTimeString([], { hour: "numeric", minute: "2-digit" });
  return d.toDateString() === new Date().toDateString() ? t : `${d.toLocaleDateString([], { month: "short", day: "numeric" })} ${t}`;
};

function Facet({ label, k, counts, chips, onPick, top = 6 }: { label: string; k: string; counts?: Record<string, number>; chips: Chip[]; onPick: (k: string, v: string, cur?: Chip) => void; top?: number }) {
  const cur = chips.find((c) => c.key === k);
  const list = Object.entries(counts || {}).sort((a, b) => b[1] - a[1]);
  const shown = list.slice(0, top);
  if (cur && !shown.some(([v]) => v.toLowerCase() === cur.value.toLowerCase())) shown.unshift([cur.value, counts?.[cur.value] || 0]);
  if (!shown.length) return null;
  return (
    <div className="wfacet" role="group" aria-label={label}>
      <span className="wfacet-l">{label}</span>
      {shown.map(([v, n]) => {
        const on = !!cur && cur.value.toLowerCase() === v.toLowerCase();
        return (
          <button key={v} className={`wpill ${on ? "on" : ""}`} aria-pressed={on} onClick={() => onPick(k, v, on ? cur : undefined)} title={`${n} matching lines`}>
            {k === "agent" && <AgentIcon agent={v} size={11} />}
            <span className="trunc">{v}</span>
            {n > 0 && <span className="n">{n}</span>}
          </button>
        );
      })}
    </div>
  );
}

function ResultCard({ g, title, terms, threadOf }: { g: SessionHits; title: string; terms: string[]; threadOf: Record<string, string> }) {
  const [open, setOpen] = useState(false);
  const n = g.hits.length;
  return (
    <div className="wsr-card">
      <SessionHitRow g={g} title={title} href={hitHref(g.hits[0], terms.length ? terms : markedTerms(g.hits), threadOf)} />
      {n > 1 && (
        <button className="wsr-more" onClick={() => setOpen(!open)} aria-expanded={open}>
          {open ? "Hide matches" : `Show all ${n} matches`}
        </button>
      )}
      {open && (
        <ol className="wsr-hits">
          {g.hits.map((h) => (
            <li key={h.id}>
              <Link to={hitHref(h, terms.length ? terms : markedTerms([h]), threadOf)}>
                <span className="k">
                  {KIND[h.kind] || h.kind}
                  {h.tool ? ` · ${h.tool}` : ""}
                  {h.turn ? ` · turn ${h.turn}` : ""}
                </span>
                <span className="tm">{fmtWhen(h.ts)}</span>
                <Snip className={`wsnip ${h.kind === "command" || h.kind === "file" ? "mono" : ""}`} html={h.snippet} />
              </Link>
            </li>
          ))}
        </ol>
      )}
    </div>
  );
}

/** Ask errors from the daemon in words the reader can act on. */
function askError(e: unknown, st: Settings | null): string {
  const msg = (e as Error).message || "";
  if (/daily cap/i.test(msg)) return `The daily cap for model calls is reached${st ? ` (${fmtUsd(st.spentTodayUsd)} of ${fmtUsd(st.dailyCapUsd)} spent today)` : ""}. Ask works again tomorrow, or raise the cap.`;
  if (/enrichment is disabled/i.test(msg)) return "Model calls are off, so Ask cannot run. Turn them on under Rules.";
  if (e instanceof ApiError && (e.status === 404 || e.status === 405)) return "This daemon has no Ask endpoint.";
  return `Ask failed: ${msg}`;
}

function AskAnswer({ ask, hits, model }: { ask: AskResponse; hits: Hit[]; model: string }) {
  const v = useV3();
  const titleOf = useTitleOf();
  const byId = useMemo(() => new Map(hits.map((h) => [String(h.id), h])), [hits]);
  const cite = new Map(ask.citations.map((c) => [c.n, c]));
  const hrefOf = (c: AskResponse["citations"][number]) => {
    const h = byId.get(String(c.hitId)) || ({ session: c.session, ts: c.ts } as Hit);
    return hitHref(h, markedTerms([h]), v.threadOf);
  };
  const parts = ask.answer.split(/(\[\d{1,2}\])/);
  const sources = groupHits(ask.citations.map((c) => byId.get(String(c.hitId)) || ({ id: c.hitId, session: c.session, ts: c.ts, agent: c.session.split(":")[0], repo: "", cwd: "", kind: "", snippet: "" } as Hit)), v.threadOf);
  return (
    <div className="wask-a" aria-live="polite">
      <p>
        {parts.map((p, i) => {
          const m = /^\[(\d{1,2})\]$/.exec(p);
          const c = m ? cite.get(Number(m[1])) : undefined;
          return c ? (
            <Link key={i} className="wcite" to={hrefOf(c)} title="Open the cited line">
              {p}
            </Link>
          ) : (
            <span key={i}>{p}</span>
          );
        })}
      </p>
      {sources.length > 0 && (
        <div className="wask-src">
          <span className="wask-l">Cited</span>
          {sources.map((g) => (
            <Link key={g.session} to={hitHref(g.hits[0], [], v.threadOf)} className="wpill">
              <AgentIcon agent={g.agent} size={11} />
              <span className="trunc">{titleOf(g)}</span>
              <span className="n">{g.hits.map((h) => `[${ask.citations.find((c) => String(c.hitId) === String(h.id))?.n ?? "?"}]`).join(" ")}</span>
            </Link>
          ))}
        </div>
      )}
      <div className="wask-m">
        Answered by {model || "the enrichment model"} · cost {ask.costUsd > 0 && ask.costUsd < 0.01 ? "$" + ask.costUsd.toFixed(4) : fmtUsd(ask.costUsd)} · {ask.citations.length} {ask.citations.length === 1 ? "citation" : "citations"}
      </div>
    </div>
  );
}

export function SearchView({ onBack }: { onBack?: () => void }) {
  const [sp, setSp] = useSearchParams();
  const nav = useNavigate();
  const v = useV3();
  const titleOf = useTitleOf();
  const [text, setText] = useState(sp.get("q") || "");
  const mode = sp.get("mode") === "ask" ? "ask" : "search";
  const ts = useTranscriptSearch(text, { limit: 200 });
  const inRef = useRef<HTMLInputElement>(null);
  const [settings, setSettings] = useState<Settings | null>(null);
  const [ask, setAsk] = useState<{ q: string; res?: AskResponse; err?: string; busy?: boolean } | null>(null);

  useEffect(() => inRef.current?.focus(), [mode]);
  // "Open in Search" from the thread list changes ?q= while this page stays mounted.
  const urlQ = sp.get("q") || "";
  useEffect(() => {
    if (urlQ) setText((t) => (t.trim() === urlQ.trim() ? t : urlQ));
  }, [urlQ]);
  // Keep ?q= in the address so a search can be shared or reopened.
  useEffect(() => {
    const t = window.setTimeout(() => {
      const n = new URLSearchParams(sp);
      if (text.trim()) n.set("q", text.trim());
      else n.delete("q");
      if (n.toString() !== sp.toString()) setSp(n, { replace: true });
    }, 400);
    return () => window.clearTimeout(t);
  }, [text]); // eslint-disable-line react-hooks/exhaustive-deps
  useEffect(() => {
    if (mode === "ask") api.settings().then(setSettings, () => setSettings(null));
  }, [mode]);

  const setMode = (m: "search" | "ask") => {
    const n = new URLSearchParams(sp);
    if (m === "ask") n.set("mode", "ask");
    else n.delete("mode");
    setSp(n, { replace: true });
  };
  const res = ts.res;
  const chips = res?.query.chips || [];
  // Hits past the first 200, fetched by offset; they belong to one query only.
  const [more, setMore] = useState<{ q: string; hits: Hit[]; busy: boolean }>({ q: "", hits: [], busy: false });
  const extra = useMemo(() => (res && more.q === ts.q ? more.hits : []), [res, more, ts.q]);
  const allHits = useMemo(() => [...(res?.hits || []), ...extra], [res, extra]);
  const groups = useMemo(() => groupHits(allHits, v.threadOf), [allHits, v.threadOf]);
  const loadMore = () => {
    const q = ts.q;
    setMore({ q, hits: extra, busy: true });
    v3.search({ q, limit: 200, offset: allHits.length }).then(
      (r) => setMore((m) => (m.q === q ? { q, hits: [...m.hits, ...r.hits], busy: false } : m)),
      (e) => {
        setMore((m) => ({ ...m, busy: false }));
        showToast("Could not load more results", (e as Error).message, { kind: "alert" });
      },
    );
  };
  const clearFilters = () => setText(chips.reduce((t, c) => removeChip(t, c), text));
  const terms = useMemo(() => markedTerms(res?.hits || [], 6), [res]);
  const pick = (k: string, val: string, cur?: Chip) => setText(cur ? (cur.source === "inferred" ? removeChip(text, cur) : setChip(text, k, "")) : setChip(text, k, val));
  const sinceCur = chips.find((c) => c.key === "since")?.value || "";

  const runAsk = () => {
    const q = text.trim();
    if (!q || ask?.busy) return;
    setAsk({ q, busy: true });
    v3.ask(q).then(
      (r) => {
        setAsk({ q, res: r });
        api.settings().then(setSettings, () => undefined);
      },
      (e) => setAsk({ q, err: askError(e, settings) }),
    );
  };
  const off = mode === "ask" && settings != null && !settings.enrichEnabled;

  return (
    <section className="wchat wsearch" aria-label="Search">
      <div className="wbar">
        {onBack && (
          <button className="btn sm ghost wback" onClick={onBack} aria-label="Back to threads">
            ‹
          </button>
        )}
        <div className="wcrumb">
          <b>Search</b>
          <span className="rp hidephone">every agent transcript on this machine</span>
        </div>
        <span className="sp" />
        <div className="wseg" role="tablist" aria-label="Mode">
          <button role="tab" aria-selected={mode === "search"} onClick={() => setMode("search")}>
            Search
          </button>
          <button role="tab" aria-selected={mode === "ask"} onClick={() => setMode("ask")}>
            Ask
          </button>
        </div>
      </div>
      <div className="wfeed wsr">
        <form
          className="wsq"
          onSubmit={(e) => {
            e.preventDefault();
            if (mode === "ask") runAsk();
            // Results of an older query are still on screen during the delay and the fetch.
            else if (groups[0] && ts.q === text.trim() && !ts.loading) nav(hitHref(groups[0].hits[0], terms, v.threadOf));
          }}
        >
          <input
            ref={inRef}
            type="search"
            value={text}
            onChange={(e) => setText(e.target.value)}
            placeholder={mode === "ask" ? "Ask about your past sessions, e.g. why did the stg tunnel fail" : "Words, \"phrases\", agent:codex repo:name tool:Bash since:7d"}
            aria-label={mode === "ask" ? "Question" : "Search transcripts"}
          />
          {mode === "ask" && (
            <button className="btn sm pri" type="submit" disabled={!text.trim() || !!ask?.busy || off}>
              {ask?.busy ? "Asking…" : "Ask"}
            </button>
          )}
        </form>
        <ChipRow chips={chips} onRemove={(c) => setText(removeChip(text, c))} />
        {res && (
          <div className="wfacets">
            <Facet label="Agent" k="agent" counts={res.facets.agent} chips={chips} onPick={pick} />
            <Facet label="Repo" k="repo" counts={res.facets.repo} chips={chips} onPick={pick} />
            <Facet label="Tool" k="tool" counts={res.facets.tool} chips={chips} onPick={pick} />
            <div className="wfacet" role="group" aria-label="Time">
              <span className="wfacet-l">Time</span>
              {TIMES.map((t) => (
                <button key={t.v || "any"} className={`wpill ${sinceCur === t.v ? "on" : ""}`} aria-pressed={sinceCur === t.v} onClick={() => setText(setChip(text, "since", t.v))}>
                  {t.l}
                </button>
              ))}
            </div>
          </div>
        )}
        {mode === "ask" && (
          <div className="wask-p">
            <div className="wask-note">
              {off
                ? "Model calls are off, so Ask cannot run. Turn them on under Rules."
                : `Ask sends the 20 best matching lines to ${settings?.enrichModel || "the enrichment model"} and answers with citations. Each question is a paid model call that counts toward the daily cap${settings ? ` (${fmtUsd(settings.spentTodayUsd)} of ${fmtUsd(settings.dailyCapUsd)} spent today)` : ""}.`}
              {off && (
                <Link to="/rules" className="lk">
                  {" "}
                  Open Rules
                </Link>
              )}
            </div>
            {ask?.busy && <div className="wnone">Asking about “{ask.q}”…</div>}
            {ask?.err && (
              <div className="werr" role="alert">
                {ask.err}
              </div>
            )}
            {ask?.res && <AskAnswer ask={ask.res} hits={res?.hits || []} model={settings?.enrichModel || ""} />}
          </div>
        )}
        {!text.trim() && (
          <div className="state">
            <b>Search every transcript</b>
            <div>Prompts, replies, commands, tool output and errors of all your agent sessions. Results group by session; open one to jump to the matching turn.</div>
          </div>
        )}
        {text.trim().length === 1 && <div className="wnone">Type one more character.</div>}
        {ts.loading && !res && <div className="wnone">Searching…</div>}
        {ts.missing && <div className="werr">Transcript search is not available on this daemon.</div>}
        {ts.err && !ts.missing && <div className="werr">Search failed: {ts.err}</div>}
        {res && (
          <div className="wsr-meta" aria-live="polite">
            {groups.length} {groups.length === 1 ? "session" : "sessions"} · {res.total} matching {res.total === 1 ? "line" : "lines"}
            {res.total > allHits.length ? ` (best ${allHits.length} shown)` : ""}
            {ts.loading ? " · updating" : ""}
          </div>
        )}
        {res && res.total === 0 && !ts.loading && (
          <div className="state">
            <b>No matches</b>
            {chips.length > 0 ? (
              <>
                <div>Filters: {chips.map((c) => `${c.key}:${c.value}`).join(", ")}. A filter can hide the line you want.</div>
                <button className="btn sm" onClick={clearFilters}>
                  Clear filters
                </button>
              </>
            ) : (
              <div>Try fewer or shorter words, or another spelling.</div>
            )}
          </div>
        )}
        {groups.map((g) => (
          <ResultCard key={g.session} g={g} title={titleOf(g)} terms={terms} threadOf={v.threadOf} />
        ))}
        {res && res.total > allHits.length && (
          <button className="btn wearlier" disabled={more.busy} onClick={loadMore}>
            {more.busy ? "Loading…" : `Load ${Math.min(200, res.total - allHits.length)} more`}
          </button>
        )}
      </div>
    </section>
  );
}
