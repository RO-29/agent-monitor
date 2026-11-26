// One icon set for the desktop rail and the phone tab bar.
const PATHS: Record<string, string> = {
  threads: "M4 5.5A2.5 2.5 0 0 1 6.5 3h7A2.5 2.5 0 0 1 16 5.5v5a2.5 2.5 0 0 1-2.5 2.5H9l-3.5 3v-3A2.5 2.5 0 0 1 4 10.5z",
  search: "M9 3.5a5.5 5.5 0 1 1 0 11 5.5 5.5 0 0 1 0-11zM13 13l3.5 3.5",
  alerts: "M10 3a4.5 4.5 0 0 0-4.5 4.5c0 3.5-1.5 5-1.5 5h12s-1.5-1.5-1.5-5A4.5 4.5 0 0 0 10 3zM8.3 15.5a1.8 1.8 0 0 0 3.4 0",
  settings: "M10 7.2a2.8 2.8 0 1 1 0 5.6 2.8 2.8 0 0 1 0-5.6zM10 2.5v2M10 15.5v2M2.5 10h2M15.5 10h2M4.7 4.7l1.4 1.4M13.9 13.9l1.4 1.4M4.7 15.3l1.4-1.4M13.9 6.1l1.4-1.4",
  more: "M5 10h.01M10 10h.01M15 10h.01",
};

export function NavIcon({ name }: { name: keyof typeof PATHS | string }) {
  return (
    <svg className="navico" width="20" height="20" viewBox="0 0 20 20" aria-hidden="true">
      <path d={PATHS[name] || ""} fill="none" stroke="currentColor" strokeWidth={name === "more" ? 3 : 1.6} strokeLinecap="round" strokeLinejoin="round" />
    </svg>
  );
}
