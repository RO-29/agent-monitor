import { useEffect, useMemo, useRef, useState } from "react";
import { useNavigate, useSearchParams } from "react-router-dom";
import { api } from "../../api/client";
import type { Session, Thread } from "../../api/types";
import { TOOLS, agentTag, fmtAgo, fmtTok, fmtUsd, isAttention, isLive, modelShort, projectName, shortCwd, stateRank, titleFor, toolName } from "../../lib/format";
import { Icon, ToolLogo } from "../../lib/icons";
import { onLiveEvent, useLive } from "../../lib/ws";
import { SegStrip, StatePill } from "../../app/ui";
import { useSearch } from "../../app/search";
import AttentionPanel from "../../components/attention/AttentionPanel";

type StateFilter = "all" | "live" | "attention" | "completed";
type Sort = "activity" | "cost" | "errors";

function useThreads() {
  const [data, setData] = useState<{ threads: Thread[]; threadOf: Record<string, string> } | null>(null);
  const [err, setErr] = useState("");
  useEffect(() => {
    let alive = true;
    let debounce: number | undefined;
    const load = () => api.threads().then((r) => alive && (setData(r), setErr(""))).catch((e) => alive && setErr(String(e.message || e)));
    load();
    const t = window.setInterval(load, 8000);
    const off = onLiveEvent((e) => {
      if (e.kind === "upsert" || e.kind === "segment" || e.kind === "remove") {
        window.clearTimeout(debounce);
        debounce = window.setTimeout(load, 1500);
      }
    });
    return () => {
      alive = false;
      window.clearInterval(t);
      window.clearTimeout(debounce);
      off();
    };
  }, []);
  return { data, err };
}

function matchState(t: { state: Session["state"]; attention?: boolean }, f: StateFilter) {
  if (f === "all") return true;
  if (f === "live") return isLive(t.state);
  if (f === "attention") return t.attention || isAttention(t.state);
  return t.state === "completed" || t.state === "abandoned";
}

