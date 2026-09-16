import { useEffect, useState } from "react";
import { useLocation, useNavigate } from "react-router-dom";
import { api } from "../../api/client";
import type { PermissionRequest, Session, Talk } from "../../api/types";
import { fmtAgo, projectName, titleFor } from "../../lib/format";
import { Icon, ToolLogo } from "../../lib/icons";
import { attentionOf, dropPerm, dropTalk, useLive } from "../../lib/ws";
import { showToast } from "../../app/toast";

// One bar, never a stack. It renders the live attention set, so it cannot
// replay a backlog on load and cannot grow past one row.

type Kind = "perm" | "input" | "talk";

interface Item {
  key: string;
  kind: Kind;
  tool: Session["tool"];
  who: string; // project or agent label
  what: string; // the headline: what is blocked
  ask: string; // the concrete question or command, "" when there is none
  ts: number;
  sessionId?: string;
  perm?: PermissionRequest;
  talk?: Talk;
}

// Filler the agents emit when they have no real question; repeating it under
// "waiting for you" says the same thing twice.
const FILLER = /^(claude|codex|the agent) is waiting for your (answer|input|reply)\.?$/i;
const askOf = (s?: string) => (s && !FILLER.test(s.trim()) ? s.trim() : "");

function permAsk(r: PermissionRequest): string {
  const v = (r.input.command || r.input.file_path || r.input.url || r.input.query) as string | undefined;
  return v ? String(v) : "";
}

export default function AlertBar() {
  const live = useLive();
  const nav = useNavigate();
  const loc = useLocation();
  const [open, setOpen] = useState(false);
  const [snoozed, setSnoozed] = useState("");
  const [busy, setBusy] = useState("");

  const { perms, waiting, talks } = attentionOf(live);
  const viewing = loc.pathname.startsWith("/session/") ? decodeURIComponent(loc.pathname.split("/")[2] || "") : "";

  const items: Item[] = [
    ...perms.map((r): Item => {
      const s = [...live.sessions.values()].find((x) => x.sessionId === r.sessionId);
      return {
        key: `perm-${r.id}`,
        kind: "perm",
        tool: s?.tool || "claude",
        who: projectName(r.cwd || s?.cwd || ""),
        what: `${r.toolName} wants permission`,
        ask: permAsk(r),
        ts: r.createdAt,
        sessionId: s?.id,
        perm: r,
      };
    }),
    ...talks.map((t): Item => ({
      key: `talk-${t.id}`,
      kind: "talk",
      tool: "claude",
      who: t.fromLabel,
      what: `talk for ${t.toLabel || t.toAgent.slice(0, 14)}`,
      ask: t.message.slice(0, 160),
      ts: t.createdAt,
      sessionId: t.toAgent,
      talk: t,
    })),
    ...waiting.map((s): Item => ({
      key: `wait-${s.id}`,
      kind: "input",
      tool: s.tool,
      who: projectName(s.cwd),
      what: s.state === "awaiting-permission" ? "waiting on a permission prompt" : "waiting for you",
      ask: askOf(s.permissionMessage || s.lastMessage) || askOf(titleFor(s)),
      ts: s.lastActivityAt,
      sessionId: s.id,
    })),
  ].filter((i) => !i.sessionId || i.sessionId !== viewing);

  const sig = items.map((i) => i.key).join("|");
  useEffect(() => {
    if (snoozed && snoozed !== sig) setSnoozed("");
    if (items.length === 1 && open) setOpen(false); // a transient empty set must not collapse it
  }, [sig]); // eslint-disable-line react-hooks/exhaustive-deps

  if (!items.length || snoozed === sig) return null;
  const front = items[0];
  const rest = items.length - 1;

  const go = (i: Item) => i.sessionId && nav(`/session/${encodeURIComponent(i.sessionId)}?tab=pane`);
  const respond = async (i: Item, behavior: "allow" | "deny") => {
    setBusy(i.key);
    try {
      if (i.perm) {
        await api.respondPerm(i.perm.id, behavior);
        dropPerm(i.perm.id);
      } else if (i.talk) {
        await api.respondTalk(i.talk.id, behavior);
        dropTalk(i.talk.id);
      }
    } catch (e) {
      showToast("Response failed", String((e as Error).message));
    } finally {
      setBusy("");
    }
  };

  const actions = (i: Item) => (
    <>
      {i.kind === "perm" && (
        <>
          <button className="ok" disabled={busy === i.key} onClick={() => respond(i, "allow")}>allow once</button>
          <button className="no" disabled={busy === i.key} onClick={() => respond(i, "deny")}>deny</button>
        </>
      )}
      {i.kind === "talk" && (
        <>
          <button className="ok" disabled={busy === i.key} onClick={() => respond(i, "allow")}>deliver</button>
          <button className="no" disabled={busy === i.key} onClick={() => respond(i, "deny")}>deny</button>
        </>
      )}
      {i.sessionId && <button onClick={() => go(i)}>open</button>}
    </>
  );

  const row = (i: Item) => (
    <>
      <span className={`dot ${i.kind}`} />
      <ToolLogo tool={i.tool} size={13} />
      <span className="who">{i.who}</span>
      <span className="what">{i.what}</span>
      {i.ask && <span className="ask" title={i.ask}>{i.ask}</span>}
      <span className="age">{fmtAgo(i.ts, "")}</span>
    </>
  );

  return (
    <div className={`alertbar ${front.kind}`}>
      {open && (
        <div className="list">
          {items.map((i) => (
            <div key={i.key} className="line">
              {row(i)}
              <span className="acts">{actions(i)}</span>
            </div>
          ))}
        </div>
      )}
      <div className="line front">
        {rest > 0 && <span className="count">{items.length} need you</span>}
        {!open && row(front)}
        {open && <span className="what">every agent waiting on you</span>}
        <span className="acts">
          {!open && actions(front)}
          {rest > 0 && (
            <button className="more" onClick={() => setOpen(!open)}>
              {open ? "collapse" : `+${rest} more`} <Icon name={open ? "chevd" : "chev"} size={10} />
            </button>
          )}
          <button className="x" title="hide until something changes" onClick={() => setSnoozed(sig)}>
            <Icon name="x" size={12} />
          </button>
        </span>
      </div>
    </div>
  );
}
