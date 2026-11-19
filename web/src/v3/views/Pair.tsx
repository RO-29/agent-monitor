// /pair#<code>: trades the single-use pairing code for the device cookie, then strips it from the address bar.
import { useEffect, useRef, useState } from "react";
import { mockWanted } from "../api/orch";
import "../styles/v3.css";
import "../styles/settings.css";

type St = { k: "busy" } | { k: "ok" } | { k: "err"; msg: string };

let taken: string | null = null;
function takeToken(): string {
  if (taken !== null) return taken;
  const h = window.location.hash.replace(/^#/, "");
  // Strip at once so the token never stays in history or a shared screenshot.
  if (h) history.replaceState(null, "", window.location.pathname + window.location.search);
  const m = /(?:^|&)(?:code|token)=([^&]+)/.exec(h);
  try {
    taken = decodeURIComponent(m ? m[1] : h).trim();
  } catch {
    taken = "";
  }
  return taken;
}

async function pair(token: string): Promise<St> {
  if (mockWanted()) return token.startsWith("amd_") ? { k: "ok" } : { k: "err", msg: "The token is not valid. Mock mode accepts any token that starts with amd_." };
  try {
    const r = await fetch("/api/devices/session", { method: "POST", credentials: "same-origin", headers: { "Content-Type": "application/json" }, body: JSON.stringify(token.startsWith("amd_") ? { token } : { code: token }) });
    if (r.ok) return { k: "ok" };
    if (r.status === 401) return { k: "err", msg: "The pairing code is used, expired (5 minutes) or not valid, or the device was revoked. Add the device again on the Mac and use the new link." };
    if (r.status === 404) return { k: "err", msg: "This daemon does not support device pairing yet." };
    const t = await r.text();
    let msg = t.slice(0, 200);
    try {
      msg = (JSON.parse(t) as { error?: string }).error || msg;
    } catch {
      /* plain text */
    }
    return { k: "err", msg: msg || `HTTP ${r.status}` };
  } catch (e) {
    return { k: "err", msg: `The daemon is unreachable (${(e as Error).message}).` };
  }
}

export default function Pair() {
  const [token] = useState(takeToken);
  const [st, setSt] = useState<St>(token ? { k: "busy" } : { k: "err", msg: "This link has no token. Open the full pair link from Settings, Devices on the Mac." });
  const sent = useRef(false);
  useEffect(() => {
    if (!token || sent.current) return;
    sent.current = true;
    void pair(token).then(setSt);
  }, [token]);
  return (
    <div className="v3 pairpg">
      <main className="pair-card" aria-live="polite">
        {st.k === "busy" && (
          <>
            <span className="pair-ic run" aria-hidden>
              ◌
            </span>
            <h1>Pairing this device</h1>
            <p>Checking the token with agent-monitor.</p>
          </>
        )}
        {st.k === "ok" && (
          <>
            <span className="pair-ic ok" aria-hidden>
              ✓
            </span>
            <h1>This device is paired</h1>
            <p>This browser now signs in with its own device token. You can revoke it at any time in Settings, Devices.</p>
            <a className="btn pri" href="/w">
              Open the workspace
            </a>
          </>
        )}
        {st.k === "err" && (
          <>
            <span className="pair-ic err" aria-hidden>
              !
            </span>
            <h1>Pairing failed</h1>
            <p role="alert">{st.msg}</p>
            <a className="btn" href="/">
              Go to agent-monitor
            </a>
          </>
        )}
      </main>
    </div>
  );
}
