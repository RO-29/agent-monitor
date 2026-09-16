import { useMemo } from "react";
import type { Span } from "../../api/types";
import { fmtDur } from "../../lib/format";
import { shortTool } from "./lib";

// Ranked answers to "where did the time and the failures go". No time axis:
// ordering by size is what a multi-day session actually needs.

export default function ProfilePanel({ calls, turnMs, onSelect }: { calls: Span[]; turnMs: number; onSelect: (id: string) => void }) {
  const p = useMemo(() => {
    const byTool = new Map<string, { ms: number; n: number; err: number }>();
    const byFile = new Map<string, number>();
    for (const c of calls) {
      const t = byTool.get(c.name) || { ms: 0, n: 0, err: 0 };
      t.ms += c.dur;
      t.n++;
      if (c.err) t.err++;
      byTool.set(c.name, t);
      if ((c.fam === "edit" || c.fam === "read") && c.res) {
        const f = c.res.split("/").slice(-2).join("/");
        byFile.set(f, (byFile.get(f) || 0) + 1);
      }
    }
    const toolMs = [...byTool.values()].reduce((a, b) => a + b.ms, 0);
    const tools = [...byTool.entries()].sort((a, b) => b[1].ms - a[1].ms);
    return {
      tools: tools.slice(0, 7),
      model: Math.max(turnMs - toolMs, 0),
      total: Math.max(turnMs, toolMs),
      slowest: [...calls].sort((a, b) => b.dur - a.dur).slice(0, 7),
      broke: tools.filter(([, v]) => v.err > 0).sort((a, b) => b[1].err - a[1].err).slice(0, 6),
      files: [...byFile.entries()].sort((a, b) => b[1] - a[1]).slice(0, 7),
    };
  }, [calls, turnMs]);

  const bar = (v: number, of: number) => <i style={{ width: `${of > 0 ? (v / of) * 100 : 0}%` }} />;

  return (
    <div className="tn-profile">
      <section>
        <div className="k">where the time went</div>
        <div className="rank">
          <div className="r">
            <span className="lab">model</span>
            <span className="track">{bar(p.model, p.total)}</span>
            <span className="num">{fmtDur(p.model)}</span>
          </div>
          {p.tools.map(([name, v]) => (
            <div className="r" key={name}>
              <span className="lab" title={name}>{shortTool(name)}</span>
              <span className="track">{bar(v.ms, p.total)}</span>
              <span className="num">{fmtDur(v.ms)}</span>
            </div>
          ))}
        </div>
      </section>
      <section>
        <div className="k">slowest calls</div>
        <div className="rank">
          {p.slowest.map((c) => (
            <div className="r click" key={c.id} onClick={() => onSelect(c.id)}>
              <span className="lab">{shortTool(c.name)}</span>
              <span className="res ltr" title={c.res || ""}>{c.res || c.text || ""}</span>
              <span className="num">{fmtDur(c.dur)}</span>
            </div>
          ))}
          {p.slowest.length === 0 && <div className="none">no calls</div>}
        </div>
      </section>
      <section>
        <div className="k">where it broke</div>
        <div className="rank">
          {p.broke.map(([name, v]) => (
            <div className="r" key={name}>
              <span className="lab" title={name}>{shortTool(name)}</span>
              <span className="track">{bar(v.err, p.broke[0][1].err)}</span>
              <span className="num bad">{v.err}</span>
            </div>
          ))}
          {p.broke.length === 0 && <div className="none">no errors in this segment</div>}
        </div>
      </section>
      <section>
        <div className="k">files it kept touching</div>
        <div className="rank">
          {p.files.map(([f, n]) => (
            <div className="r" key={f}>
              <span className="res wide" title={f}>{f}</span>
              <span className="num">{n}x</span>
            </div>
          ))}
          {p.files.length === 0 && <div className="none">no file reads or edits</div>}
        </div>
      </section>
    </div>
  );
}
