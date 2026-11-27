import { useEffect, useMemo, useRef, useState } from "react";
import { useNavigate, useParams } from "react-router-dom";
import { fmtAgo, modelShort, projectName } from "../../lib/format";
import { useLive } from "../../lib/ws";
import { DenyTarget, needsSecond, quickDecide } from "../components/ApprovalCard";
import { AskKeysCtx, askFromApproval, WsApproval } from "./workspace/WsApproval";
import { AgentBadge, DrivenTag, Empty, ErrorState, Loading, stateInfo } from "../components/ui";
import { drivenIdOf, threadHref } from "../driven";
import { DrivenDetail } from "./DrivenDetail";
import { typing, useShell } from "../ctx";
import { approvalKeyOk } from "../keys";
import { useV3 } from "../live";
import { activity, buildRows, LISTS, listRows, repoOf, type Row } from "../model";
import { ThreadView } from "./thread/ThreadView";
import { useNeedsYou } from "../api/needsYou";

export function usePhone() {
  const q = "(max-width: 760px)";
  const [m, setM] = useState(() => typeof window !== "undefined" && window.matchMedia(q).matches);
  useEffect(() => {
    const mq = window.matchMedia(q);
    const on = () => setM(mq.matches);
    mq.addEventListener("change", on);
    return () => mq.removeEventListener("change", on);
  }, []);
  return m;
}

const CAP = 150;

export default function Inbox() {
  const { list: listParam } = useParams();
  const list = decodeURIComponent(listParam || "home");
  const live = useLive();
  const st = useV3();
  const nav = useNavigate();
  const phone = usePhone();
  const shell = useShell();
  const ny = useNeedsYou();
  const rows = useMemo(() => buildRows(st, live, ny), [st.version, live.version, ny.version]); // eslint-disable-line react-hooks/exhaustive-deps

  const groups: { label: string; rows: Row[]; cap: number }[] = useMemo(() => {
    if (list === "home")
      return [
        { label: "Needs you", rows: listRows(rows, "needs-you"), cap: CAP },
        { label: "Working", rows: listRows(rows, "running"), cap: CAP },
        { label: "Ready", rows: listRows(rows, "idle-today"), cap: 20 },
        { label: "Done today", rows: listRows(rows, "done-today"), cap: 20 },
      ];
    const l = LISTS.find((x) => x.key === list);
    return [{ label: l ? l.label : list.startsWith("project:") ? list.slice(8) : list, rows: listRows(rows, list), cap: CAP }];
  }, [rows, list]);

  const [more, setMore] = useState<Record<string, number>>({});
  const flat = groups.flatMap((g) => g.rows.slice(0, more[g.label] || g.cap));
  const [selId, setSelId] = useState<string | null>(null);
  const sel = flat.find((r) => r.id === selId) || flat[0];
  const [denyId, setDenyId] = useState<string | null>(null);
  const listRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    listRef.current?.querySelector(".item.sel")?.scrollIntoView({ block: "nearest" });
  }, [sel?.id]);

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (typing(e) || e.metaKey || e.ctrlKey || e.altKey) return;
      if (document.querySelector(".scrim, .drawer")) return;
      const i = sel ? flat.indexOf(sel) : -1;
      if (e.key === "j" || e.key === "ArrowDown") {
        e.preventDefault();
        const n = flat[Math.min(i + 1, flat.length - 1)];
        if (n) setSelId(n.id);
      } else if (e.key === "k" || e.key === "ArrowUp") {
        e.preventDefault();
        const n = flat[Math.max(i - 1, 0)];
        if (n) setSelId(n.id);
      } else if (e.key === "Enter" && sel && !(e.target as Element | null)?.closest?.("button, a, summary, [role=button]")) {
        nav(threadHref(sel.id, sel.thread));
      } else if ((e.key === "a" || e.key === "A") && sel?.approvals[0]) {
        if (!approvalKeyOk(e)) return;
        e.preventDefault();
        if (needsSecond(sel.approvals[0], e.key)) return;
        void quickDecide(sel.approvals[0], e.key === "A" && e.shiftKey ? "always" : "allow");
      } else if (e.key === "d" && sel?.approvals[0]) {
        if (!approvalKeyOk(e)) return;
        e.preventDefault();
        setDenyId(sel.approvals[0].id);
      } else if (e.key === "o" && sel?.session) {
        shell.openPane(sel.session);
      }
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [flat, sel, nav, shell]);

  const head = groups.length === 1 ? groups[0] : null;
  const loading = st.threadsStatus === "loading" && rows.length === 0;

  return (
    <DenyTarget.Provider value={{ id: denyId, clear: () => setDenyId(null) }}>
      <div className="view inbox">
        <div className="qlist">
          <div className="lhead">
            <b>{head ? head.label : "Threads"}</b>
            <span className="muted sm">{head ? head.rows.length : groups[0].rows.length + groups[1].rows.length}</span>
            <span className="xs muted ml-auto">{list === "needs-you" || list === "home" ? "needs you first, oldest first" : "latest first"}</span>
          </div>
          <div className="items" ref={listRef}>
            {loading && <Loading rows={8} />}
            {st.threadsStatus === "error" && rows.length === 0 && <ErrorState error={st.threadsError} />}
            {!loading && flat.length === 0 && st.threadsStatus !== "error" && (
              <Empty title={list === "needs-you" || list === "home" ? "Nothing needs you" : "No threads in this list"}>
                {list === "home" ? "No agent needs you or runs right now." : "Pick another list on the left, or search with ⌘K."}
              </Empty>
            )}
            {groups.map((g, gi) => {
              const cap = more[g.label] || g.cap;
              if (!g.rows.length && groups.length > 1) return null;
              return (
                <div key={g.label}>
                  {groups.length > 1 && (gi > 0 || g.label !== "Needs you") && (
                    <div className="groupline">
                      {g.label} · {g.rows.length}
                    </div>
                  )}
                  {g.rows.slice(0, cap).map((r) => (
                    <Item
                      key={r.id}
                      r={r}
                      sel={sel?.id === r.id}
                      onClick={() => {
                        if (phone) nav(threadHref(r.id, r.thread));
                        else setSelId(r.id);
                      }}
                    />
                  ))}
                  {g.rows.length > cap && (
                    <button className="btn sm" style={{ margin: "8px 14px" }} onClick={() => setMore({ ...more, [g.label]: cap + CAP })}>
                      Show {Math.min(CAP, g.rows.length - cap)} more of {g.rows.length - cap}
                    </button>
                  )}
                </div>
              );
            })}
          </div>
        </div>
        {sel ? (
          drivenIdOf(sel.thread) ? (
            <div className="detail">
              <DrivenDetail
                key={sel.id}
                id={drivenIdOf(sel.thread)!}
                row={sel}
                approvals={sel.approvals.map((a) => (
                  <AskKeysCtx.Provider value={false}><WsApproval key={a.id} ask={askFromApproval(a)} /></AskKeysCtx.Provider>
                ))}
              />
            </div>
          ) : sel.thread ? (
            <ThreadView key={sel.id} threadId={sel.id} embedded />
          ) : (
            <OrphanDetail r={sel} />
          )
        ) : (
          <div className="detail">
            <Empty title="Select a thread">
              <span className="kbd">j</span> <span className="kbd">k</span> move · <span className="kbd">a</span> allow · <span className="kbd">d</span> deny · <span className="kbd">⌘K</span> search
            </Empty>
          </div>
        )}
      </div>
    </DenyTarget.Provider>
  );
}

