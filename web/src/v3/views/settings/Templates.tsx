// Settings → Templates: saved thread setups (agent, workspace, prompt, subagents, review), edit, copy, run in a folder.
import { useEffect, useState } from "react";
import { useNavigate } from "react-router-dom";
import { showToast } from "../../../app/toast";
import { projectName, shortCwd } from "../../../lib/format";
import { isMissing } from "../../api/client";
import { agentCls, agentLabel, useAgents, type OPermissionMode, type OSandbox } from "../../api/orch";
import { tplVars, tplWhere, p7, type OTemplate, type OTemplateWorkspace } from "../../api/p7";
import { TERM_AGENTS, TermStarting, useTermStart, WherePick } from "../workspace/TermStart";
import { ErrorState, Loading } from "../../components/ui";
import { ACP_MODES, agentSub, FanRowEdit, kindOf, ModelPick, newFan, toSpec, useRecentFolders, type FanRow } from "../workspace/NewThread";
import { PERM_MODES, permModeLabel } from "../workspace/rows";
import { SettingsTabs, Switch } from "./Tabs";
import "../../styles/settings.css";
import "../../styles/p6.css";
import "../../styles/workspace.css";
import "../../styles/p7.css";

export const WS_LABEL: Record<string, string> = { here: "This checkout", worktree: "New worktree", "read-only": "Read only", shared: "This checkout" };

export function tplMeta(t: OTemplate): string {
  const mode = kindOf(t.agent, []) === "codex" ? (t.sandbox === "workspace-write" ? "can edit" : "read only") : permModeLabel(t.permissionMode || "manual").toLowerCase();
  const term = tplWhere(t) === "terminal";
  return [term ? "in a terminal" : "headless", t.model, term ? "" : WS_LABEL[t.workspace] || t.workspace, mode, t.children?.length ? `${t.children.length} ${t.children.length === 1 ? "subagent" : "subagents"}` : "", t.review ? "review at the end" : ""].filter(Boolean).join(" · ");
}

/** Folder input with recent folders as quiet chips. */
export function FolderPick({ value, onChange, id }: { value: string; onChange: (v: string) => void; id: string }) {
  const recent = useRecentFolders();
  return (
    <div className="p7-folder">
      <input className="inp wfull" list={`${id}-dl`} value={value} placeholder={recent[0] ? shortCwd(recent[0]) : "/path/to/repo"} aria-label="Folder" onChange={(e) => onChange(e.target.value)} />
      <datalist id={`${id}-dl`}>
        {recent.map((c) => (
          <option key={c} value={c} />
        ))}
      </datalist>
      {recent.length > 0 && (
        <div className="wrecent">
          {recent.slice(0, 6).map((c) => (
            <button key={c} type="button" className={`chip t ${value === c ? "on" : ""}`} onClick={() => onChange(c)} title={c}>
              {projectName(c)}
            </button>
          ))}
        </div>
      )}
    </div>
  );
}

function RunPanel({ t, onClose }: { t: OTemplate; onClose: () => void }) {
  const recent = useRecentFolders();
  const nav = useNavigate();
  const [cwd, setCwd] = useState("");
  const [vars, setVars] = useState<Record<string, string>>({});
  const [busy, setBusy] = useState(false);
  const term = useTermStart();
  const names = tplVars(t);
  const dir = cwd.trim() || recent[0] || "";
  const can = !!dir && names.every((k) => vars[k]?.trim()) && !busy;
  const go = async () => {
    setBusy(true);
    try {
      const r = await p7.runTemplate(t.id, dir, names.length ? Object.fromEntries(names.map((k) => [k, vars[k].trim()])) : undefined);
      if (r.terminal) {
        term.follow(t.agent, r.terminal);
        setBusy(false);
      } else if (r.thread) nav(`/w/${encodeURIComponent(r.thread.id)}`);
    } catch (e) {
      showToast("Could not run the template", (e as Error).message, { kind: "alert" });
      setBusy(false);
    }
  };
  if (term.st)
    return (
      <div className="p7-run">
        <TermStarting st={term.st} compact onClose={() => (term.reset(), onClose())} />
      </div>
    );
  return (
    <div className="p7-run">
      <label className="p7-l">Folder</label>
      <FolderPick id={`run-${t.id}`} value={cwd} onChange={setCwd} />
      {names.map((k) => (
        <label key={k} className="p7-var">
          <span className="mono">{`{{${k}}}`}</span>
          <input className="inp" value={vars[k] || ""} placeholder={`Value for ${k}`} onChange={(e) => setVars({ ...vars, [k]: e.target.value })} />
        </label>
      ))}
      <div className="sx-row end">
        <button className="btn ghost sm" onClick={onClose}>
          Cancel
        </button>
        <button className="btn pri sm" disabled={!can} onClick={go}>
          {busy ? "Starting…" : `Run in ${projectName(dir) || "folder"}`}
        </button>
      </div>
    </div>
  );
}

