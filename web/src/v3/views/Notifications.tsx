// /notifications: everything the notifier sent, newest first, with read state and links to the threads.
import { useMemo, useState } from "react";
import { Link, useNavigate } from "react-router-dom";
import { fmtAgo } from "../../lib/format";
import { linkOf, markRead, useNotify, type ONotification } from "../api/notify";
import { useOrch } from "../api/orch";
import type { NKind } from "../api/settings";
import { Empty, Loading, Missing } from "../components/ui";
import { startOfToday } from "../model";
import "../styles/p6.css";

const KIND: Record<NKind, { label: string; ic: string }> = {
  "needs-you": { label: "Needs you", ic: "!" },
  finished: { label: "Finished", ic: "✓" },
  error: { label: "Error", ic: "✗" },
  spend: { label: "Spend", ic: "$" },
};
type Filter = "" | NKind;

function Item({ n, onOpen }: { n: ONotification; onOpen: (n: ONotification) => void }) {
  const orch = useOrch();
  const t = n.threadId ? orch.threads.get(n.threadId) : undefined;
  const k = KIND[n.kind] || KIND.finished;
  return (
    <li className={`nt-i k-${n.kind} ${n.read ? "" : "unread"} ${n.resolved ? "resolved" : ""}`}>
      <button className="nt-b" onClick={() => onOpen(n)}>
        <span className="nt-ic" aria-hidden>
          {n.resolved ? KIND.finished.ic : k.ic}
        </span>
        <span className="nt-m">
          <span className="nt-t">
            <b>{n.title}</b>
            {n.count && n.count > 1 ? <span className="pill p-mute">{n.count} events</span> : null}
          </span>
          {n.body && <span className="nt-body">{n.body}</span>}
          <span className="nt-meta">
            <span>{n.resolved ? "Answered" : k.label}</span>
            {t && <span className="trunc">{t.repo || t.title}</span>}
            <span>{fmtAgo(n.at)}</span>
          </span>
        </span>
        {!n.read && <span className="nt-dot" aria-label="unread" />}
      </button>
    </li>
  );
}

export default function Notifications() {
  const st = useNotify();
  const nav = useNavigate();
  const [filter, setFilter] = useState<Filter>("");
  const list = useMemo(() => st.list.filter((n) => !filter || n.kind === filter), [st.list, filter]);
  const t0 = startOfToday();
  const groups = [
    { label: "Today", items: list.filter((n) => n.at >= t0) },
    { label: "Earlier", items: list.filter((n) => n.at < t0) },
  ].filter((g) => g.items.length);
  const open = (n: ONotification) => {
    void markRead([n.id]);
    nav(linkOf(n));
  };
  const counts = (k: NKind) => st.list.filter((n) => n.kind === k && !n.read).length;
  return (
    <div className="view pad nt">
      <div className="viewhead">
        <div>
          <h2>Notifications</h2>
          <p>
            {st.unread ? `${st.unread} unread.` : "All read."} Rules live in <Link to="/settings/notifications">Settings, Notifications</Link>.
          </p>
        </div>
        <button className="btn" disabled={!st.unread} onClick={() => markRead("all")}>
          Mark all read
        </button>
      </div>
      <div className="nt-f" role="tablist" aria-label="Filter">
        {(["", "needs-you", "finished", "error", "spend"] as Filter[]).map((f) => (
          <button key={f || "all"} role="tab" aria-selected={filter === f} className={filter === f ? "on" : ""} onClick={() => setFilter(f)}>
            {f ? KIND[f].label : "All"}
            {f && counts(f) > 0 && <b>{counts(f)}</b>}
          </button>
        ))}
      </div>
      {st.status === "loading" && !st.list.length ? (
        <Loading rows={5} />
      ) : st.status === "missing" && !st.list.length ? (
        <div className="ibox">
          <Missing endpoint="/api/notify" what="Notifications are not available on this daemon yet" />
        </div>
      ) : groups.length === 0 ? (
        <Empty title={filter ? "Nothing of this kind" : "No notifications yet"}>Driven threads that need you, finish, fail or hit a spend limit show up here.</Empty>
      ) : (
        groups.map((g) => (
          <section key={g.label} className="nt-g">
            <h3>{g.label}</h3>
            <ul>
              {g.items.map((n) => (
                <Item key={n.id} n={n} onOpen={open} />
              ))}
            </ul>
          </section>
        ))
      )}
    </div>
  );
}
