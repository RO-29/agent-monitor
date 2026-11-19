// Dictation for every message box: browser speech recognition, then optional cleanup through the daemon.
import { useCallback, useEffect, useRef, useState, useSyncExternalStore } from "react";
import { req } from "../v3/api/orch";
import { showToast } from "../app/toast";
import "../v3/styles/voice.css";

// Minimal Web Speech API shape; WebKit exposes it as webkitSpeechRecognition.
export interface SpeechRec {
  continuous: boolean;
  interimResults: boolean;
  lang: string;
  start: () => void;
  stop: () => void;
  onresult: ((e: { resultIndex: number; results: ArrayLike<{ isFinal: boolean; 0: { transcript: string } }> }) => void) | null;
  onend: (() => void) | null;
  onerror: ((e: { error: string }) => void) | null;
}
export const SpeechCtor = (): (new () => SpeechRec) | undefined => {
  if (typeof window === "undefined") return undefined;
  const w = window as unknown as { SpeechRecognition?: new () => SpeechRec; webkitSpeechRecognition?: new () => SpeechRec };
  return w.SpeechRecognition || w.webkitSpeechRecognition;
};

// ---- cleanup setting ----

const joinSpoken = (a: string, b: string) => (!a ? b : !b ? a : /\s$/.test(a) || /^\s/.test(b) ? a + b : `${a} ${b}`);

export type CleanMode = "always" | "never" | "ask";
const MODE_KEY = "am-voice-clean";
const subs = new Set<() => void>();
export function getCleanMode(): CleanMode {
  try {
    const v = localStorage.getItem(MODE_KEY);
    return v === "ask" || v === "never" ? v : "always";
  } catch {
    return "always";
  }
}
export function setCleanMode(m: CleanMode) {
  try {
    localStorage.setItem(MODE_KEY, m);
  } catch {
    /* storage blocked */
  }
  subs.forEach((f) => f());
}
export const useCleanMode = () =>
  useSyncExternalStore(
    (f) => {
      subs.add(f);
      return () => subs.delete(f);
    },
    getCleanMode,
    getCleanMode,
  );

// ---- daemon ----

export interface VoiceStatus {
  available: boolean;
  engine?: string;
}
export interface VoiceClean {
  text: string;
  cleaned: boolean;
  ms?: number;
  error?: string;
}
let statusP: { at: number; p: Promise<VoiceStatus> } | null = null;
/** GET /api/voice/status, cached 5 min; an older daemon answers {available:false}. */
export function voiceStatus(): Promise<VoiceStatus> {
  if (!statusP || Date.now() - statusP.at > 300_000) {
    statusP = {
      at: Date.now(),
      p: req<VoiceStatus>("GET", "/api/voice/status").then(
        (s) => ({ available: !!s.available, engine: s.engine }),
        () => ({ available: false }),
      ),
    };
  }
  return statusP.p;
}
export function useVoiceStatus(): VoiceStatus | null {
  const [s, setS] = useState<VoiceStatus | null>(null);
  useEffect(() => {
    let alive = true;
    void voiceStatus().then((v) => alive && setS(v));
    return () => {
      alive = false;
    };
  }, []);
  return s;
}
export const cleanText = (text: string, mode?: string) => req<VoiceClean>("POST", "/api/voice/clean", mode ? { text, mode } : { text });

// ---- dictation ----

interface Pending {
  base: string;
  raw: string;
}
export interface Dictation {
  supported: boolean;
  listening: boolean;
  toggle: () => void;
  stop: () => void;
  /** Dictated text waiting for a "Clean up" tap (mode "ask"). */
  pending: Pending | null;
  cleaning: boolean;
  clean: () => void;
  dismiss: () => void;
}

