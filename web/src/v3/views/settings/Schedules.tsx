// Settings → Schedules: run a template in a folder on a 5-field cron, inside the daemon.
import { useEffect, useState } from "react";
import { Link, useNavigate } from "react-router-dom";
import { showToast } from "../../../app/toast";
import { fmtAgo, projectName, shortCwd } from "../../../lib/format";
import { isMissing } from "../../api/client";
import { agentCls, toMs } from "../../api/orch";
import { tplVars, tplWhere, p7, type OSchedule, type OTemplate } from "../../api/p7";
import { TermStarting, useTermStart } from "../workspace/TermStart";
import { ErrorState, Loading } from "../../components/ui";
import { CRON_PRESETS, describeCron, fmtRun, nextRuns, parseCron } from "./cron";
import { SettingsTabs, Switch } from "./Tabs";
import { FolderPick } from "./Templates";
import "../../styles/settings.css";
import "../../styles/p6.css";
import "../../styles/workspace.css";
import "../../styles/p7.css";

function cronInfo(expr: string): { text: string; next: number[]; err: string } {
  try {
    return { text: describeCron(expr), next: nextRuns(parseCron(expr), 3), err: "" };
  } catch (e) {
    return { text: "", next: [], err: (e as Error).message };
  }
}

interface Preview {
  text: string;
  next: number[];
  err: string;
  from: "daemon" | "browser" | "wait";
}
/** The daemon owns validation and next runs; the browser parser is only a fallback when the daemon cannot answer. */
function useCronPreview(expr: string): Preview {
  const [p, setP] = useState<Preview>({ text: "", next: [], err: "", from: "wait" });
  useEffect(() => {
    let alive = true;
    setP((x) => ({ ...x, from: "wait" }));
    const t = window.setTimeout(() => {
      p7.cronPreview(expr.trim()).then(
        (r) => alive && setP(r.valid ? { text: describeCron(expr), next: (r.next || []).slice(0, 3), err: "", from: "daemon" } : { text: "", next: [], err: r.error || "The daemon rejects this expression.", from: "daemon" }),
        () => alive && setP({ ...cronInfo(expr), from: "browser" }),
      );
    }, 250);
    return () => {
      alive = false;
      window.clearTimeout(t);
    };
  }, [expr]);
  return p;
}

const tzName = () => {
  try {
    return Intl.DateTimeFormat().resolvedOptions().timeZone;
  } catch {
    return "";
  }
};

