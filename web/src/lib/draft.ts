import { useCallback, useEffect, useRef, useState } from "react";

const PREFIX = "am-draft:";

function read(key: string): string {
  try {
    return localStorage.getItem(PREFIX + key) || "";
  } catch {
    return "";
  }
}

function write(key: string, v: string) {
  try {
    if (v) localStorage.setItem(PREFIX + key, v);
    else localStorage.removeItem(PREFIX + key);
  } catch {
    /* storage blocked */
  }
}

// Unsent message-box text, kept per thread in this browser so a reload keeps it; empty text removes the entry.
export function useDraft(key: string): [string, (v: string | ((cur: string) => string)) => void] {
  const [st, setSt] = useState(() => ({ k: key, v: read(key) }));
  const cur = st.k === key ? st : { k: key, v: read(key) };
  if (cur !== st) setSt(cur);
  useEffect(() => write(cur.k, cur.v), [cur.k, cur.v]);
  const live = useRef(cur);
  live.current = cur;
  // Writes storage at once, so a send that ends after the view left this thread still clears its draft.
  const set = useCallback(
    (v: string | ((cur: string) => string)) => {
      const prev = live.current.k === key ? live.current.v : read(key);
      const next = typeof v === "function" ? v(prev) : v;
      write(key, next);
      if (live.current.k === key) live.current = { k: key, v: next };
      setSt((p) => (p.k === key ? { k: key, v: next } : p));
    },
    [key],
  );
  return [cur.v, set];
}

/** Drafts for a set of fields (one per question) under one scope; clear() drops them all after a send. */
export function useDraftMap(scope: string): [(k: string | number) => string, (k: string | number, v: string) => void, () => void] {
  const [st, setSt] = useState<{ s: string; m: Record<string, string> }>({ s: scope, m: {} });
  const m = st.s === scope ? st.m : {};
  if (st.s !== scope) setSt({ s: scope, m: {} });
  const get = (k: string | number) => m[k] ?? read(`${scope}:${k}`);
  const set = (k: string | number, v: string) => {
    try {
      if (v) localStorage.setItem(`${PREFIX}${scope}:${k}`, v);
      else localStorage.removeItem(`${PREFIX}${scope}:${k}`);
    } catch {
      /* storage blocked */
    }
    setSt((p) => ({ s: scope, m: { ...(p.s === scope ? p.m : {}), [k]: v } }));
  };
  const clear = () => {
    try {
      for (let i = localStorage.length - 1; i >= 0; i--) {
        const k = localStorage.key(i);
        if (k?.startsWith(`${PREFIX}${scope}:`)) localStorage.removeItem(k);
      }
    } catch {
      /* storage blocked */
    }
    setSt({ s: scope, m: {} });
  };
  return [get, set, clear];
}

/** Setter arg that clears a sent draft but keeps text typed while the send was in flight. */
export const clearSent = (sent: string) => (cur: string) => (cur.trim() === sent.trim() ? "" : cur);
