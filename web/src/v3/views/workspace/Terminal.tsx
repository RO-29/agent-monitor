// Browser shell over the daemon pty WebSocket (V3-SPEC phase 6 A). Lazy chunk: xterm never enters the main bundle.
import { useEffect, useRef, useState } from "react";
import { Link } from "react-router-dom";
import { Terminal as XTerm, type IDisposable, type ITheme } from "@xterm/xterm";
import { FitAddon } from "@xterm/addon-fit";
import "@xterm/xterm/css/xterm.css";
import { shortCwd } from "../../../lib/format";
import { mockWanted } from "../../api/orch";

type Phase =
  | { k: "connecting"; again: boolean }
  | { k: "live" }
  | { k: "retry"; inMs: number }
  | { k: "paused" }
  | { k: "gaveup" }
  | { k: "exit"; code: number }
  | { k: "refused"; status: number; msg: string };

const FONT = '"IBM Plex Mono", ui-monospace, "SF Mono", Menlo, monospace';
const REFUSED = new Set([400, 401, 403, 404, 429]);

function themeOf(el: Element): ITheme {
  const cs = getComputedStyle(el);
  const v = (n: string, d: string) => cs.getPropertyValue(n).trim() || d;
  const light = cs.colorScheme === "light";
  const base = {
    background: v("--bg", light ? "#f4f3ef" : "#121210"),
    foreground: v("--fg", light ? "#1d1c19" : "#ecebe6"),
    cursor: v("--accent", light ? "#2f5bd3" : "#86a3ff"),
    cursorAccent: v("--bg", light ? "#f4f3ef" : "#121210"),
    selectionBackground: light ? "rgba(47,91,211,.22)" : "rgba(134,163,255,.3)",
    scrollbarSliderBackground: light ? "rgba(40,35,20,.18)" : "rgba(255,255,255,.14)",
    red: v("--err", ""),
    green: v("--ok", ""),
    yellow: v("--need", ""),
    blue: v("--accent", ""),
    magenta: v("--sub", ""),
    cyan: v("--run", ""),
  };
  return light
    ? { ...base, black: "#1d1c19", white: "#a39e93", brightBlack: "#8b867b", brightRed: "#a3271f", brightGreen: "#1f6a3e", brightYellow: "#94540a", brightBlue: "#2448b0", brightMagenta: "#6340a8", brightCyan: "#0a6676", brightWhite: "#6e6a61" }
    : { ...base, black: "#2d2c27", white: "#b0ada3", brightBlack: "#7f7b71", brightRed: "#ff8f87", brightGreen: "#7fdca5", brightYellow: "#ffc56b", brightBlue: "#a8bdff", brightMagenta: "#c3adff", brightCyan: "#7ad8e8", brightWhite: "#ecebe6" };
}

const wsUrl = (path: string, cols: number, rows: number) => {
  const sep = path.includes("?") ? "&" : "?";
  return `${location.protocol === "https:" ? "wss" : "ws"}://${location.host}${path}${sep}cols=${cols}&rows=${rows}`;
};

// A browser WebSocket hides the HTTP status of a refused upgrade, so a plain GET asks the same route why.
async function whyRefused(path: string): Promise<{ status: number; msg: string } | null> {
  try {
    const r = await fetch(path, { cache: "no-store", credentials: "same-origin" });
    const text = await r.text();
    if (text.trimStart().startsWith("<")) return { status: 404, msg: "missing" };
    let msg = text.slice(0, 200);
    try {
      msg = (JSON.parse(text) as { error?: string }).error || msg;
    } catch {
      /* plain text */
    }
    if (!REFUSED.has(r.status) || /websocket|upgrade|protocol/i.test(msg)) return null;
    return { status: r.status, msg };
  } catch {
    return null;
  }
}

