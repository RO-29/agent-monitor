// New thread: folder, agent, where it runs (a tmux terminal or headless), the fields that apply, prompt.
import { clearSent, useDraft } from "../../../lib/draft";
import { useDocTitle } from "../../../lib/title";
import { useEffect, useMemo, useRef, useState } from "react";
import { Link, useNavigate } from "react-router-dom";
import { ApiError } from "../../api/client";
import { showToast } from "../../../app/toast";
import { projectName } from "../../../lib/format";
import { agentCls, agentLabel, createThread, modelsOf, toMs, useAgents, useOrch, type OAgent, type OAgentInfo, type OApprovalPolicy, type OChildSpec, type OPermissionMode, type ORootWorkspace, type OSandbox, type OWhere, type OWorkspace } from "../../api/orch";
import { useV3 } from "../../live";
import { PERM_MODES, permModeLabel } from "./rows";
import { tplVars as varsOf, tplWhere, p7, type OTemplate } from "../../api/p7";
import { TERM_AGENTS, TermStarting, useTermStart, WherePick } from "./TermStart";
import { Composer, useAttachments } from "./Composer";

/** Folders of recent watched and driven threads, newest first. */
export function useRecentFolders(n = 12): string[] {
  const orch = useOrch();
  const v = useV3();
  return useMemo(() => {
    const seen = new Map<string, number>();
    for (const t of v.threads) if (t.cwd) seen.set(t.cwd, Math.max(seen.get(t.cwd) || 0, t.lastAt));
    for (const t of orch.threads.values()) if (t.cwd) seen.set(t.cwd, Math.max(seen.get(t.cwd) || 0, toMs(t.updatedAt)));
    return [...seen.entries()].sort((a, b) => b[1] - a[1]).map(([c]) => c).slice(0, n);
  }, [v.threads, orch.threads, n]);
}

/** Fills {{cwd}}, {{date}} and free variables; {{branch}} stays for the agent to read. */
export function fillVars(prompt: string, vars: Record<string, string>): string {
  return prompt.replace(/\{\{\s*([\w-]+)\s*\}\}/g, (m, k: string) => vars[k] || (k === "branch" ? "the current branch" : m));
}

export interface FanRow {
  key: number;
  title: string;
  prompt: string;
  agent: OAgent;
  workspace: OWorkspace;
  model: string;
}
let fanKey = 0;
export const newFan = (agent: OAgent = "claude"): FanRow => ({ key: ++fanKey, title: "", prompt: "", agent, workspace: "shared", model: "" });
const MAX_FAN = 8;
const OTHER = "__other_folder__";
// ACP agents get the two modes that make sense without Claude's plan and auto modes.
export const ACP_MODES = PERM_MODES.filter((m) => m.key === "manual" || m.key === "acceptEdits");
export const kindOf = (id: string, agents: OAgentInfo[]) => agents.find((a) => a.id === id)?.kind || (id === "claude" || id === "codex" ? id : "acp");
export const agentSub = (a: OAgentInfo, inTerm = false) =>
  !a.installed
    ? "not installed"
    : a.kind === "codex" || a.kind === "claude"
      ? inTerm
        ? "CLI in tmux"
        : a.kind === "codex"
          ? "app-server · headless"
          : "stream-json · headless"
      : `ACP${a.version ? ` · ${a.version}` : ""}`;

interface FormSig {
  agent: string;
  kind: string;
  prompt: string;
  model: string;
  sandbox: OSandbox;
  perm: OPermissionMode;
  wspace: ORootWorkspace;
  fan: FanRow[];
  where: OWhere;
}
/** Every field that changes what runs; fields that do not apply to the agent kind are left out. */
function sigOf(f: FormSig): string {
  return JSON.stringify([
    f.agent,
    f.prompt,
    f.model.trim(),
    f.kind === "codex" ? f.sandbox : f.kind === "acp" && f.perm !== "acceptEdits" ? "manual" : f.perm,
    f.where,
    f.where === "terminal" ? "" : f.wspace,
    f.fan.map((r) => [r.agent, r.title, r.prompt, r.workspace, r.model.trim()]),
  ]);
}

