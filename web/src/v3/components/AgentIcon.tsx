import { agentLabel, agentVar } from "../api/orch";

// Simple own glyphs per agent (not brand logos), tinted with the agent colour token.
const PATHS: Record<string, JSX.Element> = {
  claude: <path d="M8 1.8v12.4M1.8 8h12.4M3.6 3.6l8.8 8.8M12.4 3.6l-8.8 8.8" strokeWidth="1.7" strokeLinecap="round" />,
  codex: (
    <>
      <path d="M8 1.6 13.6 4.8v6.4L8 14.4 2.4 11.2V4.8Z" strokeWidth="1.4" strokeLinejoin="round" fill="none" />
      <path d="M5.8 6.3 7.8 8l-2 1.7M8.8 10h1.8" strokeWidth="1.3" strokeLinecap="round" strokeLinejoin="round" fill="none" />
    </>
  ),
  cursor: <path d="M3 2.2 13 7.4 8.6 8.6 7 13.4Z" strokeWidth="1.4" strokeLinejoin="round" fill="currentColor" fillOpacity=".25" />,
  opencode: <path d="M5.6 2.6C4 2.6 4 4 4 5.2S3.6 7.6 2.6 8c1 .4 1.4 1.4 1.4 2.8s0 2.6 1.6 2.6M10.4 2.6C12 2.6 12 4 12 5.2s.4 2.4 1.4 2.8c-1 .4-1.4 1.4-1.4 2.8s0 2.6-1.6 2.6" strokeWidth="1.4" strokeLinecap="round" fill="none" />,
  gemini: <path d="M8 1.6C8.6 5.6 10.4 7.4 14.4 8 10.4 8.6 8.6 10.4 8 14.4 7.4 10.4 5.6 8.6 1.6 8 5.6 7.4 7.4 5.6 8 1.6Z" strokeWidth="1.2" strokeLinejoin="round" fill="currentColor" fillOpacity=".3" />,
  pi: <path d="M3 4.4h10M6 4.4v8.4M10.2 4.4v6.6c0 1.2.6 1.8 1.8 1.8" strokeWidth="1.5" strokeLinecap="round" fill="none" />,
};

/** Normalises agent ids such as "cursor-agent" or "claude:<uuid>" to an icon key. */
export function agentKey(agent?: string): string {
  const a = (agent || "").toLowerCase().split(":")[0];
  if (a.startsWith("cursor")) return "cursor";
  if (a.startsWith("claude")) return "claude";
  if (a.startsWith("codex")) return "codex";
  if (a.startsWith("opencode")) return "opencode";
  if (a.startsWith("gemini")) return "gemini";
  if (a === "pi" || a.startsWith("pi-")) return "pi";
  return a;
}

export function AgentIcon({ agent, size = 13 }: { agent?: string; size?: number }) {
  const k = agentKey(agent);
  const p = PATHS[k];
  const label = agentLabel(k);
  return (
    <span className="agicon" style={{ color: agentVar(k) }} title={label} aria-label={label} role="img">
      {p ? (
        <svg viewBox="0 0 16 16" width={size} height={size} stroke="currentColor" fill="none" aria-hidden>
          {p}
        </svg>
      ) : (
        <span className="agicon-l" style={{ fontSize: size * 0.7 }}>{(k[0] || "?").toUpperCase()}</span>
      )}
    </span>
  );
}
