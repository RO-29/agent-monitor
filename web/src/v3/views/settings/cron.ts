// 5-field cron (minute hour day-of-month month day-of-week, local time): parse, describe, next runs.

const MON = ["jan", "feb", "mar", "apr", "may", "jun", "jul", "aug", "sep", "oct", "nov", "dec"];
const DOW = ["sun", "mon", "tue", "wed", "thu", "fri", "sat"];
const DAY_NAMES = ["Sunday", "Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday"];
const MONTH_NAMES = ["January", "February", "March", "April", "May", "June", "July", "August", "September", "October", "November", "December"];

interface Field {
  set: Set<number>;
  any: boolean; // exactly *
  starStart: boolean; // starts with * (the Vixie day rule)
  step?: number; // */n
}
export interface Cron {
  min: Field;
  hour: Field;
  dom: Field;
  mon: Field;
  dow: Field;
}

// Same grammar as the daemon's orch_cron.go, so the offline preview never accepts what the daemon rejects.
const MON_N: Record<string, number> = Object.fromEntries(MON.map((m, i) => [m, i + 1]));
const DOW_N: Record<string, number> = Object.fromEntries(DOW.map((d, i) => [d, i]));
const atoi = (t: string) => (/^[+-]?\d+$/.test(t) ? Number(t) : NaN);

function field(src: string, name: string, lo: number, hi: number, names?: Record<string, number>, sunday7 = false): Field {
  const set = new Set<number>();
  const value = (t: string) => {
    const k = t.toLowerCase();
    if (names && k in names) return names[k];
    const n = atoi(t);
    if (!Number.isInteger(n) || n < lo || n > hi) throw new Error(`${name}: "${t}" is not in ${lo}-${hi}`);
    return n;
  };
  let step: number | undefined;
  const parts = src.split(",");
  for (const part of parts) {
    if (part === "") throw new Error(`${name}: empty list item`);
    const slash = part.indexOf("/");
    const range = slash < 0 ? part : part.slice(0, slash);
    let s = 1;
    if (slash >= 0) {
      s = atoi(part.slice(slash + 1));
      if (!Number.isInteger(s) || s < 1 || s > hi) throw new Error(`${name}: bad step "${part.slice(slash + 1)}"`);
    }
    let a = lo;
    let b = hi;
    if (range === "*") {
      if (sunday7) b = 6;
      if (slash >= 0 && parts.length === 1) step = s;
    } else if (range.includes("-")) {
      const i = range.indexOf("-");
      a = value(range.slice(0, i));
      b = value(range.slice(i + 1));
      if (a > b) throw new Error(`${name}: range ${range} runs backwards`);
    } else {
      a = value(range);
      b = slash >= 0 ? hi : a;
    }
    for (let v = a; v <= b; v += s) {
      if (sunday7 && v === 7) {
        set.add(0);
        break;
      }
      set.add(v);
    }
  }
  return { set, any: src === "*", starStart: src.startsWith("*"), step };
}

const MACROS: Record<string, string> = { "@yearly": "0 0 1 1 *", "@annually": "0 0 1 1 *", "@monthly": "0 0 1 * *", "@weekly": "0 0 * * 0", "@daily": "0 0 * * *", "@midnight": "0 0 * * *", "@hourly": "0 * * * *" };

export function parseCron(expr: string): Cron {
  const e = expr.trim();
  const f = (MACROS[e.toLowerCase()] || e).split(/\s+/).filter(Boolean);
  if (f.length !== 5) throw new Error(`cron needs 5 fields (minute hour day-of-month month day-of-week), got ${f.length}`);
  return { min: field(f[0], "minute", 0, 59), hour: field(f[1], "hour", 0, 23), dom: field(f[2], "day of month", 1, 31), mon: field(f[3], "month", 1, 12, MON_N), dow: field(f[4], "day of week", 0, 7, DOW_N, true) };
}

function dayMatch(c: Cron, d: Date): boolean {
  if (!c.mon.set.has(d.getMonth() + 1)) return false;
  const dm = c.dom.set.has(d.getDate());
  const dw = c.dow.set.has(d.getDay());
  return c.dom.starStart || c.dow.starStart ? dm && dw : dm || dw;
}

