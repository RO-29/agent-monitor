// Hosts the browser terminal: a resizable bottom split on desktop, full screen on phone. xterm loads on first open.
import { lazy, Suspense, useEffect, useRef, useState, useSyncExternalStore } from "react";
import { shortCwd } from "../../../lib/format";
import "../../styles/term.css";

const TermPanel = lazy(() => import("./Terminal"));

export interface TermTarget {
  key: string;
  url: string;
  cwd: string;
}
export const drivenTerm = (id: string, cwd: string): TermTarget => ({ key: `t:${id}`, url: `/api/orch/threads/${encodeURIComponent(id)}/term`, cwd });
export const cwdTerm = (cwd: string): TermTarget | null => (cwd && cwd.startsWith("/") ? { key: `c:${cwd}`, url: `/api/term?cwd=${encodeURIComponent(cwd)}`, cwd } : null);

// One open flag for the whole Workspace, so the panel stays open across thread switches.
let open = false;
const subs = new Set<() => void>();
const setOpen = (v: boolean) => {
  open = v;
  subs.forEach((f) => f());
};
export const toggleTerm = () => setOpen(!open);
export const closeTerm = () => setOpen(false);
export const useTermOpen = () =>
  useSyncExternalStore(
    (f) => (subs.add(f), () => subs.delete(f)),
    () => open,
  );

/** ctrl+` toggles the panel; capture phase so xterm does not swallow it. */
export function useTermHotkey() {
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (!e.ctrlKey || e.metaKey || e.altKey || e.code !== "Backquote") return;
      e.preventDefault();
      e.stopPropagation();
      toggleTerm();
    };
    window.addEventListener("keydown", onKey, true);
    return () => window.removeEventListener("keydown", onKey, true);
  }, []);
}

const H_KEY = "am-term-h";
const readH = () => {
  try {
    const n = Number(localStorage.getItem(H_KEY));
    return n >= 120 ? n : 300;
  } catch {
    return 300;
  }
};
const useMedia = (q: string) => {
  const [m, setM] = useState(() => window.matchMedia(q).matches);
  useEffect(() => {
    const mq = window.matchMedia(q);
    const f = () => setM(mq.matches);
    mq.addEventListener("change", f);
    return () => mq.removeEventListener("change", f);
  }, [q]);
  return m;
};

export function TermDock({ target }: { target: TermTarget | null }) {
  const isOpen = useTermOpen();
  const phone = useMedia("(max-width:760px)");
  const [h, setH] = useState(readH);
  const box = useRef<HTMLDivElement>(null);
  if (!isOpen) return null;

  const drag = (e: React.PointerEvent<HTMLDivElement>) => {
    const sec = box.current?.parentElement;
    if (!sec) return;
    e.preventDefault();
    const y0 = e.clientY;
    const h0 = box.current!.getBoundingClientRect().height;
    const max = Math.max(160, sec.getBoundingClientRect().height - 150);
    let last = h0;
    const move = (ev: PointerEvent) => {
      last = Math.round(Math.min(max, Math.max(120, h0 - (ev.clientY - y0))));
      setH(last);
    };
    const up = () => {
      window.removeEventListener("pointermove", move);
      window.removeEventListener("pointerup", up);
      document.body.classList.remove("wterm-dragging");
      try {
        localStorage.setItem(H_KEY, String(last));
      } catch {
        /* private mode */
      }
    };
    document.body.classList.add("wterm-dragging");
    window.addEventListener("pointermove", move);
    window.addEventListener("pointerup", up);
  };
  const nudge = (e: React.KeyboardEvent) => {
    if (e.key !== "ArrowUp" && e.key !== "ArrowDown") return;
    e.preventDefault();
    const n = Math.max(120, h + (e.key === "ArrowUp" ? 40 : -40));
    setH(n);
    try {
      localStorage.setItem(H_KEY, String(n));
    } catch {
      /* private mode */
    }
  };

  return (
    <div ref={box} className={`wterm ${phone ? "full" : ""}`} style={phone ? undefined : { height: h }} role="region" aria-label="Terminal">
      {!phone && <div className="wterm-grip" role="separator" aria-orientation="horizontal" aria-label="Resize the terminal" tabIndex={0} onPointerDown={drag} onKeyDown={nudge} />}
      {!target ? (
        <>
          <div className="wterm-h">
            <b>Terminal</b>
            <span className="sp" />
            <button className="btn sm ghost" onClick={closeTerm} aria-label="Close the terminal">
              {phone ? "Close" : "×"}
            </button>
          </div>
          <div className="wterm-msg">
            <b>No folder for this thread</b>
            <span>agent-monitor does not know where this session runs, so it cannot open a shell there.</span>
          </div>
        </>
      ) : (
        <Suspense
          fallback={
            <div className="wterm-h">
              <b>Terminal</b>
              <span className="wterm-cwd mono trunc">{shortCwd(target.cwd)}</span>
              <span className="wterm-st">Loading…</span>
            </div>
          }
        >
          <TermPanel key={target.key} url={target.url} cwd={target.cwd} phone={phone} onClose={closeTerm} />
        </Suspense>
      )}
    </div>
  );
}
