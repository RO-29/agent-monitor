// Lock screen alerts for the iPhone through the Bark app; the daemon encrypts each alert (AES-256-GCM).
import { useEffect, useState } from "react";
import { req } from "../../api/orch";
import { copy } from "../../../lib/clipboard";
import { Switch } from "./Tabs";

interface BarkView {
  enabled: boolean;
  server: string;
  deviceKeySet: boolean;
  deviceKeyHint?: string;
  key: string;
  iv: string;
  algorithm: string;
  mode: string;
  padding: string;
  phoneActive?: boolean;
}

export function BarkBox() {
  const [v, setV] = useState<BarkView | null>(null);
  const [url, setUrl] = useState("");
  const [msg, setMsg] = useState("");
  const [err, setErr] = useState("");
  const [busy, setBusy] = useState(false);
  useEffect(() => {
    req<BarkView>("GET", "/api/push/bark").then(setV, (e) => setErr(e instanceof Error ? e.message : String(e)));
  }, []);
  const run = async (fn: () => Promise<unknown>, ok: string) => {
    setBusy(true);
    try {
      await fn();
      setMsg(ok);
      setErr("");
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };
  const save = (body: Record<string, unknown>, ok: string) => run(async () => setV(await req<BarkView>("POST", "/api/push/bark", body)), ok);
  const row = (label: string, value: string, secret = false) => (
    <div className="sx-field" key={label}>
      <label>{label}</label>
      <div style={{ display: "flex", gap: 8, alignItems: "center", minWidth: 0 }}>
        <code className="mono" style={{ overflow: "hidden", textOverflow: "ellipsis", minWidth: 0 }}>{value}</code>
        {secret && (
          <button className="btn ghost sm" onClick={() => void copy(value).then(() => setMsg(`${label} copied.`))}>
            Copy
          </button>
        )}
      </div>
    </div>
  );
  return (
    <div className="ibox">
      <h3>iPhone lock screen</h3>
      <p className="sx-p">
        The iPhone app alerts only while it runs. For alerts on a locked phone, this Mac sends each alert through the free{" "}
        <a href="https://apps.apple.com/app/id1403753865" target="_blank" rel="noreferrer">
          Bark app
        </a>
        . The Mac encrypts the alert, so Bark's server sees no text. A tap opens the thread in agent-monitor.
      </p>
      {err && <p className="err-t">{err}</p>}
      {v && (
        <>
          <div className="sx-field">
            <label htmlFor="bark-url">Bark URL {v.deviceKeySet && <small>(set, {v.deviceKeyHint})</small>}</label>
            <div style={{ display: "flex", gap: 8 }}>
              <input id="bark-url" className="inp mono" placeholder="https://api.day.app/your-key/" value={url} onChange={(e) => setUrl(e.target.value)} />
              <button className="btn" disabled={busy || !url.trim()} onClick={() => void save({ deviceKey: url }, "Bark URL saved.").then(() => setUrl(""))}>
                Save
              </button>
            </div>
            <small>Open Bark on the phone and copy the URL on its home screen.</small>
          </div>
          <p className="sx-p">In Bark, open Push Encryption and enter these values:</p>
          {row("Algorithm", v.algorithm)}
          {row("Mode", v.mode)}
          {row("Padding", v.padding)}
          {row("Key", v.key, true)}
          {row("IV", v.iv, true)}
          <Switch on={v.enabled} label="Lock screen alerts" disabled={busy || !v.deviceKeySet} onChange={(on) => void save({ enabled: on }, on ? "Lock screen alerts are on." : "Lock screen alerts are off.")} />
          <div style={{ display: "flex", gap: 8, flexWrap: "wrap" }}>
            <button className="btn pri" disabled={busy || !v.deviceKeySet} onClick={() => void run(() => req("POST", "/api/push/bark/test", {}), "Test sent.")}>
              Send test
            </button>
            <button className="btn deny" disabled={busy} onClick={() => void save({ newKey: true }, "New key made. Enter it in Bark again.")}>
              New key
            </button>
          </div>
          {msg && <p className="sx-p muted">{msg}</p>}
          <p className="sx-p muted">No push goes out while the iPhone app is open, because it alerts by itself.</p>
        </>
      )}
    </div>
  );
}
