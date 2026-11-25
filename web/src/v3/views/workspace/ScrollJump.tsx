import { useEffect, useState, type RefObject } from "react";

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
  return (
    <div className="wjump">
      <div className="wjump-b">
        {pos.fresh && (
          <button type="button" className="btn sm" onClick={toNew} aria-label="Go to the first new message" title="Go to the first new message">
            New {pos.fresh === "up" ? "↑" : "↓"}
          </button>
        )}
        {pos.top && (
          <button type="button" className="btn sm" onClick={() => go("top")} aria-label="Go to top" title="Go to top">
            ↑
          </button>
        )}
        {pos.bottom && (
          <button type="button" className="btn sm" onClick={() => go("bottom")} aria-label="Go to bottom" title="Go to bottom">
            ↓
          </button>
        )}
      </div>
    </div>
  );
}
