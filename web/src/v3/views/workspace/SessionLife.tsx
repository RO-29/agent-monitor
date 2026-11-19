import { useEffect, useState } from "react";
import { api } from "../../../api/client";
import { showToast } from "../../../app/toast";

/** End session for a live terminal (two clicks), Resume in terminal for an ended one. */
export function SessionLifeBtn({ sid, live, owned, prompt, files, onResumed }: { sid: string; live: boolean; owned: boolean; prompt: string; files?: string[]; onResumed: () => void }) {
  const [armed, setArmed] = useState(false);
  const [busy, setBusy] = useState(false);
  useEffect(() => {
    if (!armed) return;
    const t = window.setTimeout(() => setArmed(false), 3000);
    return () => window.clearTimeout(t);
  }, [armed]);
  if (live) {
    const end = async () => {
      if (!armed) return setArmed(true);
      setArmed(false);
      setBusy(true);
      try {
        const r = await api.endSession(sid);
        showToast(r.ended ? "Session ended" : "Exit sent", r.ended ? "The agent exited. Resume it any time." : "The agent has not exited yet. Check its terminal.");
      } catch (e) {
        showToast("Could not end the session", (e as Error).message, { kind: "alert" });
      } finally {
        setBusy(false);
      }
    };
    return (
      <button type="button" className={`btn sm ${armed ? "deny" : "ghost"}`} disabled={busy} onClick={() => void end()} title="Stop the turn and exit the agent">
        {busy ? "Ending…" : armed ? "Confirm end" : "End session"}
      </button>
    );
  }
  if (owned) return null; // a terminal still owns it, but agent-monitor cannot reach it
  const resume = async () => {
    setBusy(true);
    try {
      const r = await api.resumeSession(sid, prompt.trim(), files);
      onResumed();
      showToast("Resumed in a terminal", `tmux session ${r.terminal.session}${prompt.trim() ? ". Your message goes there." : "."}`);
    } catch (e) {
      showToast("Could not resume", (e as Error).message, { kind: "alert" });
    } finally {
      setBusy(false);
    }
  };
  return (
    <button type="button" className="btn sm ghost" disabled={busy} onClick={() => void resume()} title="Resume this session in a new terminal; the text in the box becomes its first message">
      {busy ? "Resuming…" : "Resume in terminal"}
    </button>
  );
}
