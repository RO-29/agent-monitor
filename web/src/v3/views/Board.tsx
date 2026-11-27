import { useMemo, useState } from "react";
import { Link, useNavigate } from "react-router-dom";
import { fmtAgo, fmtUsd, projectName } from "../../lib/format";
import { useLive } from "../../lib/ws";
import { useApi, v3 } from "../api/client";
import type { V3Thread } from "../api/types";
import { LegacyPermCard } from "../components/ApprovalCard";
import { AskKeysCtx, askFromApproval, WsApproval } from "./workspace/WsApproval";
import { AgentBadge, Empty, ErrorState, Loading, stateInfo } from "../components/ui";
import { useShell } from "../ctx";
import { useV3 } from "../live";
import { activity, buildRows, threadTitle, type Row } from "../model";
import { useNeedsYou } from "../api/needsYou";

type GroupBy = "state" | "project" | "agent";
const STATE_COLS: { key: Row["bucket"]; label: string; dot: string }[] = [
  { key: "needs-you", label: "Needs you", dot: "d-need" },
  { key: "running", label: "Working", dot: "d-run" },
  { key: "idle-today", label: "Ready", dot: "d-idle" },
  { key: "done-today", label: "Done today", dot: "d-ok" },
];
const COL_CAP = 12;

export default function Board() {
  const live = useLive();
  const st = useV3();
  const shell = useShell();
  const [by, setBy] = useState<GroupBy>("state");
  const [agent, setAgent] = useState<"all" | "claude" | "codex">("all");
  const all = useApi(() => v3.threads(true), [], { poll: 30_000 });
  const ny = useNeedsYou();
  const rows = useMemo(() => buildRows(st, live, ny), [st.version, live.version, ny.version]); // eslint-disable-line react-hooks/exhaustive-deps
  const kidsOf = useMemo(() => {
    const m = new Map<string, V3Thread[]>();
    for (const t of all.data?.threads || []) if (t.parentThread) m.set(t.parentThread, [...(m.get(t.parentThread) || []), t]);
    m.forEach((v) => v.sort((a, b) => b.lastAt - a.lastAt));
    return m;
  }, [all.data]);

  const active = rows.filter((r) => r.bucket !== "older" && (agent === "all" || r.agent === agent));
  const cols: { key: string; label: string; dot?: string; rows: Row[] }[] = useMemo(() => {
    if (by === "state") return STATE_COLS.map((c) => ({ ...c, rows: sortCol(active.filter((r) => r.bucket === c.key), c.key) }));
    const keyOf = (r: Row) => (by === "project" ? projectName(r.cwd) : r.agent);
    const m = new Map<string, Row[]>();
    for (const r of active) m.set(keyOf(r), [...(m.get(keyOf(r)) || []), r]);
    return [...m.entries()]
      .sort((a, b) => b[1].length - a[1].length)
      .slice(0, 8)
      .map(([k, rs]) => ({ key: k, label: k, rows: rs.sort((a, b) => stateInfo(a.session?.state || "", a.approvals.length).rank - stateInfo(b.session?.state || "", b.approvals.length).rank || b.lastAt - a.lastAt) }));
  }, [by, active]); // eslint-disable-line react-hooks/exhaustive-deps

  return (
    <div className="view" style={{ display: "flex", flexDirection: "column" }}>
      <div className="bbar">
        <b style={{ fontSize: 14 }}>Today</b>
        <span className="seg">
          {(["state", "project", "agent"] as GroupBy[]).map((g) => (
            <button key={g} className={by === g ? "on" : ""} onClick={() => setBy(g)}>
              by {g}
            </button>
          ))}
        </span>
        <span className="seg">
          {(["all", "claude", "codex"] as const).map((a) => (
            <button key={a} className={agent === a ? "on" : ""} onClick={() => setAgent(a)}>
              {a === "all" ? "Claude + Codex" : a === "claude" ? "Claude" : "Codex"}
            </button>
          ))}
        </span>
        {all.missing && <span className="xs muted">Child stacks need /api/v3/threads.</span>}
        <button className="searchbar ml-auto hidephone" style={{ width: 260 }} onClick={() => shell.openPalette()}>
          ⌕ Filter or search <span className="kbd ml-auto">⌘K</span>
        </button>
      </div>
      {st.threadsStatus === "loading" && rows.length === 0 ? (
        <Loading rows={8} />
      ) : st.threadsStatus === "error" && rows.length === 0 ? (
        <ErrorState error={st.threadsError} />
      ) : (
        <div className="cols" style={by !== "state" ? { gridTemplateColumns: `repeat(${Math.max(cols.length, 1)}, minmax(240px, 1fr))` } : undefined}>
          {cols.length === 0 && <Empty title="No activity today" />}
          {cols.map((c) => (
            <Column key={c.key} label={c.label} dot={c.dot} rows={c.rows} kidsOf={kidsOf} />
          ))}
        </div>
      )}
    </div>
  );
}

