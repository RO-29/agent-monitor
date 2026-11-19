// Detail pane for a driven thread inside Threads and the Control room: outcome, numbers, a reply box, and the way into the Workspace.
import { usd } from "../components/Spend";
import { useState } from "react";
import { Link } from "react-router-dom";
import { showToast } from "../../app/toast";
import { fmtAgo, fmtUsd, shortCwd } from "../../lib/format";
import { allowOnce, sendMessage, spendBlockOf, toMs, useOrch, useOrchThread } from "../api/orch";
import { Markdown } from "../components/Markdown";
import { AgentBadge, DrivenTag } from "../components/ui";
import type { Row } from "../model";

const STATUS: Record<string, [string, string]> = {
  starting: ["starting", "p-run"],
  running: ["running", "p-run"],
  waiting: ["needs you", "p-need"],
  idle: ["ready", "p-ok"],
  error: ["error", "p-err"],
  closed: ["closed", "p-mute"],
};

export function DrivenDetail({ id, row, approvals, compact }: { id: string; row?: Row; approvals?: React.ReactNode; compact?: boolean }) {
  const orch = useOrch();
  const { detail } = useOrchThread(id);
  const t = detail?.thread || orch.threads.get(id);
  const [text, setText] = useState("");
  const [busy, setBusy] = useState(false);
  if (!t) return <div className="drvd muted">Loading the driven thread…</div>;
  const runs = detail?.runs || [];
  const cost = runs.reduce((n, r) => n + (r.usage?.costUsd || 0), 0);
  const last = detail ? [...detail.items].reverse().find((i) => i.kind === "agent" && i.text) : undefined;
  const block = orch.blocked.get(t.id);
  const [label, pill] = STATUS[t.status] || [t.status, "p-mute"];
  const kids = (t.children || []).map((c) => orch.threads.get(c)).filter(Boolean);
  const href = `/w/${encodeURIComponent(t.id)}`;
  const send = async () => {
    const body = text.trim();
    if (!body || busy) return;
    setBusy(true);
    try {
      await sendMessage(t.id, body);
      setText("");
      showToast("Sent", t.status === "running" ? "Steered the running turn." : "Started a new turn.");
    } catch (e) {
      if (!spendBlockOf(e)) showToast("Send failed", (e as Error).message, { kind: "alert" });
    }
    setBusy(false);
  };
  return (
    <div className="drvd">
      {!compact && (
        <div className="dhead">
          <AgentBadge tool={t.agent} />
          <h3 className="trunc">{t.title}</h3>
          <DrivenTag />
          <span className={`pill ${pill}`}>{label}</span>
          <Link className="btn sm pri ml-auto" to={href}>
            Open in Workspace
          </Link>
        </div>
      )}
      {approvals}
      {t.error && <div className="blk">{t.error}</div>}
      {last ? (
        <div className="out">
          <Markdown text={last.text.slice(0, 3000)} collapseAt={12} />
        </div>
      ) : (
        <div className="muted sm">{t.status === "running" || t.status === "starting" ? "Working on the first answer…" : "No answer yet."}</div>
      )}
      <div className="kvs">
        <span>
          <b>{runs.length}</b> {runs.length === 1 ? "turn" : "turns"}
        </span>
        <span>
          {cost ? (
            <>
              <b>{fmtUsd(cost)}</b> spent
            </>
          ) : (
            "no cost reported"
          )}
        </span>
        <span className="mono">{t.worktree?.branch || t.branch}</span>
        <span className="mono">{shortCwd(t.cwd)}</span>
        <span>updated {fmtAgo(toMs(t.updatedAt))}</span>
        {row?.thread?.parentThread && <span>subagent</span>}
      </div>
      {kids.length > 0 && (
        <div className="kvs">
          {kids.map((k) => (
            <Link key={k!.id} to={`/w/${encodeURIComponent(k!.id)}`}>
              ↳ {k!.title} <span className="muted">· {STATUS[k!.status]?.[0] || k!.status}</span>
            </Link>
          ))}
        </div>
      )}
      {block ? (
        <div className="blk" role="alert">
          <span>
            Spend limit reached: {usd(block.spent)} of {usd(block.limit)}. New turns wait for you.
          </span>
          <button className="btn sm pri" onClick={() => allowOnce(t.id).catch((e) => showToast("Could not allow the turn", (e as Error).message, { kind: "alert" }))}>
            Allow this thread once more
          </button>
          <Link className="btn sm ghost" to="/settings/limits">
            Limits
          </Link>
        </div>
      ) : (
        t.status !== "closed" && (
          <div className="reply">
            <textarea
              rows={2}
              value={text}
              placeholder={t.status === "running" ? "Steer the running turn. ⌘↵ sends" : "Reply to this thread. ⌘↵ sends"}
              onChange={(e) => setText(e.target.value)}
              onKeyDown={(e) => {
                e.stopPropagation();
                if (e.key === "Enter" && (e.metaKey || e.ctrlKey)) {
                  e.preventDefault();
                  void send();
                }
              }}
            />
            <button className="btn pri" disabled={busy || !text.trim()} onClick={send}>
              Send
            </button>
          </div>
        )
      )}
    </div>
  );
}
