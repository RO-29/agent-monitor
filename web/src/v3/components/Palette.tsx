import { useEffect, useMemo, useRef, useState } from "react";
import { useNavigate } from "react-router-dom";
import { fmtAgo, fmtUsd, projectName } from "../../lib/format";
import { isMissing, v3 } from "../api/client";
import type { AskResponse, Chip, ContextDoc, ContextResponse, Hit, SearchResponse, SearchStatus } from "../api/types";
import { useV3 } from "../live";
import { threadTitle } from "../model";
import { AgentBadge, DrivenTag, Kbd } from "./ui";
import { toMs, useOrch, type OrchState } from "../api/orch";
import { hitHref, markedTerms, Snip } from "../views/workspace/search";

/** Sample-data hits from driven threads (?mock=1 only): agent text, commands and file paths. */
function mockDrivenHits(orch: OrchState, q: string): Hit[] {
  const words = q.toLowerCase().split(/\s+/).filter((w) => w && !w.includes(":"));
  if (!words.length) return [];
  const out: Hit[] = [];
  for (const d of orch.details.values())
    for (const it of d.items) {
      const text = it.kind === "command" ? it.command || "" : it.kind === "fileChange" ? (it.files || []).map((f) => f.path).join(" ") : it.kind === "agent" || it.kind === "user" ? it.text : "";
      const low = text.toLowerCase();
      if (!text || !words.every((w) => low.includes(w))) continue;
      const i = low.indexOf(words[0]);
      const snip = text.slice(Math.max(0, i - 40), i + 120).replace(new RegExp(`(${words[0].replace(/[.*+?^${}()|[\]\\]/g, "\\$&")})`, "i"), "<mark>$1</mark>");
      out.push({ id: `drv-${it.id}`, session: `orch:${d.thread.id}`, agent: d.thread.agent, cwd: d.thread.cwd, repo: d.thread.repo, ts: toMs(it.startedAt), kind: it.kind === "command" ? "command" : it.kind === "fileChange" ? "file" : it.kind === "user" ? "prompt" : "reply", snippet: snip, threadId: d.thread.id, drivenId: d.thread.id });
    }
  return out.sort((a, b) => b.ts - a.ts).slice(0, 8);
}

const KIND_COLOR: Record<string, string> = { reply: "var(--codex)", prompt: "var(--accent)", command: "var(--run)", tool: "var(--run)", output: "var(--fg-2)", error: "var(--err)", file: "var(--need)", summary: "var(--sub)" };
const KIND_LABEL: Record<string, string> = { reply: "Replies", prompt: "Prompts", command: "Commands", tool: "Tools", output: "Outputs", error: "Errors", file: "Files", summary: "Summaries" };

const INFER: Record<string, (v: string) => string[]> = {
  agent: (v) => [v],
  "is": () => ["failed", "failure", "failures", "errors", "error", "broken", "fail"],
  since: (v) => [v, "today", "yesterday", "monday", "tuesday", "wednesday", "thursday", "friday", "saturday", "sunday", "mon", "tue", "wed", "thu", "fri", "sat", "sun", "week"],
  repo: (v) => [v],
  file: (v) => [v],
  kind: (v) => [v, v + "s", v === "reply" ? "replies" : v],
};

/** Removes a chip from the query text: typed tokens go, inferred words become quoted literals. */
export function removeChip(text: string, c: Chip): string {
  if (c.source === "typed") {
    const esc = c.value.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
    return text.replace(new RegExp(`(^|\\s)${c.key}:("?)${esc}\\2(?=\\s|$)`, "i"), " ").replace(/\s+/g, " ").trim();
  }
  const cands = new Set((INFER[c.key]?.(c.value.toLowerCase()) || [c.value.toLowerCase()]).map((x) => x.toLowerCase()));
  const parts = text.split(/(\s+)/);
  for (let i = 0; i < parts.length; i++) {
    const w = parts[i];
    if (!w.trim() || w.startsWith('"')) continue;
    if (cands.has(w.toLowerCase())) {
      parts[i] = `"${w}"`;
      return parts.join("");
    }
  }
  return text;
}