/** Speech into a text value: partial text shows live; the result is cleaned per the Voice setting. */
export function useDictation(value: string, setValue: (v: string) => void, cleanKind?: string): Dictation {
  const rec = useRef<SpeechRec | null>(null);
  const [listening, setListening] = useState(false);
  const [pending, setPending] = useState<Pending | null>(null);
  const [cleaning, setCleaning] = useState(false);
  const cur = useRef(value);
  cur.current = value;
  const set = useRef(setValue);
  set.current = setValue;
  const kind = useRef(cleanKind);
  kind.current = cleanKind;
  const seg = useRef<Pending>({ base: "", raw: "" });
  const supported = !!SpeechCtor();
  useEffect(() => () => rec.current?.stop(), []);
  // A send clears the box: stop listening so late results do not bring the text back.
  useEffect(() => {
    if (value.trim()) return;
    setPending(null);
    const r = rec.current;
    if (r) {
      r.onresult = null;
      seg.current.raw = "";
      r.stop();
    }
  }, [value]);

  const run = useCallback(async (p: Pending) => {
    setCleaning(true);
    try {
      const r = await cleanText(p.raw, kind.current);
      if (r.error && !r.cleaned) throw new Error(r.error);
      const now = cur.current;
      const out = r.text || p.raw;
      if (now === p.base + p.raw) set.current(p.base + out);
      else if (now.includes(p.raw)) set.current(now.replace(p.raw, out));
      else showToast("Text changed", "The dictated text was edited, so it was not cleaned.");
    } catch (e) {
      showToast("Cleanup failed", (e as Error).message);
    } finally {
      setCleaning(false);
      setPending(null);
    }
  }, []);

  const finish = useCallback(() => {
    setListening(false);
    rec.current = null;
    const p = { ...seg.current, raw: seg.current.raw.trim() };
    if (!p.raw) return;
    const m = getCleanMode();
    if (m === "never") return;
    void voiceStatus().then((s) => {
      if (!s.available) return;
      if (m === "always") void run(p);
      else setPending(p);
    });
  }, [run]);

  const stop = useCallback(() => rec.current?.stop(), []);
  const toggle = useCallback(() => {
    if (rec.current) return rec.current.stop();
    const C = SpeechCtor();
    if (!C) return showToast("Dictation is not available", "This browser has no speech recognition. Use Safari or Chrome, or the Mac app.");
    const r = new C();
    r.continuous = true;
    r.interimResults = true;
    r.lang = navigator.language || "en-US";
    const v = cur.current;
    seg.current = { base: v ? v.replace(/\s*$/, " ") : "", raw: "" };
    setPending(null);
    // Safari and the Mac app's WebView can restart the result list after a pause: keep what came before.
    let kept = "";
    let last = { n: 0, text: "" };
    r.onresult = (e) => {
      let fin = "";
      let tmp = "";
      for (let i = 0; i < e.results.length; i++) {
        const x = e.results[i];
        if (x.isFinal) fin += x[0].transcript;
        else tmp += x[0].transcript;
      }
      const now = fin + tmp;
      if (last.text && (e.results.length < last.n || (now.length < last.text.length && !now.startsWith(last.text.slice(0, 8))))) {
        kept = joinSpoken(kept, last.text);
      }
      last = { n: e.results.length, text: now };
      seg.current.raw = joinSpoken(kept, now);
      set.current(seg.current.base + seg.current.raw);
    };
    r.onend = finish;
    r.onerror = (e) => {
      if (e.error === "not-allowed" || e.error === "service-not-allowed") showToast("Microphone blocked", "Allow the microphone and speech recognition for agent-monitor, then try again.");
      else if (e.error !== "no-speech" && e.error !== "aborted") showToast("Dictation stopped", e.error);
    };
    rec.current = r;
    try {
      r.start();
      setListening(true);
    } catch (err) {
      rec.current = null;
      showToast("Dictation failed", (err as Error).message);
    }
  }, [finish]);

  return {
    supported,
    listening,
    toggle,
    stop,
    pending,
    cleaning,
    clean: () => pending && void run(pending),
    dismiss: () => setPending(null),
  };
}