let fk = 0;
function TemplateForm({ init, onSave, onCancel }: { init: OTemplate; onSave: (t: OTemplate) => Promise<void>; onCancel: () => void }) {
  const reg = useAgents();
  const agents = reg.list;
  const [t, setT] = useState<OTemplate>(init);
  const [fan, setFan] = useState<FanRow[]>(() => (init.children || []).map((c) => ({ ...newFan(c.agent), key: ++fk, title: c.title, prompt: c.prompt, workspace: c.workspace, model: c.model || "" })));
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState("");
  const set = (p: Partial<OTemplate>) => setT({ ...t, ...p });
  const kind = kindOf(t.agent, agents);
  const modes = kind === "acp" ? ACP_MODES : PERM_MODES;
  const fanHalf = fan.some((r) => r.title.trim() && !r.prompt.trim());
  const termOff = !TERM_AGENTS.has(kind) ? `${agentLabel(t.agent)} runs headless only.` : fan.length ? "A template with subagents runs headless." : "";
  const inTerm = !termOff && t.where === "terminal";
  const can = !!t.name.trim() && !!t.prompt.trim() && !fanHalf && !busy;
  const save = async () => {
    setBusy(true);
    setErr("");
    try {
      const children = fan.filter((r) => r.prompt.trim()).map(toSpec);
      const base: OTemplate = { ...t, where: inTerm ? "terminal" : "headless", name: t.name.trim(), description: t.description?.trim() || undefined, prompt: t.prompt.trim(), children: children.length ? children : undefined };
      await onSave(kind === "codex" ? { ...base, permissionMode: undefined } : { ...base, sandbox: undefined });
    } catch (e) {
      setErr((e as Error).message);
    }
    setBusy(false);
  };
  return (
    <div className="ibox p7-form">
      <h3>{init.id ? `Edit ${init.name}` : "New template"}</h3>
      <div className="sx-fields p7-two">
        <label className="sx-field">
          <span>Name</span>
          <input className="inp" value={t.name} autoFocus placeholder="Fix and review" onChange={(e) => set({ name: e.target.value })} />
        </label>
        <label className="sx-field">
          <span>Description (optional)</span>
          <input className="inp" value={t.description || ""} placeholder="One line for the list" onChange={(e) => set({ description: e.target.value })} />
        </label>
      </div>
      <h4 className="wh">Prompt</h4>
      <textarea className="inp wfull p7-prompt" rows={4} value={t.prompt} placeholder="What the agent does. {{cwd}}, {{branch}}, {{date}} and your own {{name}} variables work." onChange={(e) => set({ prompt: e.target.value })} />
      <span className="wfhint">
        Built in: <code>{"{{cwd}}"}</code> <code>{"{{branch}}"}</code> <code>{"{{date}}"}</code>. Any other <code>{"{{name}}"}</code> is asked for when the template runs.
      </span>
      {!inTerm && (
        <>
          <h4 className="wh">Workspace</h4>
          <div className="wpick three" role="radiogroup" aria-label="Workspace">
            {(["here", "worktree", "read-only"] as OTemplateWorkspace[]).map((w) => (
              <button key={w} className="wopt" role="radio" aria-checked={t.workspace === w} aria-pressed={t.workspace === w} onClick={() => set({ workspace: w, sandbox: kind === "codex" ? (w === "read-only" ? "read-only" : t.sandbox || "workspace-write") : t.sandbox })}>
                <b>{WS_LABEL[w]}</b>
                <span>{w === "here" ? "The folder as it is" : w === "worktree" ? "A new am/ branch in its own worktree" : "Reads only, edits nothing"}</span>
              </button>
            ))}
          </div>
        </>
      )}
      <h4 className="wh">Agent</h4>
      <div className="wpick wagents" role="radiogroup" aria-label="Agent">
        {agents.map((a) => (
          <button key={a.id} className={`wopt ${a.installed ? "" : "off"}`} role="radio" aria-checked={t.agent === a.id} aria-pressed={t.agent === a.id} disabled={!a.installed} onClick={() => set({ agent: a.id, model: "" })}>
            <b>
              <span className={`wag ${agentCls(a.id)}`} />
              <span className="trunc">{a.name}</span>
            </b>
            <span>{agentSub(a)}</span>
          </button>
        ))}
      </div>
      <h4 className="wh">Where</h4>
      <WherePick where={inTerm ? "terminal" : "headless"} onChange={(where) => set({ where })} termOff={termOff} />
      <div className="wfields">
        {kind === "codex" ? (
          <label>
            <h4 className="wh">Sandbox</h4>
            <select className="inp wfull" value={t.sandbox || "read-only"} disabled={!inTerm && t.workspace === "read-only"} onChange={(e) => set({ sandbox: e.target.value as OSandbox })}>
              <option value="read-only">Read only</option>
              <option value="workspace-write">Workspace write</option>
            </select>
          </label>
        ) : (
          <label>
            <h4 className="wh">Permission mode</h4>
            <select className="inp wfull" value={t.permissionMode || "manual"} onChange={(e) => set({ permissionMode: e.target.value as OPermissionMode })}>
              {modes.map((m) => (
                <option key={m.key} value={m.key}>
                  {m.label}
                </option>
              ))}
            </select>
          </label>
        )}
        <label>
          <h4 className="wh">Model</h4>
          <ModelPick agent={t.agent} agents={agents} value={t.model || ""} onChange={(model) => set({ model })} full />
        </label>
      </div>
      <h4 className="wh">Subagents (optional)</h4>
      {fan.length > 0 && (
        <div className="wfan">
          {fan.map((r, i) => (
            <FanRowEdit key={r.key} r={r} i={i} agents={agents} onChange={(n) => setFan((f) => f.map((x) => (x.key === r.key ? n : x)))} onRemove={() => setFan((f) => f.filter((x) => x.key !== r.key))} />
          ))}
        </div>
      )}
      <div className="wfan-add">
        <button className="btn sm" disabled={fan.length >= 8} onClick={() => setFan((f) => [...f, { ...newFan(f.length % 2 ? "codex" : "claude"), key: ++fk }])}>
          ＋ Add subagent
        </button>
        {fanHalf && <span className="muted sm">Each subagent with a title also needs a prompt.</span>}
      </div>
      <div className="p7-sw">
        <Switch on={!!t.review} label="Review when the run ends" hint="Codex reads the diff in a read-only thread and lists findings." onChange={(review) => set({ review })} />
      </div>
      <div className="sx-row end">
        {err && (
          <span className="sx-note err-t" role="alert">
            {err}
          </span>
        )}
        <button className="btn ghost" onClick={onCancel}>
          Cancel
        </button>
        <button className="btn pri" disabled={!can} onClick={save}>
          {busy ? "Saving…" : "Save template"}
        </button>
      </div>
    </div>
  );
}

