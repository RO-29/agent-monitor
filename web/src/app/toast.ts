// In-page toast stack (capped at 4). Alerts (permission / input needed) live
// 12 s and are clickable; plain toasts live 8 s. Mirrors the legacy behaviour.
import { useSyncExternalStore } from "react";

export interface Toast {
  id: number;
  title: string;
  body?: string;
  kind: "info" | "alert";
  onClick?: () => void;
}

let toasts: Toast[] = [];
let nextId = 1;
const listeners = new Set<() => void>();
function emit() {
  toasts = [...toasts];
  listeners.forEach((l) => l());
}

export function showToast(title: string, body?: string, opts: { kind?: "info" | "alert"; onClick?: () => void; ms?: number } = {}) {
  const t: Toast = { id: nextId++, title, body, kind: opts.kind || "info", onClick: opts.onClick };
  toasts.push(t);
  while (toasts.length > 4) toasts.shift();
  emit();
  window.setTimeout(() => dismissToast(t.id), opts.ms ?? (t.kind === "alert" ? 12000 : 8000));
}
export function dismissToast(id: number) {
  const n = toasts.filter((t) => t.id !== id);
  if (n.length !== toasts.length) {
    toasts = n;
    emit();
  }
}
export function useToasts(): Toast[] {
  return useSyncExternalStore(
    (l) => {
      listeners.add(l);
      return () => listeners.delete(l);
    },
    () => toasts,
    () => toasts,
  );
}