function Item({ r, sel, onClick }: { r: Row; sel: boolean; onClick: () => void }) {
  const info = stateInfo(r.session?.state || r.thread?.state || "idle", r.approvals.length + r.perms.length);
  const a = r.approvals[0];
  const pill = a ? (a.kind === "question" ? "question" : a.kind === "plan" ? "plan" : "approve") : r.needs ? "Needs you" : info.label;
  const act = activity(r);
  const t = r.thread;
  return (
    <div className={`item ${sel ? "sel" : ""}`} onClick={onClick} role="button" tabIndex={-1}>
      <AgentBadge tool={r.agent} />
      <div className="t trunc" title={r.title}>
        {drivenIdOf(t) && <DrivenTag />}
        {r.title}
      </div>
      <span className="xs muted tab-num">{r.bucket === "running" ? "now" : fmtAgo(r.needs ? r.sinceAt : r.lastAt, "")}</span>
      {act && (
        <div className="lv trunc" title={act}>
          {r.bucket === "running" && <span className="dot d-run" style={{ marginRight: 6 }} />}
          {act}
        </div>
      )}
      <div className="meta">
        {(r.needs || r.bucket === "running") && <span className={`pill ${info.pill}`}>{pill}</span>}
        <span className="trunc">
          {t ? repoOf(t) : projectName(r.cwd)}
          {modelShort(t?.model) ? ` · ${modelShort(t?.model)}` : ""}
          {t && t.turns > 1 ? ` · ${t.turns.toLocaleString()} turns` : ""}
          {(t?.childCount || 0) > 1 ? ` · ${t!.childCount} child runs` : ""}
        </span>
      </div>
      {t && (!!t.childAttention || (t.errorsOpen || 0) > 0) && (
        <div className="kids">
          {!!t.childAttention && <span className="pill p-need">Child needs you</span>}
          {(t.errorsOpen || 0) > 0 && <span className="pill p-err">{t.errorsOpen} errors not recovered</span>}
        </div>
      )}
    </div>
  );
}

function OrphanDetail({ r }: { r: Row }) {
  return (
    <div className="detail">
      <div className="dhead">
        <AgentBadge tool={r.agent} />
        <h3>{r.title}</h3>
        <span className="pill p-need">Needs you</span>
        <span className="xs muted">{r.cwd}</span>
      </div>
      <div className="stream">
        <div className="xs muted">This request comes from a session the thread index has not listed yet (often a child run).</div>
        {r.approvals.map((a) => (
          <ApprovalLazy key={a.id} id={a.id} />
        ))}
      </div>
    </div>
  );
}
function ApprovalLazy({ id }: { id: string }) {
  const st = useV3();
  const a = st.approvals.get(id);
  if (!a) return null;
  return <AskKeysCtx.Provider value={false}><WsApproval ask={askFromApproval(a)} /></AskKeysCtx.Provider>;
}
