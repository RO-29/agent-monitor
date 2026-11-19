import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { Link, Outlet, useLocation, useNavigate } from "react-router-dom";
import type { Session } from "../api/types";
import { Boundary } from "./components/Boundary";
import { useNotifications } from "../app/notify";
import { KEYS, useBoolPref, useTheme } from "../app/prefs";
import { dismissToast, useToasts } from "../app/toast";
import PaneBridge from "../components/bridge/PaneBridge";
import { titleFor } from "../lib/format";
import { useLive } from "../lib/ws";
import { Palette } from "./components/Palette";
import { Kbd } from "./components/ui";
import { Ctx, typing } from "./ctx";
import { chord, overlayOpen, setApproveKeys, useApproveKeys } from "./keys";
import { setDrivenThreads, startV3Live, useV3 } from "./live";
import { SHORT } from "./layout";
import { useOrch } from "./api/orch";
import { setNotifyNav, useNotify } from "./api/notify";
import { drivenRows, drivenSig } from "./driven";
import { buildRows, counts } from "./model";
import "./styles/v3.css";
import "./styles/p6.css";
import { useNeedsYou } from "./api/needsYou";

export const VIEWS: { to: string; label: string; ic: string; key?: string }[] = [
  { to: "/board", label: "Board", ic: "▦", key: "b" },
  { to: "/explore", label: "Explore spans", ic: "⌕", key: "x" },
  { to: "/errors", label: "Errors", ic: "!", key: "e" },
  { to: "/tracing", label: "Tracing", ic: "⌁", key: "r" },
  { to: "/monitors", label: "Monitors", ic: "◉", key: "m" },
  { to: "/intel", label: "Intelligence", ic: "✦", key: "n" },
  { to: "/rules", label: "Rules", ic: "⚙", key: "u" },
];
export const LOOKS: { to: string; label: string; note: string }[] = [
  { to: "/desk", label: "Desk", note: "overview" },
  { to: "/control", label: "Control room", note: "dark, dense" },
  { to: "/messages", label: "Messages", note: "chat" },
  { to: "/timeline", label: "Timeline", note: "g t" },
];

function titleOf(path: string): string {
  if (path === "/desk") return "Desk";
  if (path.startsWith("/threads") || path.startsWith("/inbox")) return "Threads";
  if (path.startsWith("/t/")) return "Trace";
  if (path === "/w/search") return "Search";
  if (path === "/" || path === "/w" || path.startsWith("/w/")) return "Threads";
  if (path.startsWith("/timeline")) return "Timeline";
  if (path.startsWith("/traces/")) return "Trace";
  if (path === "/more") return "More";
  if (path.startsWith("/commands")) return "History";
  if (path.startsWith("/settings/devices")) return "Devices";
  if (path.startsWith("/settings/notifications") || path.startsWith("/notifications")) return "Notifications";
  if (path.startsWith("/settings/limits")) return "Limits";
  if (path.startsWith("/settings/templates")) return "Templates";
  if (path.startsWith("/settings/schedules")) return "Schedules";
  return [...VIEWS, ...LOOKS].find((v) => v.to !== "/" && path.startsWith(v.to))?.label || "agent-monitor";
}

type Menu = "more" | "phone" | null;