// ---- Cmd+Shift+M: the mic of the focused box, else the box focused last ----

const mics = new Set<{ el: HTMLElement; toggle: () => void }>();
let lastBox: HTMLElement | null = null;
let keyOn = false;
function onKey(e: KeyboardEvent) {
  if (!(e.metaKey || e.ctrlKey) || !e.shiftKey || e.altKey || e.code !== "KeyM") return;
  const live = [...mics].filter((m) => m.el.isConnected);
  if (!live.length) return;
  const act = document.activeElement;
  const boxOf = (m: { el: HTMLElement }) => (m.el.closest("[data-vox]") as HTMLElement | null) || m.el.parentElement;
  const hit = live.find((m) => act && boxOf(m)?.contains(act)) || live.find((m) => lastBox && boxOf(m) === lastBox) || (live.length === 1 ? live[0] : undefined);
  if (!hit) return;
  e.preventDefault();
  hit.toggle();
}
function onFocus(e: FocusEvent) {
  const b = (e.target as HTMLElement | null)?.closest?.("[data-vox]") as HTMLElement | null;
  if (b) lastBox = b;
}

const MicIcon = () => (
  <svg viewBox="0 0 16 16" width="14" height="14" fill="none" stroke="currentColor" strokeWidth="1.5" strokeLinecap="round" aria-hidden>
    <rect x="5.5" y="1.8" width="5" height="8" rx="2.5" />
    <path d="M3.2 7.6a4.8 4.8 0 0 0 9.6 0M8 12.4v2" />
  </svg>
);

/** The microphone toggle; hidden where the browser has no speech recognition. */
export function MicButton({ d, disabled, className = "" }: { d: Dictation; disabled?: boolean; className?: string }) {
  const ref = useRef<HTMLButtonElement>(null);
  const tog = useRef(d.toggle);
  tog.current = d.toggle;
  useEffect(() => {
    const el = ref.current;
    if (!el) return;
    if (!keyOn) {
      keyOn = true;
      window.addEventListener("keydown", onKey, true);
      window.addEventListener("focusin", onFocus);
    }
    const m = { el, toggle: () => tog.current() };
    mics.add(m);
    return () => {
      mics.delete(m);
    };
  }, [d.supported]);
  if (!d.supported) return null;
  const label = d.listening ? "Stop dictation" : "Dictate";
  return (
    <button
      ref={ref}
      type="button"
      className={`vox-mic ${d.listening ? "on" : ""} ${className}`}
      onClick={d.toggle}
      disabled={disabled && !d.listening}
      aria-pressed={d.listening}
      aria-label={label}
      title={`${label} (⌘⇧M)`}
    >
      <MicIcon />
      {d.listening && <span className="vox-live">Listening</span>}
    </button>
  );
}

/** "Clean up" chip after dictation when the Voice setting is "ask". */
export function CleanChip({ d }: { d: Dictation }) {
  if (!d.pending && !d.cleaning) return null;
  return (
    <span className="vox-chip" role="status">
      {d.cleaning ? (
        <span className="muted">Cleaning up…</span>
      ) : (
        <>
          <button type="button" className="vox-clean" onClick={d.clean} title="Remove filler words and fix punctuation in the dictated text">
            Clean up
          </button>
          <button type="button" className="vox-x" onClick={d.dismiss} aria-label="Keep the text as dictated">
            ✕
          </button>
        </>
      )}
    </span>
  );
}

type InputRest = Omit<React.InputHTMLAttributes<HTMLInputElement>, "value" | "onChange">;
/** A one-line answer field with a microphone. */
export function VoiceInput({ value, onChange, ...rest }: InputRest & { value: string; onChange: (v: string) => void }) {
  const d = useDictation(value, onChange);
  return (
    <span className="vox-field" data-vox>
      <input {...rest} value={value} onChange={(e) => onChange(e.target.value)} />
      <MicButton d={d} disabled={rest.disabled} />
      <CleanChip d={d} />
    </span>
  );
}