export default function ThreadsPage() {
  const live = useLive();
  const nav = useNavigate();
  const [sp, setSp] = useSearchParams();
  const search = useSearch().trim().toLowerCase();
  const { data, err } = useThreads();
  const view = sp.get("view") || "threads";
  const tool = sp.get("tool") || "all";
  const stateF = (sp.get("state") as StateFilter) || (view === "live" ? "live" : view === "attention" ? "attention" : "all");
  const project = sp.get("project") || "all";
  const sort = (sp.get("sort") as Sort) || "activity";
  const mode = sp.get("mode") === "flat" || view === "live" ? "flat" : "threads";
  const [sideOpen, setSideOpen] = useState(false);
  const set = (k: string, v: string) => {
    const n = new URLSearchParams(sp);
    if (!v || v === "all") n.delete(k);
    else n.set(k, v);
    setSp(n, { replace: true });
  };

  const sessions = useMemo(() => [...live.sessions.values()], [live.version]); // eslint-disable-line react-hooks/exhaustive-deps
  const projects = useMemo(() => {
    const m = new Map<string, number>();
    sessions.forEach((s) => m.set(s.cwd, (m.get(s.cwd) || 0) + 1));
    return [...m.entries()].sort((a, b) => b[1] - a[1]).slice(0, 40);
  }, [sessions]);

  const threads = useMemo(() => {
    if (!data) return [];
    let list = data.threads.filter((t) => (tool === "all" || t.tools.includes(tool as Session["tool"])) && matchState(t, stateF) && (project === "all" || t.cwd === project));
    if (search) list = list.filter((t) => `${t.title} ${t.cwd} ${t.lastPoint || ""} ${t.lastOutcome || ""}`.toLowerCase().includes(search));
    if (sort === "cost") list = [...list].sort((a, b) => b.costUsd - a.costUsd);
    else if (sort === "errors") list = [...list].sort((a, b) => b.errors - a.errors);
    return list;
  }, [data, tool, stateF, project, search, sort]);

  const flat = useMemo(() => {
    let list = sessions.filter((s) => (tool === "all" || s.tool === tool) && matchState({ state: s.state }, stateF) && (project === "all" || s.cwd === project));
    if (search) list = list.filter((s) => `${titleFor(s)} ${s.cwd} ${s.lastMessage || ""} ${s.firstMessage || ""}`.toLowerCase().includes(search));
    return list.sort((a, b) => stateRank(a.state) - stateRank(b.state) || b.lastActivityAt - a.lastActivityAt);
  }, [sessions, tool, stateF, project, search]);

  // Projects view: group threads by cwd.
  const grouped = useMemo(() => {
    if (view !== "projects") return null;
    const m = new Map<string, Thread[]>();
    threads.forEach((t) => m.set(t.cwd, [...(m.get(t.cwd) || []), t]));
    return [...m.entries()].sort((a, b) => Math.max(...b[1].map((t) => t.lastAt)) - Math.max(...a[1].map((t) => t.lastAt)));
  }, [threads, view]);

  const chip = (label: string, on: boolean, onClick: () => void) => (
    <button key={label} className={`chip ${on ? "on" : ""}`} onClick={onClick}>
      {label}
    </button>
  );

  return (
    <div style={{ flex: 1, display: "flex", minHeight: 0 }}>
      <div style={{ flex: 1, minWidth: 0, display: "flex", flexDirection: "column", minHeight: 0 }}>
        <div className="pagehead" style={{ gap: 8 }}>
          <div className="row" style={{ gap: 10, flexWrap: "wrap" }}>
            <h1 style={{ flex: "none" }}>{view === "live" ? "Live sessions" : view === "attention" ? "Needs attention" : view === "projects" ? "Projects" : view === "learnings" ? "Learnings" : "Threads"}</h1>
            <span className="num muted">{mode === "flat" ? flat.length : threads.length}</span>
            <div style={{ flex: 1 }} />
            <select className="sel" value={tool} onChange={(e) => set("tool", e.target.value)}>
              <option value="all">agent · all</option>
              {TOOLS.map((t) => (
                <option key={t} value={t}>{toolName(t)}</option>
              ))}
            </select>
            <select className="sel" value={stateF} onChange={(e) => set("state", e.target.value)}>
              <option value="all">state · all</option>
              <option value="live">live</option>
              <option value="attention">attention</option>
              <option value="completed">completed</option>
            </select>
            <select className="sel" value={project} onChange={(e) => set("project", e.target.value)} style={{ maxWidth: 200 }}>
              <option value="all">project · all</option>
              {projects.map(([cwd, n]) => (
                <option key={cwd} value={cwd}>{projectName(cwd)} · {n}</option>
              ))}
            </select>
            <select className="sel" value={sort} onChange={(e) => set("sort", e.target.value)}>
              <option value="activity">sort · last activity</option>
              <option value="cost">sort · cost</option>
              <option value="errors">sort · errors</option>
            </select>
            {chip("group by thread", mode === "threads", () => set("mode", ""))}
            {chip("flat sessions", mode === "flat", () => set("mode", "flat"))}
            <button className="btn sm mobile-only" onClick={() => setSideOpen((o) => !o)}>
              <Icon name="alert" size={12} /> attention
            </button>
          </div>
        </div>
        <div style={{ flex: 1, overflowY: "auto", minHeight: 0 }}>
          {view === "learnings" && <div className="empty">Pick a thread and open its ledger. Learnings are collected per thread.</div>}
          {view !== "learnings" && err && <div className="empty">threads unavailable: {err}</div>}
          {view !== "learnings" && mode === "threads" && !grouped && (
            <>
              <div className="tgrid head k">
                <span>thread</span>
                <span>segments</span>
                <span>tokens · usd · turns · errors</span>
                <span style={{ textAlign: "right" }}>age</span>
              </div>
              {threads.map((t) => <ThreadRow key={t.id} t={t} sessions={live.sessions} onOpen={(p) => nav(p)} />)}
              {data && threads.length === 0 && <div className="empty">No threads match. Clear the filters or switch to flat sessions.</div>}
            </>
          )}
          {view !== "learnings" && mode === "threads" && grouped && grouped.map(([cwd, ts]) => (
            <div key={cwd}>
              <div className="grouphead row" style={{ gap: 8 }}>
                <Icon name="folder" size={13} color="var(--muted)" />
                <span style={{ fontWeight: 600 }}>{projectName(cwd)}</span>
                <span className="mono muted" style={{ fontSize: 11 }}>{shortCwd(cwd)}</span>
                <span className="num muted" style={{ fontSize: 11 }}>· {ts.length} thread{ts.length === 1 ? "" : "s"}</span>
              </div>
              {ts.map((t) => <ThreadRow key={t.id} t={t} sessions={live.sessions} onOpen={(p) => nav(p)} />)}
            </div>
          ))}
          {view !== "learnings" && mode === "flat" && <FlatSessions list={flat} threadOf={data?.threadOf || {}} />}
        </div>
      </div>
      <AttentionPanel open={sideOpen} />
    </div>
  );
}