function MenuBody({ path, onTheme, theme, notifyOn, onNotify, part = "all" }: { path: string; onTheme: () => void; theme: string; notifyOn: boolean; onNotify: () => void; part?: "views" | "more" | "all" }) {
  const lookOn = (to: string) => path.startsWith(to);
  const views = part !== "more";
  const more = part !== "views";
  return (
    <>
      {views && <div className="mh">Other views</div>}
      {views && LOOKS.map((l) => (
        <Link key={l.to} to={l.to} className={lookOn(l.to) ? "on" : ""}>
          {l.label}
          <small>{l.note}</small>
        </Link>
      ))}
      {more && <div className="mh">Tools</div>}
      {more && VIEWS.map((v) => (
        <Link key={v.to} to={v.to} className={path.startsWith(v.to) ? "on" : ""}>
          {v.label}
          {v.key && <small>g {v.key}</small>}
        </Link>
      ))}
      {more && (
        <>
          <Link to="/commands" className={path.startsWith("/commands") ? "on" : ""}>
            History
            <small>prompts, ! commands</small>
          </Link>
          <div className="mh">Settings</div>
          <Link to="/settings/devices" className={path.startsWith("/settings/devices") ? "on" : ""}>
            Devices
            <small>tokens</small>
          </Link>
          <Link to="/settings/notifications" className={path.startsWith("/settings/notifications") ? "on" : ""}>
            Notifications
            <small>rules, push</small>
          </Link>
          <Link to="/settings/limits" className={path.startsWith("/settings/limits") ? "on" : ""}>
            Limits
            <small>spend</small>
          </Link>
          <Link to="/settings/templates" className={path.startsWith("/settings/templates") ? "on" : ""}>
            Templates
            <small>saved setups</small>
          </Link>
          <Link to="/settings/schedules" className={path.startsWith("/settings/schedules") ? "on" : ""}>
            Schedules
            <small>cron</small>
          </Link>
          <button onClick={onTheme}>{theme === "dark" ? "Light theme" : "Dark theme"}</button>
          <button onClick={onNotify} title="Browser notifications for new approvals while this tab is hidden">
            {notifyOn ? "Alerts on" : "Alerts off"}
            <small>approvals</small>
          </button>
          <ApproveKeysToggle />
        </>
      )}
    </>
  );
}

function ApproveKeysToggle() {
  const on = useApproveKeys();
  return (
    <button onClick={() => setApproveKeys(!on)} role="menuitemcheckbox" aria-checked={on} title="Approve and deny with the a and d keys">
      {on ? "Approval keys on" : "Approval keys off"}
      <small>a, d</small>
    </button>
  );
}

