// Opening a search hit: find the matched item in the loaded feed, page back when it is older, scroll and flash it.
import { createContext, useEffect, useRef, useState, type MutableRefObject, type RefObject } from "react";
import { toMs, type OItem, type ORun } from "../../api/orch";

export interface HitReq {
  key: string;
  span?: string;
  turn?: number;
  ts?: number;
  session?: string;
  terms: string[];
}

/** ?hit=<turn or item id>&ts=&session=&q= from a search result link. */
export function hitFromParams(sp: URLSearchParams): HitReq | null {
  const hit = sp.get("hit") || "";
  const ts = Number(sp.get("ts") || 0) || undefined;
  if (!hit && !ts) return null;
  const turn = /^\d+$/.test(hit) ? Number(hit) : undefined;
  return { key: sp.toString(), span: hit && turn === undefined ? hit : undefined, turn, ts, session: sp.get("session") || undefined, terms: (sp.get("q") || "").toLowerCase().split(/\s+/).filter((w) => w.length > 1) };
}

/** The highlighted item id; RunBlock opens its group and ItemRow flashes it. */
export const HitCtx = createContext("");

const textOf = (i: OItem) => [i.text, i.command, i.output, ...(i.files || []).map((f) => f.path)].join("\n").toLowerCase();

/** The run that holds ts: the last one that started at or before it. */
function runAt(runs: ORun[], ts: number): ORun | undefined {
  let best: ORun | undefined;
  for (const r of runs) if (toMs(r.startedAt) <= ts + 1000 && (!best || toMs(r.startedAt) >= toMs(best.startedAt))) best = r;
  return best;
}

export function locateHit(items: OItem[], runs: ORun[], h: HitReq): { item?: OItem; older: boolean } {
  if (h.span) {
    const it = items.find((i) => i.id === h.span || i.id.endsWith("/" + h.span));
    if (it) return { item: it, older: false };
  }
  let run = h.turn != null ? runs.find((r) => r.index === h.turn) : undefined;
  if (h.ts) {
    const byTs = runAt(runs, h.ts);
    // The turn number wins only when its time agrees with ts.
    if (!run || (byTs && byTs.id !== run.id && toMs(run.startedAt) > h.ts + 1000)) run = byTs;
  }
  const minIdx = runs.length ? Math.min(...runs.map((r) => r.index)) : 0;
  const minTs = runs.length ? Math.min(...runs.map((r) => toMs(r.startedAt))) : 0;
  const older = !run && runs.length > 0 && ((h.ts != null && h.ts < minTs) || (h.turn != null && h.turn < minIdx));
  const pool = run ? items.filter((i) => i.runId === run!.id) : h.ts && !runs.length ? items : [];
  if (!pool.length) return { older };
  const near = (i: OItem) => (h.ts ? Math.abs(toMs(i.startedAt) - h.ts) : 0);
  const has = (i: OItem) => h.terms.some((t) => textOf(i).includes(t));
  const best = [...pool].sort((a, b) => Number(has(b)) - Number(has(a)) || near(a) - near(b))[0];
  return { item: best, older: false };
}

/** Runs once per hit: pages back through loadEarlier until the item is loaded, then scrolls to it. */
export function useHitFocus(o: {
  hit: HitReq | null;
  items: OItem[];
  runs: ORun[];
  scroller: RefObject<HTMLElement>;
  pinned: MutableRefObject<boolean>;
  loadEarlier?: () => void;
  earlierBusy?: boolean;
  onReveal?: () => void;
}): string {
  const [hl, setHl] = useState("");
  const done = useRef("");
  const pages = useRef(0);
  const { hit, items, runs } = o;
  useEffect(() => {
    if (!hit || done.current === hit.key) return;
    o.pinned.current = false;
    o.onReveal?.();
    if (!items.length) return;
    const r = locateHit(items, runs, hit);
    if (r.item) {
      done.current = hit.key;
      pages.current = 0;
      const id = r.item.id;
      setHl(id);
      // Wait for the group to open before scrolling.
      window.setTimeout(() => {
        const el = o.scroller.current?.querySelector(`[data-iid="${CSS.escape(id)}"]`);
        el?.scrollIntoView({ block: "center" });
      }, 60);
      return;
    }
    if (r.older && o.loadEarlier && pages.current < 100) {
      if (!o.earlierBusy) {
        pages.current++;
        o.loadEarlier();
      }
      return;
    }
    done.current = hit.key;
  }, [hit?.key, items, runs, o.earlierBusy, !!o.loadEarlier]); // eslint-disable-line react-hooks/exhaustive-deps
  return hl;
}