/** Offline fallback: the next n run times after `from` in this browser's time, searching five years ahead. */
export function nextRuns(c: Cron, n: number, from = Date.now()): number[] {
  const out: number[] = [];
  const start = new Date(from);
  start.setSeconds(0, 0);
  start.setMinutes(start.getMinutes() + 1);
  const hours = [...c.hour.set].sort((a, b) => a - b);
  const mins = [...c.min.set].sort((a, b) => a - b);
  const day = new Date(start.getFullYear(), start.getMonth(), start.getDate());
  for (let i = 0; i < 366 * 5 && out.length < n; i++, day.setDate(day.getDate() + 1)) {
    if (!dayMatch(c, day)) continue;
    for (const h of hours) {
      for (const m of mins) {
        const t = new Date(day.getFullYear(), day.getMonth(), day.getDate(), h, m).getTime();
        if (t >= start.getTime()) out.push(t);
        if (out.length >= n) return out;
      }
    }
  }
  return out;
}

const pad = (n: number) => String(n).padStart(2, "0");
const joinAnd = (a: string[]) => (a.length < 2 ? a.join("") : `${a.slice(0, -1).join(", ")} and ${a[a.length - 1]}`);
const sorted = (s: Set<number>) => [...s].sort((a, b) => a - b);

function timeText(c: Cron): string {
  const h = sorted(c.hour.set);
  const m = sorted(c.min.set);
  if (c.min.any && c.hour.any) return "every minute";
  if (c.min.step && c.hour.any) return `every ${c.min.step} minutes`;
  if (c.hour.any && m.length === 1) return m[0] === 0 ? "every hour" : `every hour at :${pad(m[0])}`;
  if (c.hour.step && m.length === 1) return `every ${c.hour.step} hours at :${pad(m[0])}`;
  if (m.length === 1 && h.length <= 4) return `at ${joinAnd(h.map((x) => `${pad(x)}:${pad(m[0])}`))}`;
  if (m.length === 1) return `at :${pad(m[0])} past ${h.length} hours`;
  if (c.hour.any) return `at ${m.length} minutes of every hour`;
  return `${m.length * h.length} times`;
}

/** A plain-language preview, for example "Every weekday at 09:00". */
export function describeCron(expr: string): string {
  let c: Cron;
  try {
    c = parseCron(expr);
  } catch {
    return "";
  }
  const t = timeText(c);
  const dows = sorted(c.dow.set);
  let days = "";
  if (!c.dow.any) {
    const k = dows.join(",");
    days = k === "1,2,3,4,5" ? "every weekday" : k === "0,6" ? "on weekends" : dows.length === 1 ? `every ${DAY_NAMES[dows[0]]}` : `on ${joinAnd(dows.map((d) => DAY_NAMES[d]))}`;
  }
  if (!c.dom.any) {
    const ds = sorted(c.dom.set);
    const dt = `on day ${joinAnd(ds.map(String))} of the month`;
    days = days ? `${dt} and ${days}` : dt;
  }
  if (!days && c.dom.any && c.dow.any) days = /^every (minute|\d+ minutes|hour|\d+ hours)/.test(t) ? "" : "every day";
  if (!c.mon.any) days = `${days ? `${days}, ` : ""}in ${joinAnd(sorted(c.mon.set).map((m) => MONTH_NAMES[m - 1]))}`;
  const s = [days, t].filter(Boolean).join(" ");
  return s.charAt(0).toUpperCase() + s.slice(1);
}

export const CRON_PRESETS: { cron: string; label: string }[] = [
  { cron: "0 9 * * 1-5", label: "Weekdays 09:00" },
  { cron: "0 18 * * *", label: "Daily 18:00" },
  { cron: "0 9 * * 1", label: "Mondays 09:00" },
  { cron: "0 * * * *", label: "Hourly" },
];

export function fmtRun(ts: number, now = Date.now()): string {
  const d = new Date(ts);
  const today = new Date(now);
  const tomorrow = new Date(now);
  tomorrow.setDate(today.getDate() + 1);
  const same = (a: Date, b: Date) => a.toDateString() === b.toDateString();
  const hm = `${pad(d.getHours())}:${pad(d.getMinutes())}`;
  if (same(d, today)) return `Today ${hm}`;
  if (same(d, tomorrow)) return `Tomorrow ${hm}`;
  return `${DAY_NAMES[d.getDay()].slice(0, 3)} ${d.getDate()} ${MONTH_NAMES[d.getMonth()].slice(0, 3)} ${hm}`;
}
