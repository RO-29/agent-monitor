import { useEffect, useState, type RefObject } from "react";

/** Top and Bottom buttons over a chat feed; each shows only when that end is far away. */
export function ScrollJump({ el }: { el: RefObject<HTMLDivElement | null> }) {
  const [pos, setPos] = useState({ top: false, bottom: false });
  useEffect(() => {
    const f = el.current;
    if (!f) return;
    const read = () => {
      const far = Math.max(300, f.clientHeight);
      setPos({ top: f.scrollTop > far, bottom: f.scrollHeight - f.scrollTop - f.clientHeight > far });
    };
    read();
    f.addEventListener("scroll", read, { passive: true });
    const ro = new ResizeObserver(read);
    ro.observe(f);
    return () => (f.removeEventListener("scroll", read), ro.disconnect());
  }, [el]);
  const go = (to: "top" | "bottom") => {
    const f = el.current;
    if (f) f.scrollTo({ top: to === "top" ? 0 : f.scrollHeight, behavior: "auto" });
  };
  if (!pos.top && !pos.bottom) return null;
  return (
    <div className="wjump">
      <div className="wjump-b">
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
