import { useCallback, useEffect, useState } from "react";
import { Link, Outlet, useLocation, useNavigate, useParams } from "react-router-dom";
import { api } from "../api/client";
import type { ContextRow, Health } from "../api/types";
import { fmtTok, isLive, pct } from "../lib/format";
import { Icon } from "../lib/icons";
import { ToolLogo } from "../lib/icons";
import { useLive } from "../lib/ws";
import HelpOverlay from "../components/help/HelpOverlay";
import { KEYS, useBoolPref, useTheme } from "./prefs";
import { useNotifications } from "./notify";
import { bindSearchInput, setSearch, useSearch } from "./search";
import { dismissToast, showToast, useToasts } from "./toast";
import { Gauge } from "./ui";
import "../styles/app.css";

function Crumbs() {
  const loc = useLocation();
  const params = useParams();
  const parts: { label: string; to?: string }[] = [];
  const p = loc.pathname;
  if (p === "/") parts.push({ label: "threads" });
  else if (p.startsWith("/session/")) {
    const id = decodeURIComponent(params.id || p.split("/")[2] || "");
    parts.push({ label: "threads", to: "/" }, { label: `session ${id.split(":").pop()?.slice(0, 8)}` });
  } else if (p.startsWith("/thread/")) {
    const id = decodeURIComponent(params.id || p.split("/")[2] || "");
    parts.push({ label: "threads", to: "/" }, { label: `thread ${id.split(":").pop()?.slice(0, 8)}`, to: p.endsWith("/learnings") ? `/thread/${encodeURIComponent(id)}` : undefined });
    if (p.endsWith("/learnings")) parts.push({ label: "learnings" });
    else parts.push({ label: "story" });
  }
  return (
    <div className="crumbs">
      {parts.map((c, i) => (
        <span key={i} style={{ display: "contents" }}>
          {i > 0 && <span className="sep">/</span>}
          {c.to ? <Link to={c.to}>{c.label}</Link> : <span className="cur">{c.label}</span>}
        </span>
      ))}
    </div>
  );
}

function ContextCard() {
  const [rows, setRows] = useState<ContextRow[]>([]);
  useEffect(() => {
    let alive = true;
    const tick = () => api.contextLive().then((r) => alive && setRows(r.context)).catch(() => null);
    tick();
    const t = window.setInterval(tick, 6000);
    return () => {
      alive = false;
      window.clearInterval(t);
    };
  }, []);
  const live = useLive();
  const shown = rows.filter((r) => r.window > 0).slice(0, 6);
  return (
    <div className="card" style={{ padding: 10, display: "flex", flexDirection: "column", gap: 6, marginTop: 8 }}>
      <div className="row" style={{ justifyContent: "space-between" }}>
        <span className="k" style={{ whiteSpace: "nowrap" }}>context · live</span>
        <span className="num muted" style={{ fontSize: 10.5, whiteSpace: "nowrap" }}>{shown.length}</span>
      </div>
      {shown.length === 0 && <div className="muted" style={{ fontSize: 11 }}>no live session</div>}
      {shown.map((r) => {
        const s = live.sessions.get(r.sessionId);
        const p = pct(r.used, r.window);
        return (
          <Link key={r.sessionId} to={`/session/${encodeURIComponent(r.sessionId)}`} className="row" style={{ gap: 8, color: "inherit" }} title={`${fmtTok(r.used)} / ${fmtTok(r.window)}`}>
            <span className="row" style={{ gap: 4, width: 52, flex: "none" }}>
              {s && <ToolLogo tool={s.tool} size={10} />}
              <span className="mono" style={{ fontSize: 10.5, color: "var(--text-2)" }}>#{(s?.sessionId || r.sessionId).replace(/[^a-z0-9]/gi, "").slice(0, 4).toLowerCase()}</span>
            </span>
            <div style={{ flex: 1 }}>
              <Gauge value={r.used} max={r.window} />
            </div>
            <span className="num" style={{ fontSize: 10.5, width: 34, textAlign: "right", color: p >= 80 ? "var(--orange)" : "var(--text)" }}>{Math.round(p)}%</span>
          </Link>
        );
      })}
    </div>
  );
}

