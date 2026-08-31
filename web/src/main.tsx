import React from "react";
import ReactDOM from "react-dom/client";
import { BrowserRouter } from "react-router-dom";
import App from "./App";
import "./styles/tokens.css";
import { connectLive } from "./lib/ws";

// Theme before first paint: stored choice, else dark (dark-first design).
try {
  const t = localStorage.getItem("agent-monitor-theme");
  document.documentElement.dataset.theme = t === "light" ? "light" : "dark";
} catch {
  document.documentElement.dataset.theme = "dark";
}

connectLive();

ReactDOM.createRoot(document.getElementById("root")!).render(
  <React.StrictMode>
    <BrowserRouter>
      <App />
    </BrowserRouter>
  </React.StrictMode>,
);
