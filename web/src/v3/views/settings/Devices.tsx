// Settings → Devices: paired devices, add (token shown once), revoke, and the device-token-only switch.
import { useCallback, useEffect, useRef, useState } from "react";
import { fmtAgo } from "../../../lib/format";
import { isMissing } from "../../api/client";
import { devicesApi, type Device, type NewDevice } from "../../api/devices";
import { ErrorState, Loading, Missing } from "../../components/ui";
import "../../styles/settings.css";
import { SettingsTabs, Switch } from "./Tabs";
import { copy } from "../../../lib/clipboard";

function CopyField({ label, value, mono = true }: { label: string; value: string; mono?: boolean }) {
  const [st, setSt] = useState<"" | "ok" | "fail">("");
  const inp = useRef<HTMLInputElement>(null);
  return (
    <div className="sx-copy">
      <span className="sx-copy-l">{label}</span>
      <div className="sx-copy-r">
        <input ref={inp} className={`inp ${mono ? "mono" : ""}`} readOnly value={value} aria-label={label} onFocus={(e) => e.currentTarget.select()} />
        <button
          className="btn"
          onClick={async () => {
            const ok = await copy(value);
            setSt(ok ? "ok" : "fail");
            if (!ok) inp.current?.select();
            window.setTimeout(() => setSt(""), 2000);
          }}
        >
          {st === "ok" ? "Copied" : "Copy"}
        </button>
      </div>
      {st === "fail" && <span className="sx-note err-t">Copy is blocked here. The text is selected: copy it by hand.</span>}
    </div>
  );
}

function AddDevice({ onAdded }: { onAdded: () => void }) {
  const [name, setName] = useState("");
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState("");
  const [made, setMade] = useState<NewDevice | null>(null);
  const add = async () => {
    const n = name.trim();
    if (!n || busy) return;
    setBusy(true);
    setErr("");
    try {
      setMade(await devicesApi().add(n));
      setName("");
      onAdded();
    } catch (e) {
      setErr((e as Error).message);
    }
    setBusy(false);
  };
  if (made)
    return (
      <div className="ibox sx-made" role="region" aria-label="New device token">
        <h3>
          Paired “{made.device.name}”<span className="pill p-need">shown once</span>
        </h3>
        <p className="sx-p">
          Copy the token or the pair link now. agent-monitor keeps only a hash of the token, so this page cannot show it again. If you lose it, revoke the device and add it again.
        </p>
        <CopyField label="Pair link" value={made.pairUrl} />
        <p className="sx-note">Open the pair link on the new device. The page signs that browser in and removes the token from the address bar.</p>
        <CopyField label="Token" value={made.token} />
        <p className="sx-note">For apps and scripts: send it as the header Authorization: Bearer &lt;token&gt;. The AgentBar app reads it from AGENT_MONITOR_TOKEN.</p>
        <div className="sx-row end">
          <button className="btn pri" onClick={() => setMade(null)}>
            Done, hide the token
          </button>
        </div>
      </div>
    );
  return (
    <div className="ibox">
      <h3>Add a device</h3>
      <p className="sx-p">Each device gets its own token. You can revoke one phone without signing out the others.</p>
      <div className="sx-row">
        <input
          className="inp sx-grow"
          value={name}
          maxLength={60}
          placeholder="Device name, for example iPhone 16"
          aria-label="Device name"
          onChange={(e) => setName(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === "Enter") void add();
          }}
        />
        <button className="btn pri" disabled={!name.trim() || busy} onClick={add}>
          {busy ? "Adding…" : "Add device"}
        </button>
      </div>
      {err && (
        <p className="sx-note err-t" role="alert">
          {err}
        </p>
      )}
    </div>
  );
}

function DeviceRow({ d, onRevoked }: { d: Device; onRevoked: () => void }) {
  const [ask, setAsk] = useState(false);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState("");
  const revoke = async () => {
    setBusy(true);
    setErr("");
    try {
      await devicesApi().revoke(d.id);
      setAsk(false);
      onRevoked();
    } catch (e) {
      setErr((e as Error).message);
    }
    setBusy(false);
  };
  const created = d.createdAt ? new Date(d.createdAt).toLocaleDateString(undefined, { year: "numeric", month: "short", day: "numeric" }) : "unknown";
  return (
    <li className={`sx-dev ${d.revokedAt ? "off" : ""}`}>
      <div className="sx-dev-m">
        <div className="sx-dev-n">
          <b className="trunc">{d.name || "Unnamed device"}</b>
          {d.current && <span className="pill p-ok">this device</span>}
          {d.revokedAt && <span className="pill p-mute">revoked {fmtAgo(d.revokedAt)}</span>}
        </div>
        <div className="sx-dev-meta">
          <span>Added {created}</span>
          <span>{d.lastUsedAt ? `Last used ${fmtAgo(d.lastUsedAt)}` : "Never used"}</span>
          {d.lastIp && <span className="mono">{d.lastIp}</span>}
        </div>
        {err && (
          <span className="sx-note err-t" role="alert">
            {err}
          </span>
        )}
      </div>
      {!d.revokedAt &&
        (ask ? (
          <div className="sx-confirm" role="group" aria-label={`Confirm revoke of ${d.name}`}>
            <span>{d.current ? "Revoke this device? This browser loses access at once." : "Revoke? It loses access at once."}</span>
            <button className="btn deny" disabled={busy} onClick={revoke}>
              {busy ? "Revoking…" : "Revoke"}
            </button>
            <button className="btn ghost" disabled={busy} onClick={() => setAsk(false)}>
              Cancel
            </button>
          </div>
        ) : (
          <button className="btn sx-revoke" onClick={() => setAsk(true)}>
            Revoke
          </button>
        ))}
    </li>
  );
}