function ScheduleForm({ init, templates, onSave, onCancel }: { init: OSchedule; templates: OTemplate[]; onSave: (s: OSchedule) => Promise<void>; onCancel: () => void }) {
  const [s, setS] = useState<OSchedule>(init);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState("");
  const set = (p: Partial<OSchedule>) => setS({ ...s, ...p });
  const tpl = templates.find((t) => t.id === s.templateId);
  const names = tpl ? tplVars(tpl) : [];
  const headlessOnly = !!tpl && tplWhere({ ...tpl, where: "terminal" }) === "headless";
  const info = useCronPreview(s.cron);
  const can = !!tpl && !!s.cwd.trim() && info.from !== "wait" && !info.err && names.every((k) => s.vars?.[k]?.trim()) && !busy;
  const save = async () => {
    setBusy(true);
    setErr("");
    try {
      const vars = names.length ? Object.fromEntries(names.map((k) => [k, s.vars![k].trim()])) : undefined;
      await onSave({ ...s, cwd: s.cwd.trim(), cron: s.cron.trim().replace(/\s+/g, " "), vars, where: headlessOnly ? "" : s.where || "" });
    } catch (e) {
      setErr((e as Error).message);
    }
    setBusy(false);
  };
  return (
    <div className="ibox p7-form">
      <h3>{init.id ? "Edit schedule" : "New schedule"}</h3>
      <label className="p7-l" htmlFor="sch-tpl">
        Template
      </label>
      <select id="sch-tpl" className="inp wfull" value={s.templateId} onChange={(e) => set({ templateId: e.target.value })}>
        <option value="" disabled>
          Pick a template
        </option>
        {templates.map((t) => (
          <option key={t.id} value={t.id}>
            {t.name}
          </option>
        ))}
      </select>
      {tpl?.description && <span className="wfhint">{tpl.description}</span>}
      <label className="p7-l">Folder</label>
      <FolderPick id="sch" value={s.cwd} onChange={(cwd) => set({ cwd })} />
      {names.map((k) => (
        <label key={k} className="p7-var">
          <span className="mono">{`{{${k}}}`}</span>
          <input className="inp" value={s.vars?.[k] || ""} placeholder={`Value for ${k}`} onChange={(e) => set({ vars: { ...(s.vars || {}), [k]: e.target.value } })} />
        </label>
      ))}
      <label className="p7-l" htmlFor="sch-where">
        Where
      </label>
      <select id="sch-where" className="inp wfull" value={headlessOnly ? "headless" : s.where || ""} disabled={headlessOnly} onChange={(e) => set({ where: e.target.value as OSchedule["where"] })}>
        <option value="">{`As the template says (${tpl && tplWhere(tpl) === "terminal" ? "in a terminal" : "headless"})`}</option>
        <option value="terminal">In a terminal</option>
        <option value="headless">Headless</option>
      </select>
      <span className="wfhint">
        {headlessOnly
          ? tpl?.children?.length
            ? "This template has subagents, so it always runs headless."
            : "This agent runs headless only."
          : "In a terminal starts a tmux session you can attach to. Headless runs with no terminal."}
      </span>
      <label className="p7-l" htmlFor="sch-cron">
        When
      </label>
      <div className="p7-cron">
        <input id="sch-cron" className="inp mono" value={s.cron} placeholder="0 9 * * 1-5" spellCheck={false} onChange={(e) => set({ cron: e.target.value })} />
        <div className="wrecent">
          {CRON_PRESETS.map((p) => (
            <button key={p.cron} type="button" className={`chip t ${s.cron.trim() === p.cron ? "on" : ""}`} onClick={() => set({ cron: p.cron })}>
              {p.label}
            </button>
          ))}
        </div>
      </div>
      <div className="p7-cronx" aria-live="polite">
        {info.from === "wait" && !info.text && !info.err ? (
          <span className="muted">Checking with the daemon…</span>
        ) : info.err ? (
          <span className="err-t">{info.err}</span>
        ) : (
          <>
            <b>{info.text}</b>
            <span className="muted">
              Next: {info.next.map((t) => fmtRun(t)).join(", ") || "none in the next five years"}
              {tzName() ? ` (shown in ${tzName()})` : ""}
            </span>
          </>
        )}
        {info.from === "browser" && <span className="muted xs">The daemon did not answer. This preview comes from the browser and uses the browser's time zone.</span>}
      </div>
      <span className="wfhint">minute hour day month weekday, in this Mac's local time. A run never starts while the previous one still runs. Runs missed while the daemon was down are not replayed; at most one catch-up run.</span>
      <div className="p7-sw">
        <Switch on={s.enabled} label="Enabled" onChange={(enabled) => set({ enabled })} />
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
          {busy ? "Saving…" : "Save schedule"}
        </button>
      </div>
    </div>
  );
}

export default function Schedules() {
  const nav = useNavigate();
  const [list, setList] = useState<OSchedule[] | null>(null);
  const [tpls, setTpls] = useState<OTemplate[]>([]);
  const [err, setErr] = useState("");
  const [missing, setMissing] = useState(false);
  const [edit, setEdit] = useState<OSchedule | null>(null);
  const [del, setDel] = useState("");
  const [busy, setBusy] = useState("");
  const [termOf, setTermOf] = useState("");
  const term = useTermStart();
  const load = () => {
    p7.templates().then(setTpls, () => setTpls([]));
    return p7.schedules().then(
      (l) => {
        setList(l);
        setErr("");
      },
      (e) => (isMissing(e) ? (setMissing(true), setList([])) : setErr((e as Error).message)),
    );
  };
  useEffect(() => {
    void load();
  }, []);
  const save = async (s: OSchedule) => {
    await p7.saveSchedule(s);
    setEdit(null);
    void load();
  };
  const toggle = async (s: OSchedule, enabled: boolean) => {
    setList((l) => l?.map((x) => (x.id === s.id ? { ...x, enabled } : x)) || l);
    try {
      await p7.saveSchedule({ ...s, enabled });
    } catch (e) {
      showToast("Could not change the schedule", (e as Error).message, { kind: "alert" });
      void load();
    }
  };
  const runNow = async (s: OSchedule) => {
    setBusy(s.id);
    try {
      const r = await p7.runSchedule(s.id);
      if (r.terminal) {
        setTermOf(s.id);
        term.follow(tplOf(s)?.agent || "claude", r.terminal);
        void load();
      } else if (r.thread) nav(`/w/${encodeURIComponent(r.thread.id)}`);
      else void load();
    } catch (e) {
      showToast("Could not run now", (e as Error).message, { kind: "alert" });
    }
    setBusy("");
  };
  const tplOf = (s: OSchedule) => tpls.find((t) => t.id === s.templateId);
  const remove = async (s: OSchedule) => {
    try {
      await p7.deleteSchedule(s.id);
      setDel("");
      void load();
    } catch (e) {
      showToast("Could not delete", (e as Error).message, { kind: "alert" });
    }
  };
  return (
    <div className="view pad sx p7 ws wsform">
      <SettingsTabs />
      <div className="viewhead">
        <div>
          <h2>Schedules</h2>
          <p>Run a template in a folder at set times. The daemon starts the thread itself, so it runs only while agent-monitor runs. Spend limits apply.</p>
        </div>
        {!edit && (
          <button className="btn" disabled={missing || !tpls.length} onClick={() => setEdit({ id: "", templateId: tpls[0]?.id || "", cwd: "", cron: "0 9 * * 1-5", enabled: true })}>
            New schedule
          </button>
        )}
      </div>
      {err && !list ? (
        <ErrorState error={err} onRetry={load} />
      ) : !list ? (
        <Loading rows={3} />
      ) : (
        <div className="sx-col">
          {missing && <div className="sx-warn">This daemon has no schedule routes yet. Update agent-monitor to add schedules.</div>}
          {edit && <ScheduleForm key={edit.id || "new"} init={edit} templates={tpls} onSave={save} onCancel={() => setEdit(null)} />}
          {list.length > 0 && (
            <div className="ibox">
              <ul className="p7-list">
                {list.map((s) => {
                  const tpl = tpls.find((t) => t.id === s.templateId);
                  const info = cronInfo(s.cron);
                  const next = s.enabled ? toMs(s.nextRunAt) : 0;
                  return (
                    <li key={s.id} className={`p7-row ${s.enabled ? "" : "off"}`}>
                      <div className="p7-rm">
                        <div className="p7-rt">
                          <span className={`wag ${agentCls(tpl?.agent)}`} aria-hidden />
                          <b>{tpl?.name || "Missing template"}</b>
                          <span className="muted" title={s.cwd}>
                            in {projectName(s.cwd) || shortCwd(s.cwd)}
                          </span>
                        </div>
                        <div className="p7-rd">
                          {info.text || s.cron} <span className="muted mono">{s.cron}</span>
                        </div>
                        <div className="p7-rs">
                          {s.enabled ? (next ? `Next ${fmtRun(next)}` : "No next run") : "Paused"}
                          {s.lastRunAt ? (
                            <>
                              {" · Last "}
                              {s.lastThreadId ? <Link to={`/w/${encodeURIComponent(s.lastThreadId)}`}>{fmtAgo(toMs(s.lastRunAt))}</Link> : fmtAgo(toMs(s.lastRunAt))}
                              {s.lastSession && !s.lastThreadId ? <span title={`tmux attach -t ${s.lastSession}`}>{` in tmux ${s.lastSession}`}</span> : ""}
                              {s.lastCatchUp ? " (catch-up run)" : ""}
                            </>
                          ) : (
                            " · Never ran"
                          )}
                          {s.vars && Object.keys(s.vars).length > 0 && ` · ${Object.entries(s.vars).map(([k, v]) => `${k}: ${v}`).join(", ")}`}
                        </div>
                        {s.lastError && <div className="p7-rs err-t">{s.lastError}</div>}
                        {s.where && <div className="p7-rs">{s.where === "terminal" ? "Runs in a terminal" : "Runs headless"}</div>}
                      </div>
                      <div className="p7-ra">
                        <Switch on={s.enabled} label="Enabled" onChange={(v) => toggle(s, v)} />
                        <button className="wlink" disabled={busy === s.id || !tpl} onClick={() => runNow(s)}>
                          {busy === s.id ? "Starting…" : "Run now"}
                        </button>
                        <button className="wlink" onClick={() => setEdit(structuredClone(s))}>
                          Edit
                        </button>
                        <button className="wlink" onClick={() => setDel(del === s.id ? "" : s.id)}>
                          Delete
                        </button>
                      </div>
                      {termOf === s.id && term.st && (
                        <div className="p7-run">
                          <TermStarting st={term.st} compact onClose={() => (term.reset(), setTermOf(""))} />
                        </div>
                      )}
                      {del === s.id && (
                        <div className="sx-confirm">
                          <span>Delete this schedule? Threads it started stay.</span>
                          <button className="btn sm deny" onClick={() => remove(s)}>
                            Delete
                          </button>
                          <button className="btn sm ghost" onClick={() => setDel("")}>
                            Cancel
                          </button>
                        </div>
                      )}
                    </li>
                  );
                })}
              </ul>
            </div>
          )}
          {!missing && !list.length && !edit && (
            <div className="muted">
              No schedules yet. {tpls.length ? "Add one to run a template at set times." : <>First add a <Link to="/settings/templates">template</Link>.</>}
            </div>
          )}
        </div>
      )}
    </div>
  );
}