function ThreadRow({ t, sessions, onOpen }: { t: Thread; sessions: Map<string, Session>; onOpen: (p: string) => void }) {
  const newest = t.sessions[t.sessions.length - 1];
  const s = sessions.get(newest);
  const line = t.lastOutcome || t.lastPoint || s?.lastMessage || "";
  return (
    <div className={`tgrid ${t.attention ? "attn" : ""}`} onClick={() => onOpen(`/session/${encodeURIComponent(newest)}`)}>
      <div style={{ minWidth: 0, display: "flex", flexDirection: "column", gap: 5 }}>
        <div className="row" style={{ gap: 8, minWidth: 0 }}>
          {t.tools.map((tl) => <ToolLogo key={tl} tool={tl} size={14} />)}
          <span className="ell" style={{ fontWeight: 600 }}>{t.title || "Untitled"}</span>
          <StatePill state={t.state} />
          {t.attention && (
            <span className="pill" style={{ color: "var(--red)", background: "rgba(248,113,113,.14)" }}>
              <Icon name="alert" size={10} color="var(--red)" sw={2.4} /> permission
            </span>
          )}
          {t.sessions.length > 1 && <span className="chip" style={{ height: 18, fontSize: 10.5 }}>{t.sessions.length} sessions</span>}
        </div>
        <div className="row" style={{ gap: 8, minWidth: 0, whiteSpace: "nowrap" }}>
          <span className="mono muted" style={{ fontSize: 11, flex: "none" }}>{projectName(t.cwd)}{t.ref ? ` · ${t.ref}` : ""}</span>
          <span className="mono muted" style={{ fontSize: 11, flex: "none" }}>· {modelShort(t.model) || "—"}</span>
          {line && <span className="muted ell" style={{ fontSize: 11.5 }}>· {line}</span>}
          <span style={{ flex: 1 }} />
          <span className="rowchips row" style={{ gap: 6 }}>
            <button className="chip" style={{ height: 18, fontSize: 10.5 }} onClick={(e) => { e.stopPropagation(); onOpen(`/thread/${encodeURIComponent(t.id)}`); }}>
              <Icon name="book" size={10} /> story
            </button>
            <button className="chip" style={{ height: 18, fontSize: 10.5 }} onClick={(e) => { e.stopPropagation(); onOpen(`/thread/${encodeURIComponent(t.id)}/learnings`); }}>
              <Icon name="spark" size={10} /> {t.learnings} learnings
            </button>
          </span>
        </div>
      </div>
      <div style={{ display: "flex", flexDirection: "column", gap: 5 }}>
        <SegStrip weights={t.segWeights || []} kinds={t.segKinds || []} height={10} />
        <div className="num muted" style={{ fontSize: 10.5, whiteSpace: "nowrap" }}>
          {t.segments} seg · {t.clears} clear · {t.compacts} compact
        </div>
      </div>
      <div className="stats col-stats">
        <span><span className="lab">tok</span>{fmtTok(t.tokens.input + t.tokens.output + t.tokens.cacheRead + t.tokens.cacheCreate)}</span>
        <span><span className="lab">usd</span>{fmtUsd(t.costUsd, t.costEstimated)}</span>
        <span><span className="lab">turns</span>{t.turns.toLocaleString()}</span>
        <span style={{ color: t.errors ? "var(--red)" : undefined }}><span className="lab">err</span>{t.errors}</span>
      </div>
      <div className="num muted col-age" style={{ fontSize: 11, textAlign: "right" }}>{fmtAgo(t.lastAt, "")}</div>
    </div>
  );
}

function FlatSessions({ list, threadOf }: { list: Session[]; threadOf: Record<string, string> }) {
  const nav = useNavigate();
  const groups = TOOLS.map((t) => [t, list.filter((s) => s.tool === t)] as const).filter(([, g]) => g.length);
  const seen = useRef(0);
  seen.current++;
  if (!list.length) return <div className="empty">No sessions match. Clear the search or the filters.</div>;
  return (
    <>
      {groups.map(([t, g]) => (
        <div key={t}>
          <div className="grouphead row" style={{ gap: 8 }}>
            <ToolLogo tool={t} size={13} />
            <span className="k" style={{ color: "var(--text-2)" }}>{toolName(t)}</span>
            <span className="num muted" style={{ fontSize: 11 }}>{g.length}</span>
          </div>
          {g.map((s) => (
            <div key={s.id} className={`srow ${s.state === "awaiting-permission" ? "attn" : ""}`} onClick={() => nav(`/session/${encodeURIComponent(s.id)}`)}>
              <div className="row" style={{ gap: 8, minWidth: 0 }}>
                <ToolLogo tool={s.tool} size={14} />
                <span className="ell" style={{ fontWeight: 600, flex: 1 }}>{titleFor(s) || <i className="muted">Untitled session</i>}</span>
                <span className="num muted" style={{ fontSize: 11 }}>{fmtAgo(s.lastActivityAt, "")}</span>
              </div>
              <div className="row" style={{ gap: 8, minWidth: 0, whiteSpace: "nowrap" }}>
                <span className="mono" style={{ fontSize: 11, color: `var(--tool-${s.tool})` }}>{toolName(s.tool)}{s.model ? ` · ${modelShort(s.model)}` : ""}</span>
                <span className="mono muted" style={{ fontSize: 11 }}>#{agentTag(s)}</span>
                <span className="mono muted ell" style={{ fontSize: 11 }} title={s.cwd}>{projectName(s.cwd)}</span>
                {threadOf[s.id] && threadOf[s.id] !== s.id && <span className="chip" style={{ height: 16, fontSize: 10 }}><Icon name="link" size={9} /> thread</span>}
                <span style={{ flex: 1 }} />
                <StatePill state={s.state} />
              </div>
              {s.state === "awaiting-permission" && s.permissionMessage && (
                <div className="mono ell" style={{ fontSize: 11, color: "var(--red)" }}>{s.permissionMessage}</div>
              )}
            </div>
          ))}
        </div>
      ))}
    </>
  );
}