function mockShell(term: XTerm, cwd: string, onExit: (code: number) => void): IDisposable {
  const p = `\x1b[32m${shortCwd(cwd) || "~"}\x1b[0m \x1b[90m$\x1b[0m `;
  term.write("\x1b[90mSample shell (mock mode). Nothing runs; typed text echoes here.\x1b[0m\r\n\r\n");
  term.write(`${p}git status --short\r\n \x1b[31mM\x1b[0m web/src/v3/views/workspace/Terminal.tsx\r\n\x1b[31m??\x1b[0m web/src/v3/styles/term.css\r\n`);
  term.write(`${p}ls\r\n\x1b[34minternal\x1b[0m  \x1b[34mweb\x1b[0m  go.mod  go.sum  main.go  V3-SPEC.md\r\n${p}`);
  let line = "";
  const run = (cmd: string) => {
    const c = cmd.trim();
    if (!c) return;
    if (c === "clear") return term.clear();
    if (c === "pwd") return term.write(`${cwd}\r\n`);
    if (c === "exit") return onExit(0);
    if (c.startsWith("echo ")) return term.write(`${c.slice(5)}\r\n`);
    term.write(`mock: ${c.split(" ")[0]}: not run in mock mode\r\n`);
  };
  return term.onData((d) => {
    for (const ch of d) {
      if (ch === "\r") {
        term.write("\r\n");
        const c = line;
        line = "";
        run(c);
        if (c.trim() !== "exit") term.write(p);
      } else if (ch === "\x7f") {
        if (line) {
          line = line.slice(0, -1);
          term.write("\b \b");
        }
      } else if (ch === "\x03") {
        line = "";
        term.write(`^C\r\n${p}`);
      } else if (ch >= " ") {
        line += ch;
        term.write(ch);
      }
    }
  });
}

