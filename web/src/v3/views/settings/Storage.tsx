// Settings → Storage: how long the daemon keeps files attached to messages, and a clean-up button.
import { useEffect, useState } from "react";
import { req } from "../../api/orch";
import { showToast } from "../../../app/toast";
import { SettingsTabs } from "./Tabs";
import "../../styles/settings.css";
import "../../styles/p6.css";

interface UploadStats {
  files: number;
  bytes: number;
  deleted: number;
  oldest?: number;
  maxAgeHours: number;
  quotaBytes: number;
}

export const AGES: { h: number; l: string }[] = [
  { h: 1, l: "1 hour" },
  { h: 24, l: "1 day" },
  { h: 168, l: "7 days" },
  { h: 0, l: "30 days" },
  { h: 2160, l: "90 days" },
  { h: -1, l: "Keep" },
];

const mb = (n: number) => (n < 1 << 20 ? `${Math.max(1, Math.round(n / 1024))} KB` : `${(n / (1 << 20)).toFixed(1)} MB`);

export default function Storage() {
  const [st, setSt] = useState<UploadStats | null>(null);
  const [err, setErr] = useState("");
  const [busy, setBusy] = useState(false);
  const load = () =>
    req<UploadStats>("GET", "/api/orch/uploads/stats")
      .then((s) => (setSt(s), setErr("")))
      .catch((e) => setErr(e instanceof Error ? e.message : String(e)));
  useEffect(() => {
    load();
  }, []);

  const setAge = async (h: number) => {
    setBusy(true);
    try {
      await req("POST", "/api/settings", { uploadMaxAgeHours: h });
      await load();
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };
  const clean = async (all: boolean) => {
    setBusy(true);
    try {
      const s = await req<UploadStats>("POST", "/api/orch/uploads/prune", all ? { all: true } : {});
      setSt(s);
      showToast(s.deleted ? `Deleted ${s.deleted} file${s.deleted === 1 ? "" : "s"}.` : "No files to delete.");
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };
  const age = st?.maxAgeHours ?? 0;
  return (
    <div className="view pad sx">
      <SettingsTabs />
      <div className="viewhead">
        <div>
          <h2>Storage</h2>
          <p>Files you attach to messages are kept on the Mac running agent-monitor. This setting applies to the web, the Mac app and the iPhone.</p>
        </div>
      </div>
      <div className="sx-col">
        {err && <p className="err-t">{err}</p>}
        <div className="ibox">
          <h3>Delete attached files after</h3>
          <div className="seg" role="radiogroup" aria-label="Delete attached files after">
            {AGES.map((a) => (
              <button key={a.h} role="radio" aria-checked={age === a.h} disabled={busy || !st} className={age === a.h ? "on" : ""} onClick={() => setAge(a.h)}>
                {a.l}
              </button>
            ))}
          </div>
          <p className="sx-p">
            {age === -1
              ? `Files stay until the ${st ? mb(st.quotaBytes) : "500 MB"} limit is full. Then new uploads fail until you clean up.`
              : "The daemon checks every hour. An agent that has not read a file yet loses it when the file is deleted."}
          </p>
        </div>
        <div className="ibox">
          <h3>Attached files now</h3>
          <p className="sx-p">
            {st ? `${st.files} file${st.files === 1 ? "" : "s"}, ${mb(st.bytes)} of ${mb(st.quotaBytes)}.` : "Loading…"}
            {st?.oldest ? ` Oldest from ${new Date(st.oldest).toLocaleString()}.` : ""}
          </p>
          <div className="row" style={{ display: "flex", gap: 8, flexWrap: "wrap" }}>
            <button className="btn pri" disabled={busy || !st || age === -1} onClick={() => clean(false)}>
              Clean up now
            </button>
            <button className="btn deny" disabled={busy || !st?.files} onClick={() => clean(true)}>
              Delete all files
            </button>
          </div>
        </div>
      </div>
    </div>
  );
}
