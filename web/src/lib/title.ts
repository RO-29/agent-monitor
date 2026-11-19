import { useEffect } from "react";

/** Sets the page title (the Mac app shows it as the window title) and restores the default on unmount. */
export function useDocTitle(title: string | undefined) {
  useEffect(() => {
    const t = (title || "").replace(/\s+/g, " ").trim();
    if (!t) return;
    document.title = t.length > 80 ? t.slice(0, 79) + "…" : t;
    return () => {
      document.title = "agent-monitor";
    };
  }, [title]);
}