export default function TermPanel({ url, cwd, phone, onClose }: { url: string; cwd: string; phone: boolean; onClose: () => void }) {
  const host = useRef<HTMLDivElement>(null);
  const [phase, setPhase] = useState<Phase>({ k: "connecting", again: false });
  const [note, setNote] = useState("");
  const act = useRef({ restart: () => {}, now: () => {} });

  useEffect(() => {
    const el = host.current!;
    const term = new XTerm({ fontFamily: FONT, fontSize: phone ? 12 : 13, lineHeight: 1.2, cursorBlink: true, scrollback: 5000, theme: themeOf(el) });
    const fit = new FitAddon();
    term.loadAddon(fit);
    term.attachCustomKeyEventHandler((e) => !(e.ctrlKey && e.code === "Backquote"));
    const mock = mockWanted();
    const enc = new TextEncoder();
    let disposed = false;
    let ws: WebSocket | null = null;
    let timer = 0;
    let tries = 0;
    let everLive = false;
    let exited = false;
    let replay = false;
    let sent = { cols: 0, rows: 0 };
    let noteTimer = 0;
    let mockSub: IDisposable | null = null;
    const flash = (s: string) => {
      setNote(s);
      window.clearTimeout(noteTimer);
      noteTimer = window.setTimeout(() => setNote(""), 3000);
    };

    const sendResize = () => {
      if (ws?.readyState !== WebSocket.OPEN || (term.cols === sent.cols && term.rows === sent.rows)) return;
      sent = { cols: term.cols, rows: term.rows };
      ws.send(JSON.stringify({ type: "resize", cols: term.cols, rows: term.rows }));
    };
    const doFit = () => {
      if (disposed || !el.clientWidth || !el.clientHeight) return;
      try {
        fit.fit();
      } catch {
        /* not laid out yet */
      }
      sendResize();
    };
    let raf = 0;
    const ro = new ResizeObserver(() => {
      cancelAnimationFrame(raf);
      raf = requestAnimationFrame(doFit);
    });

    // Retries stop after 10 failures and wait while the tab is hidden.
    let waiting = false;
    const schedule = () => {
      if (tries >= 10) return setPhase({ k: "gaveup" });
      const d = Math.min(15000, 500 * 2 ** tries++);
      setPhase({ k: "retry", inMs: d });
      timer = window.setTimeout(connect, d);
    };
    const onVis = () => {
      if (!document.hidden && waiting && !disposed) connect();
    };
    document.addEventListener("visibilitychange", onVis);
    const connect = () => {
      if (disposed) return;
      window.clearTimeout(timer);
      waiting = document.hidden;
      if (waiting) return setPhase({ k: "paused" });
      setPhase({ k: "connecting", again: everLive });
      const s = new WebSocket(wsUrl(url, term.cols, term.rows));
      s.binaryType = "arraybuffer";
      ws = s;
      sent = { cols: 0, rows: 0 };
      let opened = 0;
      s.onopen = () => {
        opened = Date.now();
      };
      s.onmessage = (ev) => {
        if (ws !== s) return;
        if (typeof ev.data === "string") {
          let m: { type?: string; new?: boolean; scrollback?: number; cols?: number; rows?: number; code?: number };
          try {
            m = JSON.parse(ev.data);
          } catch {
            return;
          }
          if (m.type === "hello") {
            replay = (m.scrollback || 0) > 0;
            if (m.new && everLive && !replay) term.write("\r\n\x1b[90m[new shell]\x1b[0m\r\n");
            sent = { cols: m.cols || 0, rows: m.rows || 0 };
            sendResize();
            setPhase({ k: "live" });
            if (everLive) flash("Reconnected");
            everLive = true;
          } else if (m.type === "exit") {
            exited = true;
            setPhase({ k: "exit", code: m.code ?? 0 });
          }
          return;
        }
        // The first binary frame after hello is the replay: clear first so it does not duplicate.
        if (replay) {
          replay = false;
          term.reset();
        }
        term.write(new Uint8Array(ev.data as ArrayBuffer));
      };
      s.onclose = () => {
        if (disposed || ws !== s) return;
        ws = null;
        if (exited) return;
        if (opened && Date.now() - opened > 5000) tries = 0;
        if (opened) return schedule();
        void whyRefused(url).then((r) => {
          if (disposed || ws) return;
          if (r) setPhase({ k: "refused", ...r });
          else schedule();
        });
      };
    };

    const send = (b: Uint8Array) => {
      if (ws?.readyState === WebSocket.OPEN) ws.send(b);
    };
    const subs: IDisposable[] = mock
      ? []
      : [
          term.onData((d) => send(enc.encode(d))),
          term.onBinary((d) => send(Uint8Array.from(d, (c) => c.charCodeAt(0) & 255))),
        ];
    const startMock = () => {
      mockSub?.dispose();
      mockSub = mockShell(term, cwd, (code) => {
        exited = true;
        setPhase({ k: "exit", code });
      });
      setPhase({ k: "live" });
    };

    act.current = {
      restart: () => {
        exited = false;
        tries = 0;
        term.reset();
        if (mock) startMock();
        else connect();
        term.focus();
      },
      now: () => {
        tries = 0;
        connect();
      },
    };

    // Measure cells after the webfont arrives, or xterm keeps a fallback width.
    const fonts = document.fonts?.load(`13px ${FONT}`).catch(() => undefined) ?? Promise.resolve();
    void Promise.race([fonts, new Promise((r) => setTimeout(r, 1200))]).then(() => {
      if (disposed) return;
      term.open(el);
      doFit();
      ro.observe(el);
      term.focus();
      if (mock) startMock();
      else connect();
    });

    const retheme = () => (term.options.theme = themeOf(el));
    const mo = new MutationObserver(retheme);
    mo.observe(document.documentElement, { attributes: true, attributeFilter: ["data-theme", "class"] });
    const mq = window.matchMedia("(prefers-color-scheme: dark)");
    mq.addEventListener("change", retheme);

    return () => {
      disposed = true;
      window.clearTimeout(timer);
      window.clearTimeout(noteTimer);
      cancelAnimationFrame(raf);
      ro.disconnect();
      mo.disconnect();
      mq.removeEventListener("change", retheme);
      document.removeEventListener("visibilitychange", onVis);
      subs.forEach((x) => x.dispose());
      mockSub?.dispose();
      ws?.close(1000);
      ws = null;
      term.dispose();
    };
  }, [url, cwd, phone]);

  const st =
    phase.k === "live"
      ? note
        ? { cls: "ok", l: note }
        : { cls: "ok", l: mockWanted() ? "Sample" : "Connected" }
      : phase.k === "connecting"
        ? { cls: "run", l: phase.again ? "Reconnecting…" : "Connecting…" }
        : phase.k === "retry"
          ? { cls: "need", l: `Disconnected. Retry in ${Math.ceil(phase.inMs / 1000)} s` }
          : phase.k === "paused"
            ? { cls: "need", l: "Disconnected. Reconnects when this tab is shown" }
            : phase.k === "gaveup"
              ? { cls: "err", l: "Disconnected after 10 tries" }
          : phase.k === "exit"
            ? { cls: "mute", l: `Exited (${phase.code})` }
            : { cls: "err", l: "Refused" };

  return (
    <>
      <div className="wterm-h">
        <b>Terminal</b>
        <span className="wterm-cwd mono trunc" title={cwd}>
          {shortCwd(cwd)}
        </span>
        <span className={`wterm-st ${st.cls}`} role="status">
          {st.l}
        </span>
        <span className="sp" />
        {(phase.k === "retry" || phase.k === "gaveup") && (
          <button className="btn sm ghost" onClick={() => act.current.now()}>
            Reconnect now
          </button>
        )}
        <button className="btn sm ghost" onClick={onClose} aria-label="Close the terminal" title={phone ? undefined : "Close (ctrl+`)"}>
          {phone ? "Close" : "×"}
        </button>
      </div>
      <div className="wterm-body">
        <div className="wterm-x" ref={host} />
        {phase.k === "exit" && (
          <div className="wterm-over">
            <b>The shell exited with code {phase.code}</b>
            <button className="btn sm pri" onClick={() => act.current.restart()}>
              Start a new shell
            </button>
          </div>
        )}
        {phase.k === "refused" && <Refused status={phase.status} msg={phase.msg} onRetry={() => act.current.restart()} />}
      </div>
    </>
  );
}

