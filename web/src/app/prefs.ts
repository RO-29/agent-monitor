// localStorage-backed preferences. Keys are the legacy ones so an existing
// browser keeps its settings across the rewrite.
import { useSyncExternalStore } from "react";

export const KEYS = {
  theme: "agent-monitor-theme",
  notify: "agent-monitor-notify",
  sound: "agent-monitor-sound",
  collapsed: "agent-monitor-collapsed",
  noAutoRegister: "agent-monitor-no-auto-register",
} as const;

function read(key: string): string | null {
  try {
    return localStorage.getItem(key);
  } catch {
    return null;
  }
}
function write(key: string, v: string | null) {
  try {
    if (v === null) localStorage.removeItem(key);
    else localStorage.setItem(key, v);
  } catch {
    /* storage blocked */
  }
  bump();
}

const listeners = new Set<() => void>();
let version = 0;
function bump() {
  version++;
  listeners.forEach((l) => l());
}
function usePrefsVersion() {
  return useSyncExternalStore(
    (l) => {
      listeners.add(l);
      return () => listeners.delete(l);
    },
    () => version,
    () => version,
  );
}

export type Theme = "dark" | "light";
export type ThemeMode = Theme | "system";
function systemTheme(): Theme {
  try {
    return window.matchMedia("(prefers-color-scheme: light)").matches ? "light" : "dark";
  } catch {
    return "dark";
  }
}
export function getThemeMode(): ThemeMode {
  const v = read(KEYS.theme);
  return v === "light" || v === "system" ? v : "dark";
}
export function getTheme(): Theme {
  const m = getThemeMode();
  return m === "system" ? systemTheme() : m;
}
export function setThemeMode(m: ThemeMode) {
  write(KEYS.theme, m);
  document.documentElement.dataset.theme = getTheme();
}
export function setTheme(t: Theme) {
  setThemeMode(t);
}
export function useTheme(): [Theme, () => void] {
  usePrefsVersion();
  const t = getTheme();
  return [t, () => setTheme(t === "dark" ? "light" : "dark")];
}
// "system" follows the OS; the Mac app drives the choice through these hooks.
try {
  window.matchMedia("(prefers-color-scheme: light)").addEventListener("change", () => {
    if (getThemeMode() !== "system") return;
    document.documentElement.dataset.theme = getTheme();
    bump();
  });
} catch {
  /* no matchMedia */
}
declare global {
  interface Window {
    __amSetTheme?: (m: ThemeMode) => void;
    __amTheme?: () => ThemeMode;
  }
}
window.__amSetTheme = (m) => setThemeMode(m === "light" || m === "system" ? m : "dark");
window.__amTheme = getThemeMode;

export function useBoolPref(key: string, def: boolean): [boolean, (v: boolean) => void] {
  usePrefsVersion();
  const raw = read(key);
  const v = raw === null ? def : raw !== "0";
  return [v, (nv) => write(key, nv ? "1" : "0")];
}

export function getNoAutoRegister(): string[] {
  try {
    const v = JSON.parse(read(KEYS.noAutoRegister) || "[]");
    return Array.isArray(v) ? v : [];
  } catch {
    return [];
  }
}
export function setNoAutoRegister(id: string, on: boolean) {
  const cur = new Set(getNoAutoRegister());
  if (on) cur.add(id);
  else cur.delete(id);
  write(KEYS.noAutoRegister, JSON.stringify([...cur]));
}
