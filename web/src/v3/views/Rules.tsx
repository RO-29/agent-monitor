import { useEffect, useState } from "react";
import { api } from "../../api/client";
import type { Settings } from "../../api/types";
import { showToast } from "../../app/toast";
import { fmtAgo, fmtUsd, projectName } from "../../lib/format";
import { useApi, v3 } from "../api/client";
import type { Approval, ApprovalSettings, Rule, RuleSuggestion } from "../api/types";
import { AgentBadge, CopyBtn, ErrorState, Load, Loading, Missing } from "../components/ui";

const ACTION_PILL: Record<string, string> = { allow: "p-ok", deny: "p-err", hold: "p-need" };
const asList = <T,>(d: unknown, key: string): T[] => (Array.isArray(d) ? (d as T[]) : ((d as Record<string, T[]>)?.[key] || []));

export default function Rules() {
  return (
    <div className="view pad sx">
      <div className="viewhead">
        <div>
          <h2>Rules</h2>
          <p>How the daemon answers approval requests: rules first, then the hold window. Answers land in the audit log with the surface that gave them.</p>
        </div>
      </div>
      <div className="grid2">
        <RulesBox />
        <SettingsBox />
      </div>
      <div style={{ marginTop: 16 }}>
        <HooksBox />
      </div>
      <div style={{ marginTop: 16 }}>
        <AuditBox />
      </div>
    </div>
  );
}

// Calls the user allowed by hand 3 or more times in 30 days, with no deny of the same shape.
function Suggestions({ onAdded }: { onAdded: () => void }) {
  const res = useApi(() => v3.ruleSuggestions(), []);
  const [busy, setBusy] = useState("");
  const list = asList<RuleSuggestion>(res.data, "suggestions");
  if (!list.length) return null;
  const act = async (s: RuleSuggestion, add: boolean) => {
    setBusy(s.key);
    try {
      if (add) {
        await v3.saveRule({ action: "allow", agent: s.agent, tool: s.tool, pattern: s.pattern, cwdPrefix: s.cwdPrefix, note: `suggested: you allowed it ${s.count} times` });
        showToast("Rule added", `${s.tool} matching ${s.pattern} is now allowed.`);
        onAdded();
      } else await v3.dismissSuggestion(s.key);
      res.reload();
    } catch (e) {
      showToast(add ? "Rule not saved" : "Not dismissed", (e as Error).message, { kind: "alert" });
    } finally {
      setBusy("");
    }
  };
  return (
    <div className="sugbox" role="region" aria-label="Suggested rules">
      <div className="xs muted">Suggested: you allowed these by hand, and never denied them</div>
      {list.map((s) => (
        <div key={s.key} className="row sugrow">
          <span className="pill p-ok">allow</span>
          <span className="grow1 trunc" title={s.example}>
            {s.agent && <AgentBadge tool={s.agent} small />} <code>{s.tool}</code> matching <code>{s.pattern}</code>
            {s.cwdPrefix && <span className="muted"> in {projectName(s.cwdPrefix)}</span>}
            <span className="muted">
              {" "}
              · {s.count}× · last {fmtAgo(s.lastAt)}
            </span>
          </span>
          <button className="btn sm" disabled={!!busy} onClick={() => act(s, true)}>
            Add
          </button>
          <button className="btn sm ghost" disabled={!!busy} onClick={() => act(s, false)} aria-label={`Dismiss ${s.pattern}`}>
            Dismiss
          </button>
        </div>
      ))}
    </div>
  );
}

