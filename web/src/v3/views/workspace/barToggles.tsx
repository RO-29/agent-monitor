// Buttons that hide and show the thread list, the details panel and the thread top bar on wide layouts.
import { useEffect, useState } from "react";
import { SHORT, setHidden, toggleBar, useLayout } from "../../layout";

// The details panel docks at 1101 px and wider; below that the button opens a drawer.
function useDocked() {
  const q = "(min-width:1101px)";
  const [on, setOn] = useState(() => window.matchMedia(q).matches);
  useEffect(() => {
    const m = window.matchMedia(q);
    const f = () => setOn(m.matches);
    m.addEventListener("change", f);
    return () => m.removeEventListener("change", f);
  }, []);
  return on;
}

export function PanelIcon({ side }: { side: "left" | "right" }) {
  const x = side === "left" ? 5.5 : 10.5;
  return (
    <svg viewBox="0 0 16 16" width="15" height="15" aria-hidden>
      <rect x="1.75" y="2.75" width="12.5" height="10.5" rx="2" fill="none" stroke="currentColor" strokeWidth="1.4" />
      <path d={`M${x} 3v10`} stroke="currentColor" strokeWidth="1.4" />
    </svg>
  );
}

/** In the thread list header: hides the list. */
export function ListHideBtn() {
  return (
    <button className="wlay-btn" onClick={() => setHidden("left", true)} title={`Hide thread list (${SHORT.left})`} aria-label="Hide thread list">
      <PanelIcon side="left" />
    </button>
  );
}

function Chevron({ dir }: { dir: "left" | "right" }) {
  return (
    <svg viewBox="0 0 16 16" width="12" height="12" aria-hidden>
      <path d={dir === "right" ? "M6 3.5 10.5 8 6 12.5" : "M10 3.5 5.5 8 10 12.5"} fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round" strokeLinejoin="round" />
    </svg>
  );
}

/** Full-height strips on the edges of the window that bring back a hidden list or details panel. */
export function EdgeBtns({ side }: { side: boolean }) {
  const l = useLayout();
  return (
    <>
      {l.left && (
        <button className="wlay-edge l" onClick={() => setHidden("left", false)} title={`Show thread list (${SHORT.left})`} aria-label="Show thread list">
          <span className="wlay-tab">
            <Chevron dir="right" />
          </span>
        </button>
      )}
      {l.right && side && (
        <button className="wlay-edge r" onClick={() => setHidden("right", false)} title={`Show details (${SHORT.right})`} aria-label="Show details">
          <span className="wlay-tab">
            <Chevron dir="left" />
          </span>
        </button>
      )}
    </>
  );
}

/** At the left of the thread header while the thread list is hidden: brings it back. */
export function ListShowBtn() {
  const l = useLayout();
  if (!l.left) return null;
  return (
    <button className="wlay-lshow" onClick={() => setHidden("left", false)} title={`Show thread list (${SHORT.left})`} aria-label="Show thread list">
      <PanelIcon side="left" />
    </button>
  );
}

/** Details panel toggle at the far right of the header; below 1101 px it opens the drawer. */
export function DetailsBtn({ onSide }: { onSide: () => void }) {
  const l = useLayout();
  const shown = useDocked() && !l.right;
  const label = shown ? "Hide details" : "Show details";
  return (
    <button className={`wsidebtn wlay-rbtn ${shown ? "wlay-on" : ""}`} onClick={onSide} title={`${label} (${SHORT.right})`} aria-label={label} aria-pressed={shown}>
      <PanelIcon side="right" />
    </button>
  );
}

/** ⋯ menu item for the top bar. */
export function TopBarItem() {
  const l = useLayout();
  return (
    <button role="menuitem" className="wlay-wide" onClick={() => toggleBar("top")}>
      {l.top ? "Show top bar" : "Hide top bar"}
      <kbd className="wkbd">{SHORT.top}</kbd>
    </button>
  );
}

/** While the top bar is hidden: a hover strip that slides it in, and a small button that restores it. */
export function TopReveal() {
  const l = useLayout();
  if (!l.top) return null;
  return (
    <>
      <div className="wlay-strip" aria-hidden />
      <button className="wlay-show" onClick={() => setHidden("top", false)} title={`Show top bar (${SHORT.top})`} aria-label="Show top bar">
        <svg viewBox="0 0 16 16" width="12" height="12" aria-hidden>
          <path d="M4 6l4 4 4-4" fill="none" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round" strokeLinejoin="round" />
        </svg>
      </button>
    </>
  );
}
