// Typed clients for the v3 endpoints. A 404 means "endpoint not built yet" and is surfaced as `missing`.
import { useCallback, useEffect, useRef, useState } from "react";
import type {
  ApprovalSettings,
  Approval,
  ApprovalsResponse,
  AskResponse,
  ChildrenResponse,
  ContextResponse,
  DecideBody,
  ErrorsResponse,
  GraphResponse,
  HooksSnippet,
  IntelResponse,
  LatencyResponse,
  MonitorsResponse,
  NowResponse,
  Rule,
  RuleSuggestion,
  SearchResponse,
  SearchStatus,
  SpansResponse,
  TimelineResponse,
  V3Health,
  V3ThreadsResponse,
  XTraceResponse,
} from "./types";
import type { UsageLimits } from "./usage";

export class ApiError extends Error {
  status: number;
  body: unknown;
  constructor(status: number, msg: string, body?: unknown) {
    super(msg);
    this.status = status;
    this.body = body;
  }
}
export const isMissing = (e: unknown) => e instanceof ApiError && (e.status === 404 || e.status === 405 || e.status === 501);

async function req<T>(method: string, url: string, body?: unknown): Promise<T> {
  let r: Response;
  try {
    const json = body !== undefined ? JSON.stringify(body) : undefined;
    r = await fetch(url, {
      method,
      cache: "no-store",
      headers: body !== undefined ? { "Content-Type": "application/json" } : undefined,
      body: json,
      // A decision sent as the page closes must still arrive; keepalive allows bodies up to 64 KB.
      keepalive: method !== "GET" && (json?.length ?? 0) < 60_000,
    });
  } catch (e) {
    throw new ApiError(0, `daemon unreachable (${(e as Error).message})`);
  }
  const text = await r.text();
  let j: unknown = undefined;
  try {
    j = text ? JSON.parse(text) : undefined;
  } catch {
    j = undefined;
  }
  if (!r.ok) {
    const msg = (j as { error?: string } | undefined)?.error || text.slice(0, 200) || `HTTP ${r.status}`;
    throw new ApiError(r.status, msg, j);
  }
  // The SPA fallback answers unknown GETs with index.html; treat that as a missing endpoint.
  if (j === undefined && text.trimStart().startsWith("<")) throw new ApiError(404, "endpoint not available");
  return (j ?? {}) as T;
}
const get = <T,>(url: string) => req<T>("GET", url);
const enc = encodeURIComponent;
const qs = (o: Record<string, string | number | undefined | null | boolean>) => {
  const p = new URLSearchParams();
  for (const [k, v] of Object.entries(o)) if (v !== undefined && v !== null && v !== "" && v !== false) p.set(k, String(v));
  const s = p.toString();
  return s ? "?" + s : "";
};

// Under ?mock=1 the dev mock answers decisions so nothing is posted to the daemon.
let decideHook: ((id: string, b: DecideBody) => Promise<Approval>) | null = null;
export const setDecideHook = (h: typeof decideHook) => (decideHook = h);

