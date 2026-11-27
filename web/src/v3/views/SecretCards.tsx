// Secret cards: an agent's request_secret. The value goes to the daemon only, which hands it to the agent once.
import { useEffect, useRef, useState } from "react";
import { fmtAgo, projectName } from "../../lib/format";

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

export function SecretCards() {
  const [list, setList] = useState<SecretReq[]>([]);
  useEffect(() => {
    let stop = false;
    const load = async () => {
      try {
        const r = await fetch("/api/secrets", { credentials: "same-origin", cache: "no-store" });
        if (r.ok && !stop) setList(((await r.json()).secrets || []) as SecretReq[]);
      } catch {
        /* the next poll tries again */
      }
    };
    void load();
    const t = setInterval(load, 3000);
    return () => {
      stop = true;
      clearInterval(t);
    };
  }, []);
  if (!list.length) return null;
  return (
    <div style={{ display: "grid", gap: 10, marginBottom: 12 }} onKeyDown={(e) => e.stopPropagation()}>
      {list.map((s) => (
        <SecretCard key={s.id} s={s} onDone={() => setList((l) => l.filter((x) => x.id !== s.id))} />
      ))}
    </div>
  );
}

function SecretCard({ s, onDone }: { s: SecretReq; onDone: () => void }) {
  const ref = useRef<HTMLInputElement>(null);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState("");
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
      {s.url && (
        <a className="sm" href={s.url} target="_blank" rel="noreferrer noopener">
          Get it here: {new URL(s.url).host}
        </a>
      )}
      <input ref={ref} className="inp mono" type="password" name={`am-secret-${s.id}`} autoComplete="off" spellCheck={false} placeholder="Secret" aria-label={`Secret: ${s.name}`} autoFocus />
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
