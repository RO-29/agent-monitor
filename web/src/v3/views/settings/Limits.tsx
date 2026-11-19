// Settings → Limits: spend per driven thread and per day, and token rates for agents that report no cost (Codex).
import { useEffect, useState } from "react";
import { isMissing } from "../../api/client";
import type { OSpendStatus } from "../../api/orch";
import { DEFAULT_SPEND, settingsApi, type CodexRate, type SpendSettings } from "../../api/settings";
import { SpendBar } from "../../components/Spend";
import { UsagePanel } from "../../components/Usage";
import { ErrorState, Loading } from "../../components/ui";
import { SettingsTabs } from "./Tabs";
import "../../styles/settings.css";
import "../../styles/p6.css";

const num = (v: string) => Math.max(0, Number(v.replace(/[^0-9.]/g, "")) || 0);

function UsdField({ id, label, value, onChange, hint }: { id: string; label: string; value: string; onChange: (v: string) => void; hint: string }) {
  return (
    <div className="sx-field">
      <label htmlFor={id}>{label}</label>
      <div className="sx-usd">
        <span aria-hidden>$</span>
        <input id={id} className="inp mono" inputMode="decimal" value={value} placeholder="0" onChange={(e) => onChange(e.target.value)} />
      </div>
      <small>{hint}</small>
    </div>
  );
}

export default function Limits() {
  const [cfg, setCfg] = useState<SpendSettings | null>(null);
  const [known, setKnown] = useState(true);
  const [thread, setThread] = useState("");
  const [day, setDay] = useState("");
  const [rates, setRates] = useState<[string, CodexRate][]>([]);
  const [status, setStatus] = useState<OSpendStatus | null>(null);
  const [err, setErr] = useState("");
  const [busy, setBusy] = useState(false);
  const [saved, setSaved] = useState(false);
  const fill = (sp: SpendSettings) => {
    setCfg(sp);
    setThread(sp.perThreadUsd ? String(sp.perThreadUsd) : "");
    setDay(sp.perDayUsd ? String(sp.perDayUsd) : "");
    setRates(Object.entries(sp.codexRates));
  };
  const load = () => {
    settingsApi()
      .get()
      .then((r) => {
        fill(r.spend);
        setKnown(r.known.spend);
        setErr("");
      })
      .catch((e) => (isMissing(e) ? (fill(DEFAULT_SPEND), setKnown(false)) : setErr((e as Error).message)));
    settingsApi()
      .spend()
      .then(setStatus)
      .catch(() => setStatus(null));
  };
  useEffect(load, []);
  const next: SpendSettings = { perThreadUsd: num(thread), perDayUsd: num(day), codexRates: Object.fromEntries(rates.filter(([m]) => m.trim()).map(([m, r]) => [m.trim(), r])) };
  const dirty = !!cfg && JSON.stringify(next) !== JSON.stringify(cfg);
  const save = async () => {
    setBusy(true);
    setErr("");
    try {
      const r = await settingsApi().saveSpend(next);
      fill(r.spend);
      setSaved(true);
      window.setTimeout(() => setSaved(false), 1800);
      settingsApi().spend().then(setStatus).catch(() => undefined);
    } catch (e) {
      setErr((e as Error).message);
    }
    setBusy(false);
  };
  const setRate = (i: number, patch: Partial<{ m: string; in: number; out: number }>) =>
    setRates(rates.map(([m, r], j) => (j === i ? [patch.m ?? m, { in: patch.in ?? r.in, out: patch.out ?? r.out }] : [m, r])));
  const dayNow = status?.day;
  return (
    <div className="view pad sx">
      <SettingsTabs />
      <div className="viewhead">
        <div>
          <h2>Limits</h2>
          <p>Plan usage for the 5-hour and weekly windows of each agent, then spend caps for driven threads. At 80% you get a notification. At 100% agent-monitor stops the running turns in that scope and refuses new ones until you raise the limit or allow one more turn.</p>
        </div>
      </div>
      <div className="sx-col usx">
        <UsagePanel />
      </div>
      <h3 className="usx-h">Spend caps</h3>
      {err && !cfg ? (
        <ErrorState error={err} onRetry={load} />
      ) : !cfg ? (
        <Loading rows={4} />
      ) : (
        <div className="sx-col">
          {!known && <div className="sx-warn">This daemon does not enforce spend limits yet. The values save, but nothing stops a thread until agent-monitor is updated.</div>}
          {dayNow && (
            <div className="ibox">
              <h3>Today</h3>
              <SpendBar label="All driven threads" spent={dayNow.spent} limit={num(day)} note={num(day) && dayNow.spent >= num(day) ? "New turns wait until tomorrow, a higher limit, or Allow once more on a thread." : undefined} />
            </div>
          )}
          <div className="ibox">
            <h3>Caps</h3>
            <div className="sx-fields">
              <UsdField id="lim-thread" label="Per thread" value={thread} onChange={setThread} hint="Counts every run of one driven thread. Empty or 0 means no limit." />
              <UsdField id="lim-day" label="Per day" value={day} onChange={setDay} hint="All driven threads together, reset at local midnight. Empty or 0 means no limit." />
            </div>
          </div>
          <div className="ibox">
            <h3>Token rates</h3>
            <p className="sx-p">Codex reports tokens but no cost. Add a rate per model to count its turns against the caps. The model * is the fallback for models without a row. Without a rate, Codex turns count as $0.</p>
            {rates.length > 0 && (
              <div className="sx-rates" role="table" aria-label="Token rates">
                <div className="sx-rate h" role="row">
                  <span role="columnheader">Model</span>
                  <span role="columnheader">In, $/1M</span>
                  <span role="columnheader">Out, $/1M</span>
                  <span />
                </div>
                {rates.map(([m, r], i) => (
                  <div key={i} className="sx-rate" role="row">
                    <input className="inp mono" aria-label="Model" value={m} placeholder="gpt-5.5 or *" onChange={(e) => setRate(i, { m: e.target.value })} />
                    <input className="inp mono" aria-label="Dollars per million input tokens" inputMode="decimal" value={r.in || ""} placeholder="0" onChange={(e) => setRate(i, { in: num(e.target.value) })} />
                    <input className="inp mono" aria-label="Dollars per million output tokens" inputMode="decimal" value={r.out || ""} placeholder="0" onChange={(e) => setRate(i, { out: num(e.target.value) })} />
                    <button className="btn ghost sm" aria-label={`Remove ${m || "rate"}`} onClick={() => setRates(rates.filter((_, j) => j !== i))}>
                      ✕
                    </button>
                  </div>
                ))}
              </div>
            )}
            <button className="btn sm" onClick={() => setRates([...rates, ["", { in: 0, out: 0 }]])}>
              ＋ Add a rate
            </button>
          </div>
          <div className="sx-row end sx-save">
            {err && (
              <span className="sx-note err-t" role="alert">
                {err}
              </span>
            )}
            {saved && <span className="muted sm">Saved</span>}
            <button className="btn ghost" disabled={!dirty || busy} onClick={() => cfg && fill(cfg)}>
              Reset
            </button>
            <button className="btn pri" disabled={!dirty || busy} onClick={save}>
              {busy ? "Saving…" : "Save limits"}
            </button>
          </div>
        </div>
      )}
    </div>
  );
}
