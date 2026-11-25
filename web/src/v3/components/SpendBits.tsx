// Cost and token figures: a header badge per session, today's total, and a warning for plan windows near their limit.
import { api } from "../../api/client";
import type { TokenUsage } from "../../api/types";
import { fmtTok, fmtUsd } from "../../lib/format";
import { useApi } from "../api/client";
import { fmtResetIn, useUsageGroups } from "../api/usage";
import { agentName } from "../live";

/** "$1.20 · 340k tok" for a session or thread header; nothing when it has spent nothing yet. */
export function CostBadge({ cost, est, tokens }: { cost?: number; est?: boolean; tokens?: TokenUsage }) {
  const tok = tokens ? tokens.input + tokens.output + tokens.cacheCreate : 0;
  if (!(cost && cost > 0) && !tok) return null;
  const parts = [cost && cost > 0 ? fmtUsd(cost, est) : "", tok ? `${fmtTok(tok)} tok` : ""].filter(Boolean);
  const title = tokens
    ? `${fmtTok(tokens.input)} in · ${fmtTok(tokens.output)} out · ${fmtTok(tokens.cacheCreate)} cache write · ${fmtTok(tokens.cacheRead)} cache read${est ? ". Cost estimated from list prices." : ""}`
    : "Cost of this thread";
  return (
    <span className="wcost mono hidephone" title={title} aria-label={`Spent ${parts.join(", ")}`}>
      {parts.join(" · ")}
    </span>
  );
}

/** Today's spend across every watched session and driven thread, from GET /api/v3/now. */
export function SpendTodayBox() {
  const st = useApi(() => api.now(), [], { poll: 60_000 });
  if (!st.data) return null;
  return (
    <div className="ibox">
      <h3>All agents today</h3>
      <p className="sx-p">
        <b className="mono">{fmtUsd(st.data.spendToday)}</b> across terminal sessions and driven threads since local midnight. Codex counts only with a token rate below.
      </p>
    </div>
  );
}

/** One line per plan window at 80% or more, so a near limit shows before the meters. */
export function NearLimitWarning({ at = 80 }: { at?: number }) {
  const st = useUsageGroups(60_000);
  const near = (st.data?.groups || []).flatMap((g) =>
    g.a.windows.filter((w) => w.usedPercent != null && w.usedPercent >= at).map((w) => ({ key: `${g.key}:${w.key}`, agent: g.a.agent, w })),
  );
  if (!near.length) return null;
  return (
    <div className="sx-warn" role="status">
      {near.map(({ key, agent, w }) => (
        <div key={key}>
          {agentName(agent)} {w.label.toLowerCase()} window at {Math.round(w.usedPercent!)}%
          {w.resetsAt ? `, ${fmtResetIn(w.resetsAt)}` : ""}.
        </div>
      ))}
    </div>
  );
}
