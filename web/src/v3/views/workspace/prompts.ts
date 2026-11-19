// Prompt history from the agents' own history files (GET /api/prompts): this session's first, then all others.
import { useCallback, useRef, useState } from "react";
import { req } from "../../api/orch";

export interface PromptRun {
  text: string;
  at?: number;
  agent?: string;
  sessionId?: string;
  cwd?: string;
  bash?: boolean;
}
export interface PromptHistory {
  list: string[];
  load: () => Promise<string[]>;
}

const fetchPrompts = (q: string) => req<{ items?: PromptRun[] }>("GET", `/api/prompts?bash=no&${q}`).then((r) => (r.items || []).filter((x) => x?.text?.trim()));

/** Loads on focus and again after 20 s; an older daemon gives an empty list. */
export function usePromptHistory(session?: string): PromptHistory {
  const [list, setList] = useState<string[]>([]);
  const at = useRef({ key: "", t: 0, list: [] as string[] });
  const load = useCallback(async () => {
    const key = session || "";
    if (at.current.key === key && Date.now() - at.current.t < 20_000) return at.current.list;
    const [mine, all] = await Promise.allSettled([
      session ? fetchPrompts(`limit=100&session=${encodeURIComponent(session)}`) : Promise.resolve([]),
      fetchPrompts("limit=300"),
    ]);
    const seen = new Set<string>();
    const out: string[] = [];
    for (const r of [mine, all]) {
      if (r.status !== "fulfilled") continue;
      for (const x of r.value) {
        const k = x.text.trim();
        if (!seen.has(k)) {
          seen.add(k);
          out.push(x.text);
        }
      }
    }
    at.current = { key, t: Date.now(), list: out };
    setList(out);
    return out;
  }, [session]);
  return { list, load };
}
