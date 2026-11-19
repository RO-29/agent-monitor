// Spend bar: amount spent against a limit, amber from 80%, red at the limit. Limit 0 means no limit.

export const usd = (n: number) => `$${n >= 1000 ? Math.round(n).toLocaleString() : n.toFixed(2)}`;

export function SpendBar({ label, spent, limit, note }: { label: string; spent: number; limit: number; note?: string }) {
  const pct = limit > 0 ? Math.min(100, (spent / limit) * 100) : 0;
  const lvl = limit <= 0 ? "off" : pct >= 100 ? "over" : pct >= 80 ? "warn" : "ok";
  return (
    <div className={`spb ${lvl}`}>
      <div className="spb-h">
        <span>{label}</span>
        <span className="mono">
          {usd(spent)}
          {limit > 0 ? <span className="muted"> of {usd(limit)}</span> : <span className="muted"> · no limit</span>}
        </span>
      </div>
      {limit > 0 && (
        <div className="spb-t" role="progressbar" aria-label={`${label} spend`} aria-valuemin={0} aria-valuemax={100} aria-valuenow={Math.round(pct)}>
          <i style={{ width: `${pct}%` }} />
          <b style={{ left: "80%" }} aria-hidden />
        </div>
      )}
      {note && <div className="spb-n">{note}</div>}
    </div>
  );
}
