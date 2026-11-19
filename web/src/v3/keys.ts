// Shared guards for single-key shortcuts, and the setting that turns single-key approval off (WCAG 2.1.4).
import { useSyncExternalStore } from "react";

const AK = "am-approve-keys";
const read = () => {
  try {
    return localStorage.getItem(AK) !== "0";
  } catch {
    return true;
  }
};
let approveOn = read();
const subs = new Set<() => void>();
export const approveKeysOn = () => approveOn;
export function setApproveKeys(on: boolean) {
  approveOn = on;
  try {
    localStorage.setItem(AK, on ? "1" : "0");
  } catch {
    /* storage off */
  }
  subs.forEach((f) => f());
}
declare global {
  interface Window {
    __amApproveKeys?: (on?: boolean) => boolean;
  }
}
// The Mac app reads and flips this setting from its menu.
window.__amApproveKeys = (on) => {
  if (typeof on === "boolean") setApproveKeys(on);
  return approveOn;
};
export const useApproveKeys = () =>
  useSyncExternalStore(
    (f) => (subs.add(f), () => subs.delete(f)),
    approveKeysOn,
  );

/** Shell sets this while a "g" sequence waits for its second key. */
export const chord = { pending: false };

/** True for text fields, contenteditable, the xterm terminal and lists that own their keys ([data-ownkeys]). */
export function inField(el: EventTarget | Element | null): boolean {
  const t = el as HTMLElement | null;
  if (!t || !t.tagName) return false;
  if (t.tagName === "INPUT" || t.tagName === "TEXTAREA" || t.tagName === "SELECT" || t.isContentEditable) return true;
  return !!t.closest?.(".xterm, [contenteditable='true'], [data-ownkeys]");
}

/** A dialog, menu, palette, drawer or picker is open over the page. */
export const overlayOpen = () => !!document.querySelector("[role='dialog'], [role='alertdialog'], [role='menu'], .scrim, .drawer, .wpop");

/** All the conditions under which a/d may decide an approval. */
export function approvalKeyOk(e: KeyboardEvent): boolean {
  if (!approveOn || e.repeat || e.defaultPrevented || e.metaKey || e.ctrlKey || e.altKey || chord.pending) return false;
  if (inField(e.target) || inField(document.activeElement)) return false;
  return !overlayOpen();
}

// Second press on the same high-risk or plan request within 4 s confirms it.
let armed: { id: string; key: string; at: number } | null = null;
export function confirmKey(id: string, key: string): boolean {
  const ok = !!armed && armed.id === id && armed.key === key && Date.now() - armed.at < 4000;
  armed = ok ? null : { id, key, at: Date.now() };
  return ok;
}