function sortCol(rs: Row[], key: string) {
  return key === "needs-you" ? rs.sort((a, b) => a.sinceAt - b.sinceAt) : rs.sort((a, b) => b.lastAt - a.lastAt);
}

function Column({ label, dot, rows, kidsOf }: { label: string; dot?: string; rows: Row[]; kidsOf: Map<string, V3Thread[]> }) {
  const [cap, setCap] = useState(COL_CAP);
  return (
    <div className="col">
      <div className="colh">
        {dot && <span className={`dot ${dot}`} />}
        <span className="trunc">{label}</span> <span className="c">{rows.length}</span>
      </div>
      {rows.length === 0 && <div className="xs muted" style={{ paddingLeft: 4 }}>empty</div>}
      {rows.slice(0, cap).map((r) => (
        <Card key={r.id} r={r} kids={kidsOf.get(r.id) || []} />
      ))}
      {rows.length > cap && (
        <button className="btn sm" onClick={() => setCap(cap + COL_CAP)}>
          + {rows.length - cap} more
        </button>
      )}
    </div>
  );
}

function Card({ r, kids }: { r: Row; kids: V3Thread[] }) {
  const nav = useNavigate();
  const t = r.thread;
  const needs = r.bucket === "needs-you";
  const info = stateInfo(r.session?.phase || t?.phase || r.session?.state || t?.state || "", r.approvals.length);
  const card = (
    <div className={`tc ${needs ? "needc" : ""}`} onClick={() => nav(`/t/${encodeURIComponent(r.id)}`)}>
      <div className="row">
        <AgentBadge tool={r.agent} />
        <span className="key trunc">{projectName(r.cwd)}</span>
        {needs ? <span className="pill p-need ml-auto">{fmtAgo(r.sinceAt, "")}</span> : <span className="xs muted ml-auto">{fmtAgo(r.lastAt, "")}</span>}
      </div>
      <div className="tt">{r.title}</div>
      {!r.approvals.length && activity(r) && (
        <div className="st trunc">
          {r.bucket === "running" && <span className="dot d-run" style={{ marginRight: 6 }} />}
          {r.bucket === "idle-today" || r.bucket === "done-today" ? "last: " : ""}
          {activity(r)}
        </div>
      )}
      {r.approvals.slice(0, 1).map((a) => (
        <div key={a.id} style={{ marginTop: 8 }} onClick={(e) => e.stopPropagation()}>
          <AskKeysCtx.Provider value={false}><WsApproval ask={askFromApproval(a)} /></AskKeysCtx.Provider>
        </div>
      ))}
      {r.perms.slice(0, 1).map((p) => (
        <div key={p.id} style={{ marginTop: 8 }} onClick={(e) => e.stopPropagation()}>
          <LegacyPermCard p={p} compact />
        </div>
      ))}
      {kids.slice(0, 3).map((k) => {
        const ki = stateInfo(k.state);
        return (
          <Link key={k.id} to={`/t/${encodeURIComponent(k.id)}`} className="kid" onClick={(e) => e.stopPropagation()}>
            <AgentBadge tool={k.tools[0] === "codex" ? "codex" : "subagent"} small />
            <span className="trunc">{threadTitle(k)}</span>
            <span className={`pill ${k.errors ? "p-err" : ki.pill} ml-auto`}>{k.errors ? `${k.errors} err` : ki.label}</span>
          </Link>
        );
      })}
      {kids.length > 3 && <div className="kid muted">+ {kids.length - 3} more children</div>}
      {t && (
        <div className="ft">
          {r.bucket === "done-today" && t.outputs > 0 && <span className="pill p-ok">{t.outputs} outputs</span>}
          {t.errors > 0 && <span className="pill p-err">{t.errors} errors</span>}
          <span>
            {t.turns.toLocaleString()} turns{t.compacts ? ` · ${t.compacts} compactions` : ""} · {fmtUsd(t.costUsd, t.costEstimated)}
          </span>
          {!needs && info.label !== "done" && <span className={`pill ${info.pill} ml-auto`}>{info.label}</span>}
        </div>
      )}
    </div>
  );
  return kids.length ? <div className="stack">{card}</div> : card;
}
