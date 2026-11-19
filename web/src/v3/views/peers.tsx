// Other machines in the main views: their requests and sessions, read through this Mac's grants (/api/m/...).
import { useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { machinesApi, type PeerInfo } from "../api/machines";
import { showToast } from "../../app/toast";
import { fmtAgo } from "../../lib/format";
import "../styles/settings.css";

export interface PeerNeed {
  kind: string;
  key: string;
  title?: string;
  message?: string;
  agent?: string;
  approval?: { approvalId?: string; id?: string; tool?: string; summary?: string };
}
export interface PeerSession {
  id: string;
  title?: string;
  firstMessage?: string;
  state: string;
  cwd: string;
  lastActivityAt: number;
  entrypoint?: string;
}
export const PEER_PLACE: Record<string, string> = { "claude-desktop": "Desktop", cli: "Terminal", "sdk-cli": "SDK" };

export async function decidePeer(peer: string, n: PeerNeed, behavior: "allow" | "deny") {
  const id = n.approval?.approvalId || n.approval?.id || n.key;
  try {
    await machinesApi.peer(peer, "POST", `approvals/${encodeURIComponent(id)}/decide`, { behavior, surface: "peer" });
    return true;
  } catch (e) {
    showToast(behavior === "allow" ? "Not allowed" : "Not denied", `${peer}: ${(e as Error).message}`, { kind: "alert" });
    return false;
  }
}

interface Feed {
  peers: PeerInfo[];
  needs: Record<string, PeerNeed[]>;
  sessions: Record<string, PeerSession[]>;
}

/** Online peers with their open requests (every 5 s) and recent sessions (every 15 s); no peers, no requests. */
export function usePeerFeed(withSessions = false): Feed {
  const [peers, setPeers] = useState<PeerInfo[]>([]);
  const [needs, setNeeds] = useState<Record<string, PeerNeed[]>>({});
  const [sessions, setSessions] = useState<Record<string, PeerSession[]>>({});
  useEffect(() => {
    let live = true;
    const load = () => machinesApi.peers().then((l) => live && setPeers(l.filter((p) => p.status === "online"))).catch(() => {});
    void load();
    const t = window.setInterval(load, 30000);
    return () => ((live = false), window.clearInterval(t));
  }, []);
  const names = peers.map((p) => p.name).join(",");
  useEffect(() => {
    if (!names) return;
    let live = true;
    const loadNeeds = () =>
      names.split(",").forEach((p) =>
        machinesApi
          .peer<{ items: PeerNeed[] }>(p, "GET", "needs-you")
          .then((r) => live && setNeeds((m) => ({ ...m, [p]: r.items || [] })))
          .catch(() => {}),
      );
    const loadSessions = () =>
      names.split(",").forEach((p) =>
        machinesApi
          .peer<PeerSession[] | { sessions: PeerSession[] }>(p, "GET", "sessions?slim=1")
          .then((r) => {
            const l = (Array.isArray(r) ? r : r.sessions || []).sort((a, b) => b.lastActivityAt - a.lastActivityAt);
            if (live) setSessions((m) => ({ ...m, [p]: l.slice(0, 40) }));
          })
          .catch(() => {}),
      );
    loadNeeds();
    if (withSessions) loadSessions();
    const a = window.setInterval(loadNeeds, 5000);
    const b = withSessions ? window.setInterval(loadSessions, 15000) : 0;
    return () => {
      live = false;
      window.clearInterval(a);
      if (b) window.clearInterval(b);
    };
  }, [names, withSessions]);
  return { peers, needs, sessions };
}

/** Requests waiting on other machines, each with its machine name; Allow and Deny go through that machine's grant. */
export function PeerNeedsBox({ feed, compact }: { feed: Feed; compact?: boolean }) {
  const [gone, setGone] = useState<Record<string, true>>({});
  const all = feed.peers.flatMap((p) => (feed.needs[p.name] || []).filter((n) => !gone[p.name + n.key]).map((n) => ({ peer: p, n })));
  if (!all.length) return null;
  return (
    <section className={`peer-needs ${compact ? "compact" : ""}`} aria-label="Requests on other machines">
      <div className="peer-needs-h">On other machines · {all.length}</div>
      {all.map(({ peer, n }) => (
        <div key={peer.name + n.key} className="peer-need">
          <div style={{ minWidth: 0 }}>
            <span className="peer-chip">{peer.name}</span> <b>{n.title || n.agent || n.kind}</b>
            <div className="muted sm">{n.approval?.summary || n.message || n.kind}</div>
          </div>
          {n.kind === "approval" && peer.level !== "view" ? (
            <div className="btns">
              <button className="btn sm allow" onClick={() => void decidePeer(peer.name, n, "allow").then((ok) => ok && setGone((g) => ({ ...g, [peer.name + n.key]: true })))}>
                Allow
              </button>
              <button className="btn sm deny" onClick={() => void decidePeer(peer.name, n, "deny").then((ok) => ok && setGone((g) => ({ ...g, [peer.name + n.key]: true })))}>
                Deny
              </button>
            </div>
          ) : (
            <Link className="btn sm ghost" to={`/m/${encodeURIComponent(peer.name)}`} title={peer.level === "view" ? `This Mac has view access on ${peer.name}: answer it there` : undefined}>
              Open
            </Link>
          )}
        </div>
      ))}
    </section>
  );
}

const ACTIVE = new Set(["running", "awaiting-input", "awaiting-permission"]);

/** Each online peer's active sessions, then its latest ones, under the thread list. */
export function PeerThreads() {
  const feed = usePeerFeed(true);
  if (!feed.peers.length) return null;
  return (
    <div className="peer-threads" aria-label="Other machines">
      {feed.peers.map((p) => {
        const l = feed.sessions[p.name] || [];
        const shown = [...l.filter((s) => ACTIVE.has(s.state)), ...l.filter((s) => !ACTIVE.has(s.state)).slice(0, 5)].slice(0, 12);
        const waiting = (feed.needs[p.name] || []).length;
        return (
          <div key={p.name} className="peer-tgroup">
            <Link className="peer-thead" to={`/m/${encodeURIComponent(p.name)}`}>
              <span className="peer-chip">{p.name}</span>
              <span className="muted sm">{p.level}</span>
              {waiting > 0 && <span className="peer-wait">{waiting} need you</span>}
            </Link>
            {shown.map((s) => (
              <Link key={s.id} className="peer-trow" to={`/m/${encodeURIComponent(p.name)}?s=${encodeURIComponent(s.id)}`}>
                <span className={`peer-dot ${ACTIVE.has(s.state) ? (s.state === "running" ? "run" : "need") : ""}`} />
                <span className="trunc">{s.title || (s.firstMessage || "").slice(0, 60) || s.id}</span>
                <span className="muted xs">
                  {PEER_PLACE[s.entrypoint || ""] || ""} · {fmtAgo(s.lastActivityAt)}
                </span>
              </Link>
            ))}
          </div>
        );
      })}
    </div>
  );
}