function RulesBox() {
  const res = useApi(() => v3.rules(), []);
  const [draft, setDraft] = useState<Partial<Rule>>({ action: "allow", tool: "Bash" });
  const [busy, setBusy] = useState(false);
  const rules = asList<Rule>(res.data, "rules");
  const save = async () => {
    setBusy(true);
    try {
      await v3.saveRule({ ...draft, agent: draft.agent || undefined });
      setDraft({ action: "allow", tool: "Bash" });
      res.reload();
    } catch (e) {
      showToast("Rule not saved", (e as Error).message, { kind: "alert" });
    } finally {
      setBusy(false);
    }
  };
  return (
    <div className="ibox">
      <h3>Approval rules</h3>
      {res.missing ? (
        <Missing endpoint="/api/approvals/rules" />
      ) : res.error && !res.data ? (
        <ErrorState error={res.error} onRetry={res.reload} />
      ) : !res.data ? (
        <Loading />
      ) : (
        <>
          <Suggestions onAdded={res.reload} />
          <div className="sm" style={{ display: "flex", flexDirection: "column", gap: 6 }}>
            {rules.length === 0 && <div className="muted">No rules. Every request waits for a person.</div>}
            {rules.map((r) => (
              <div key={r.id} className="row" style={{ borderBottom: "1px solid var(--line)", paddingBottom: 6 }}>
                <span className={`pill ${ACTION_PILL[r.action]}`}>{r.action}</span>
                <span className="grow1 trunc">
                  {r.agent && <AgentBadge tool={r.agent} small />} <code>{r.tool || "any tool"}</code>
                  {r.pattern && (
                    <>
                      {" "}
                      matching <code>{r.pattern}</code>
                    </>
                  )}
                  {r.cwdPrefix && <span className="muted"> in {r.cwdPrefix}</span>}
                  {r.note && <span className="muted"> · {r.note}</span>}
                </span>
                <span className="xs muted tab-num" style={{ flex: "none" }}>
                  {r.hits} hits
                </span>
                <button
                  className="btn sm ghost"
                  aria-label="Delete rule"
                  onClick={() =>
                    v3
                      .deleteRule(r.id)
                      .then(res.reload)
                      .catch((e) => showToast("Delete failed", (e as Error).message, { kind: "alert" }))
                  }
                >
                  ✕
                </button>
              </div>
            ))}
          </div>
          <div className="form" style={{ marginTop: 12 }}>
            <label>
              Action
              <select className="inp" value={draft.action} onChange={(e) => setDraft({ ...draft, action: e.target.value as Rule["action"] })}>
                <option value="allow">allow</option>
                <option value="deny">deny</option>
                <option value="hold">hold</option>
              </select>
            </label>
            <label>
              Agent
              <select className="inp" value={draft.agent || ""} onChange={(e) => setDraft({ ...draft, agent: (e.target.value || undefined) as Rule["agent"] })}>
                <option value="">any</option>
                <option value="claude">Claude</option>
                <option value="codex">Codex</option>
              </select>
            </label>
            <label>
              Tool
              <input className="inp" value={draft.tool || ""} onChange={(e) => setDraft({ ...draft, tool: e.target.value })} placeholder="Bash, Edit, mcp__x__y" />
            </label>
            <label>
              Pattern (glob)
              <input className="inp" value={draft.pattern || ""} onChange={(e) => setDraft({ ...draft, pattern: e.target.value })} placeholder="git diff*" />
            </label>
            <label>
              Folder prefix
              <input className="inp" value={draft.cwdPrefix || ""} onChange={(e) => setDraft({ ...draft, cwdPrefix: e.target.value })} placeholder="/Users/me/repo" />
            </label>
            <label>
              Note
              <input className="inp" value={draft.note || ""} onChange={(e) => setDraft({ ...draft, note: e.target.value })} />
            </label>
            <button className="btn pri" disabled={busy || !(draft.tool || draft.pattern)} onClick={save}>
              Add rule
            </button>
          </div>
          <div className="xs muted" style={{ marginTop: 8 }}>
            Deny rules match first, then allow, then hold.
          </div>
        </>
      )}
    </div>
  );
}