export const v3 = {
  approvals: () => get<ApprovalsResponse>("/api/approvals"),
  decide: (id: string, b: DecideBody) => (decideHook ? decideHook(id, b) : req<Approval>("POST", `/api/approvals/${enc(id)}/decide`, { surface: "web", ...b })),
  rules: () => get<{ rules: Rule[] } | Rule[]>("/api/approvals/rules"),
  saveRule: (r: Partial<Rule>) => req<Rule>("POST", "/api/approvals/rules", r),
  deleteRule: (id: string) => req<unknown>("DELETE", `/api/approvals/rules/${enc(id)}`),
  ruleSuggestions: () => get<{ suggestions: RuleSuggestion[] }>("/api/approvals/rules/suggest"),
  dismissSuggestion: (key: string) => req<unknown>("POST", `/api/approvals/rules/suggest/${enc(key)}/dismiss`),
  audit: (q: { since?: number; limit?: number } = {}) => get<{ approvals?: Approval[]; audit?: Approval[] } | Approval[]>(`/api/approvals/audit${qs(q)}`),
  approvalSettings: () => get<ApprovalSettings>("/api/approvals/settings"),
  saveApprovalSettings: (s: ApprovalSettings) => req<ApprovalSettings>("POST", "/api/approvals/settings", s),
  hooksSnippet: () => get<HooksSnippet>("/api/approvals/hooks-snippet"),

  searchStatus: () => get<SearchStatus>("/api/search/status"),
  search: (q: { q: string; scope?: string; kind?: string; limit?: number; offset?: number }) => get<SearchResponse>(`/api/search${qs(q)}`),
  context: (id: string | number, n = 2) => get<ContextResponse>(`/api/search/context${qs({ id: String(id), n })}`),
  ask: (q: string) => req<AskResponse>("POST", "/api/search/ask", { q }),
  spans: (q: { q?: string; groupBy?: string; minDur?: number; limit?: number; since?: string; repo?: string; agent?: string }) =>
    get<SpansResponse>(`/api/spans${qs(q)}`),
  latency: (q: { since?: string; repo?: string; agent?: string } = {}) => get<LatencyResponse>(`/api/tools/latency${qs(q)}`),
  errors: (q: { since?: string; repo?: string; agent?: string } = {}) => get<ErrorsResponse>(`/api/errors${qs(q)}`),
  graph: (q: { since?: string; repo?: string; agent?: string } = {}) => get<GraphResponse>(`/api/graph${qs(q)}`),

  threads: (all = false) => get<V3ThreadsResponse>(`/api/v3/threads${all ? "?all=1" : ""}`),
  children: (threadId: string) => get<ChildrenResponse>(`/api/v3/thread/${enc(threadId)}/children`),
  xtrace: (sessionId: string, turn: number | string) => get<XTraceResponse>(`/api/v3/trace/${enc(sessionId)}${qs({ turn })}`),
  timeline: (day: string, stacks = false) => get<TimelineResponse>(`/api/v3/timeline${qs({ day, stacks: stacks ? "1" : undefined })}`),
  now: () => get<NowResponse>("/api/v3/now"),
  monitors: () => get<MonitorsResponse>("/api/v3/monitors"),
  intel: (range: string) => get<IntelResponse>(`/api/v3/intel${qs({ range })}`),
  health: () => get<V3Health>("/api/v3/health"),
  usageLimits: () => get<UsageLimits>("/api/usage/limits"),
};

export interface ApiState<T> {
  data: T | null;
  error: string | null;
  missing: boolean;
  loading: boolean;
  reload: () => void;
  setData: (d: T | null) => void;
}

/** Runs `fn` when deps change (and every `poll` ms), keeping the last good data across reloads. */
export function useApi<T>(fn: () => Promise<T>, deps: unknown[], opts: { poll?: number; skip?: boolean } = {}): ApiState<T> {
  const [data, setData] = useState<T | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [missing, setMissing] = useState(false);
  const [loading, setLoading] = useState(!opts.skip);
  const seq = useRef(0);
  const fnRef = useRef(fn);
  fnRef.current = fn;

  const run = useCallback(() => {
    const my = ++seq.current;
    setLoading(true);
    fnRef
      .current()
      .then((d) => {
        if (my !== seq.current) return;
        setData(d);
        setError(null);
        setMissing(false);
      })
      .catch((e: unknown) => {
        if (my !== seq.current) return;
        if (isMissing(e)) setMissing(true);
        else setError((e as Error).message || String(e));
      })
      .finally(() => my === seq.current && setLoading(false));
  }, []);

  useEffect(() => {
    if (opts.skip) {
      setLoading(false);
      return;
    }
    setData(null);
    setMissing(false);
    setError(null);
    run();
    if (!opts.poll) return;
    const t = window.setInterval(() => {
      if (!document.hidden) run();
    }, opts.poll);
    return () => window.clearInterval(t);
  }, deps); // eslint-disable-line react-hooks/exhaustive-deps

  return { data, error, missing, loading, reload: run, setData };
}
