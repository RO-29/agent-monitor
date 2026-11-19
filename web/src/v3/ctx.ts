import { createContext, useContext } from "react";
import type { Session } from "../api/types";

export interface ShellCtx {
  openPalette: (opts?: { q?: string; scope?: string }) => void;
  openPane: (s: Session) => void;
  setScope: (repo: string | undefined) => void;
}
export const Ctx = createContext<ShellCtx>({ openPalette: () => {}, openPane: () => {}, setScope: () => {} });
export const useShell = () => useContext(Ctx);

/** True while typing in a field, so single-key shortcuts stay quiet. */
export const typing = (e: KeyboardEvent) => {
  const t = e.target as HTMLElement | null;
  return !!t && (t.tagName === "INPUT" || t.tagName === "TEXTAREA" || t.tagName === "SELECT" || t.isContentEditable || !!t.closest?.("[data-ownkeys]"));
};
