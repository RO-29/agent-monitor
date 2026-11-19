// One other machine: its requests and its sessions, read through this Mac's grant there (/api/m/<peer>/...).
import { useCallback, useEffect, useState } from "react";
import { Link, useParams, useSearchParams } from "react-router-dom";
import { decidePeer, PEER_PLACE, type PeerNeed, type PeerSession } from "./peers";
import { fmtAgo } from "../../lib/format";
import { machinesApi } from "../api/machines";
import { Markdown } from "../components/Markdown";
import { showToast } from "../../app/toast";
import "../styles/settings.css";

interface PeerItem {
  id: string;
  kind: string;
  text: string;
}


function SessionItems({ peer, id }: { peer: string; id: string }) {
  const [items, setItems] = useState<PeerItem[] | null>(null);
  const [err, setErr] = useState("");
  useEffect(() => {
    let live = true;
    const load = () =>
      machinesApi
        .peer<{ items: PeerItem[] }>(peer, "GET", `session/${encodeURIComponent(id)}/items`)
        .then((r) => live && setItems((r.items || []).filter((i) => i.kind === "user" || i.kind === "agent" || i.kind === "error").slice(-40)))
        .catch((e: Error) => live && setErr(e.message));
    void load();
    const t = window.setInterval(load, 5000);
    return () => ((live = false), window.clearInterval(t));
  }, [peer, id]);
  if (err) return <p className="err-t sm">{err}</p>;
  if (!items) return <p className="muted sm">Loading…</p>;
  if (!items.length) return <p className="muted sm">No messages yet.</p>;
  return (
    <div className="peer-items">
      {items.map((i) => (
        <div key={i.id} className={`peer-it ${i.kind}`}>
          <span className="muted sm">{i.kind === "user" ? "You" : i.kind === "error" ? "Error" : "Agent"}</span>
          {i.kind === "agent" ? <Markdown text={i.text} collapseAt={20} /> : <pre className="peer-pre">{i.text}</pre>}
        </div>
      ))}
    </div>
  );
}

export default function PeerPage() {
  const peer = useParams().peer || "";
  const [needs, setNeeds] = useState<PeerNeed[]>([]);
  const [sessions, setSessions] = useState<PeerSession[] | null>(null);
  const [err, setErr] = useState("");
  const [params] = useSearchParams();
  const [open, setOpen] = useState(params.get("s") || "");
  const load = useCallback(() => {
    machinesApi
      .peer<{ items: PeerNeed[] }>(peer, "GET", "needs-you")
      .then((r) => setNeeds(r.items || []))
      .catch((e: Error) => setErr(e.message));
    machinesApi
      .peer<PeerSession[] | { sessions: PeerSession[] }>(peer, "GET", "sessions?slim=1")
      .then((r) => {
        const l = Array.isArray(r) ? r : r.sessions || [];
        setSessions(l.sort((a, b) => b.lastActivityAt - a.lastActivityAt).slice(0, 40));
        setErr("");
      })
      .catch((e: Error) => setErr(e.message));
  }, [peer]);
  useEffect(() => {
    load();
    const t = window.setInterval(load, 5000);
    return () => window.clearInterval(t);
  }, [load]);
  const decide = async (n: PeerNeed, behavior: "allow" | "deny") => {
    if (await decidePeer(peer, n, behavior)) load();
  };
  return (
    <div className="view pad">
      <div className="viewhead">
        <div>
          <h2>{peer}</h2>
          <p>
            Another machine, read through the access it gave this Mac. <Link to="/settings/machines">Manage machines</Link>
          </p>
        </div>
      </div>
      {err && <p className="err-t">{err}</p>}
      <div className="ibox">
        <h3>Needs you{needs.length ? ` · ${needs.length}` : ""}</h3>
        {needs.length === 0 && <p className="muted sm">Nothing waits on {peer}.</p>}
        {needs.map((n) => (
          <div key={n.key} className="peer-need">
            <div>
              <b>{n.title || n.agent || n.kind}</b>
              <div className="muted sm">{n.approval?.summary || n.message || n.kind}</div>
            </div>
            {n.kind === "approval" && (
              <div className="btns">
                <button className="btn sm allow" onClick={() => void decide(n, "allow")}>
                  Allow
                </button>
                <button className="btn sm deny" onClick={() => void decide(n, "deny")}>
                  Deny
                </button>
              </div>
            )}
          </div>
        ))}
      </div>
      <div className="ibox">
        <h3>Sessions</h3>
        {!sessions && !err && <p className="muted sm">Loading…</p>}
        {sessions?.map((s) => (
          <div key={s.id} className="peer-sess">
            <button className="peer-row" onClick={() => setOpen(open === s.id ? "" : s.id)} aria-expanded={open === s.id}>
              <b>{s.title || (s.firstMessage || "").slice(0, 80) || s.id}</b>
              <span className="muted sm">
                {PEER_PLACE[s.entrypoint || ""] ? `${PEER_PLACE[s.entrypoint || ""]} · ` : ""}
                {s.state} · {s.cwd.replace(/^\/Users\/[^/]+/, "~")} · {fmtAgo(s.lastActivityAt)}
              </span>
            </button>
            {open === s.id && <SessionItems peer={peer} id={s.id} />}
          </div>
        ))}
      </div>
    </div>
  );
}
