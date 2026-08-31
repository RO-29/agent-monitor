// Topbar search text, shared with the threads list ("/" focuses it).
import { useSyncExternalStore } from "react";

let q = "";
const ls = new Set<() => void>();
export function setSearch(v: string) {
  q = v;
  ls.forEach((l) => l());
}
export function useSearch(): string {
  return useSyncExternalStore(
    (l) => {
      ls.add(l);
      return () => ls.delete(l);
    },
    () => q,
    () => q,
  );
}
export let searchInput: HTMLInputElement | null = null;
export function bindSearchInput(el: HTMLInputElement | null) {
  searchInput = el;
}
