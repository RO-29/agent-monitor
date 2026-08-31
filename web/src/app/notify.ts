// Notification policy (ported from the legacy UI):
//   awaiting-permission → notify whenever the message text changes
//   awaiting-input      → notify once, on the transition into the state
//   any other state     → reset both memories
// Permission requests and talks (WS) notify on arrival.
import { useEffect, useRef } from "react";
import type { Session, WsEvent } from "../api/types";
import { projectName, titleFor, toolName } from "../lib/format";
import { onLiveEvent, useLive } from "../lib/ws";
import { KEYS } from "./prefs";
import { showToast } from "./toast";

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
      const title = `${toolName(s.tool)} · ${projectName(s.cwd)} needs you`;
      const go = () => navigate(`/session/${encodeURIComponent(s.id)}?tab=pane`);
      osNotify(title, msg, s.id, go);
      showToast(title, `${titleFor(s)} — ${msg}`, { kind: "alert", onClick: go });
      playBeep();
    };
    // First snapshot: alert for anything already waiting, then track deltas.
    live.sessions.forEach((s) => {
      if (s.state === "awaiting-permission") {
        const msg = s.permissionMessage || s.lastMessage || "needs permission";
        if (permMsg.current.get(s.id) !== msg) {
          permMsg.current.set(s.id, msg);
          fire(s, msg);
        }
      } else if (s.state === "awaiting-input") {
        if (seen.current && !inputNotified.current.has(s.id)) {
          inputNotified.current.add(s.id);
          fire(s, s.lastMessage || "waiting for your reply");
        } else inputNotified.current.add(s.id);
      } else {
        permMsg.current.delete(s.id);
        inputNotified.current.delete(s.id);
      }
    });
    seen.current = true;
  }, [live.version]); // eslint-disable-line react-hooks/exhaustive-deps

  useEffect(() => {
    return onLiveEvent((e: WsEvent) => {
      if (e.kind === "perm-add") {
        const body = JSON.stringify(e.request.input).slice(0, 120);
        osNotify(`${e.request.toolName} wants permission`, body, `perm-${e.request.id}`, () => navigate("/"));
        showToast(`${e.request.toolName} wants permission`, body, { kind: "alert", onClick: () => navigate("/") });
        playBeep();
      } else if (e.kind === "talk-request") {
        const go = () => navigate(`/session/${encodeURIComponent(e.talk.toAgent)}?tab=pane`);
        osNotify(`Incoming talk from ${e.talk.fromLabel}`, e.talk.message.slice(0, 140), `talk-${e.talk.id}`, go);
        showToast(`Incoming talk from ${e.talk.fromLabel}`, e.talk.message.slice(0, 140), { kind: "alert", onClick: go });
        playBeep();
      }
    });
  }, [navigate]);
}
