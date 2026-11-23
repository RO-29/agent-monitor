// Settings → Machines: which machines this Mac can see, and which devices and machines can see this Mac.
import { useCallback, useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { fmtAgo } from "../../../lib/format";
import { machinesApi, type Grant, type PeerInfo } from "../../api/machines";
import { showToast } from "../../../app/toast";
import { copy } from "../../../lib/clipboard";
import "../../styles/settings.css";
import { SettingsTabs } from "./Tabs";
import { levelText } from "../../api/fleet";

const LEVEL_HELP: Record<string, string> = {
  view: "sees sessions, threads and requests",
  control: "also answers requests and drives agents",
  admin: "also terminals, search and settings",
};

function Peers() {
  const [list, setList] = useState<PeerInfo[] | null>(null);
  const [url, setUrl] = useState("");
  const [code, setCode] = useState("");
  const [name, setName] = useState("");
  const [busy, setBusy] = useState(false);
  const load = useCallback(() => void machinesApi.peers().then(setList).catch((e: Error) => showToast("Machines", e.message, { kind: "alert" })), []);
  useEffect(load, [load]);
  const add = async () => {
    setBusy(true);
    try {
      await machinesApi.addPeer(url.trim(), code.trim(), name.trim());
      setUrl(""), setCode(""), setName("");
      load();
    } catch (e) {
      showToast("Not added", (e as Error).message, { kind: "alert" });
    } finally {
      setBusy(false);
    }
  };
  return (
    <div className="ibox">
      <h3>This Mac can see</h3>
      <p className="sx-p">Other machines that gave this Mac access. Each one decides what this Mac may do there.</p>
      {list && list.length > 0 && (
        <ul className="sx-devs">
          {list.map((p) => (
            <li key={p.name} className="sx-dev">
              <div>
                <b>
                  <Link to={`/m/${encodeURIComponent(p.name)}`}>{p.name}</Link>
                </b>
                <div className="muted sm">
                  {p.url} · <span className="pill">{levelText(p.level)}</span> · <span className={p.status === "online" ? "ok-t" : "err-t"}>{p.status}</span>
                </div>
              </div>
              <button className="btn sm ghost" onClick={() => void machinesApi.removePeer(p.name).then(load)}>
                Remove
              </button>
            </li>
          ))}
        </ul>
      )}
      {list && list.length === 0 && <p className="sx-p muted">None yet.</p>}
      <div className="sx-col" style={{ gap: 8, marginTop: 10 }}>
        <input className="inp" placeholder="Address, for example http://100.64.0.9:7777" value={url} onChange={(e) => setUrl(e.target.value)} />
        <input className="inp mono" placeholder="Pairing code from that machine" value={code} onChange={(e) => setCode(e.target.value)} />
        <input className="inp" placeholder="Name (optional), for example mac2" value={name} onChange={(e) => setName(e.target.value)} />
        <button className="btn pri" disabled={busy || !url.trim() || !code.trim()} onClick={() => void add()}>
          {busy ? "Adding…" : "Add machine"}
        </button>
      </div>
    </div>
  );
}

function Grants() {
  const [list, setList] = useState<Grant[] | null>(null);
  const [name, setName] = useState("");
  const [level, setLevel] = useState("view");
  const [made, setMade] = useState<{ addr: string; code: string } | null>(null);
  const load = useCallback(() => void machinesApi.grants().then(setList).catch((e: Error) => showToast("Access", e.message, { kind: "alert" })), []);
  useEffect(load, [load]);
  const act = async (f: () => Promise<unknown>) => {
    try {
      await f();
    } catch (e) {
      showToast("Not changed", (e as Error).message, { kind: "alert" });
    }
    load();
  };
  const make = async () => {
    try {
      const r = await machinesApi.machineCode(name.trim(), level);
      const [addr, code] = r.pairUrl.split("/pair#");
      setMade({ addr, code });
      setName("");
      load();
    } catch (e) {
      showToast("No code", (e as Error).message, { kind: "alert" });
    }
  };
  const active = (list || []).filter((g) => !g.revokedAt);
  return (
    <div className="ibox">
      <h3>Who can see this Mac</h3>
      <p className="sx-p">Your devices and other machines with access here. A machine is never admin and works only from the address it paired from.</p>
      {active.length > 0 && (
        <ul className="sx-devs">
          {active.map((g) => (
            <li key={g.id} className="sx-dev">
              <div>
                <b>{g.name}</b> <span className="muted sm">{g.kind}</span>
                <div className="muted sm">
                  {g.lastUsedAt ? `last used ${fmtAgo(g.lastUsedAt)}` : "not used yet"}
                  {g.boundIp ? ` · from ${g.boundIp}` : ""} · {LEVEL_HELP[g.level] || ""}
                </div>
              </div>
              <div className="btns">
                <select className="inp" value={g.level} onChange={(e) => void act(() => machinesApi.setLevel(g.id, e.target.value))} aria-label={`Level of ${g.name}`}>
                  <option value="view">View only</option>
                  <option value="control">Full</option>
                  {g.kind !== "machine" && <option value="admin">Admin</option>}
                </select>
                <button className="btn sm deny" onClick={() => void act(() => machinesApi.revoke(g.id))}>
                  Revoke
                </button>
              </div>
            </li>
          ))}
        </ul>
      )}
      <h3 style={{ marginTop: 14 }}>Let another machine see this Mac</h3>
      <div className="sx-col" style={{ gap: 8 }}>
        <input className="inp" placeholder="Name of the other machine, for example rohit-mac" value={name} onChange={(e) => setName(e.target.value)} />
        <select className="inp" value={level} onChange={(e) => setLevel(e.target.value)}>
          <option value="view">View only: {LEVEL_HELP.view}</option>
          <option value="control">Full: {LEVEL_HELP.control}</option>
        </select>
        <button className="btn pri" disabled={!name.trim()} onClick={() => void make()}>
          Create pairing code
        </button>
        {made && (
          <div className="sx-note">
            On the other machine: Settings → Machines → Add machine. Address <code>{made.addr}</code>, code{" "}
            <code onClick={() => void copy(made.code)} title="Copy">
              {made.code}
            </code>
            . The code works once.
          </div>
        )}
      </div>
    </div>
  );
}

export default function Machines() {
  return (
    <div className="view pad sx">
      <SettingsTabs />
      <div className="viewhead">
        <div>
          <h2>Machines</h2>
          <p>Each machine keeps its own sessions. A machine sees another one only when that machine allows it.</p>
        </div>
      </div>
      <div className="sx-col">
        <Peers />
        <Grants />
      </div>
    </div>
  );
}