const BLANK: OTemplate = { id: "", name: "", agent: "claude", permissionMode: "manual", workspace: "here", prompt: "", where: "terminal" };

export default function Templates() {
  const [list, setList] = useState<OTemplate[] | null>(null);
  const [err, setErr] = useState("");
  const [missing, setMissing] = useState(false);
  const [edit, setEdit] = useState<OTemplate | null>(null);
  const [run, setRun] = useState("");
  const [del, setDel] = useState("");
  const load = () =>
    p7.templates().then(
      (l) => {
        setList(l);
        setErr("");
      },
      (e) => (isMissing(e) ? (setMissing(true), setList([])) : setErr((e as Error).message)),
    );
  useEffect(() => {
    void load();
  }, []);
  const save = async (t: OTemplate) => {
    await p7.saveTemplate(t);
    setEdit(null);
    showToast(t.id ? "Template saved" : "Template added", t.name);
    void load();
  };
  const remove = async (t: OTemplate) => {
    try {
      await p7.deleteTemplate(t.id);
      setDel("");
      void load();
    } catch (e) {
      showToast("Could not delete", (e as Error).message, { kind: "alert" });
    }
  };
  const builtin = (list || []).filter((t) => t.builtin);
  const own = (list || []).filter((t) => !t.builtin);
  const row = (t: OTemplate) => (
    <li key={t.id} className="p7-row">
      <div className="p7-rm">
        <div className="p7-rt">
          <span className={`wag ${agentCls(t.agent)}`} aria-hidden />
          <b>{t.name}</b>
          <span className="muted">{agentLabel(t.agent)}</span>
        </div>
        {t.description && <div className="p7-rd">{t.description}</div>}
        <div className="p7-rs">{tplMeta(t)}</div>
      </div>
      <div className="p7-ra">
        <button className="wlink" onClick={() => setRun(run === t.id ? "" : t.id)}>
          Run
        </button>
        {t.builtin ? (
          <button className="wlink" onClick={() => setEdit({ ...structuredClone(t), id: "", builtin: false, name: `${t.name} copy` })}>
            Copy
          </button>
        ) : (
          <>
            <button className="wlink" onClick={() => setEdit(structuredClone(t))}>
              Edit
            </button>
            <button className="wlink" onClick={() => setDel(del === t.id ? "" : t.id)}>
              Delete
            </button>
          </>
        )}
      </div>
      {del === t.id && (
        <div className="sx-confirm">
          <span>Delete {t.name}? Schedules that use it stop running.</span>
          <button className="btn sm deny" onClick={() => remove(t)}>
            Delete
          </button>
          <button className="btn sm ghost" onClick={() => setDel("")}>
            Cancel
          </button>
        </div>
      )}
      {run === t.id && <RunPanel t={t} onClose={() => setRun("")} />}
    </li>
  );
  return (
    <div className="view pad sx p7 ws wsform">
      <SettingsTabs />
      <div className="viewhead">
        <div>
          <h2>Templates</h2>
          <p>Saved setups for new threads: agent, workspace, prompt and subagents. Run one in any folder, start one from New thread, or put one on a schedule.</p>
        </div>
        {!edit && (
          <button className="btn" onClick={() => setEdit({ ...BLANK })} disabled={missing}>
            New template
          </button>
        )}
      </div>
      {err && !list ? (
        <ErrorState error={err} onRetry={load} />
      ) : !list ? (
        <Loading rows={3} />
      ) : (
        <div className="sx-col">
          {missing && <div className="sx-warn">This daemon has no template routes yet. Update agent-monitor to save and run templates.</div>}
          {edit && <TemplateForm key={edit.id || "new"} init={edit} onSave={save} onCancel={() => setEdit(null)} />}
          {own.length > 0 && (
            <div className="ibox">
              <h3>Yours</h3>
              <ul className="p7-list">{own.map(row)}</ul>
            </div>
          )}
          {builtin.length > 0 && (
            <div className="ibox">
              <h3>
                Built in <span className="muted">copy to change</span>
              </h3>
              <ul className="p7-list">{builtin.map(row)}</ul>
            </div>
          )}
          {!missing && !list.length && <div className="muted">No templates yet.</div>}
        </div>
      )}
    </div>
  );
}
