// Secret cards: an agent's request_secret. The value goes to the daemon only, which hands it to the agent once.
import { useEffect, useRef, useState } from "react";
import { fmtAgo, projectName } from "../../lib/format";
import { onLiveEvent } from "../../lib/ws";

interface SecretReq {
  id: string;
  name: string;
  reason?: string;
  url?: string;
  agent?: string;
  cwd?: string;
  createdAt: number;
}

async function post(id: string, body: object) {
  const r = await fetch(`/api/secrets/${encodeURIComponent(id)}/answer`, { method: "POST", credentials: "same-origin", headers: { "Content-Type": "application/json" }, body: JSON.stringify(body) });
  if (!r.ok) throw new Error((await r.json().catch(() => null))?.error || `HTTP ${r.status}`);
}

// The live socket sends kind "secret" on each change; the slow poll covers a dropped socket.
export function SecretCards({ dock = false }: { dock?: boolean }) {
  const [list, setList] = useState<SecretReq[]>([]);
  const done = useRef(new Set<string>());
  useEffect(() => {
    let stop = false;
    let busy = false;
    const load = async () => {
      if (busy || stop) return;
      busy = true;
      try {
        const r = await fetch("/api/secrets", { credentials: "same-origin", cache: "no-store" });
        if (r.ok && !stop) setList((((await r.json()).secrets || []) as SecretReq[]).filter((x) => !done.current.has(x.id)));
      } catch {
        /* the next event or poll tries again */
      }
      busy = false;
    };
    void load();
    const off = onLiveEvent((e) => {
      if ((e as unknown as { kind?: string }).kind === "secret") void load();
    });
    const t = setInterval(() => document.hidden || void load(), 15_000);
    const vis = () => document.hidden || void load();
    document.addEventListener("visibilitychange", vis);
    return () => {
      stop = true;
      off();
      clearInterval(t);
      document.removeEventListener("visibilitychange", vis);
    };
  }, []);
  if (!list.length) return null;
  const finish = (id: string) => {
    done.current.add(id);
    setList((l) => l.filter((x) => x.id !== id));
  };
  return (
    <div className={dock ? "secret-dock" : undefined} style={{ display: "grid", gap: 10, marginBottom: dock ? 0 : 12 }} onKeyDown={(e) => e.stopPropagation()} aria-live="polite">
      {list.map((s) => (
        <SecretCard key={s.id} s={s} onDone={() => finish(s.id)} />
      ))}
    </div>
  );
}

function hostOf(url: string): string | null {
  try {
    const u = new URL(url);
    return u.protocol === "https:" || u.protocol === "http:" ? u.host : null;
  } catch {
    return null;
  }
}

function SecretCard({ s, onDone }: { s: SecretReq; onDone: () => void }) {
  const ref = useRef<HTMLInputElement>(null);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState("");
  // Take focus only when the user is not typing elsewhere, so keystrokes never land in the secret field.
  useEffect(() => {
    const a = document.activeElement;
    if (!a || !a.matches("input, textarea, [contenteditable=true]")) ref.current?.focus();
  }, []);
  const host = s.url ? hostOf(s.url) : null;
  const answer = async (save: boolean) => {
    setBusy(true);
    setErr("");
    try {
      const v = ref.current?.value || "";
      if (save && !v.trim()) throw new Error("Type the secret first");
      await post(s.id, save ? { type: "save", secret: v } : { type: "decline" });
      if (ref.current) ref.current.value = "";
      onDone();
    } catch (e) {
      setErr((e as Error).message);
    }
    setBusy(false);
  };
  return (
    <form className="ibox" style={{ display: "grid", gap: 8 }} onSubmit={(e) => (e.preventDefault(), void answer(true))} autoComplete="off">
      <div>
        <b>{s.agent || projectName(s.cwd || "") || "An agent"} asks for a secret: {s.name}</b>{" "}
        <span className="xs muted">{fmtAgo(s.createdAt)}</span>
      </div>
      {s.reason && <div className="sm muted">{s.reason}</div>}
      {s.url && host && (
        <a className="sm" href={s.url} target="_blank" rel="noreferrer noopener">
          Get it here: {host}
        </a>
      )}
      <input ref={ref} className="inp mono" type="password" name={`am-secret-${s.id}`} autoComplete="off" spellCheck={false} placeholder="Secret" aria-label={`Secret: ${s.name}`} />
      <div className="row" style={{ gap: 8 }}>
        <button className="btn pri" type="submit" disabled={busy}>
          {busy ? "Sending…" : "Give secret"}
        </button>
        <button className="btn ghost" type="button" disabled={busy} onClick={() => void answer(false)}>
          Decline
        </button>
        <span className="xs muted">The agent gets it once, through a pipe that is gone after one read or 10 minutes. It never goes into the chat.</span>
      </div>
      {err && (
        <span className="xs err-t" role="alert">
          {err}
        </span>
      )}
    </form>
  );
}