export default function Shell() {
  const live = useLive();
  const nav = useNavigate();
  const loc = useLocation();
  const [theme, toggleTheme] = useTheme();
  const [notifyOn, setNotifyOn] = useBoolPref(KEYS.notify, true);
  const [soundOn, setSoundOn] = useBoolPref(KEYS.sound, true);
  const [help, setHelp] = useState(false);
  const [railOpen, setRailOpen] = useState(false);
  const [health, setHealth] = useState<Health | null>(null);
  const search = useSearch();
  const go = useCallback((p: string) => nav(p), [nav]);
  useNotifications(go);

  useEffect(() => {
    let alive = true;
    const tick = () => api.health().then((h) => alive && setHealth(h)).catch(() => alive && setHealth(null));
    tick();
    const t = window.setInterval(tick, 6000);
    return () => {
      alive = false;
      window.clearInterval(t);
    };
  }, []);
  useEffect(() => setRailOpen(false), [loc.pathname]);

  // Keyboard: "/" focuses search, "?" opens help, Esc closes overlays / goes back on mobile.
  useEffect(() => {
    const k = (e: KeyboardEvent) => {
      const tag = (e.target as HTMLElement | null)?.tagName;
      const typing = tag === "INPUT" || tag === "TEXTAREA" || tag === "SELECT";
      if (e.key === "/" && !typing) {
        e.preventDefault();
        const el = document.getElementById("topbar-search") as HTMLInputElement | null;
        el?.focus();
      } else if (e.key === "?" && !typing) setHelp(true);
      else if (e.key === "Escape") {
        if (help) setHelp(false);
        else if (railOpen) setRailOpen(false);
        else if (typing) (e.target as HTMLElement).blur();
        else if (window.innerWidth <= 600 && loc.pathname !== "/") nav("/");
      }
    };
    window.addEventListener("keydown", k);
    return () => window.removeEventListener("keydown", k);
  }, [help, railOpen, loc.pathname, nav]);

  const sessions = [...live.sessions.values()];
  const liveN = sessions.filter((s) => isLive(s.state)).length;
  const attnN = sessions.filter((s) => s.state === "awaiting-permission").length + live.perms.size;
  const toolCount = (t: string) => sessions.filter((s) => s.tool === t && isLive(s.state)).length;
  const projects = new Set(sessions.filter((s) => isLive(s.state)).map((s) => s.cwd)).size;
  const view = new URLSearchParams(loc.search).get("view") || (loc.pathname === "/" ? "threads" : "");
  const toolFilter = new URLSearchParams(loc.search).get("tool") || "";
  const toasts = useToasts();
  const tmux = health?.tmuxPanes || 0;

  const enableNotify = async () => {
    if (notifyOn) {
      setNotifyOn(false);
      showToast("Notifications muted");
      return;
    }
    if (typeof Notification === "undefined") {
      showToast("Notifications unsupported in this browser");
      return;
    }
    let perm = Notification.permission;
    if (perm === "default") perm = await Notification.requestPermission();
    if (perm === "denied") {
      showToast("Notifications blocked by the browser", "Allow them in the site settings, then reload (see Help).", { ms: 12000 });
      return;
    }
    setNotifyOn(true);
    showToast("Notifications on", "You get an alert when an agent waits for permission or input.");
    try {
      const n = new Notification("agent-monitor", { body: "Notifications work.", tag: "agent-monitor-test" });
      window.setTimeout(() => n.close(), 6000);
    } catch {
      /* ignore */
    }
  };

  const navItem = (label: string, icon: string, n: number | string, to: string, on: boolean) => (
    <button key={label} className={`navitem ${on ? "on" : ""}`} onClick={() => nav(to)}>
      <Icon name={icon} size={14} color={on ? "var(--accent)" : "var(--muted)"} />
      <span className="label">{label}</span>
      <span className="n">{n}</span>
    </button>
  );
  const agentItem = (label: string, tool: string) => (
    <button key={tool} className={`navitem ${toolFilter === tool ? "on" : ""}`} onClick={() => nav(`/?tool=${tool}`)}>
      <ToolLogo tool={tool} size={14} />
      <span className="label">{label}</span>
      <span className="n">{toolCount(tool)}</span>
    </button>
  );

  return (
    <div className={`app ${railOpen ? "rail-open" : ""}`}>
      <header className="topbar">
        <button className="btn icon hamb" onClick={() => setRailOpen((o) => !o)} aria-label="menu">
          <Icon name="menu" />
        </button>
        {loc.pathname !== "/" && (
          <button className="btn icon backbtn" onClick={() => nav(-1)} aria-label="back">
            <Icon name="chevl" />
          </button>
        )}
        <Link to="/" className="brand">
          <span className="brand-mark">
            <Icon name="pulse" size={13} color="#07080b" sw={2.4} />
          </span>
          <span>agent-monitor</span>
        </Link>
        <Crumbs />
        <div className="spacer" style={{ flex: 1 }} />
        <div className="searchbox">
          <Icon name="search" size={13} />
          <input id="topbar-search" ref={bindSearchInput} value={search} onChange={(e) => setSearch(e.target.value)} placeholder="Search threads, sessions, paths…" onFocus={() => loc.pathname !== "/" && nav("/")} />
          <span className="kbd">/</span>
        </div>
        {tmux >= 12 && (
          <span className="chip" style={{ color: tmux >= 18 ? "var(--red)" : "var(--yellow)" }} title={`${tmux} tmux panes · ${health?.registeredPanes || 0} agent-driven — consider closing some`}>
            <Icon name="layers" size={11} /> {tmux} panes
          </span>
        )}
        <button className={`btn icon ${notifyOn ? "on" : ""}`} onClick={enableNotify} title={notifyOn ? "notifications on" : "notifications muted"}>
          <Icon name={notifyOn ? "bell" : "bellOff"} />
        </button>
        <button className="btn icon" onClick={toggleTheme} title="toggle theme">
          <Icon name={theme === "dark" ? "sun" : "moon"} />
        </button>
        <span className="chip" style={{ color: live.connected ? "var(--green)" : "var(--red)", borderColor: live.connected ? "rgba(74,222,128,.3)" : "rgba(248,113,113,.3)" }}>
          <span style={{ width: 6, height: 6, borderRadius: "50%", background: "currentColor" }} />
          {live.connected ? `live · ${liveN}` : "reconnecting…"}
        </span>
      </header>
      <div className="body">
        <nav className="rail">
          <div className="k">view</div>
          {navItem("Live", "pulse", liveN, "/?view=live", view === "live")}
          {navItem("Needs attention", "alert", attnN, "/?view=attention", view === "attention")}
          {navItem("Threads", "thread", "", "/", view === "threads" && !toolFilter)}
          {navItem("Projects", "folder", projects, "/?view=projects", view === "projects")}
          {navItem("Learnings", "book", "", "/?view=learnings", view === "learnings")}
          <div className="k">agents</div>
          {agentItem("Claude Code", "claude")}
          {agentItem("Codex", "codex")}
          {agentItem("Cursor", "cursor")}
          {agentItem("opencode", "opencode")}
          <div style={{ flex: 1 }} />
          <ContextCard />
          <div className="railfoot">
            <button className={`btn sm ${notifyOn ? "on" : ""}`} onClick={enableNotify}>
              <Icon name="bell" size={12} /> {notifyOn ? "Notify on" : "Notify"}
            </button>
            <button className={`btn sm ${soundOn ? "on" : ""}`} onClick={() => setSoundOn(!soundOn)}>
              <Icon name={soundOn ? "volume" : "volumeOff"} size={12} /> {soundOn ? "Sound" : "Muted"}
            </button>
            <button className="btn sm" onClick={toggleTheme}>
              <Icon name={theme === "dark" ? "sun" : "moon"} size={12} /> {theme === "dark" ? "Light" : "Dark"}
            </button>
            <button className="btn sm" onClick={() => setHelp(true)}>
              <Icon name="help" size={12} /> Help
            </button>
          </div>
        </nav>
        <main className="main">
          <Outlet />
        </main>
      </div>
      {help && <HelpOverlay onClose={() => setHelp(false)} />}
      <div className="toasts">
        {toasts.map((t) => (
          <div key={t.id} className={`toast ${t.kind}`} onClick={t.onClick} style={{ cursor: t.onClick ? "pointer" : "default" }}>
            <div style={{ minWidth: 0 }}>
              <div className="ttl">{t.title}</div>
              {t.body && <div className="t2" style={{ marginTop: 2 }}>{t.body}</div>}
            </div>
            <span className="x" onClick={(e) => { e.stopPropagation(); dismissToast(t.id); }}>
              <Icon name="x" size={12} />
            </span>
          </div>
        ))}
      </div>
    </div>
  );
}
