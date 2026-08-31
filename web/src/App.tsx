import { Navigate, Route, Routes, useLocation } from "react-router-dom";
import Shell from "./app/Shell";
import ThreadsPage from "./pages/threads/ThreadsPage";
import SessionPage from "./pages/session/SessionPage";
import StoryPage from "./pages/story/StoryPage";
import LearningsPage from "./pages/learnings/LearningsPage";
import TVPage from "./pages/tv/TVPage";

// Legacy deep links: /#s=<id>&f=<facet>&q=<query>  →  /session/<id>?f=&q=
function LegacyHashRedirect() {
  const loc = useLocation();
  const m = /(?:^|[#&])s=([^&]+)/.exec(loc.hash || "");
  if (!m) return null;
  const id = decodeURIComponent(m[1]);
  const f = /(?:^|[#&])f=([^&]+)/.exec(loc.hash || "");
  const q = /(?:^|[#&])q=([^&]+)/.exec(loc.hash || "");
  const p = new URLSearchParams();
  if (f) p.set("f", decodeURIComponent(f[1]));
  if (q) p.set("q", decodeURIComponent(q[1]));
  const s = p.toString();
  return <Navigate to={`/session/${encodeURIComponent(id)}${s ? "?" + s : ""}`} replace />;
}

export default function App() {
  const loc = useLocation();
  if (loc.pathname === "/" && /(?:^|[#&])s=/.test(loc.hash || "")) return <LegacyHashRedirect />;
  return (
    <Routes>
      {/* TV widget: no shell, its own compact layout (AgentTV points at /tv) */}
      <Route path="/tv" element={<TVPage />} />
      <Route element={<Shell />}>
        <Route path="/" element={<ThreadsPage />} />
        <Route path="/threads" element={<Navigate to="/" replace />} />
        <Route path="/session/:id" element={<SessionPage />} />
        <Route path="/thread/:id" element={<StoryPage />} />
        <Route path="/thread/:id/story" element={<StoryPage />} />
        <Route path="/thread/:id/learnings" element={<LearningsPage />} />
        <Route path="*" element={<Navigate to="/" replace />} />
      </Route>
    </Routes>
  );
}