export default function V3Shell() {
  useEffect(startV3Live, []);
  const live = useLive();
  const st = useV3();
  const loc = useLocation();
  const nav = useNavigate();
  const [theme, toggleTheme] = useTheme();
  const [notifyOn, setNotify] = useBoolPref(KEYS.notify, true);
  const toasts = useToasts();
  const [pal, setPal] = useState<{ q?: string; scope?: string } | null>(null);
  const [pane, setPane] = useState<Session | null>(null);
  const [help, setHelp] = useState(false);
  const approveKeys = useApproveKeys();
  const [menu, setMenu] = useState<Menu>(null);
  const scopeRef = useRef<string | undefined>(undefined);

  const ny = useNeedsYou();
  const rows = useMemo(() => buildRows(st, live, ny), [st.version, live.version, ny.version]); // eslint-disable-line react-hooks/exhaustive-deps
  const c = useMemo(() => counts(rows), [rows]);
  const liveCount = [...live.sessions.values()].filter((s) => s.state === "running" || s.state.startsWith("awaiting")).length;

  const goSession = useCallback(
    (path: string) => {
      const m = /^\/session\/([^?]+)/.exec(path);
      if (!m) return nav(path);
      const sid = decodeURIComponent(m[1]);
      nav(`/w/${encodeURIComponent(st.threadOf[sid] || sid)}`);
    },
    [nav, st.threadOf],
  );
  useNotifications(goSession);

  // Driven threads join the v3 rows; the signature keeps streaming text from re-deriving every list.
  const orch = useOrch();
  const dsig = useMemo(() => drivenSig(orch), [orch.version]); // eslint-disable-line react-hooks/exhaustive-deps
  useEffect(() => setDrivenThreads(drivenRows(orch)), [dsig]); // eslint-disable-line react-hooks/exhaustive-deps
  const nt = useNotify();
  useEffect(() => {
    setNotifyNav(nav);
  }, [nav]);

  const openPalette = useCallback((o?: { q?: string; scope?: string }) => setPal({ scope: scopeRef.current, ...(o || {}) }), []);
  const ctx = useMemo(() => ({ openPalette, openPane: (s: Session) => setPane(s), setScope: (r: string | undefined) => (scopeRef.current = r) }), [openPalette]);

  useEffect(() => {
    let g = false;
    let gt: number | undefined;
    const onKey = (e: KeyboardEvent) => {
      if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === "k") {
        e.preventDefault();
        setPal((p) => (p ? null : { scope: scopeRef.current }));
        return;
      }
      if (typing(e) || e.metaKey || e.ctrlKey || e.altKey) return;
      if (e.key === "Escape") {
        setHelp(false);
        setPane(null);
        setMenu(null);
      }
      if (pal) return;
      if (e.key === "/") {
        e.preventDefault();
        setPal({ scope: scopeRef.current });
      } else if (e.key === "?") setHelp((h) => !h);
      else if (e.key === "c" && !g && !e.shiftKey && !overlayOpen()) {
        e.preventDefault();
        nav("/w/new");
      } else if (g) {
        g = false;
        chord.pending = false;
        window.clearTimeout(gt);
        // The second key belongs to the chord: a/d/e must not also reach the approval or list keys.
        e.preventDefault();
        e.stopImmediatePropagation();
        const v = VIEWS.find((x) => x.key === e.key);
        if (e.key === "i" || e.key === "h") nav("/w");
        else if (e.key === "d") nav("/desk");
        else if (e.key === "l") nav("/threads");
        else if (e.key === "t") nav("/timeline");
        else if (e.key === "w") nav("/w");
        else if (e.key === "a") nav("/notifications");
        else if (v) nav(v.to);
      } else if (e.key === "g") {
        g = true;
        chord.pending = true;
        window.clearTimeout(gt);
        gt = window.setTimeout(() => (g = chord.pending = false), 900);
      }
    };
    // Capture phase: the chord must see its second key before any view's listener does.
    window.addEventListener("keydown", onKey, true);
    return () => window.removeEventListener("keydown", onKey, true);
  }, [nav, pal]);

  const path = loc.pathname;
  useEffect(() => setMenu(null), [path]);
  useEffect(() => {
    if (!menu) return;
    const off = (e: MouseEvent) => {
      if (!(e.target as HTMLElement).closest(".dmenu, [data-menu]")) setMenu(null);
    };
    window.addEventListener("mousedown", off);
    return () => window.removeEventListener("mousedown", off);
  }, [menu]);
  const need = ny.status === "loading" ? c["needs-you"] : ny.count;
  const placeOn = (p: "work" | "alerts" | "settings" | "more") =>
    p === "alerts"
      ? path.startsWith("/notifications")
      : p === "work"
        ? path === "/" || path === "/w" || (path.startsWith("/w/") && path !== "/w/search")
        : p === "settings"
          ? path.startsWith("/settings")
          : path === "/more" || [...LOOKS, ...VIEWS].some((v) => path.startsWith(v.to)) || path.startsWith("/threads") || path.startsWith("/t/");
  const toggleNotify = () => {
    if (!notifyOn && typeof Notification !== "undefined" && Notification.permission === "default") void Notification.requestPermission();
    setNotify(!notifyOn);
  };
  const menuBody = <MenuBody path={path} theme={theme} onTheme={toggleTheme} notifyOn={notifyOn} onNotify={toggleNotify} />;

  return (
    <Ctx.Provider value={ctx}>
      <div className="v3">
        <div className="shell desk">
          <nav className="drail" aria-label="Places">
            <Link to="/w" className="logo" aria-label="agent-monitor home" title="agent-monitor" />
            <Link to="/w" className={`rb ${placeOn("work") ? "on" : ""}`} title={`Threads (g h)${need ? ` · ${need} need you` : ""}`}>
              Threads{need > 0 && <i className="nd" aria-label={`${need} need you`} />}
            </Link>
            <Link to="/w/search" className={`rb ${path === "/w/search" ? "on" : ""}`} title="Search every transcript (⌘K for the quick search)">
              Search
            </Link>
            <Link to="/notifications" className={`rb ${placeOn("alerts") ? "on" : ""}`} title={`Notifications (g a)${nt.unread ? ` · ${nt.unread} unread` : ""}`}>
              Alerts{nt.unread > 0 && <em className="un">{nt.unread > 99 ? "99+" : nt.unread}</em>}
            </Link>
            <Link to="/settings/devices" className={`rb ${placeOn("settings") ? "on" : ""}`} title="Devices, notifications, limits">
              Settings
            </Link>
            <span className="sp" />
            <button className={`rb sm ${menu === "more" || placeOn("more") ? "on" : ""}`} data-menu onClick={() => setMenu(menu === "more" ? null : "more")} aria-haspopup="menu" aria-expanded={menu === "more"}>
              More
            </button>
            <span className="lv" title={`${liveCount} live sessions · daemon ${live.connected ? "connected" : "offline"}`}>
              <span className={`dot ${live.connected ? "d-ok" : "d-err"}`} />
              {live.connected ? "live" : "offline"}
            </span>
            {menu === "more" && (
              <div className="dmenu" role="menu" style={{ left: 70, bottom: 40 }}>
                {menuBody}
              </div>
            )}
          </nav>
          <div className="main">
            <header className="ptop">
              <Link to="/w" className="logo" aria-label="Threads" />
              <b>{titleOf(path)}</b>
              <span className="ml-auto" />
              <span className={`dot ${live.connected ? "d-ok" : "d-err"}`} title={live.connected ? "live" : "reconnecting"} />
              <button className="btn sm ghost" data-menu onClick={() => setMenu(menu === "phone" ? null : "phone")} aria-label="Menu" aria-expanded={menu === "phone"}>
                ⋯
              </button>
              {menu === "phone" && (
                <div className="dmenu" role="menu">
                  {menuBody}
                </div>
              )}
            </header>
            <Boundary resetKey={path} what="This page">
              <Outlet />
            </Boundary>
          </div>
        </div>
        <nav className="tabbar" aria-label="Places">
          <Link to="/w" className={placeOn("work") ? "on" : ""}>
            <span className="ti">☰</span>Threads{need > 0 && <i className="nd" aria-label={`${need} need you`} />}
          </Link>
          <Link to="/w/search" className={path === "/w/search" ? "on" : ""}>
            <span className="ti">⌕</span>Search
          </Link>
          <Link to="/notifications" className={placeOn("alerts") ? "on" : ""} aria-label={`Alerts${nt.unread ? `, ${nt.unread} unread` : ""}`}>
            <span className="ti">◔</span>Alerts{nt.unread > 0 && <span className="bd un">{nt.unread > 99 ? "99+" : nt.unread}</span>}
          </Link>
          <Link to="/more" className={placeOn("more") || placeOn("settings") ? "on" : ""}>
            <span className="ti">⋯</span>More
          </Link>
        </nav>
        {pal && <Palette initial={pal} onClose={() => setPal(null)} />}
        {pane && (
          <div className="drawer" role="dialog" aria-label="Terminal pane">
            <div className="dh">
              <b className="trunc">Pane · {titleFor(pane)}</b>
              <button className="btn sm ml-auto" onClick={() => setPane(null)}>
                Close <Kbd>esc</Kbd>
              </button>
            </div>
            <div className="db">
              <PaneBridge session={pane} />
            </div>
          </div>
        )}
        {help && (
          <div className="scrim" onClick={() => setHelp(false)}>
            <div className="help" onClick={(e) => e.stopPropagation()}>
              <h3>Keyboard</h3>
              <dl>
                <dt><Kbd>⌘K</Kbd> <Kbd>/</Kbd></dt><dd>Search everything</dd>
                <dt><Kbd>j</Kbd> <Kbd>k</Kbd></dt><dd>Move between requests and threads</dd>
                <dt><Kbd>a</Kbd> <Kbd>A</Kbd></dt><dd>Allow, always allow the selected or highlighted request. High risk and plans need a second press.</dd>
                <dt><Kbd>d</Kbd></dt><dd>Deny the selected or highlighted request</dd>
                <dt><Kbd>c</Kbd></dt><dd>New thread</dd>
                <dt><Kbd>enter</Kbd></dt><dd>Open the thread</dd>
                <dt><Kbd>e</Kbd></dt><dd>Next error in Transcript</dd>
                <dt><Kbd>g</Kbd> then <Kbd>h</Kbd></dt><dd>Threads</dd>
                <dt><Kbd>g</Kbd> then <Kbd>d</Kbd> <Kbd>l</Kbd> <Kbd>t</Kbd></dt><dd>Desk, thread table, timeline</dd>
                <dt><Kbd>e</Kbd> <Kbd>p</Kbd> <Kbd>z</Kbd></dt><dd>Settle, pin, snooze the open driven thread</dd>
                <dt><Kbd>g</Kbd> then <Kbd>a</Kbd></dt><dd>Notifications</dd>
                <dt><Kbd>g</Kbd> then <Kbd>b</Kbd></dt><dd>Board</dd>
                <dt><Kbd>g</Kbd> then <Kbd>r</Kbd> <Kbd>x</Kbd> <Kbd>e</Kbd></dt><dd>Tracing, explore, errors</dd>
                <dt><Kbd>g</Kbd> then <Kbd>m</Kbd> <Kbd>n</Kbd> <Kbd>u</Kbd></dt><dd>Monitors, intelligence, rules</dd>
                <dt><Kbd>⇧⌫</Kbd></dt><dd>Clear search scope</dd>
                <dt><Kbd>{SHORT.left}</Kbd></dt><dd>Hide or show the thread list</dd>
                <dt><Kbd>{SHORT.right}</Kbd></dt><dd>Hide or show the details panel</dd>
                <dt><Kbd>{SHORT.top}</Kbd></dt><dd>Hide or show the thread top bar (focus mode)</dd>
              </dl>
              <label className="row" style={{ marginTop: 12, fontSize: 12.5 }}>
                <input type="checkbox" checked={approveKeys} onChange={(e) => setApproveKeys(e.target.checked)} />
                Approve and deny with single keys (a, A, d)
              </label>
            </div>
          </div>
        )}
        {toasts.length > 0 && (
          <div className="toasts">
            {toasts.map((t) => (
              <div key={t.id} className={`toast ${t.kind === "alert" ? "alert" : ""} ${t.output ? "has-out" : ""}`} onClick={() => (t.onClick ? (t.onClick(), dismissToast(t.id)) : dismissToast(t.id))} role="status">
                <b>{t.title}</b>
                {t.body && <div className="fg2 xs">{t.body}</div>}
                {t.output && (
                  <pre className="toast-out" onClick={(e) => e.stopPropagation()}>
                    {t.output}
                  </pre>
                )}
              </div>
            ))}
          </div>
        )}
      </div>
    </Ctx.Provider>
  );
}

export function MorePage() {
  const loc = useLocation();
  const [theme, toggleTheme] = useTheme();
  const [notifyOn, setNotify] = useBoolPref(KEYS.notify, true);
  return (
    <div className="desk-page">
      <div className="desk-col" style={{ maxWidth: 520 }}>
        <div className="dmenu" style={{ position: "static", boxShadow: "none" }}>
          <MenuBody path={loc.pathname} theme={theme} onTheme={toggleTheme} notifyOn={notifyOn} onNotify={() => setNotify(!notifyOn)} />
        </div>
      </div>
    </div>
  );
}
