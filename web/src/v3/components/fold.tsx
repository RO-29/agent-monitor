// Collapsible sections whose open or closed state persists per section in localStorage.
import { useState, type ReactNode } from "react";

const PREFIX = "am-fold:";

function readFold(id: string, def: boolean): boolean {
  try {
    const v = localStorage.getItem(PREFIX + id);
    return v === null ? def : v === "1";
  } catch {
    return def;
  }
}

/** Returns [closed, toggle]. */
export function useFold(id: string, defClosed = false): [boolean, () => void] {
  const [closed, setClosed] = useState(() => readFold(id, defClosed));
  const toggle = () =>
    setClosed((c) => {
      try {
        localStorage.setItem(PREFIX + id, c ? "0" : "1");
      } catch {
        /* storage blocked */
      }
      return !c;
    });
  return [closed, toggle];
}

/** The chevron button that heads a foldable section. */
export function FoldHead({ closed, onToggle, children, className = "" }: { closed: boolean; onToggle: () => void; children: ReactNode; className?: string }) {
  return (
    <button type="button" className={`fold-h ${className}`} aria-expanded={!closed} onClick={onToggle}>
      <span className={`fold-i ${closed ? "" : "open"}`} aria-hidden>
        ›
      </span>
      {children}
    </button>
  );
}

/** A side-panel box (.wbox) whose title toggles it; closed shows the summary, open shows `right`. */
export function FoldBox({ id, title, right, summary, className = "", children }: { id: string; title: ReactNode; right?: ReactNode; summary?: ReactNode; className?: string; children?: ReactNode }) {
  const [closed, toggle] = useFold(`side:${id}`);
  return (
    <div className={`wbox ${className} ${closed ? "fold-c" : ""}`}>
      <h4>
        <FoldHead closed={closed} onToggle={toggle}>
          {title}
        </FoldHead>
        {closed && summary != null && summary !== "" ? <span className="r muted sm fold-s">{summary}</span> : right}
      </h4>
      {!closed && children}
    </div>
  );
}
