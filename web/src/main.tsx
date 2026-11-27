import React from "react";
import { Boundary } from "./v3/components/Boundary";
import ReactDOM from "react-dom/client";
import { BrowserRouter } from "react-router-dom";
import V3App from "./v3/V3App";
import "./styles/tokens.css";
import { connectLive } from "./lib/ws";
import { installPasskeyFetch } from "./v3/api/passkey";

// Theme before first paint: stored choice (light, dark or system), else dark.
try {
  const qt = new URLSearchParams(location.search).get("theme");
  if (qt === "light" || qt === "dark" || qt === "system") localStorage.setItem("agent-monitor-theme", qt);
  const t = localStorage.getItem("agent-monitor-theme");
  const sys = t === "system" && window.matchMedia("(prefers-color-scheme: light)").matches;
  document.documentElement.dataset.theme = t === "light" || sys ? "light" : "dark";
} catch {
  document.documentElement.dataset.theme = "dark";
}

// The pair page runs before this browser has credentials; the live socket would only fail there.
if (location.pathname !== "/pair") connectLive();

// Errors the Mac app's blank-page watchdog reads and logs.
const amErrors: string[] = ((window as unknown as { __amErrors?: string[] }).__amErrors = []);
window.addEventListener("error", (e) => amErrors.push(String(e.message || e.error)).valueOf() > 20 && amErrors.shift());
window.addEventListener("unhandledrejection", (e) => amErrors.push("rejection: " + String((e.reason as Error)?.message || e.reason)).valueOf() > 20 && amErrors.shift());

// A page open across a daemon update asks for chunk files the new build removed: reload once to get the new ones.
function reloadForNewBuild() {
  try {
    const last = Number(sessionStorage.getItem("am-chunk-reload") || 0);
    if (Date.now() - last < 30_000) return;
    sessionStorage.setItem("am-chunk-reload", String(Date.now()));
  } catch {
    /* storage blocked: reload anyway */
  }
  location.reload();
}
window.addEventListener("vite:preloadError", (e) => {
  e.preventDefault();
  reloadForNewBuild();
});
window.addEventListener("unhandledrejection", (e) => {
  if (/dynamically imported module|Importing a module script failed|error loading dynamically/i.test(String((e.reason as Error)?.message || e.reason))) reloadForNewBuild();
});

// After an install the daemon serves a new build: a hidden window reloads, a visible one offers a Reload button.
function watchForNewBuild() {
  const own = document.querySelector<HTMLScriptElement>('script[type="module"][src*="/assets/index-"]')?.getAttribute("src");
  if (!own) return;
  let stale = false;
  const offer = () => {
    if (document.getElementById("am-newbuild")) return;
    const b = document.createElement("button");
    b.id = "am-newbuild";
    b.textContent = "New version · Reload";
    b.onclick = () => location.reload();
    document.body.appendChild(b);
  };
  const check = async () => {
    if (!stale) {
      try {
        const html = await (await fetch("/", { cache: "no-store", credentials: "same-origin" })).text();
        const live = html.match(/\/assets\/index-[\w-]+\.js/)?.[0];
        stale = !!live && !own.endsWith(live);
      } catch {
        return; // daemon restarting: try again later
      }
    }
    if (!stale) return;
    if (document.hidden) reloadForNewBuild();
    else offer();
  };
  window.setInterval(() => void check(), 60_000);
  document.addEventListener("visibilitychange", () => void check());
}
if (location.pathname !== "/pair") watchForNewBuild();

installPasskeyFetch();

ReactDOM.createRoot(document.getElementById("root")!).render(
  <React.StrictMode>
    <BrowserRouter>
      <Boundary what="agent-monitor">
        <V3App />
      </Boundary>
    </BrowserRouter>
  </React.StrictMode>,
);
