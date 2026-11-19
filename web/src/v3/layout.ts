// Workspace layout: which of the thread list, details panel and thread top bar the user hid. Wide layouts only.
import { useSyncExternalStore } from "react";

export type Bar = "left" | "right" | "top";
export type Layout = Record<Bar, boolean>;

const LK = "am-ws-layout";
const none: Layout = { left: false, right: false, top: false };
const read = (): Layout => {
  try {
    const o = JSON.parse(localStorage.getItem(LK) || "{}") as Partial<Layout>;
    return { left: o.left === true, right: o.right === true, top: o.top === true };
  } catch {
    return none;
  }
};
let cur = read();
const subs = new Set<() => void>();
type Handler = { postMessage: (v: unknown) => void };
// The Mac app's toolbar side buttons follow every change, keyboard shortcuts included.
const tellShell = () => (window as unknown as { webkit?: { messageHandlers?: Record<string, Handler | undefined> } }).webkit?.messageHandlers?.layout?.postMessage(cur);
const emit = () => {
  subs.forEach((f) => f());
  tellShell();
};

export const layoutNow = () => cur;
export function setHidden(b: Bar, hidden: boolean) {
  if (cur[b] === hidden) return;
  cur = { ...cur, [b]: hidden };
  try {
    localStorage.setItem(LK, JSON.stringify(cur));
  } catch {
    /* storage off */
  }
  emit();
}

// Below 1101 px the details panel is a drawer; the Workspace sets this to open it instead.
export const drawer: { open: null | (() => void) } = { open: null };
export const wide = () => window.matchMedia("(min-width:761px)").matches;
const docked = () => window.matchMedia("(min-width:1101px)").matches;

export function toggleBar(b: Bar) {
  if (b === "right" && !docked() && drawer.open) return drawer.open();
  setHidden(b, !cur[b]);
}

export const isMac = /Mac|iPhone|iPad/.test(navigator.platform || navigator.userAgent);
export const modKey = isMac ? "⌘" : "Ctrl+";
export const SHORT: Record<Bar, string> = { left: `${modKey}\\`, right: isMac ? "⌥⌘\\" : "Ctrl+Alt+\\", top: isMac ? "⇧⌘\\" : "Ctrl+Shift+\\" };

/** The bar a key event toggles: mod+\ left, mod+alt+\ right, mod+shift+\ top. */
export function barForKey(e: KeyboardEvent): Bar | null {
  if (e.code !== "Backslash" || e.repeat || (isMac ? !e.metaKey || e.ctrlKey : !e.ctrlKey || e.metaKey)) return null;
  if (e.altKey && e.shiftKey) return null;
  // Ctrl+\ is SIGQUIT in the terminal; leave it there.
  if (!isMac && (e.target as HTMLElement | null)?.closest?.(".xterm")) return null;
  return e.shiftKey ? "top" : e.altKey ? "right" : "left";
}

window.addEventListener("storage", (e) => {
  if (e.key !== LK) return;
  cur = read();
  emit();
});

declare global {
  interface Window {
    __amToggle?: (b: Bar) => Layout;
    __amLayout?: () => Layout;
  }
}
// The Mac app's View menu toggles these and reads them back for its item titles.
window.__amToggle = (b) => {
  if (b === "left" || b === "right" || b === "top") toggleBar(b);
  return cur;
};
window.__amLayout = () => cur;

export const useLayout = () =>
  useSyncExternalStore(
    (f) => (subs.add(f), () => subs.delete(f)),
    layoutNow,
  );
