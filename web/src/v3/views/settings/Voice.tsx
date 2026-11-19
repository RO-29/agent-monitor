// Settings → Voice: whether dictated text goes through the daemon's cleanup, and what this browser supports.
import { setCleanMode, SpeechCtor, useCleanMode, useVoiceStatus, type CleanMode } from "../../../lib/voice";
import { SettingsTabs } from "./Tabs";
import "../../styles/settings.css";
import "../../styles/p6.css";

const MODES: { v: CleanMode; l: string; hint: string }[] = [
  { v: "always", l: "Always", hint: "Dictated text is cleaned when you stop talking. This is the default." },
  { v: "ask", l: "Ask", hint: "A Clean up chip shows after you dictate. One tap cleans the text." },
  { v: "never", l: "Never", hint: "The text stays exactly as the browser heard it." },
];

export default function VoiceSettings() {
  const mode = useCleanMode();
  const st = useVoiceStatus();
  const speech = !!SpeechCtor();
  const cur = MODES.find((m) => m.v === mode)!;
  return (
    <div className="view pad sx">
      <SettingsTabs />
      <div className="viewhead">
        <div>
          <h2>Voice</h2>
          <p>Dictate into any message box with the microphone button or ⌘⇧M. This setting is kept in this browser.</p>
        </div>
      </div>
      <div className="sx-col">
        <div className="ibox">
          <h3>Dictation</h3>
          <p className="sx-p">
            {speech ? "This browser has speech recognition. The microphone shows in the thread, new thread and approval boxes." : "This browser has no speech recognition, so the microphone is hidden. Safari, Chrome and the Mac app have it."}
          </p>
        </div>
        <div className="ibox">
          <h3>
            Clean up dictated text
            {st && <span className={`pill ${st.available ? "p-ok" : "p-mute"}`}>{st.available ? st.engine || "available" : "not available"}</span>}
          </h3>
          <div className="seg" role="radiogroup" aria-label="Clean up dictated text">
            {MODES.map((m) => (
              <button key={m.v} role="radio" aria-checked={mode === m.v} className={mode === m.v ? "on" : ""} onClick={() => setCleanMode(m.v)}>
                {m.l}
              </button>
            ))}
          </div>
          <p className="sx-p">{cur.hint}</p>
          {st && !st.available && <p className="sx-p muted">This daemon has no cleanup engine (GET /api/voice/status), so dictated text stays as heard whatever this setting says.</p>}
        </div>
      </div>
    </div>
  );
}
