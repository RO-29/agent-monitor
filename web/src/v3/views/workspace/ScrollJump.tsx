import { useLayoutEffect, useRef, useEffect, useState, type RefObject } from "react";

/** Top and Bottom buttons over a chat feed; each shows only when that end is far away. */
export function ScrollJump({ el }: { el: RefObject<HTMLDivElement | null> }) {
  const [pos, setPos] = useState({ top: false, bottom: false, fresh: "" as "" | "up" | "down" });
  useEffect(() => {
    const f = el.current;
    if (!f) return;
    const read = () => {
      const far = Math.max(300, f.clientHeight);
      const nl = f.querySelector<HTMLElement>(".wnewline");
      const y = nl ? nl.getBoundingClientRect().top - f.getBoundingClientRect().top : 0;
      const fresh = !nl ? "" : y < 0 ? "up" : y > f.clientHeight ? "down" : "";
      setPos((p) => {
        const n = { top: f.scrollTop > far, bottom: f.scrollHeight - f.scrollTop - f.clientHeight > far, fresh } as const;
        return p.top === n.top && p.bottom === n.bottom && p.fresh === n.fresh ? p : n;
      });
    };
    let raf = 0;
    const soon = () => (cancelAnimationFrame(raf), (raf = requestAnimationFrame(read)));
    read();
    f.addEventListener("scroll", soon, { passive: true });
    const ro = new ResizeObserver(soon);
    ro.observe(f);
    // New messages change the content height, which the ResizeObserver on the box does not see.
    const mo = new MutationObserver(soon);
    mo.observe(f, { childList: true, subtree: true });
    return () => (cancelAnimationFrame(raf), f.removeEventListener("scroll", soon), ro.disconnect(), mo.disconnect());
  }, [el]);
  const go = (to: "top" | "bottom") => {
    const f = el.current;
    if (f) f.scrollTo({ top: to === "top" ? 0 : f.scrollHeight, behavior: "auto" });
  };
  const toNew = () => el.current?.querySelector<HTMLElement>(".wnewline")?.scrollIntoView({ block: "start" });
  if (!pos.top && !pos.bottom && !pos.fresh) return null;
  const chev = (up: boolean) => (
    <svg width="14" height="14" viewBox="0 0 16 16" aria-hidden="true"><path d={up ? "M4 10l4-4 4 4" : "M4 6l4 4 4-4"} fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round" strokeLinejoin="round" /></svg>
  );
  return (
    <div className="wjump">
      <div className="wjump-b" role="group" aria-label="Scroll">
        {pos.fresh && (
          <button type="button" className="wjump-new" onClick={toNew} title="Go to the first new message">
            New {chev(pos.fresh === "up")}
          </button>
        )}
        {pos.top && (
          <button type="button" onClick={() => go("top")} aria-label="Go to top" title="Go to top">
            {chev(true)}
          </button>
        )}
        {pos.bottom && (
          <button type="button" onClick={() => go("bottom")} aria-label="Go to bottom" title="Go to bottom">
            {chev(false)}
          </button>
        )}
      </div>
    </div>
  );
}

// Older turns added above the reader: keep the turn on screen where it was, as no WebKit view anchors at the top.
export function useKeepPlace(el: RefObject<HTMLDivElement | null>) {
  const at = useRef<{ h: number; top: number; t: number } | null>(null);
  useLayoutEffect(() => {
    const f = el.current, a = at.current;
    if (a && Date.now() - a.t > 15_000) at.current = null; // the load failed: forget the mark
    if (!f || !at.current || !a || f.scrollHeight === a.h) return;
    f.scrollTop = a.top + f.scrollHeight - a.h;
    at.current = null;
  });
  return () => {
    const f = el.current;
    if (f) at.current = { h: f.scrollHeight, top: f.scrollTop, t: Date.now() };
  };
}