function SettingsBox() {
  const res = useApi(() => v3.approvalSettings(), []);
  const enrich = useApi<Settings>(() => api.settings(), []);
  const [s, setS] = useState<ApprovalSettings | null>(null);
  useEffect(() => {
    if (res.data) setS(res.data);
  }, [res.data]);
  const save = () =>
    s &&
    v3
      .saveApprovalSettings(s)
      .then((r) => {
        res.setData(r && typeof r.holdSeconds === "number" ? r : s);
        showToast("Settings saved");
      })
      .catch((e) => showToast("Not saved", (e as Error).message, { kind: "alert" }));
  return (
    <div className="ibox">
      <h3>Hold window</h3>
      {res.missing ? (
        <Missing endpoint="/api/approvals/settings" />
      ) : res.error && !s ? (
        <ErrorState error={res.error} onRetry={res.reload} />
      ) : !s ? (
        <Loading rows={3} />
      ) : (
        <>
          <div className="form">
            <label>
              Hold a permission (seconds)
              <input className="inp" type="number" min={0} value={s.holdSeconds} onChange={(e) => setS({ ...s, holdSeconds: Number(e.target.value) })} />
            </label>
            <label>
              Hold a question or plan (seconds)
              <input className="inp" type="number" min={0} value={s.questionHoldSeconds} onChange={(e) => setS({ ...s, questionHoldSeconds: Number(e.target.value) })} />
            </label>
          </div>
          <label className="toggle" style={{ marginTop: 10 }}>
            Approvals for sessions with a terminal
            <select className="inp" value={s.approvalsMode ?? "both"} onChange={(e) => setS({ ...s, approvalsMode: e.target.value as "both" | "hold" })} style={{ marginLeft: 8, width: "auto" }}>
              <option value="both">In the terminal and here at once</option>
              <option value="hold">Hold here first</option>
            </select>
          </label>
          <div className="xs muted" style={{ marginTop: 4 }}>
            Both: the agent shows its own prompt at once, a card shows here too, and the first answer in either place counts. Hold: the card holds the request for the window above, then the terminal prompt.
          </div>
          <label className="toggle" style={{ marginTop: 10 }}>
            <input type="checkbox" checked={s.skipWhenFocused} onChange={(e) => setS({ ...s, skipWhenFocused: e.target.checked })} />
            Skip the hold when the agent's tmux pane has focus (you are at the terminal)
          </label>
          <div className="row" style={{ marginTop: 10 }}>
            <button className="btn pri" onClick={save}>
              Save
            </button>
            <span className="xs muted">With no answer inside the window, the agent shows its own terminal prompt.</span>
          </div>
        </>
      )}
      <h3 style={{ marginTop: 18 }}>Ask in search</h3>
      <Load st={enrich} endpoint="/api/settings" rows={2}>
        {(e) => (
          <div className="sm fg2">
            Model calls are {e.enrichEnabled ? <b className="ok-t">on</b> : <b>off</b>} ({e.enrichModel}). Spent today {fmtUsd(e.spentTodayUsd)} of the {fmtUsd(e.dailyCapUsd)} daily cap.
            <div style={{ marginTop: 8 }}>
              <button
                className="btn sm"
                onClick={() =>
                  api
                    .saveSettings({ enrichEnabled: !e.enrichEnabled })
                    .then((n) => enrich.setData(n))
                    .catch((x) => showToast("Not saved", (x as Error).message, { kind: "alert" }))
                }
              >
                {e.enrichEnabled ? "Turn model calls off" : "Turn model calls on"}
              </button>
            </div>
          </div>
        )}
      </Load>
    </div>
  );
}