export function Palette({ initial, onClose }: { initial: { q?: string; scope?: string }; onClose: () => void }) {
  const nav = useNavigate();
  const st = useV3();
  const orch = useOrch();
  const drivenOf = (h: Hit) => h.drivenId || (h.threadId && orch.threads.has(h.threadId) ? h.threadId : undefined);
  const [text, setText] = useState(initial.q || "");
  const [scope, setScope] = useState<string | undefined>(initial.scope);
  const [kind, setKind] = useState("");
  const [res, setRes] = useState<SearchResponse | null>(null);
  const [err, setErr] = useState("");
  const [missing, setMissing] = useState(false);
  const [loading, setLoading] = useState(false);
  const [sel, setSel] = useState(0);
  const [ctx, setCtx] = useState<ContextResponse | null>(null);
  const [ask, setAsk] = useState<AskResponse | null>(null);
  const [askState, setAskState] = useState<"" | "busy" | string>("");
  const [status, setStatus] = useState<SearchStatus | null>(null);
  const inRef = useRef<HTMLInputElement>(null);
  const listRef = useRef<HTMLDivElement>(null);
  const titleOf = useMemo(() => new Map(st.threads.map((t) => [t.id, threadTitle(t)])), [st.threads]);

  useEffect(() => inRef.current?.focus(), []);
  useEffect(() => {
    v3.searchStatus().then(setStatus).catch(() => setStatus(null));
  }, []);

  useEffect(() => {
    const q = text.trim();
    if (!q) {
      setRes(null);
      setErr("");
      return;
    }
    let alive = true;
    setLoading(true);
    const t = window.setTimeout(() => {
      v3.search({ q, scope, kind: kind || undefined, limit: 50 })
        .then((r) => {
          if (!alive) return;
          if (orch.mock) {
            const extra = mockDrivenHits(orch, q);
            r = { ...r, total: r.total + extra.length, hits: [...extra, ...(r.hits || [])] };
          }
          setRes(r);
          setErr("");
          setMissing(false);
          setSel(0);
        })
        .catch((e) => {
          if (!alive) return;
          if (isMissing(e)) setMissing(true);
          else setErr((e as Error).message);
        })
        .finally(() => alive && setLoading(false));
    }, 180);
    return () => {
      alive = false;
      window.clearTimeout(t);
    };
  }, [text, scope, kind]);

  const hits = res?.hits || [];
  const hit: Hit | undefined = hits[sel];
  useEffect(() => {
    setCtx(null);
    if (!hit) return;
    let alive = true;
    const t = window.setTimeout(() => {
      v3.context(hit.id, 2)
        .then((c) => alive && setCtx(c))
        .catch(() => alive && setCtx({ docs: [] }));
    }, 120);
    return () => {
      alive = false;
      window.clearTimeout(t);
    };
  }, [hit?.id]); // eslint-disable-line react-hooks/exhaustive-deps
  useEffect(() => {
    listRef.current?.querySelector(".hit.sel")?.scrollIntoView({ block: "nearest" });
  }, [sel]);

  const open = (h: Hit) => {
    onClose();
    nav(hitHref(h, markedTerms([h]), st.threadOf));
  };
  const runAsk = () => {
    const q = text.trim();
    if (!q) return;
    setAskState("busy");
    setAsk(null);
    v3.ask(q)
      .then((r) => {
        setAsk(r);
        setAskState("");
      })
      .catch((e) => setAskState(isMissing(e) ? "Ask is not available on this daemon yet." : (e as Error).message));
  };

  const chips = res?.query.chips || [];
  const facet = res?.facets.kind || {};
  const kinds = Object.entries(facet).sort((a, b) => b[1] - a[1]);
  const docs: { d: ContextDoc; hit: boolean }[] = ctx
    ? ctx.docs
      ? ctx.docs.map((d) => ({ d, hit: String(d.id) === String(hit?.id) }))
      : [...(ctx.before || []).map((d) => ({ d, hit: false })), ...(ctx.doc ? [{ d: ctx.doc, hit: true }] : []), ...(ctx.after || []).map((d) => ({ d, hit: false }))]
    : [];

  const onKey = (e: React.KeyboardEvent) => {
    e.stopPropagation();
    if (e.key === "Escape") onClose();
    else if (e.key === "ArrowDown") {
      e.preventDefault();
      setSel((s) => Math.min(s + 1, Math.max(hits.length - 1, 0)));
    } else if (e.key === "ArrowUp") {
      e.preventDefault();
      setSel((s) => Math.max(s - 1, 0));
    } else if (e.key === "Enter") {
      e.preventDefault();
      if (e.metaKey || e.ctrlKey) runAsk();
      else if (hit) open(hit);
    } else if (e.key === "Backspace" && e.shiftKey) {
      e.preventDefault();
      setScope(undefined);
    } else if (e.key === "Backspace" && !text && chips.length) {
      setText(removeChip(text, chips[chips.length - 1]));
    } else if (e.key === "Tab") {
      e.preventDefault();
      const order = ["", ...kinds.map((k) => k[0])];
      setKind(order[(order.indexOf(kind) + (e.shiftKey ? order.length - 1 : 1)) % order.length]);
    }
  };

  return (
    <div className="scrim" onMouseDown={onClose}>
      <div className="palette" role="dialog" aria-label="Search" onMouseDown={(e) => e.stopPropagation()} onKeyDown={onKey}>
        <div className="qin">
          <span className="muted">⌕</span>
          {scope && (
            <span className="chip t" title="Scope: results from this repo rank first. Shift+Backspace clears it.">
              scope:{scope}
              <button className="x" aria-label="Clear scope" onClick={() => setScope(undefined)}>
                ×
              </button>
            </span>
          )}
          {chips.map((c, i) => (
            <span key={i} className={`chip ${c.source === "inferred" ? "inf" : ""}`} title={c.source === "inferred" ? "inferred from a word you typed" : "typed filter"}>
              {c.key}:{c.value}
              <button className="x" aria-label={`Remove ${c.key}`} onClick={() => setText(removeChip(text, c))}>
                ×
              </button>
            </span>
          ))}
          <input
            ref={inRef}
            value={text}
            onChange={(e) => setText(e.target.value)}
            placeholder="Search prompts, replies, commands, files, errors…  (codex, failed, today, repo names become filters)"
            aria-label="Search query"
          />
          {loading && <span className="xs muted">searching…</span>}
        </div>
        <div className="ftabs">
          <button className={kind === "" ? "on" : ""} onClick={() => setKind("")}>
            All<b>{res ? res.total.toLocaleString() : ""}</b>
          </button>
          {kinds.map(([k, n]) => (
            <button key={k} className={kind === k ? "on" : ""} onClick={() => setKind(kind === k ? "" : k)}>
              {KIND_LABEL[k] || k}
              <b>{n.toLocaleString()}</b>
            </button>
          ))}
          <span className="ml-auto row xs">
            <span className="muted">Find ·</span>
            <button className="btn sm" onClick={runAsk} disabled={!text.trim() || askState === "busy"} title="Ask Haiku over the top 20 hits (⌘↵)">
              {askState === "busy" ? "Asking…" : "Ask"}
            </button>
          </span>
        </div>
        {(ask || (askState && askState !== "busy")) && (
          <div className="answer">
            {ask ? (
              <>
                <div className="xs muted" style={{ marginBottom: 4 }}>
                  Ask · Haiku · {ask.citations.length} citations · {fmtUsd(ask.costUsd)}
                </div>
                <div>{ask.answer}</div>
                <div className="row xs" style={{ marginTop: 6, flexWrap: "wrap" }}>
                  {ask.citations.map((c) => (
                    <sup
                      key={c.n}
                      onClick={() => {
                        const i = hits.findIndex((h) => String(h.id) === String(c.hitId));
                        if (i >= 0) setSel(i);
                      }}
                    >
                      [{c.n}] {fmtAgo(c.ts)}
                    </sup>
                  ))}
                </div>
              </>
            ) : (
              <div className="err-t sm">{askState}</div>
            )}
          </div>
        )}
        <div className="res">
          <div className="hits" ref={listRef}>
            {missing && (
              <div className="state">
                <b>Search is not available on this daemon yet</b>
                <div>
                  <code>/api/search</code> answered 404.
                </div>
              </div>
            )}
            {!missing && err && (
              <div className="state err">
                <b>Search failed</b>
                <div className="mono xs">{err}</div>
              </div>
            )}
            {!missing && !err && !text.trim() && (
              <div className="state">
                <b>Search every Claude and Codex transcript</b>
                <div>
                  Try <code>failed go test this week</code>, <code>codex repo:agent-monitor</code> or <code>file:*.tsx</code>.
                </div>
              </div>
            )}
            {!missing && !err && res && hits.length === 0 && !loading && <div className="state"><b>No matches</b><div>Remove a filter or widen the scope.</div></div>}
            {hits.map((h, i) => (
              <div key={String(h.id)} className={`hit ${i === sel ? "sel" : ""}`} onMouseEnter={() => setSel(i)} onClick={() => open(h)}>
                <div className="hh">
                  <span className="kind" style={{ color: KIND_COLOR[h.kind] || "var(--fg-2)" }}>
                    {h.kind}
                  </span>
                  <AgentBadge tool={h.agent} small />
                  <span className="trunc">{(drivenOf(h) && orch.threads.get(drivenOf(h)!)?.title) || titleOf.get(h.threadId || st.threadOf[h.session] || "") || h.repo || projectName(h.cwd)}</span>
                  {drivenOf(h) && <DrivenTag />}
                  <span style={{ flex: "none" }}>· {fmtAgo(h.ts)}</span>
                  {h.tool && <span className="ml-auto mono" style={{ flex: "none" }}>{h.tool}</span>}
                </div>
                <Snip className={`snip ${h.kind === "command" || h.kind === "file" ? "mono" : ""}`} html={h.snippet} />
              </div>
            ))}
          </div>
          <div className="prev">
            {hit ? (
              <>
                <div className="eyebrow">
                  Context{hit.turn !== undefined ? ` · turn ${hit.turn}` : ""} · {hit.repo || projectName(hit.cwd)}
                </div>
                {!ctx && <div className="muted">loading context…</div>}
                {ctx && docs.length === 0 && <div className="muted">No surrounding turn for this hit.</div>}
                {docs.map(({ d, hit: hl }) => (
                  <div key={String(d.id)} className={`ctxline ${hl ? "hitl" : ""}`}>
                    <b className="xs" style={{ textTransform: "uppercase", letterSpacing: ".05em" }}>{d.kind}</b> {(d.text || "").slice(0, 360)}
                  </div>
                ))}
                <div className="row" style={{ marginTop: "auto", gap: 6, flexWrap: "wrap" }}>
                  <button className="btn pri" onClick={() => open(hit)}>
                    {drivenOf(hit) ? "Open in Workspace" : "Open at turn"} <Kbd>↵</Kbd>
                  </button>
                </div>
                {hit.path && <div className="xs muted mono trunc">{hit.path}</div>}
              </>
            ) : (
              <div className="muted">The selected hit and the turn around it show here.</div>
            )}
          </div>
        </div>
        <div className="palfoot">
          <span><Kbd>↑</Kbd> <Kbd>↓</Kbd> move</span>
          <span><Kbd>↵</Kbd> open</span>
          <span><Kbd>⌘↵</Kbd> ask</span>
          <span><Kbd>tab</Kbd> kind</span>
          <span><Kbd>⇧⌫</Kbd> clear scope</span>
          <span className="ml-auto">
            {status
              ? `${status.fts5 ? "FTS5" : "LIKE fallback"} · ${status.docs.toLocaleString()} docs · ${status.indexedFiles}/${status.totalFiles} files${status.indexing ? " · indexing" : ""}`
              : "index status unavailable"}
          </span>
        </div>
      </div>
    </div>
  );
}
