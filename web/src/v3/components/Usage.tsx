// Plan usage meters: a compact footer for the thread list and a full panel for Settings, Limits.
import { useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { fmtAgo, fmtTok, fmtUsd, modelShort } from "../../lib/format";
import { fmtResetIn, usageLevel, useUsageLimits, type AgentUsage, type ModelUsage, type UsageCredits, type UsageLocal, type UsageWindow } from "../api/usage";
import { FoldHead, useFold } from "./fold";
import "../styles/usage.css";

const AGENT: Record<string, string> = { claude: "Claude Code", codex: "Codex", opencode: "opencode", cursor: "Cursor", pi: "pi" };
const SHORT_NAME: Record<string, string> = { claude: "Claude", codex: "Codex" };
const ORDER = ["claude", "codex", "opencode", "cursor", "pi"];
const MAIN = new Set(["claude", "codex"]);
const nameOf = (a: AgentUsage) => AGENT[a.agent] || a.agent;
const shortOf = (a: AgentUsage) => SHORT_NAME[a.agent] || nameOf(a);
const rank = (a: AgentUsage) => (ORDER.indexOf(a.agent) + 1 || 99);
const byOrder = (x: AgentUsage, y: AgentUsage) => rank(x) - rank(y) || x.agent.localeCompare(y.agent);
const isInstalled = (a: AgentUsage) => a.installed !== false;
const SHORT: Record<string, string> = { "5h": "5 h", weekly: "Week" };

function useNow(ms: number) {
  const [now, setNow] = useState(Date.now());
  useEffect(() => {
    const t = window.setInterval(() => setNow(Date.now()), ms);
    return () => window.clearInterval(t);
  }, [ms]);
  return now;
}

const pctText = (p: number) => `${Math.round(p)}%`;
const STALE_MS = 30 * 60_000;
const usd = (n: number, est: boolean) => (n >= 1000 ? `${est ? "≈" : ""}$${Math.round(n).toLocaleString("en-US")}` : fmtUsd(n, est));

function ageText(at: number, now: number): string {
  const m = Math.max(0, Math.round((now - at) / 60_000));
  if (m < 60) return `${m} min ago`;
  if (m < 48 * 60) return `${Math.round(m / 60)} h ago`;
  return `${Math.round(m / 1440)} d ago`;
}

export function UsageMeter({ w, now, compact, at }: { w: UsageWindow; now: number; compact?: boolean; at?: number }) {
  const p = w.usedPercent;
  const lvl = usageLevel(p);
  const label = compact ? SHORT[w.key] || w.label : w.label;
  const right =
    p != null ? pctText(p) : w.expired && w.lastPercent != null ? `reset, was ${pctText(w.lastPercent)}` : "no reading";
  const age = p != null && at && now - at > STALE_MS ? `read ${ageText(at, now)}` : "";
  const resetIn = p != null ? fmtResetIn(w.resetsAt, now) : "";
  const reset = age ? (compact || !resetIn ? age : `${resetIn}, ${age}`) : resetIn;
  return (
    <div className={`umt ${lvl} ${compact ? "c" : ""} ${age ? "stale" : ""}`} title={reset ? `${w.label}: ${right}, ${reset}` : `${w.label}: ${right}`}>
      <div className="umt-h">
        <span>{label}</span>
        <span className={p != null ? "mono" : "muted"}>{right}</span>
      </div>
      <div className="umt-t" role="progressbar" aria-label={`${w.label} usage`} aria-valuemin={0} aria-valuemax={100} aria-valuenow={p != null ? Math.round(p) : undefined}>
        {p != null && <i style={{ width: `${p}%` }} />}
      </div>
      {reset && <div className="umt-r">{reset}</div>}
    </div>
  );
}

const hasReading = (a: AgentUsage) => a.windows.some((w) => w.usedPercent != null || w.expired);
// Fresh input, cache writes and output; cache reads are left out, as they dwarf the rest.
const tokOf = (l?: UsageLocal) => (l ? l.tokens.input + l.tokens.cacheCreate + l.tokens.output : 0);
const localOf = (a: AgentUsage, k: "5h" | "weekly") => a.local.find((l) => l.key === k);

/** One line for an agent with no plan percent: the local token total for 5 h; the week is in the tooltip. */
function LocalOnly({ a }: { a: AgentUsage }) {
  const h5 = tokOf(localOf(a, "5h"));
  const wk = tokOf(localOf(a, "weekly"));
  const name = shortOf(a);
  return (
    <div className="umini-a umini-lo" title={`${a.note || "No plan reading."} ${fmtTok(wk)} tokens this week. Tokens counted from this Mac's transcripts (input, cache writes and output; cache reads excluded).`}>
      {name}: <b className="mono">{fmtTok(h5)}</b> tokens in 5 h <span className="muted">(local count)</span>
    </div>
  );
}

function miniSummary(agents: AgentUsage[]): string {
  return agents
    .map((a) => {
      const name = shortOf(a);
      if (!hasReading(a)) return `${name} ${fmtTok(tokOf(localOf(a, "5h")))} 5 h`;
      return `${name} ${a.windows.map((w) => (w.usedPercent != null ? pctText(w.usedPercent) : "-")).join(" / ")}`;
    })
    .join(" · ");
}

/** Thread list footer: plan meters per agent, or local token totals when an agent has no plan reading. Folds. */
export function UsageMini() {
  const st = useUsageLimits(60_000);
  const now = useNow(30_000);
  const [closed, toggle] = useFold("usage-mini");
  const all = [...(st.data?.agents || [])].sort(byOrder);
  const agents = all.filter((a) => MAIN.has(a.agent) && isInstalled(a) && (hasReading(a) || tokOf(localOf(a, "weekly")) > 0));
  const more = all.filter((a) => !MAIN.has(a.agent) && isInstalled(a));
  if (!agents.length && !more.length) return null;
  return (
    <div className={`umini ${closed ? "fold-c" : ""}`}>
      <div className="umini-top">
        <FoldHead closed={closed} onToggle={toggle} className="umini-hd">
          Usage
        </FoldHead>
        {closed && <span className="umini-sum trunc" title={miniSummary(agents)}>{miniSummary(agents)}</span>}
        {more.length > 0 && (
          <Link to="/settings/limits" className="umini-lk umini-more" title={`Also: ${more.map(nameOf).join(", ")}`} aria-label={`${more.length} more agents in Settings, Limits`}>
            +{more.length}
          </Link>
        )}
        <Link to="/settings/limits" className={`umini-lk ${more.length ? "umini-lk2" : ""}`} aria-label="Plan usage, open Settings, Limits">
          Limits
        </Link>
      </div>
      {!closed &&
        agents.map((a) =>
          hasReading(a) ? (
            <div key={a.agent} className="umini-a">
              <span className={`umini-n ${a.agent}`}>{nameOf(a)}</span>
              <div className="umini-m">
                {a.windows.map((w) => (
                  <UsageMeter key={w.key} w={w} now={now} at={a.measuredAt} compact />
                ))}
              </div>
            </div>
          ) : (
            <LocalOnly key={a.agent} a={a} />
          ),
        )}
    </div>
  );
}

function History({ w }: { w: UsageWindow }) {
  const pts = w.history || [];
  if (pts.length < 4) return <div className="uhist muted sm">{pts.length ? `${pts.length} ${pts.length === 1 ? "reading" : "readings"} so far. The chart starts at 4.` : "No history yet."}</div>;
  const W = 220;
  const H = 34;
  const t0 = pts[0].at;
  const span = Math.max(1, pts[pts.length - 1].at - t0);
  const xy = pts.map((p) => `${(((p.at - t0) / span) * W).toFixed(1)},${(H - 2 - (p.pct / 100) * (H - 4)).toFixed(1)}`).join(" ");
  return (
    <div className="uhist">
      <svg viewBox={`0 0 ${W} ${H}`} width="100%" height={H} preserveAspectRatio="none" role="img" aria-label={`${w.label} usage history, ${pts.length} readings`}>
        <line x1="0" x2={W} y1={H - 2 - 0.9 * (H - 4)} y2={H - 2 - 0.9 * (H - 4)} className="uhist-g" />
        <polyline points={xy} fill="none" className="uhist-l" strokeWidth="1.5" vectorEffect="non-scaling-stroke" />
      </svg>
      <div className="uhist-x muted">
        <span>{fmtAgo(t0)}</span>
        <span>{pts.length} readings</span>
      </div>
    </div>
  );
}

function LocalRow({ l }: { l: UsageLocal }) {
  const t = l.tokens;
  return (
    <div className="uloc" role="row">
      <span role="cell">
        {l.aligned ? `This ${l.key === "5h" ? "5-hour" : "weekly"} window` : `Last ${l.label}`}
        <small className="muted"> since {new Date(l.from).toLocaleString(undefined, { weekday: "short", hour: "2-digit", minute: "2-digit" })}</small>
      </span>
      <span role="cell" className="mono">{fmtTok(t.input + t.cacheCreate)} in</span>
      <span role="cell" className="mono">{fmtTok(t.cacheRead)} cached</span>
      <span role="cell" className="mono">{fmtTok(t.output)} out</span>
      <span role="cell" className="mono">{usd(l.costUsd, l.costEstimated)}</span>
      <span role="cell" className="mono muted">{l.messages.toLocaleString()} replies</span>
    </div>
  );
}

const FAMILY = ["opus", "fable", "sonnet", "haiku"];
const familyOf = (m: string) => FAMILY.findIndex((f) => m.toLowerCase().includes(f));
const famRank = (m: string) => {
  const i = familyOf(m);
  return i < 0 ? FAMILY.length : i;
};
const famLabel = (m: string) => {
  const i = familyOf(m);
  return i < 0 ? "" : FAMILY[i][0].toUpperCase() + FAMILY[i].slice(1);
};

/** Tokens and cost per model in the 5-hour or weekly window, Opus, Fable, Sonnet, Haiku first. */
function ModelTable({ models, agent }: { models: ModelUsage[]; agent: string }) {
  const has5 = models.some((m) => m.window === "5h");
  const [win, setWin] = useState<"5h" | "weekly">(has5 ? "5h" : "weekly");
  const rows = models.filter((m) => m.window === win).sort((a, b) => famRank(a.model) - famRank(b.model) || b.costUsd - a.costUsd || a.model.localeCompare(b.model));
  const sum = rows.reduce((t, m) => ({ i: t.i + m.inTokens, o: t.o + m.outTokens, r: t.r + m.cacheRead, w: t.w + m.cacheWrite, c: t.c + m.costUsd, n: t.n + m.messages }), { i: 0, o: 0, r: 0, w: 0, c: 0, n: 0 });
  return (
    <>
      <div className="umod-hd">
        <h4 className="uloc-h">By model</h4>
        <div className="seg" role="radiogroup" aria-label="Window">
          {(["5h", "weekly"] as const).map((w) => (
            <button key={w} role="radio" aria-checked={win === w} className={win === w ? "on" : ""} onClick={() => setWin(w)}>
              {w === "5h" ? "5 hours" : "Week"}
            </button>
          ))}
        </div>
      </div>
      <div className="umods" role="table" aria-label={`${agent} usage by model`}>
        <div className="umod umod-th" role="row">
          <span role="columnheader">Model</span>
          <span role="columnheader">In</span>
          <span role="columnheader">Out</span>
          <span role="columnheader">Cache read</span>
          <span role="columnheader">Cache write</span>
          <span role="columnheader">Cost</span>
          <span role="columnheader">Replies</span>
        </div>
        {rows.map((m) => (
          <div key={m.model} className="umod" role="row">
            <span role="cell" className="umod-m">
              {famLabel(m.model) && <b>{famLabel(m.model)}</b>}
              <span className="mono muted trunc" title={m.model}>
                {modelShort(m.model)}
              </span>
            </span>
            <span role="cell" className="mono">{fmtTok(m.inTokens)}</span>
            <span role="cell" className="mono">{fmtTok(m.outTokens)}</span>
            <span role="cell" className="mono">{fmtTok(m.cacheRead)}</span>
            <span role="cell" className="mono">{fmtTok(m.cacheWrite)}</span>
            <span role="cell" className="mono">{usd(m.costUsd, true)}</span>
            <span role="cell" className="mono muted">{m.messages.toLocaleString()}</span>
          </div>
        ))}
        {rows.length > 1 && (
          <div className="umod umod-sum" role="row">
            <span role="cell">All models</span>
            <span role="cell" className="mono">{fmtTok(sum.i)}</span>
            <span role="cell" className="mono">{fmtTok(sum.o)}</span>
            <span role="cell" className="mono">{fmtTok(sum.r)}</span>
            <span role="cell" className="mono">{fmtTok(sum.w)}</span>
            <span role="cell" className="mono">{usd(sum.c, true)}</span>
            <span role="cell" className="mono muted">{sum.n.toLocaleString()}</span>
          </div>
        )}
        {!rows.length && <div className="muted sm umod-none">No replies in this window.</div>}
      </div>
    </>
  );
}

const isSpend = (c: UsageCredits): c is Exclude<UsageCredits, number> & { spent: number; limit: number } => typeof c !== "number" && c.spent != null && c.limit != null;

function money(n: number, cur?: string): string {
  try {
    return n.toLocaleString(undefined, { style: "currency", currency: cur || "USD" });
  } catch {
    return `${n.toLocaleString()} ${cur || ""}`.trim();
  }
}

/** Credits as a bar: "<spent> of <limit> <currency> spent". */
function CreditsBar({ c }: { c: Exclude<UsageCredits, number> & { spent: number; limit: number } }) {
  const p = c.percent ?? (c.limit > 0 ? (c.spent / c.limit) * 100 : 0);
  const lvl = c.capReached ? "over" : usageLevel(p);
  const cur = c.currency || "USD";
  return (
    <div className={`umt ${lvl}`}>
      <div className="umt-h">
        <span>Usage credits</span>
        <span className="mono">{pctText(p)}</span>
      </div>
      <div className="umt-t" role="progressbar" aria-label="Usage credits spent" aria-valuemin={0} aria-valuemax={100} aria-valuenow={Math.round(p)}>
        <i style={{ width: `${Math.min(100, p)}%` }} />
      </div>
      <div className="umt-r">{`${money(c.spent, cur)} of ${money(c.limit, cur)} ${cur} spent`}</div>
    </div>
  );
}

function creditsText(c: UsageCredits): string {
  if (typeof c === "number") return `${c.toLocaleString()} credits`;
  if (isSpend(c)) return "";
  if (c.unlimited) return "Credits: unlimited";
  if (c.balance != null && c.balance !== "") return `Credits: ${typeof c.balance === "number" ? c.balance.toLocaleString() : c.balance}`;
  if (c.spendUsd != null) return `${fmtUsd(c.spendUsd)} spent`;
  if (c.hasCredits) return "Has credits";
  return "";
}

function AgentCard({ a, now }: { a: AgentUsage; now: number }) {
  if (!isInstalled(a))
    return (
      <div className="ibox ucard uoff">
        <h3>
          {nameOf(a)}
          <span className="pill p-mute">not installed</span>
        </h3>
        {a.note && <p className="sx-p muted">{a.note}</p>}
      </div>
    );
  const credits = a.credits != null ? creditsText(a.credits) : "";
  const spend = a.credits != null && isSpend(a.credits) ? a.credits : null;
  return (
    <div className="ibox ucard">
      <h3>
        {nameOf(a)}
        {a.plan && <span className="chip t">{a.plan}</span>}
        {credits && <span className="chip">{credits}</span>}
        {a.stale && <span className="pill p-mute">old reading</span>}
        <span className="muted">{a.measuredAt ? `reading ${fmtAgo(a.measuredAt)}` : "no reading"}</span>
      </h3>
      {a.alert && <p className="sx-warn">{a.alert}</p>}
      {a.note && <p className="sx-p">{a.note}</p>}
      {spend && (
        <div className="ucard-w">
          <div className="ucard-wi">
            <CreditsBar c={spend} />
          </div>
        </div>
      )}
      {a.windows.length > 0 && (
        <>
          <div className="ucard-w">
            {a.windows.map((w) => (
              <div key={w.key} className="ucard-wi">
                <UsageMeter w={w} now={now} at={a.measuredAt} />
                <History w={w} />
              </div>
            ))}
          </div>
          {a.source && (
            <p className="usrc muted">
              Plan percent: {a.source}
              {a.sourcePath && <span className="mono"> ({a.sourcePath})</span>}
            </p>
          )}
        </>
      )}
      {a.models && a.models.length > 0 && <ModelTable models={a.models} agent={nameOf(a)} />}
      {a.local.length > 0 && (
        <>
          <h4 className="uloc-h">Measured locally</h4>
          <p className="sx-p">Tokens counted from this Mac's transcripts. This is not the plan limit: other devices do not count, and the cost uses public list prices.</p>
          <div className="ulocs" role="table" aria-label={`${nameOf(a)} local usage`}>
            {a.local.map((l) => (
              <LocalRow key={l.key} l={l} />
            ))}
          </div>
          {a.localSource && <p className="usrc muted">{a.localSource}</p>}
        </>
      )}
      {!a.windows.length && !a.local.length && !a.models?.length && <p className="sx-p muted">Installed. No usage recorded yet.</p>}
    </div>
  );
}

/** Full plan usage section for Settings, Limits. */
export function UsagePanel() {
  const st = useUsageLimits(30_000);
  const now = useNow(30_000);
  if (st.missing) return <div className="sx-warn">This daemon has no /api/usage/limits endpoint yet. Update agent-monitor to see plan usage.</div>;
  if (st.error && !st.data) return <div className="sx-warn">Plan usage failed to load: {st.error}</div>;
  if (!st.data) return <div className="ibox muted sm">Reading usage…</div>;
  return (
    <>
      {[...st.data.agents].sort((x, y) => Number(!isInstalled(x)) - Number(!isInstalled(y)) || byOrder(x, y)).map((a) => (
        <AgentCard key={a.agent} a={a} now={now} />
      ))}
    </>
  );
}
