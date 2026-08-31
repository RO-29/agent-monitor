import { useEffect, useMemo, useRef, useState } from "react";
import { useSearchParams } from "react-router-dom";
import { api } from "../../api/client";
import type { DetailMsg, DetailTool, Session, SessionDetail } from "../../api/types";
import { fmtBytes, fmtTime, isLive, modelShort } from "../../lib/format";
import { Icon } from "../../lib/icons";

// Ported from the legacy grouped transcript: turns = prose message + the tool
// calls issued before the next message; flat one-row-per-event mode once any
// facet, tool filter, or query narrows the view.
const RUN_CAP = 7;
const CAP = 600;
const STUB_RE = /^\s*→\s*[\w.\-]+\s*$/;
const ERR_RE = /(^|\n)\s*(error[:\s]|traceback \(most recent|fatal:|command failed|no such file|permission denied|exit code [1-9]|npm err!|error!|cannot find|is not recognized|segmentation fault|panic:)/i;
type Facet = "all" | "prose" | "tools" | "errors" | "user";

const TOOL_COLORS: Record<string, string> = {
  Bash: "var(--fam-bash)", Read: "var(--fam-read)", Grep: "var(--fam-read)", Glob: "var(--fam-read)", ToolSearch: "var(--fam-read)",
  Edit: "var(--fam-edit)", Write: "var(--fam-edit)", MultiEdit: "var(--fam-edit)", NotebookEdit: "var(--fam-edit)",
  Task: "var(--fam-agent)", Agent: "var(--fam-agent)", SendMessage: "var(--fam-agent)", AskUserQuestion: "var(--fam-agent)",
  WebFetch: "var(--fam-web)", WebSearch: "var(--fam-web)", TaskCreate: "var(--muted)", TaskUpdate: "var(--muted)",
};
const toolColor = (n: string) => TOOL_COLORS[n] || (n.startsWith("mcp__") ? "var(--fam-mcp)" : "var(--fam-other)");

function toolPreview(t: DetailTool): string {
  let a: Record<string, unknown> = {};
  try {
    a = JSON.parse(t.args || "{}");
  } catch {
    return (t.args || "").slice(0, 140);
  }
  const s = (k: string) => (typeof a[k] === "string" ? (a[k] as string) : "");
  switch (t.name) {
    case "Bash": return s("command");
    case "Edit": case "Write": case "MultiEdit": case "NotebookEdit": return s("file_path");
    case "Read": return s("file_path") + (a.offset ? ` @${a.offset}` : "");
    case "Grep": return `"${s("pattern")}"${s("path") ? " in " + s("path") : ""}`;
    case "Glob": return s("pattern");
    case "ToolSearch": return s("query");
    case "WebFetch": return s("url");
    case "WebSearch": return s("query");
    case "Task": case "Agent": return `[${s("subagent_type") || "general"}] ${s("description")}`;
    case "TaskCreate": return s("subject");
    case "TaskUpdate": return `task #${a.taskId} → ${s("status")}`;
  }
  if (Object.keys(a).length === 0) return "(no input)";
  const j = JSON.stringify(a);
  return j.length > 140 ? j.slice(0, 140) + "…" : j;
}
const isErrTool = (t: DetailTool) => ERR_RE.test((t.result || "").slice(0, 400));
const isStub = (m: DetailMsg) => m.role === "assistant" && (!m.text || m.text === "(no text)" || STUB_RE.test(m.text));

// Slash commands are recorded as XML-ish user turns. Show "/clear" as a
// compact marker; drop pure scaffolding (caveats, reminders, notifications).
const META_SKIP = ["<local-command-caveat>", "<local-command-stdout>", "<system-reminder>", "<task-notification>", "<user-prompt-submit-hook>", "<bash-"];
function userMeta(m: DetailMsg): { skip: boolean; command?: string } {
  if (m.role !== "user") return { skip: false };
  const t = m.text.trim();
  if (t.startsWith("<command-name>")) {
    const name = /<command-name>([^<]*)<\/command-name>/.exec(t)?.[1] || "";
    const args = /<command-args>([^<]*)<\/command-args>/.exec(t)?.[1] || "";
    return { skip: false, command: `${name} ${args}`.trim() };
  }
  if (META_SKIP.some((p) => t.startsWith(p))) return { skip: true };
  return { skip: false };
}

interface Turn { key: string; ts: number; role: string; text: string; model?: string; tools: DetailTool[] }

function buildTurns(d: SessionDetail) {
  const msgs = d.messages.filter((m) => !isStub(m) && !userMeta(m).skip).sort((a, b) => a.ts - b.ts);
  const turns: Turn[] = msgs.map((m) => {
    const meta = userMeta(m);
    return { key: `msg:${m.ts}:${m.role}`, ts: m.ts, role: meta.command ? "command" : m.role, text: meta.command || m.text, model: m.model, tools: [] };
  });
  const orphans: DetailTool[] = [];
  const subTs = new Set(d.subagents.map((s) => s.ts));
  const tools = [...d.toolCalls].sort((a, b) => a.ts - b.ts);
  let ptr = -1;
  for (const t of tools) {
    if (t.isSubagent && subTs.has(t.ts)) continue;
    while (ptr + 1 < turns.length && turns[ptr + 1].ts <= t.ts) ptr++;
    // attach to the nearest preceding assistant turn
    let k = ptr;
    while (k >= 0 && turns[k].role !== "assistant") k--;
    if (k >= 0) turns[k].tools.push(t);
    else orphans.push(t);
  }
  if (orphans.length) turns.unshift({ key: "orphans", ts: orphans[0].ts, role: "setup", text: "", tools: orphans });
  const counts = { all: 0, prose: 0, tools: tools.length, errors: tools.filter(isErrTool).length, user: msgs.filter((m) => m.role === "user").length };
  counts.prose = msgs.filter((m) => m.role === "assistant").length;
  counts.all = msgs.length + tools.length;
  const toolCounts: Record<string, number> = {};
  tools.forEach((t) => (toolCounts[t.name] = (toolCounts[t.name] || 0) + 1));
  return { turns, msgs, tools, counts, toolCounts };
}

export default function Transcript({ session }: { session: Session }) {
  const [sp, setSp] = useSearchParams();
  const [detail, setDetail] = useState<SessionDetail | null>(null);
  const [err, setErr] = useState("");
  const [expanded, setExpanded] = useState<Set<string>>(new Set());
  const [runs, setRuns] = useState<Set<string>>(new Set());
  const [order, setOrder] = useState<"auto" | "asc" | "desc">("auto");
  const facet = ((sp.get("f") as Facet) || "all") as Facet;
  const toolF = sp.get("tool") || "all";
  const [q, setQ] = useState(sp.get("q") || "");
  const [qq, setQq] = useState(q);
  const scroller = useRef<HTMLDivElement>(null);

  useEffect(() => {
    const t = window.setTimeout(() => {
      setQq(q);
      const n = new URLSearchParams(sp);
      if (q) n.set("q", q);
      else n.delete("q");
      setSp(n, { replace: true });
    }, 180);
    return () => window.clearTimeout(t);
  }, [q]); // eslint-disable-line react-hooks/exhaustive-deps

  // Re-fetch when the message count changes (WS upsert bumps session.messageCount).
  useEffect(() => {
    let alive = true;
    const top = scroller.current?.scrollTop || 0;
    api
      .sessionDetail(session.id)
      .then((d) => {
        if (!alive) return;
        setDetail(d);
        requestAnimationFrame(() => {
          if (scroller.current) scroller.current.scrollTop = top;
        });
      })
      .catch((e) => alive && setErr(String(e.message || e)));
    return () => {
      alive = false;
    };
  }, [session.id, session.messageCount]);

  const S = useMemo(() => (detail ? buildTurns(detail) : null), [detail]);
  const ord = order === "auto" ? (isLive(session.state) ? "desc" : "asc") : order;
  const flatMode = facet !== "all" || toolF !== "all" || qq.trim() !== "";
  const setFacet = (f: Facet) => {
    const n = new URLSearchParams(sp);
    if (f === "all") n.delete("f");
    else n.set("f", f);
    if (f === "errors") n.delete("tool");
    setSp(n, { replace: true });
  };
  const setTool = (t: string) => {
    const n = new URLSearchParams(sp);
    if (t === "all") n.delete("tool");
    else n.set("tool", t);
    setSp(n, { replace: true });
  };
  const toggle = (k: string) =>
    setExpanded((s) => {
      const n = new Set(s);
      if (n.has(k)) n.delete(k);
      else n.add(k);
      return n;
    });

  if (err) return <div className="empty">transcript unavailable: {err}</div>;
  if (!S) return <div className="empty">loading transcript…</div>;

  const facetBtn = (f: Facet, label: string, n: number, cls = "") => (
    <button key={f} className={`chip ${facet === f ? "on" : ""} ${cls}`} onClick={() => setFacet(f)}>
      {label} <span className="num muted">{n}</span>
    </button>
  );

  const evRow = (t: DetailTool, flat = false) => {
    const k = `tool:${t.id}`;
    const open = expanded.has(k);
    const e = isErrTool(t);
    return (
      <div key={k}>
        <div className="evrow" onClick={() => toggle(k)}>
          {flat && <span className="tm">{fmtTime(t.ts)}</span>}
          <span className="sq" style={{ background: toolColor(t.name), boxShadow: e ? "inset 0 0 0 1.5px var(--red)" : undefined }} />
          <span className="nm" style={{ color: toolColor(t.name) }}>{t.name}</span>
          <span className="prev">{toolPreview(t)}</span>
          {e && <span className="src err">error</span>}
          {t.isSubagent && <span className="src" style={{ background: "rgba(192,132,252,.16)", color: "var(--magenta)" }}>subagent</span>}
          <span className="num muted" style={{ fontSize: 10.5 }}>{t.result ? fmtBytes(t.result.length) : "…"}</span>
          <Icon name={open ? "chevd" : "chev"} size={11} color="var(--muted)" />
        </div>
        {open && (
          <div className="evbody">
            <div className="k">input</div>
            <pre className="code">{prettyJSON(t.args)}</pre>
            <div className="k">output · {t.result?.length || 0} chars</div>
            <pre className="code">{t.result || "(no result yet)"}</pre>
          </div>
        )}
      </div>
    );
  };

  let body;
  if (!flatMode) {
    const turns = ord === "desc" ? [...S.turns].reverse() : S.turns;
    body = turns.map((turn) => {
      const long = turn.text.length > 320 || turn.text.split("\n").length > 6;
      const open = expanded.has(turn.key);
      const runOpen = runs.has(turn.key);
      const shown = turn.tools.length > RUN_CAP && !runOpen ? turn.tools.slice(0, RUN_CAP) : turn.tools;
      if (turn.role === "command") {
        return (
          <div key={turn.key} className="turn command" style={{ padding: "6px 16px" }}>
            <div className="turn-head">
              <span className="chip mono" style={{ height: 20, fontSize: 11, color: "var(--blue)" }}>
                <Icon name="clear" size={11} color="var(--blue)" /> {turn.text}
              </span>
              <span className="num muted">{fmtTime(turn.ts)}</span>
              <span className="muted" style={{ fontSize: 11 }}>slash command</span>
            </div>
          </div>
        );
      }
      return (
        <div key={turn.key} className={`turn ${turn.role}`}>
          <div className="turn-head">
            <span className="role">{turn.role === "user" ? "you" : turn.role === "assistant" ? "assistant" : "setup"}</span>
            <span className="num muted">{turn.key === "orphans" ? "before first reply" : fmtTime(turn.ts)}</span>
            {turn.model && <span className="chip mono" style={{ height: 18, fontSize: 10.5 }}>{modelShort(turn.model)}</span>}
            {turn.tools.length > 0 && <span className="num muted">{turn.tools.length} tool{turn.tools.length === 1 ? "" : "s"}</span>}
          </div>
          {turn.text.trim() && (
            <div className={`turn-text ${long && !open ? "clamp" : ""}`} onClick={() => long && toggle(turn.key)} title={long && !open ? "click to expand" : undefined}>
              {turn.text}
            </div>
          )}
          {shown.length > 0 && <div style={{ marginTop: 6 }}>{shown.map((t) => evRow(t))}</div>}
          {turn.tools.length > RUN_CAP && !runOpen && (
            <div className="more" onClick={() => setRuns((s) => new Set(s).add(turn.key))}>
              + {turn.tools.length - RUN_CAP} more tool calls
            </div>
          )}
        </div>
      );
    });
    if (!turns.length) body = <div className="empty">No activity recorded for this session.</div>;
  } else {
    const wantMsg = facet === "all" || facet === "prose" || facet === "user";
    const wantTool = facet === "all" || facet === "tools" || facet === "errors";
    let rows: { ts: number; m?: DetailMsg; t?: DetailTool }[] = [];
    if (wantMsg && toolF === "all") S.msgs.forEach((m) => {
      if (facet === "prose" && m.role !== "assistant") return;
      if (facet === "user" && m.role !== "user") return;
      rows.push({ ts: m.ts, m });
    });
    if (wantTool) S.tools.forEach((t) => {
      if (facet === "errors" && !isErrTool(t)) return;
      if (toolF !== "all" && t.name !== toolF) return;
      rows.push({ ts: t.ts, t });
    });
    const needle = qq.trim().toLowerCase();
    if (needle) rows = rows.filter((r) => (r.m ? r.m.text : `${r.t!.name} ${r.t!.args} ${r.t!.result}`).toLowerCase().includes(needle));
    rows.sort((a, b) => a.ts - b.ts);
    if (ord === "desc") rows.reverse();
    const total = rows.length;
    body = (
      <div style={{ padding: "6px 10px" }}>
        {rows.slice(0, CAP).map((r) =>
          r.t ? evRow(r.t, true) : (
            <div key={`m:${r.m!.ts}`} className="evrow" onClick={() => toggle(`msg:${r.m!.ts}`)}>
              <span className="tm">{fmtTime(r.m!.ts)}</span>
              <span className="nm" style={{ color: r.m!.role === "user" ? "var(--blue)" : "var(--magenta)" }}>{r.m!.role === "user" ? "you" : "asst"}</span>
              <span className="prev" style={{ fontFamily: "var(--font-sans)", color: "var(--text-2)" }}>{expanded.has(`msg:${r.m!.ts}`) ? r.m!.text : r.m!.text.split("\n")[0]}</span>
            </div>
          ),
        )}
        {total > CAP && <div className="empty">Showing first {CAP} of {total} — narrow with search.</div>}
        {total === 0 && <div className="empty">No matches. Try another facet or search.</div>}
      </div>
    );
  }

  const toolNames = Object.entries(S.toolCounts).sort((a, b) => b[1] - a[1]);
  return (
    <div style={{ flex: 1, display: "flex", flexDirection: "column", minHeight: 0 }}>
      <div className="facets">
        {facetBtn("all", "All", S.counts.all)}
        {facetBtn("prose", "Prose", S.counts.prose)}
        {facetBtn("tools", "Tools", S.counts.tools)}
        {facetBtn("errors", "Errors", S.counts.errors, S.counts.errors ? "err" : "")}
        {facetBtn("user", "You", S.counts.user)}
        <select className="sel" value={toolF} onChange={(e) => setTool(e.target.value)}>
          <option value="all">All tools</option>
          {toolNames.map(([n, c]) => (
            <option key={n} value={n}>{n} · {c}</option>
          ))}
        </select>
        <div className="searchbox" style={{ width: 240, height: 24 }}>
          <Icon name="search" size={12} />
          <input value={q} onChange={(e) => setQ(e.target.value)} placeholder="Search this session…" />
        </div>
        <div style={{ flex: 1 }} />
        <button className="chip" onClick={() => setOrder(ord === "asc" ? "desc" : "asc")}>
          {ord === "asc" ? "↓ oldest first" : "↑ newest first"}
        </button>
      </div>
      <div ref={scroller} style={{ flex: 1, overflowY: "auto", minHeight: 0 }}>{body}</div>
    </div>
  );
}

function prettyJSON(s: string) {
  try {
    return JSON.stringify(JSON.parse(s), null, 2);
  } catch {
    return s;
  }
}