export function toSpec(r: FanRow): OChildSpec {
  const prompt = r.prompt.trim();
  const title = r.title.trim() || prompt.split("\n")[0].slice(0, 80);
  const base = { title, prompt, agent: r.agent, workspace: r.workspace, model: r.model.trim() || undefined };
  return r.agent === "codex" ? { ...base, sandbox: r.workspace === "read-only" ? "read-only" : "workspace-write" } : base;
}

export function ModelPick({ agent, agents, value, onChange, full }: { agent: string; agents: OAgentInfo[]; value: string; onChange: (v: string) => void; full?: boolean }) {
  const kind = kindOf(agent, agents);
  const cls = `inp${full ? " wfull" : ""}`;
  if (kind === "codex") return <input className={cls} value={value} placeholder="Codex default" aria-label="Model" onChange={(e) => onChange(e.target.value)} />;
  const models = kind === "claude" ? [{ id: "opus", name: "Opus" }, { id: "sonnet", name: "Sonnet" }] : modelsOf(agents.find((a) => a.id === agent));
  return (
    <select className={cls} value={value} aria-label="Model" disabled={kind === "acp" && !models.length} onChange={(e) => onChange(e.target.value)}>
      <option value="">{agentLabel(agent)} default</option>
      {models.map((m) => (
        <option key={m.id} value={m.id}>
          {m.name || m.id}
        </option>
      ))}
    </select>
  );
}

/** A footer chip that is a native select: the chip shows the value, the hidden select takes the click. */
function ChipSelect({ label, value, shown, onChange, title, disabled, className = "", children }: { label: string; value: string; shown: React.ReactNode; onChange: (v: string) => void; title?: string; disabled?: boolean; className?: string; children: React.ReactNode }) {
  return (
    <label className={`wchip wcsel ${disabled ? "off" : ""} ${className}`} title={title || label}>
      {shown}
      <span aria-hidden className="wchev">▾</span>
      <select value={value} aria-label={label} disabled={disabled} onChange={(e) => onChange(e.target.value)}>
        {children}
      </select>
    </label>
  );
}

export function FanRowEdit({ r, i, agents, onChange, onRemove }: { r: FanRow; i: number; agents: OAgentInfo[]; onChange: (r: FanRow) => void; onRemove: () => void }) {
  const set = (p: Partial<FanRow>) => onChange({ ...r, ...p });
  return (
    <div className="wfan-r" role="group" aria-label={`Subagent ${i + 1}`}>
      <div className="wfan-top">
        <span className="ix">{i + 1}</span>
        <input className="inp" value={r.title} placeholder="Title, for example Tests for the token flow" aria-label="Title" onChange={(e) => set({ title: e.target.value })} />
        <button className="btn sm ghost wfan-x" onClick={onRemove} aria-label={`Remove subagent ${i + 1}`} title="Remove">
          ✕
        </button>
      </div>
      <textarea className="inp wfan-p" rows={2} value={r.prompt} placeholder="What this subagent does" aria-label="Prompt" onChange={(e) => set({ prompt: e.target.value })} />
      <div className="wfan-opts">
        <select className="inp" value={r.agent} aria-label="Agent" onChange={(e) => set({ agent: e.target.value as OAgent, model: "" })}>
          {agents.map((a) => (
            <option key={a.id} value={a.id} disabled={!a.installed}>
              {a.name}
              {a.installed ? "" : " (not installed)"}
            </option>
          ))}
        </select>
        <select className="inp" value={r.workspace} aria-label="Workspace" onChange={(e) => set({ workspace: e.target.value as OWorkspace })}>
          <option value="shared">Shared folder</option>
          <option value="worktree">Own worktree</option>
          <option value="read-only">Read only</option>
        </select>
        <ModelPick agent={r.agent} agents={agents} value={r.model} onChange={(model) => set({ model })} />
      </div>
    </div>
  );
}

