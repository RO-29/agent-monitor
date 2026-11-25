import { useEffect, useMemo, useRef, useState } from "react";
import { toMs, type OItem } from "../../api/orch";

export type Pending = { id: string; text: string; at: number; queued: boolean };

const norm = (s: string) => s.replace(/\s+/g, " ").trim();

// Messages sent from here until the transcript shows them; the iOS app has the same rule.
export function usePendingSends(items: OItem[] | undefined, reset: string) {
  const [list, setList] = useState<Pending[]>([]);
  useEffect(() => setList([]), [reset]);
  useEffect(() => {
    if (!list.length || !items) return;
    const users = items.filter((i) => i.kind === "user");
    // Each transcript message clears one bubble, so two quick "continue" sends keep the second.
    const used = new Set<OItem>();
    const left = list.filter((p) => {
      const u = users.find((u) => !used.has(u) && toMs(u.startedAt) >= p.at - 120_000 && norm(u.text || "").startsWith(norm(p.text)));
      if (u) used.add(u);
      return !u;
    });
    if (left.length !== list.length) setList(left);
  }, [items, list]);
  const add = (text: string, queued: boolean) => {
    if (!text.trim() || /^[/!]/.test(text.trim())) return;
    setList((l) => [...l, { id: `p${Date.now()}-${l.length}`, text, at: Date.now(), queued }]);
  };
  const drop = (id: string) => setList((l) => l.filter((p) => p.id !== id));
  return { list, add, drop };
}

export function PendingBubbles({ list, drop }: { list: Pending[]; drop: (id: string) => void }) {
  const [now, setNow] = useState(Date.now());
  useEffect(() => {
    if (!list.length) return;
    const t = setInterval(() => setNow(Date.now()), 5000);
    return () => clearInterval(t);
  }, [list.length]);
  return (
    <>
      {list.map((p) => {
        const late = now - p.at > (p.queued ? 600_000 : 90_000);
        const st = late ? "Not in the transcript yet." : p.queued ? "Queued: it goes when the turn ends" : "Sent. Waiting for the transcript…";
        return (
          <div key={p.id} className="wu-wrap wpend">
            <div className="wu">{p.text}</div>
            <div className={`wmeta wmeta-u ${late ? "wpend-late" : ""}`} role="status">
              {st}
              <button className="btn sm ghost" onClick={() => drop(p.id)} aria-label="Hide this message">Hide</button>
            </div>
          </div>
        );
      })}
    </>
  );
}

// The "New" line: the first agent item after the last time this chat was read to the bottom.
export function useNewMark(key: string, items: OItem[] | undefined, atBottom: boolean) {
  const sk = "chat.seen:" + key;
  const read = () => {
    try {
      return Number(localStorage.getItem(sk)) || 0;
    } catch {
      return 0;
    }
  };
  const [seenAtOpen, setSeenAtOpen] = useState(read);
  const keyRef = useRef(key);
  // The newest item at the first load: turns that arrive while the chat is open are not "new".
  const openLast = useRef(0);
  if (keyRef.current !== key) {
    keyRef.current = key;
    openLast.current = 0;
    setSeenAtOpen(read());
  }
  const last = useMemo(() => (items || []).reduce((m, i) => Math.max(m, toMs(i.startedAt)), 0), [items]);
  if (!openLast.current && last) openLast.current = last;
  useEffect(() => {
    if (!atBottom || !last) return;
    try {
      if (last > read()) localStorage.setItem(sk, String(last));
    } catch {
      /* storage blocked: the line still shows for this visit */
    }
  }, [atBottom, last, sk]); // eslint-disable-line react-hooks/exhaustive-deps
  return useMemo(() => {
    if (!seenAtOpen || !items) return "";
    const first = [...items].sort((a, b) => toMs(a.startedAt) - toMs(b.startedAt)).find((i) => i.kind !== "user" && toMs(i.startedAt) > seenAtOpen && toMs(i.startedAt) <= openLast.current);
    return first ? first.runId || "_" : "";
  }, [items, seenAtOpen]);
}

export const NewLine = () => (
  <div className="wnewline" role="separator" aria-label="New messages">
    <span>New</span>
  </div>
);
