// Transcript search shared by the thread filter, the search page and the palette.
import { useEffect, useMemo, useState } from "react";
import { Link } from "react-router-dom";
import { fmtAgo, projectName } from "../../../lib/format";
import { isMissing, v3 } from "../../api/client";
import { useOrch } from "../../api/orch";
import type { Chip, Hit, SearchResponse } from "../../api/types";
import { AgentIcon } from "../../components/AgentIcon";
import { useV3 } from "../../live";
import { threadTitle } from "../../model";

const ENT: Record<string, string> = { "&lt;": "<", "&gt;": ">", "&amp;": "&", "&quot;": '"', "&#39;": "'", "&#34;": '"', "&#x27;": "'" };
const decode = (s: string) => s.replace(/&(?:lt|gt|amp|quot|#39|#34|#x27);/g, (m) => ENT[m] || m);

/** Splits a server snippet into text and <mark> parts; every other tag stays literal text. */
export function snipParts(html: string): { t: string; m: boolean }[] {
  const out: { t: string; m: boolean }[] = [];
  let m = false;
  for (const piece of (html || "").split(/(<\/?mark>)/)) {
    if (piece === "<mark>") m = true;
    else if (piece === "</mark>") m = false;
    else if (piece) out.push({ t: decode(piece), m });
  }
  return out;
}

export function Snip({ html, className }: { html: string; className?: string }) {
  return (
    <span className={className}>
      {snipParts(html).map((p, i) => (p.m ? <mark key={i}>{p.t}</mark> : <span key={i}>{p.t}</span>))}
    </span>
  );
}

/** Marked words of the snippets, used to highlight the hit in the thread view. */
export function markedTerms(hits: Hit[], n = 5): string[] {
  const out: string[] = [];
  for (const h of hits)
    for (const p of snipParts(h.snippet))
      if (p.m && p.t.trim().length > 1 && !out.includes(p.t.trim().toLowerCase())) {
        out.push(p.t.trim().toLowerCase());
        if (out.length >= n) return out;
      }
  return out;
}

/** Workspace route of a hit: driven docs by item id, watched docs by turn and session. */
export function hitHref(h: Pick<Hit, "session" | "ts" | "turn" | "span" | "threadId" | "drivenId">, terms: string[] = [], threadOf: Record<string, string> = {}): string {
  const p = new URLSearchParams();
  let target: string;
  if (h.session.startsWith("ot_")) {
    target = h.session;
    if (h.span) p.set("hit", h.span);
  } else if (h.drivenId) target = h.drivenId;
  else {
    target = h.threadId || threadOf[h.session] || h.session;
    if (h.turn != null) p.set("hit", String(h.turn));
    p.set("session", h.session);
  }
  if (h.ts) p.set("ts", String(h.ts));
  if (terms.length) p.set("q", terms.join(" "));
  return `/w/${encodeURIComponent(target)}?${p}`;
}

export interface SessionHits {
  session: string;
  agent: string;
  repo: string;
  cwd: string;
  lastTs: number;
  hits: Hit[];
  target: string;
}

/** One group per session, in rank order of its best hit. */
export function groupHits(hits: Hit[], threadOf: Record<string, string> = {}): SessionHits[] {
  const by = new Map<string, SessionHits>();
  for (const h of hits) {
    let g = by.get(h.session);
    if (!g) {
      const target = h.session.startsWith("ot_") ? h.session : h.drivenId || h.threadId || threadOf[h.session] || h.session;
      g = { session: h.session, agent: h.agent, repo: h.repo, cwd: h.cwd, lastTs: 0, hits: [], target };
      by.set(h.session, g);
    }
    g.hits.push(h);
    g.lastTs = Math.max(g.lastTs, h.ts);
  }
  return [...by.values()];
}

/** Title of a thread target from the watched and driven lists. */
export function useTitleOf(): (g: SessionHits) => string {
  const v = useV3();
  const orch = useOrch();
  const titles = useMemo(() => new Map(v.threads.map((t) => [t.id, threadTitle(t)])), [v.threads]);
  return (g) => titles.get(g.target) || orch.threads.get(g.target)?.title || orch.details.get(g.target)?.thread.title || `${projectName(g.cwd) || g.repo || "Session"} · ${g.session.split(":").pop()?.slice(0, 8)}`;
}

export interface SearchState {
  res: SearchResponse | null;
  loading: boolean;
  err: string;
  missing: boolean;
  q: string;
}

/** Debounced GET /api/search; nothing is sent below min characters. */
export function useTranscriptSearch(q: string, { limit = 100, min = 2, delay = 250 } = {}): SearchState {
  const [st, setSt] = useState<SearchState>({ res: null, loading: false, err: "", missing: false, q: "" });
  useEffect(() => {
    const text = q.trim();
    if (text.length < min) {
      setSt({ res: null, loading: false, err: "", missing: false, q: "" });
      return;
    }
    let alive = true;
    setSt((s) => ({ ...s, loading: true }));
    const t = window.setTimeout(() => {
      v3.search({ q: text, limit }).then(
        (res) => alive && setSt({ res, loading: false, err: "", missing: false, q: text }),
        (e) => alive && setSt({ res: null, loading: false, err: (e as Error).message, missing: isMissing(e), q: text }),
      );
    }, delay);
    return () => {
      alive = false;
      window.clearTimeout(t);
    };
  }, [q, limit, min, delay]);
  return st;
}

const CHIP_KEYS = ["agent", "repo", "cwd", "kind", "tool", "file", "is", "since", "until", "session", "thread"];
const CHIP_RE = new RegExp(`(^|\\s)(${CHIP_KEYS.join("|")}):("[^"]*"|\\S+)`, "gi");

/** Typed chips in the query text, for the instant title filter. */
export function typedChips(q: string): { key: string; value: string }[] {
  return [...q.matchAll(CHIP_RE)].map((m) => ({ key: m[2].toLowerCase(), value: m[3].replace(/"/g, "").toLowerCase() }));
}
export const withoutChips = (q: string) => q.replace(CHIP_RE, " ").replace(/\s+/g, " ").trim();

/** Sets key:value in the query, replacing any earlier value of that key; an empty value removes it. */
export function setChip(q: string, key: string, value: string): string {
  const re = new RegExp(`(^|\\s)${key}:("[^"]*"|\\S+)`, "gi");
  const rest = q.replace(re, " ").replace(/\s+/g, " ").trim();
  if (!value) return rest;
  const v = /\s/.test(value) ? `"${value}"` : value;
  return `${rest} ${key}:${v}`.trim();
}

export const CHIP_LABEL: Record<string, string> = { agent: "agent", repo: "repo", cwd: "folder", kind: "kind", tool: "tool", file: "file", is: "is", since: "since", until: "until", session: "session", thread: "thread" };

export function ChipRow({ chips, onRemove }: { chips: Chip[]; onRemove?: (c: Chip) => void }) {
  if (!chips.length) return null;
  return (
    <div className="wchips" aria-label="Query filters">
      {chips.map((c) => (
        <span key={`${c.key}:${c.value}`} className={`wqchip ${c.source}`} title={c.source === "inferred" ? "Read from your words" : "Typed filter"}>
          <span className="k">{CHIP_LABEL[c.key] || c.key}</span>
          {c.value}
          {onRemove && (
            <button onClick={() => onRemove(c)} aria-label={`Remove ${c.key} ${c.value}`}>
              ×
            </button>
          )}
        </span>
      ))}
    </div>
  );
}

/** One session row: agent, title, repo, age, hit count and the best snippet. */
export function SessionHitRow({ g, title, href, compact, onClick }: { g: SessionHits; title: string; href: string; compact?: boolean; onClick?: () => void }) {
  const n = g.hits.length;
  return (
    <Link to={href} className={`wth wshit ${compact ? "compact" : ""}`} onClick={onClick} title={title}>
      <AgentIcon agent={g.agent} size={12} />
      <span className="ti">{title}</span>
      <span className="t">{fmtAgo(g.lastTs, "")}</span>
      <span className="r2">
        <span className="trunc">{g.repo || projectName(g.cwd) || "no project"}</span>
        <span className="nt">
          {n} {n === 1 ? "hit" : "hits"}
        </span>
      </span>
      <Snip className={`wsnip ${g.hits[0].kind === "command" || g.hits[0].kind === "file" ? "mono" : ""}`} html={g.hits[0].snippet} />
    </Link>
  );
}
