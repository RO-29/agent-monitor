// Settings → Notifications: per-event switches, quiet hours, the batch window, and Web Push for this browser.
import { useEffect, useState } from "react";
import { isMissing } from "../../api/client";
import { disablePush, enablePush, pushState, sendTest, type PushState } from "../../api/notify";
import { DEFAULT_NOTIFY, NKINDS, settingsApi, type NotifySettings as NS } from "../../api/settings";
import { ErrorState, Loading } from "../../components/ui";
import { copy } from "../../../lib/clipboard";
import { BarkBox } from "./BarkBox";
import { SettingsTabs, Switch } from "./Tabs";
import "../../styles/settings.css";
import "../../styles/p6.css";

function PushBox() {
  const [st, setSt] = useState<PushState | null>(null);
  const [err, setErr] = useState("");
  const [copied, setCopied] = useState(false);
  useEffect(() => {
    pushState().then(setSt).catch((e) => setErr((e as Error).message));
  }, []);
  const run = async (fn: () => Promise<PushState>) => {
    setErr("");
    setSt("busy");
    try {
      setSt(await fn());
    } catch (e) {
      setErr(isMissing(e) ? "This daemon has no push endpoints yet." : (e as Error).message);
      setSt(await pushState().catch(() => "off" as PushState));
    }
  };
  const cmd = `tailscale serve --bg ${window.location.port || "7777"}`;
  return (
    <div className="ibox">
      <h3>
        Push to this device
        {st === "on" && <span className="pill p-ok">on</span>}
      </h3>
      {st === "insecure" ? (
        <>
          <p className="sx-p">
            Browsers only allow push on a secure origin. This page runs on plain http ({window.location.host}), so push stays off here. In-app notifications and the list still work.
          </p>
          <p className="sx-p">To get push on a phone, serve agent-monitor over https on your tailnet. Run this on the Mac, then open the https address it prints and pair this device there:</p>
          <div className="sx-copy-r">
            <input className="inp mono" readOnly value={cmd} aria-label="tailscale serve command" onFocus={(e) => e.currentTarget.select()} />
            <button className="btn" onClick={async () => setCopied(await copy(cmd))}>
              {copied ? "Copied" : "Copy"}
            </button>
          </div>
          <p className="sx-note">tailscale serve keeps the daemon on 127.0.0.1 and adds a certificate for your machine name. No third-party push service is involved.</p>
        </>
      ) : st === "unsupported" ? (
        <p className="sx-p">This browser has no Web Push. On an iPhone, add the page to the Home Screen first, then open it from there.</p>
      ) : st === "denied" ? (
        <p className="sx-p">This browser blocks notifications for this site. Allow them in the site settings, then reload the page.</p>
      ) : st === null ? (
        <Loading rows={1} />
      ) : (
        <>
          <p className="sx-p">Push reaches this device when the page is closed. The rules above apply. Each device subscribes on its own.</p>
          <div className="sx-row">
            {st === "on" ? (
              <>
                <button className="btn" onClick={() => sendTest().catch((e) => setErr((e as Error).message))}>
                  Send a test
                </button>
                <button className="btn ghost" onClick={() => run(disablePush)}>
                  Turn off push here
                </button>
              </>
            ) : (
              <button className="btn pri" disabled={st === "busy"} onClick={() => run(enablePush)}>
                {st === "busy" ? "Asking the browser…" : "Turn on push here"}
              </button>
            )}
          </div>
        </>
      )}
      {err && (
        <p className="sx-note err-t" role="alert">
          {err}
        </p>
      )}
    </div>
  );
}

export default function NotifySettings() {
  const [cfg, setCfg] = useState<NS | null>(null);
  const [known, setKnown] = useState(true);
  const [err, setErr] = useState("");
  const [saving, setSaving] = useState(false);
  const [saved, setSaved] = useState(false);
  const load = () =>
    settingsApi()
      .get()
      .then((r) => {
        setCfg(r.notify);
        setKnown(r.known.notify);
        setErr("");
      })
      .catch((e) => (isMissing(e) ? (setCfg(DEFAULT_NOTIFY), setKnown(false)) : setErr((e as Error).message)));
  useEffect(() => {
    void load();
  }, []);
  const save = async (patch: Partial<NS>) => {
    if (!cfg) return;
    const was = cfg;
    setCfg({ ...cfg, ...patch });
    setSaving(true);
    setSaved(false);
    try {
      const r = await settingsApi().saveNotify(patch);
      setCfg(r.notify);
      setSaved(true);
      window.setTimeout(() => setSaved(false), 1800);
    } catch (e) {
      setCfg(was);
      setErr((e as Error).message);
    }
    setSaving(false);
  };
  const quiet = !!cfg?.quietStart && !!cfg?.quietEnd;
  return (
    <div className="view pad sx">
      <SettingsTabs />
      <div className="viewhead">
        <div>
          <h2>Notifications</h2>
          <p>What agent-monitor tells you about driven threads, and when it stays quiet. The same rules apply to the in-app list, the menu bar app and push.</p>
        </div>
        <span className="muted sm" aria-live="polite">
          {saving ? "Saving…" : saved ? "Saved" : ""}
        </span>
      </div>
      {err && !cfg ? (
        <ErrorState error={err} onRetry={load} />
      ) : !cfg ? (
        <Loading rows={4} />
      ) : (
        <div className="sx-col">
          {!known && <div className="sx-warn">This daemon does not store notification settings yet. Changes may not stick until agent-monitor is updated.</div>}
          <div className="ibox">
            <h3>Events</h3>
            {NKINDS.map((k) => (
              <Switch key={k.key} on={cfg.events[k.key]} label={k.label} hint={k.hint} onChange={(v) => save({ events: { ...cfg.events, [k.key]: v } })} />
            ))}
          </div>
          <div className="ibox">
            <h3>Quiet hours</h3>
            <Switch on={quiet} label="Hold notifications at night" hint="Held notices wait in the list. Nothing is lost." onChange={(v) => save(v ? { quietStart: "22:00", quietEnd: "07:00" } : { quietStart: "", quietEnd: "" })} />
            {quiet && (
              <>
                <div className="sx-row sx-times">
                  <label>
                    From
                    <input className="inp" type="time" value={cfg.quietStart} onChange={(e) => e.target.value && save({ quietStart: e.target.value })} />
                  </label>
                  <label>
                    To
                    <input className="inp" type="time" value={cfg.quietEnd} onChange={(e) => e.target.value && save({ quietEnd: e.target.value })} />
                  </label>
                  <span className="muted sm">local time on this Mac</span>
                </div>
                <Switch on={cfg.quietAllowsNeedsYou} label="Needs you still comes through" hint="An agent that waits for an approval can block work until morning." onChange={(v) => save({ quietAllowsNeedsYou: v })} />
              </>
            )}
          </div>
          <div className="ibox">
            <h3>Batching</h3>
            <p className="sx-p">
              Events that arrive within {cfg.batchSec} seconds of each other become one notification. A fan-out sends one notice when its last subagent ends, not one per subagent.
            </p>
          </div>
          <PushBox />
          <BarkBox />
        </div>
      )}
    </div>
  );
}