function RequireToken({ devices }: { devices: Device[] }) {
  const [on, setOn] = useState<boolean | null>(null);
  const [missing, setMissing] = useState(false);
  const [confirm, setConfirm] = useState(false);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState("");
  useEffect(() => {
    devicesApi()
      .settings()
      .then((s) => setOn(s.requireDeviceToken))
      .catch((e) => (isMissing(e) ? setMissing(true) : setErr((e as Error).message)));
  }, []);
  const others = devices.filter((d) => !d.revokedAt && !d.current).length;
  const blocked = !on && others < 1;
  const save = async (v: boolean) => {
    setBusy(true);
    setErr("");
    try {
      const s = await devicesApi().setRequire(v);
      setOn(s.requireDeviceToken);
      setConfirm(false);
    } catch (e) {
      setErr((e as Error).message);
    }
    setBusy(false);
  };
  return (
    <div className="ibox">
      <h3>Remote access</h3>
      <label className={`sx-switch ${blocked || on === null ? "dis" : ""}`}>
        <input
          type="checkbox"
          role="switch"
          checked={!!on}
          aria-checked={!!on}
          disabled={blocked || on === null || busy || missing}
          onChange={(e) => (e.target.checked ? setConfirm(true) : void save(false))}
        />
        <span className="sx-track" aria-hidden />
        <span>Require a device token for remote access</span>
      </label>
      <p className="sx-p">
        When this is on, the password stops working for browsers and apps that reach this Mac over the network. Only paired devices get in. This Mac itself (localhost) always gets in.
      </p>
      {missing && <p className="sx-note">This daemon does not have the setting yet.</p>}
      {blocked && !missing && <p className="sx-note need-t">Add at least one other active device first. Without one, turning this on can lock you out.</p>}
      {confirm && (
        <div className="sx-confirm wide" role="group" aria-label="Confirm device-token-only access">
          <span>
            Turn it on? Remote browsers that signed in with the password lose access. {others} other active {others === 1 ? "device keeps" : "devices keep"} access.
          </span>
          <button className="btn pri" disabled={busy} onClick={() => save(true)}>
            {busy ? "Saving…" : "Turn on"}
          </button>
          <button className="btn ghost" disabled={busy} onClick={() => setConfirm(false)}>
            Cancel
          </button>
        </div>
      )}
      {err && (
        <p className="sx-note err-t" role="alert">
          {err}
        </p>
      )}
    </div>
  );
}

function RemoteTerminal() {
  const [on, setOn] = useState<boolean | null>(null);
  const [missing, setMissing] = useState(false);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState("");
  useEffect(() => {
    devicesApi()
      .settings()
      .then((s) => setOn(s.remoteTerminal))
      .catch((e) => (isMissing(e) ? setMissing(true) : setErr((e as Error).message)));
  }, []);
  const save = async (v: boolean) => {
    setBusy(true);
    setErr("");
    try {
      setOn((await devicesApi().setRemoteTerminal(v)).remoteTerminal);
    } catch (e) {
      setErr((e as Error).message);
    }
    setBusy(false);
  };
  return (
    <div className="ibox">
      <h3>Remote terminal</h3>
      <Switch on={!!on} label="Allow the Workspace terminal from paired devices" disabled={on === null || busy || missing} onChange={(v) => void save(v)} />
      <p className="sx-p">
        The Workspace terminal is a full shell on this Mac, with your user and your files. When this is on, a paired device can open one. The password alone never can. This Mac itself (localhost) always can.
      </p>
      {on && <p className="sx-note need-t">Shell access from paired devices is on. Revoke a lost device at once.</p>}
      {missing && <p className="sx-note">This daemon does not have the setting yet.</p>}
      {err && (
        <p className="sx-note err-t" role="alert">
          {err}
        </p>
      )}
    </div>
  );
}

export default function Devices() {
  const [list, setList] = useState<Device[] | null>(null);
  const [err, setErr] = useState("");
  const [missing, setMissing] = useState(false);
  const load = useCallback(() => {
    devicesApi()
      .list()
      .then((l) => {
        setList(l);
        setErr("");
      })
      .catch((e) => (isMissing(e) ? setMissing(true) : setErr((e as Error).message)));
  }, []);
  useEffect(load, [load]);
  const active = (list || []).filter((d) => !d.revokedAt);
  const revoked = (list || []).filter((d) => d.revokedAt);
  const sorted = [...active.sort((a, b) => Number(!!b.current) - Number(!!a.current) || (b.lastUsedAt || 0) - (a.lastUsedAt || 0)), ...revoked];
  return (
    <div className="view pad sx">
      <SettingsTabs />
      <div className="viewhead">
        <div>
          <h2>Devices</h2>
          <p>Phones, tablets and other computers that can reach this Mac. Each one signs in with its own token.</p>
        </div>
      </div>
      {missing ? (
        <div className="ibox">
          <Missing endpoint="/api/devices" what="Device tokens are not available on this daemon yet" />
        </div>
      ) : (
        <div className="sx-col">
          <AddDevice onAdded={load} />
          <div className="ibox">
            <h3>
              Paired devices{list && <span className="muted sm">{active.length} active</span>}
            </h3>
            {err && !list ? <ErrorState error={err} onRetry={load} /> : !list ? <Loading rows={3} /> : list.length === 0 ? <p className="sx-p">No devices yet. Add one above.</p> : (
              <ul className="sx-devs">
                {sorted.map((d) => (
                  <DeviceRow key={d.id} d={d} onRevoked={load} />
                ))}
              </ul>
            )}
          </div>
          {list && <RequireToken devices={list} />}
          <RemoteTerminal />
        </div>
      )}
    </div>
  );
}