export function NewThread({ onBack }: { onBack?: () => void }) {
  const orch = useOrch();
  const reg = useAgents();
  const agents = reg.list;
  const nav = useNavigate();
  const recent = useRecentFolders();
  const [cwd, setCwd] = useState("");
  const [agent, setAgent] = useState<OAgent>("codex");
  const [sandbox, setSandbox] = useState<OSandbox>("read-only");
  const [model, setModel] = useState("");
  const [claudeModel, setClaudeModel] = useState("");
  const [acpModel, setAcpModel] = useState("");
  const [perm, setPerm] = useState<OPermissionMode>("manual");
  const [prompt, setPrompt] = useDraft("new");
  useDocTitle("New thread");
  const att = useAttachments("new");
  const [busy, setBusy] = useState(false);
  const [fan, setFan] = useState<FanRow[]>([]);
  const [wspace, setWspace] = useState<ORootWorkspace>("here");
  const [whereSel, setWhere] = useState<OWhere>("terminal");
  const [policy, setPolicy] = useState<OApprovalPolicy>("on-request");
  const term = useTermStart();
  const [fail, setFail] = useState<{ kind: "fanout" | "spend"; msg: string } | null>(null);
  const [opts, setOpts] = useState(false);
  const cwdRef = useRef<HTMLInputElement>(null);
  const [tpls, setTpls] = useState<OTemplate[] | null>(null);
  const [tpl, setTpl] = useState<OTemplate | null>(null);
  const [vars, setVars] = useState<Record<string, string>>({});
  useEffect(() => {
    p7.templates().then(setTpls, () => setTpls([]));
  }, []);
  const tplVars = tpl ? varsOf(tpl) : [];
  // The form as the template filled it; a run goes through the template route only while the form still matches.
  const tplSig = useRef("");
  const useTpl = (x: OTemplate) => {
    setTpl(x);
    setVars({});
    setPrompt(x.prompt);
    setAgent(x.agent);
    const k = kindOf(x.agent, agents);
    const f: FormSig = { agent: x.agent, kind: k, prompt: x.prompt, model: x.model || "", sandbox: "read-only", perm: x.permissionMode || "manual", wspace: x.workspace === "worktree" ? "worktree" : "here", fan: [], where: tplWhere(x) };
    setWhere(f.where);
    if (k === "codex") {
      f.sandbox = x.sandbox || (x.workspace === "read-only" ? "read-only" : "workspace-write");
      setSandbox(f.sandbox);
      setModel(f.model);
    } else if (k === "claude") {
      setClaudeModel(f.model);
      setPerm(f.perm);
    } else {
      setAcpModel(f.model);
      setPerm(f.perm);
    }
    setWspace(f.wspace);
    f.fan = (x.children || []).map((c) => ({ ...newFan(c.agent), title: c.title, prompt: c.prompt, workspace: c.workspace, model: c.model || "" }));
    setFan(f.fan);
    if (f.fan.length) setOpts(true);
    tplSig.current = sigOf(f);
  };
  const kind = kindOf(agent, agents);
  // Fan-out rows run headless, so a lead with rows runs headless too.
  const termOff = !TERM_AGENTS.has(kind) ? `${agentLabel(agent)} runs headless only.` : fan.length ? "Subagents run headless, so the lead does too." : "";
  const where: OWhere = termOff ? "headless" : whereSel;
  const inTerm = where === "terminal";
  const acpModels = kind === "acp" ? modelsOf(agents.find((a) => a.id === agent)) : [];
  const usable = agents.find((a) => a.id === agent)?.installed !== false;
  const fanFilled = fan.filter((r) => r.prompt.trim());
  const fanHalf = fan.some((r) => r.title.trim() && !r.prompt.trim());
  const missing = orch.status === "missing";
  // The default folder is copied into the field once, so a busy session elsewhere cannot move it before Start.
  const seeded = useRef(false);
  useEffect(() => {
    if (seeded.current || !recent[0]) return;
    seeded.current = true;
    setCwd((c) => c || recent[0]);
  }, [recent]);
  const dir = cwd;
  const tplSame = !!tpl && sigOf({ agent, kind, prompt, model: kind === "codex" ? model : kind === "claude" ? claudeModel : acpModel, sandbox, perm, wspace, fan, where }) === tplSig.current;
  const varsMissing = tplVars.some((k) => !vars[k]?.trim());
  const can = !!dir.trim() && !!prompt.trim() && !busy && !missing && !fanHalf && usable && !varsMissing && !att.uploading;
  const files = att.ids.length ? att.ids : undefined;

  const start = async () => {
    if (!can) return;
    setBusy(true);
    setFail(null);
    const from = window.location.pathname;
    const go = (path: string) => window.location.pathname === from && nav(path); // not when the user went elsewhere meanwhile
    try {
      // An unchanged template runs through its own route, so review and server-side variables apply.
      if (tpl && tplSame && !files) {
        const r = await p7.runTemplate(tpl.id, dir.trim(), tplVars.length ? Object.fromEntries(tplVars.map((k) => [k, vars[k].trim()])) : undefined);
        if (r.terminal) term.follow(tpl.agent, r.terminal);
        else if (r.thread) go(`/w/${encodeURIComponent(r.thread.id)}`);
        setPrompt(clearSent(prompt));
        att.clear();
        setBusy(false);
        return;
      }
      const vv = { ...vars, cwd: dir.trim(), date: new Date().toISOString().slice(0, 10) };
      if (inTerm) {
        const tb = { agent: kind as "claude" | "codex", cwd: dir.trim(), prompt: fillVars(prompt.trim(), vv), attachments: files };
        await term.start(kind === "codex" ? { ...tb, sandbox, approvalPolicy: policy, model: model.trim() || undefined } : { ...tb, permissionMode: perm, model: claudeModel || undefined });
        setPrompt(clearSent(prompt));
        att.clear();
        setBusy(false);
        return;
      }
      const base = { agent, cwd: dir.trim(), attachments: files, prompt: fillVars(prompt.trim(), vv), children: fanFilled.length ? fanFilled.map((r) => toSpec({ ...r, prompt: fillVars(r.prompt, vv) })) : undefined, workspace: wspace === "worktree" ? wspace : undefined };
      const t = await createThread(
        kind === "codex" ? { ...base, sandbox, model: model.trim() || undefined } : kind === "claude" ? { ...base, permissionMode: perm, model: claudeModel || undefined } : { ...base, permissionMode: perm === "acceptEdits" ? perm : "manual", model: acpModel || undefined },
      );
      setPrompt(clearSent(prompt));
      att.clear();
      go(`/w/${encodeURIComponent(t.id)}`);
    } catch (e) {
      const st = e instanceof ApiError ? e.status : 0;
      if (st === 422 && fanFilled.length) setFail({ kind: "fanout", msg: (e as Error).message });
      else if (st === 402) setFail({ kind: "spend", msg: (e as Error).message });
      else showToast("Could not start the thread", (e as Error).message, { kind: "alert" });
      setBusy(false);
    }
  };

  return (
    <section className="wchat wnewpage" aria-label="New thread">
      {onBack && (
        <div className="wbar">
          <button className="btn sm ghost wback" onClick={onBack} aria-label="Back to threads">
            ‹
          </button>
          <b>New thread</b>
        </div>
      )}
      <div className="wfeed">
        {term.st && term.st.phase !== "error" ? (
          <div className="wnt">
            <h2>{term.st.phase === "slow" ? "Still starting" : "Starting in a terminal"}</h2>
            <p className="sub">The session opens here when the agent links it. You can also attach to it now.</p>
            <TermStarting st={term.st} onClose={term.reset} />
          </div>
        ) : (
          <div className="wnt">
            <h2>What should the agent do?</h2>
            <p className="sub">{inTerm ? "The agent runs in a tmux session on this Mac. You can follow it from the phone." : "The thread runs on this Mac. You can follow it from the phone."}</p>
            {tpls && tpls.length > 0 && (
              <div className="wtplrow">
                <span className="muted">{tpl ? "From template" : "Start from template"}</span>
                {tpl ? (
                  <>
                    <b>{tpl.name}</b>
                    <button
                      className="wlink"
                      onClick={() => {
                        if (prompt === tpl.prompt) setPrompt("");
                        setFan([]);
                        setTpl(null);
                      }}
                    >
                      Clear
                    </button>
                  </>
                ) : (
                  tpls.slice(0, 4).map((x) => (
                    <button key={x.id} className="wlink" title={x.description || x.prompt} onClick={() => useTpl(x)}>
                      {x.name}
                    </button>
                  ))
                )}
                <Link className="wlink muted" to="/settings/templates">
                  {tpl ? "Edit templates" : "All templates"}
                </Link>
              </div>
            )}
            {tpl && !tplSame && <p className="sub">You changed the template's settings. Start runs a normal thread with the values below.</p>}
            {tplVars.length > 0 && (
              <div className="wtplvars">
                {tplVars.map((k) => (
                  <label key={k}>
                    <span className="mono">{`{{${k}}}`}</span>
                    <input className="inp" value={vars[k] || ""} placeholder={`Value for ${k}`} onChange={(e) => setVars({ ...vars, [k]: e.target.value })} />
                  </label>
                ))}
              </div>
            )}
            {missing && (
              <div className="wmissing">
                <b>Orchestrator API not available on this daemon</b>
                <span>This build does not serve /api/orch yet, so it cannot start threads.</span>
              </div>
            )}
            <Composer value={prompt} onChange={setPrompt} onSubmit={() => void start()} rows={4} autoFocus placeholder="Describe the task. Attach or drop files. ⌘↵ starts" attach={att} cwd={dir.trim() || undefined} files>
              <ChipSelect
                label="Folder"
                title={dir || "Pick a folder"}
                value={dir}
                shown={<span className="trunc">▣ {projectName(dir) || "pick a folder"}</span>}
                onChange={(v) => {
                  if (v !== OTHER) return setCwd(v);
                  setOpts(true);
                  window.setTimeout(() => cwdRef.current?.focus(), 0);
                }}
              >
                {!dir && <option value="">Pick a folder</option>}
                {[...new Set([...(dir ? [dir] : []), ...recent])].map((c) => (
                  <option key={c} value={c}>
                    {projectName(c)} ({c})
                  </option>
                ))}
                <option value={OTHER}>Other folder…</option>
              </ChipSelect>
              <ChipSelect
                label="Agent"
                value={agent}
                shown={
                  <>
                    <span className={`wag ${agentCls(agent)}`} />
                    <span className="trunc">{agents.find((a) => a.id === agent)?.name || agentLabel(agent)}</span>
                  </>
                }
                onChange={(v) => {
                  setAgent(v as OAgent);
                  setAcpModel("");
                }}
              >
                {agents.map((a) => (
                  <option key={a.id} value={a.id} disabled={!a.installed}>
                    {a.name}
                    {a.installed ? "" : " (not installed)"}
                  </option>
                ))}
              </ChipSelect>
              {kind === "codex" ? (
                <ChipSelect label="Sandbox" value={sandbox} shown={sandbox === "read-only" ? "Read only" : "Can edit"} onChange={(v) => setSandbox(v as OSandbox)}>
                  <option value="read-only">Read only</option>
                  <option value="workspace-write">Can edit the workspace</option>
                </ChipSelect>
              ) : (
                <ChipSelect label="Permission mode" value={kind === "acp" && perm !== "acceptEdits" ? "manual" : perm} shown={permModeLabel(kind === "acp" && perm !== "acceptEdits" ? "manual" : perm)} onChange={(v) => setPerm(v as OPermissionMode)}>
                  {(kind === "acp" ? ACP_MODES : PERM_MODES).map((m) => (
                    <option key={m.key} value={m.key}>
                      {m.label}
                    </option>
                  ))}
                </ChipSelect>
              )}
              <ChipSelect label="Where it runs" title={termOff || "Where it runs"} value={where} shown={inTerm ? "In tmux" : "Headless"} disabled={!!termOff} onChange={(v) => setWhere(v as OWhere)}>
                <option value="terminal">In a terminal (tmux)</option>
                <option value="headless">Headless</option>
              </ChipSelect>
              {!inTerm && (
                <ChipSelect label="Workspace" className="hidephone" value={wspace} shown={wspace === "worktree" ? "New worktree" : "This checkout"} onChange={(v) => setWspace(v as ORootWorkspace)}>
                  <option value="here">This checkout</option>
                  <option value="worktree">New worktree</option>
                </ChipSelect>
              )}
              <span className="sp" />
              <button className="btn pri wgo" disabled={!can} onClick={start} title={att.uploading ? "Wait for the files to upload" : "Start (⌘↵)"}>
                {busy ? "Starting…" : fanFilled.length ? `Start with ${fanFilled.length} ${fanFilled.length === 1 ? "subagent" : "subagents"}` : "Start"}
              </button>
            </Composer>

            <details className="wntopts" open={opts} onToggle={(e) => setOpts((e.currentTarget as HTMLDetailsElement).open)}>
              <summary>Options</summary>
              <div className="wntopts-b">
                <h4 className="wh">Folder</h4>
                <input ref={cwdRef} className="inp wfull" list="w-cwds" value={cwd} placeholder="/path/to/repo" onChange={(e) => setCwd(e.target.value)} aria-label="Working folder" />
                <datalist id="w-cwds">
                  {recent.map((c) => (
                    <option key={c} value={c} />
                  ))}
                </datalist>
                {recent.length > 0 && (
                  <div className="wrecent">
                    {recent.slice(0, 6).map((c) => (
                      <button key={c} className={`chip t ${dir === c ? "on" : ""}`} onClick={() => setCwd(c)} title={c}>
                        {projectName(c)}
                      </button>
                    ))}
                  </div>
                )}

                {!inTerm && (
                  <>
                    <h4 className="wh">Workspace</h4>
                    <div className="wpick two" role="radiogroup" aria-label="Workspace">
                      <button className="wopt" role="radio" aria-checked={wspace === "here"} aria-pressed={wspace === "here"} onClick={() => setWspace("here")}>
                        <b>This checkout</b>
                        <span>Works in the folder as it is, on its current branch</span>
                      </button>
                      <button className="wopt" role="radio" aria-checked={wspace === "worktree"} aria-pressed={wspace === "worktree"} onClick={() => setWspace("worktree")}>
                        <b>New worktree</b>
                        <span>A separate git worktree on a new am/ branch. Needs a git repo.</span>
                      </button>
                    </div>
                  </>
                )}

                <h4 className="wh">Agent</h4>
                <div className="wpick wagents" role="radiogroup" aria-label="Agent">
                  {agents.map((a) => (
                    <button
                      key={a.id}
                      className={`wopt ${a.installed ? "" : "off"}`}
                      role="radio"
                      aria-checked={agent === a.id}
                      aria-pressed={agent === a.id}
                      disabled={!a.installed}
                      title={a.installed ? a.path || a.name : `${a.name} is not installed on this Mac`}
                      onClick={() => {
                        setAgent(a.id);
                        setAcpModel("");
                      }}
                    >
                      <b>
                        <span className={`wag ${agentCls(a.id)}`} />
                        <span className="trunc">{a.name}</span>
                      </b>
                      <span>{agentSub(a, inTerm)}</span>
                    </button>
                  ))}
                </div>
                {reg.error && <span className="muted sm">The agent list did not load ({reg.error}). Showing Codex and Claude Code.</span>}

                <h4 className="wh">Where</h4>
                <WherePick where={where} onChange={setWhere} termOff={termOff} />

                {kind === "codex" ? (
                  <div className="wfields">
                    <label>
                      <h4 className="wh">Sandbox</h4>
                      <select className="inp wfull" value={sandbox} onChange={(e) => setSandbox(e.target.value as OSandbox)}>
                        <option value="read-only">Read only</option>
                        <option value="workspace-write">Workspace write</option>
                      </select>
                    </label>
                    {inTerm && (
                      <label>
                        <h4 className="wh">Approval policy</h4>
                        <select className="inp wfull" value={policy} onChange={(e) => setPolicy(e.target.value as OApprovalPolicy)}>
                          <option value="untrusted">Untrusted: ask before most commands</option>
                          <option value="on-request">On request: Codex asks when it needs to</option>
                          <option value="never">Never ask</option>
                        </select>
                      </label>
                    )}
                    <label>
                      <h4 className="wh">Model (optional)</h4>
                      <input className="inp wfull" value={model} placeholder="Codex default" onChange={(e) => setModel(e.target.value)} />
                    </label>
                  </div>
                ) : (
                  <div className="wfields">
                    <label>
                      <h4 className="wh">Permission mode</h4>
                      <select className="inp wfull" value={kind === "acp" && perm !== "acceptEdits" ? "manual" : perm} onChange={(e) => setPerm(e.target.value as OPermissionMode)}>
                        {(kind === "acp" ? ACP_MODES : PERM_MODES).map((m) => (
                          <option key={m.key} value={m.key}>
                            {m.label}
                          </option>
                        ))}
                      </select>
                      <span className="wfhint">{(kind === "acp" ? ACP_MODES : PERM_MODES).find((m) => m.key === perm)?.hint.replace("Claude", agentLabel(agent)) || ACP_MODES[0].hint}</span>
                    </label>
                    {kind === "claude" ? (
                      <label>
                        <h4 className="wh">Model</h4>
                        <ModelPick agent="claude" agents={agents} value={claudeModel} onChange={setClaudeModel} full />
                      </label>
                    ) : acpModels.length > 0 ? (
                      <label>
                        <h4 className="wh">Model</h4>
                        <ModelPick agent={agent} agents={agents} value={acpModel} onChange={setAcpModel} full />
                      </label>
                    ) : (
                      <label>
                        <h4 className="wh">Model</h4>
                        <span className="wfhint">{agentLabel(agent)} uses its own configured model.</span>
                      </label>
                    )}
                  </div>
                )}

                <h4 className="wh">Fan-out (optional)</h4>
                {fan.length > 0 && (
                  <div className="wfan">
                    {fan.map((r, i) => (
                      <FanRowEdit key={r.key} r={r} i={i} agents={agents} onChange={(n) => setFan((f) => f.map((x) => (x.key === r.key ? n : x)))} onRemove={() => setFan((f) => f.filter((x) => x.key !== r.key))} />
                    ))}
                  </div>
                )}
                <div className="wfan-add">
                  <button className="btn sm" disabled={fan.length >= MAX_FAN} onClick={() => setFan((f) => (f.length >= MAX_FAN ? f : [...f, newFan(f.length % 2 ? "codex" : "claude")]))}>
                    ＋ Add subagent
                  </button>
                  <span className="muted sm">
                    {fan.length >= MAX_FAN
                      ? `At most ${MAX_FAN} subagents.`
                      : fanHalf
                        ? "Each subagent with a title also needs a prompt."
                        : fan.length
                          ? "They start with the lead agent, which hears from them when they finish. All of them run headless."
                          : "The lead agent can also start subagents by itself with delegate_task."}
                  </span>
                </div>
              </div>
            </details>
            {term.st?.phase === "error" && <TermStarting st={term.st} onClose={term.reset} />}
            {fail && (
              <div className="wnewerr" role="alert">
                <b>{fail.kind === "fanout" ? "agent-monitor rolled back the fan-out" : "Today's spend limit is reached"}</b>
                <span>{fail.msg}</span>
                <span className="muted">
                  {fail.kind === "fanout" ? (
                    "One subagent did not start. agent-monitor stopped and closed the subagents that started and the lead thread, so nothing runs now. Fix the subagent and start again."
                  ) : (
                    <>
                      Raise the limit in <Link to="/settings/limits">Settings, Limits</Link>, or wait until tomorrow.
                    </>
                  )}
                </span>
              </div>
            )}
          </div>
        )}
      </div>
    </section>
  );
}

/** The header "+" that opens a new thread from any thread; c does the same from the keyboard. */
export function NewThreadBtn() {
  return (
    <Link className="btn sm ghost wnewb" to="/w/new" title="New thread (c)" aria-label="New thread">
      <svg viewBox="0 0 16 16" width="14" height="14" aria-hidden>
        <path d="M8 3v10M3 8h10" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round" />
      </svg>
    </Link>
  );
}
