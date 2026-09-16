// OS notifications only. What needs the user in-page is the AlertBar, which
// renders the live attention set; duplicating it as toasts is what produced
// the pile-up. Nothing fires for the backlog present when the page opens:
// the baseline is the first snapshot that actually carries sessions.
//   awaiting-permission → notify whenever the message text changes
//   awaiting-input      → notify once, on the transition into the state
//   any other state     → reset both memories
import { useEffect, useRef } from "react";
import type { Session, WsEvent } from "../api/types";
import { projectName } from "../lib/format";
import { onLiveEvent, useLive } from "../lib/ws";
import { KEYS } from "./prefs";

function pref(key: string, def: boolean) {
  try {
    const v = localStorage.getItem(key);
    return v === null ? def : v !== "0";
  } catch {
    return def;
  }
}

export function playBeep() {
  if (!pref(KEYS.sound, true)) return;
  try {
    const AC = window.AudioContext || (window as unknown as { webkitAudioContext: typeof AudioContext }).webkitAudioContext;
    const ctx = new AC();
    const g = ctx.createGain();
    g.connect(ctx.destination);
    g.gain.setValueAtTime(0.0001, ctx.currentTime);
    g.gain.exponentialRampToValueAtTime(0.25, ctx.currentTime + 0.02);
    g.gain.exponentialRampToValueAtTime(0.0001, ctx.currentTime + 0.42);
    const o = ctx.createOscillator();
    o.type = "sine";
    o.frequency.setValueAtTime(880, ctx.currentTime);
    o.frequency.setValueAtTime(660, ctx.currentTime + 0.12);
    o.connect(g);
    o.start();
    o.stop(ctx.currentTime + 0.42);
    o.onended = () => ctx.close();
  } catch {
    /* no audio */
  }
}

function osNotify(title: string, body: string, tag: string, onClick?: () => void) {
  if (!pref(KEYS.notify, true)) return;
  if (typeof Notification === "undefined" || Notification.permission !== "granted") return;
  try {
    const n = new Notification(title, { body, tag, requireInteraction: true });
    n.onclick = () => {
      window.focus();
      onClick?.();
      n.close();
    };
  } catch {
    /* blocked */
  }
}

/** Mount once (in Shell). Watches sessions + WS events and fires alerts. */
export function useNotifications(navigate: (path: string) => void) {
  const live = useLive();
  const permMsg = useRef(new Map<string, string>());
  const inputNotified = useRef(new Set<string>());
  const seen = useRef(false);

  useEffect(() => {
    const fire = (s: Session, msg: string) => {
      const go = () => navigate(`/session/${encodeURIComponent(s.id)}?tab=pane`);
      osNotify(`${projectName(s.cwd)} needs you`, msg, s.id, go);
      playBeep();
    };
    // The first snapshot that carries sessions is the baseline, not an event:
    // everything already waiting when the page opens is shown by the AlertBar.
    if (!seen.current && live.sessions.size > 0) {
      live.sessions.forEach((s) => {
        if (s.state === "awaiting-permission") permMsg.current.set(s.id, s.permissionMessage || s.lastMessage || "needs permission");
        else if (s.state === "awaiting-input") inputNotified.current.add(s.id);
      });
      seen.current = true;
      return;
    }
    live.sessions.forEach((s) => {
      if (s.state === "awaiting-permission") {
        const msg = s.permissionMessage || s.lastMessage || "needs permission";
        if (permMsg.current.get(s.id) !== msg) {
          permMsg.current.set(s.id, msg);
          fire(s, msg);
        }
      } else if (s.state === "awaiting-input") {
        if (!inputNotified.current.has(s.id)) {
          inputNotified.current.add(s.id);
          fire(s, s.lastMessage || "waiting for your reply");
        }
      } else {
        permMsg.current.delete(s.id);
        inputNotified.current.delete(s.id);
      }
    });
  }, [live.version]); // eslint-disable-line react-hooks/exhaustive-deps

  useEffect(() => {
    return onLiveEvent((e: WsEvent) => {
      if (e.kind === "perm-add") {
        const i = e.request.input;
        const one = (i.command || i.file_path || i.url || i.query) as string | undefined;
        osNotify(`${e.request.toolName} wants permission`, one ? String(one).slice(0, 160) : JSON.stringify(i).slice(0, 120), `perm-${e.request.id}`, () => navigate("/"));
        playBeep();
      } else if (e.kind === "talk-request") {
        const go = () => navigate(`/session/${encodeURIComponent(e.talk.toAgent)}?tab=pane`);
        osNotify(`Talk from ${e.talk.fromLabel}`, e.talk.message.slice(0, 140), `talk-${e.talk.id}`, go);
        playBeep();
      }
    });
  }, [navigate]);
}