function HooksBox() {
  const snip = useApi(() => v3.hooksSnippet(), []);
  const health = useApi(() => v3.health(), [], { poll: 30_000 });
  const state = (on: boolean | undefined) =>
    health.data ? (on ? <span className="pill p-ok">detected in your config</span> : <span className="pill p-need">not detected</span>) : <span className="pill p-mute">install state unknown</span>;
  const claude = snip.data ? JSON.stringify({ hooks: snip.data.claude }, null, 2) : "";
  const codex = snip.data ? (typeof snip.data.codex === "string" ? snip.data.codex : JSON.stringify(snip.data.codex, null, 2)) : "";
  return (
    <div className="ibox">
      <h3>Agent hooks</h3>
      <p className="sm fg2" style={{ margin: "0 0 10px" }}>
        Hooks make each agent ask this daemon before it shows its own prompt. The daemon writes no config file. Copy each snippet into place yourself.
        {health.missing && " The hook check (/api/v3/health) is not available on this daemon, so this page cannot tell whether the hooks are installed."}
      </p>
      {snip.missing ? (
        <Missing endpoint="/api/approvals/hooks-snippet" />
      ) : snip.error && !snip.data ? (
        <ErrorState error={snip.error} onRetry={snip.reload} />
      ) : !snip.data ? (
        <Loading rows={4} />
      ) : (
        <div className="grid2">
          <div>
            <div className="row" style={{ marginBottom: 6 }}>
              <AgentBadge tool="claude" />
              <b>Claude Code</b>
              {state(health.data?.hooks.claudeHttp)}
            </div>
            <ol className="sm fg2" style={{ margin: "0 0 8px", paddingLeft: 18 }}>
              <li>
                Open <code>~/.claude/settings.json</code>.
              </li>
              <li>
                Merge the <code>hooks</code> object below into it. Keep the hooks you already have.
              </li>
              <li>Start a new Claude Code session. Running sessions keep their old hooks.</li>
            </ol>
            <div className="snip-code">
              <pre>{claude}</pre>
              <CopyBtn text={claude} />
            </div>
          </div>
          <div>
            <div className="row" style={{ marginBottom: 6 }}>
              <AgentBadge tool="codex" />
              <b>Codex CLI</b>
              {state(health.data?.hooks.codex)}
            </div>
            <ol className="sm fg2" style={{ margin: "0 0 8px", paddingLeft: 18 }}>
              <li>
                Write the content below to <code>~/.codex/hooks.json</code> (merge it if the file exists).
              </li>
              <li>
                In Codex, run <code>/hooks</code> once and trust the hook.
              </li>
              <li>Codex accepts allow or deny only, so "Always" for Codex is a daemon rule.</li>
            </ol>
            <div className="snip-code">
              <pre>{codex}</pre>
              <CopyBtn text={codex} />
            </div>
          </div>
        </div>
      )}
    </div>
  );
}

function AuditBox() {
  const res = useApi(() => v3.audit({ limit: 50 }), [], { poll: 60_000 });
  const rows = Array.isArray(res.data) ? res.data : asList<Approval>(res.data, "approvals").length ? asList<Approval>(res.data, "approvals") : asList<Approval>(res.data, "audit");
  return (
    <div className="tblwrap">
      <div style={{ padding: "12px 14px 0" }}>
        <b className="sm">Audit log</b> <span className="xs muted">· last 50 decisions</span>
      </div>
      <Load st={res} endpoint="/api/approvals/audit" rows={4} isEmpty={() => rows.length === 0} empty={<div className="state"><b>No decisions recorded yet</b></div>}>
        {() => (
          <table className="tbl">
            <thead>
              <tr>
                <th>When</th>
                <th>Agent</th>
                <th>Request</th>
                <th>Result</th>
                <th>By</th>
              </tr>
            </thead>
            <tbody>
              {rows.map((a) => (
                <tr key={a.id}>
                  <td className="n muted">{fmtAgo(a.decidedAt || a.createdAt)}</td>
                  <td>
                    <span className="row">
                      <AgentBadge tool={a.agent} small /> {projectName(a.cwd)}
                    </span>
                  </td>
                  <td style={{ maxWidth: 480 }}>
                    <code className="trunc" style={{ display: "block" }}>
                      {a.toolName} · {a.summary}
                    </code>
                    {a.message && <div className="xs muted trunc">“{a.message}”</div>}
                  </td>
                  <td>
                    <span className={`pill ${a.state.includes("allow") ? "p-ok" : a.state.includes("den") ? "p-err" : "p-mute"}`}>{a.state}</span>
                  </td>
                  <td className="muted">{a.decidedBy || "·"}</td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </Load>
    </div>
  );
}
