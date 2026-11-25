import { useEffect } from "react";

// The title is "(N) <page>": N is the needs-you count, so a background tab still shows that something waits.
let base = "agent-monitor";
let need = 0;
const apply = () => {
  document.title = (need > 0 ? `(${need}) ` : "") + base;
};

export function setNeedCount(n: number) {
  if (n === need) return;
  need = n;
  apply();
}

/** Sets the page title (the Mac app shows it as the window title) and restores the default on unmount. */
export function useDocTitle(title: string | undefined) {
  useEffect(() => {
    const t = (title || "").replace(/\s+/g, " ").trim();
    if (!t) return;
    base = t.length > 80 ? t.slice(0, 79) + "…" : t;
    apply();
    return () => {
      base = "agent-monitor";
      apply();
    };
  }, [title]);
}
