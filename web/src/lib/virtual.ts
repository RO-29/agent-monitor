import { useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState } from "react";

// Windowing for lists whose rows are not a fixed height: a transcript turn is
// a paragraph or a page, and expanding one changes it. Rows are measured as
// they mount, unmeasured rows use an estimate, and a row that changes height
// above the viewport has its delta added to scrollTop so the view does not jump.

export interface Virtual {
  start: number;
  end: number; // exclusive
  padTop: number;
  padBottom: number;
  total: number;
  /** ref callback for each rendered row, keyed so measurements survive reorder */
  rowRef: (key: string) => (el: HTMLElement | null) => void;
}

export function useVirtual(keys: string[], scrollRef: React.RefObject<HTMLElement>, estimate = 120, overscan = 8): Virtual {
  const heights = useRef(new Map<string, number>());
  const observed = useRef(new Map<Element, string>());
  const [version, setVersion] = useState(0);
  const [scrollTop, setScrollTop] = useState(0);
  const [viewport, setViewport] = useState(800);
  const startRef = useRef(0);
  const indexOf = useRef(new Map<string, number>());
  const pending = useRef(0); // scrollTop correction owed by rows above the window

  indexOf.current = useMemo(() => new Map(keys.map((k, i) => [k, i])), [keys]);

  const ro = useRef<ResizeObserver | null>(null);
  if (!ro.current && typeof ResizeObserver !== "undefined") {
    ro.current = new ResizeObserver((entries) => {
      let changed = false;
      for (const e of entries) {
        const key = observed.current.get(e.target);
        if (!key) continue;
        const h = Math.round((e.target as HTMLElement).offsetHeight);
        if (!h) continue;
        const prev = heights.current.get(key);
        if (prev === h) continue;
        // a row above the window changing height would slide the content
        if (prev !== undefined && (indexOf.current.get(key) ?? 0) < startRef.current) pending.current += h - prev;
        heights.current.set(key, h);
        changed = true;
      }
      if (changed) setVersion((n) => n + 1);
    });
  }

  useEffect(() => () => ro.current?.disconnect(), []);

  const rowRef = useCallback((key: string) => (el: HTMLElement | null) => {
    if (!el) return;
    if (observed.current.get(el) === key) return;
    observed.current.set(el, key);
    ro.current?.observe(el);
    const h = Math.round(el.offsetHeight);
    if (h && heights.current.get(key) !== h) {
      heights.current.set(key, h);
      setVersion((n) => n + 1);
    }
  }, []);

  // Scroll and size of the scroller itself. The element does not exist on the
  // first render (the list is still loading), so this retries until it does:
  // attaching once on mount silently left the window frozen at the top.
  useEffect(() => {
    let raf = 0;
    let el: HTMLElement | null = null;
    let r: ResizeObserver | null = null;
    const onScroll = () => el && setScrollTop(el.scrollTop);
    const attach = () => {
      el = scrollRef.current;
      if (!el) {
        raf = requestAnimationFrame(attach);
        return;
      }
      el.addEventListener("scroll", onScroll, { passive: true });
      r = new ResizeObserver(() => el && setViewport(el.clientHeight || 800));
      r.observe(el);
      setViewport(el.clientHeight || 800);
      setScrollTop(el.scrollTop);
    };
    attach();
    return () => {
      cancelAnimationFrame(raf);
      el?.removeEventListener("scroll", onScroll);
      r?.disconnect();
    };
  }, [scrollRef]);

  // pay back the offset owed by rows that resized above the window
  useLayoutEffect(() => {
    if (!pending.current) return;
    const d = pending.current;
    pending.current = 0;
    const el = scrollRef.current;
    if (el) el.scrollTop += d;
  });

  const { offsets, total } = useMemo(() => {
    const off = new Array<number>(keys.length + 1);
    off[0] = 0;
    for (let i = 0; i < keys.length; i++) off[i + 1] = off[i] + (heights.current.get(keys[i]) ?? estimate);
    return { offsets: off, total: off[keys.length] };
  }, [keys, estimate, version]);

  const find = (y: number) => {
    let lo = 0;
    let hi = keys.length;
    while (lo < hi) {
      const mid = (lo + hi) >> 1;
      if (offsets[mid + 1] <= y) lo = mid + 1;
      else hi = mid;
    }
    return Math.min(lo, Math.max(keys.length - 1, 0));
  };

  const start = Math.max(0, find(scrollTop) - overscan);
  const end = Math.min(keys.length, find(scrollTop + viewport) + 1 + overscan);
  startRef.current = start;

  return { start, end, padTop: offsets[start] || 0, padBottom: Math.max(total - (offsets[end] || 0), 0), total, rowRef };
}