function Refused({ status, msg, onRetry }: { status: number; msg: string; onRetry: () => void }) {
  const devices = (
    <Link className="btn sm" to="/settings/devices">
      Settings → Devices
    </Link>
  );
  let title = "The shell could not start";
  let body: React.ReactNode = msg;
  let extra: React.ReactNode = null;
  if (status === 403) {
    title = "Remote shells are off";
    body = "From another device, a shell needs the Remote terminal setting on and a paired device token. The password alone never opens one.";
    extra = devices;
  } else if (status === 401) {
    title = "Sign in with a device token";
    body = "This browser has no valid credential. Pair it from Settings → Devices on this Mac, then open the pair link here.";
    extra = devices;
  } else if (status === 429) {
    title = "Too many shells";
    body = "agent-monitor runs at most 6 shells at once. Close the terminal of another thread, or wait 30 minutes after one closes.";
  } else if (status === 404) {
    title = msg === "missing" ? "This daemon has no terminal yet" : "Thread not found";
    body = msg === "missing" ? "Update agent-monitor to a build that serves the terminal routes." : msg;
  } else if (status === 400) {
    title = "The shell cannot start here";
    body = msg || "The folder is missing or is not a directory.";
  }
  return (
    <div className="wterm-over" role="alert">
      <b>{title}</b>
      <span>{body}</span>
      <div className="wterm-over-a">
        {extra}
        <button className="btn sm ghost" onClick={onRetry}>
          Try again
        </button>
      </div>
    </div>
  );
}
