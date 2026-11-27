import { Boundary } from "./components/Boundary";
import { lazy, Suspense, useEffect } from "react";
import { Navigate, Route, Routes, useLocation, useParams } from "react-router-dom";
import { useOrch } from "./api/orch";
import { useV3 } from "./live";
import V3Shell, { MorePage } from "./Shell";
import Inbox from "./views/Inbox";
import ThreadPage from "./views/thread/ThreadView";
import Board from "./views/Board";
import Timeline from "./views/Timeline";
import TracePage from "./views/TracePage";
import Explore from "./views/Explore";
import Errors from "./views/Errors";
import Tracing from "./views/Tracing";
import Monitors from "./views/Monitors";
import Intel from "./views/Intel";
import Rules from "./views/Rules";
import Menubar from "./views/Menubar";
import Now from "./views/Now";
import Control from "./views/Control";
import Messages from "./views/Messages";
import Workspace from "./views/workspace/Workspace";

const TVPage = lazy(() => import("./TvRoute"));
const FloatPage = lazy(() => import("./views/Float"));
const ApprovalsPage = lazy(() => import("./views/Approvals"));
const PairPage = lazy(() => import("./views/Pair"));
const MachinesPage = lazy(() => import("./views/settings/Machines"));
const PeerPage = lazy(() => import("./views/PeerPage"));
const DevicesPage = lazy(() => import("./views/settings/Devices"));
const NotifySettingsPage = lazy(() => import("./views/settings/NotifySettings"));
const LimitsPage = lazy(() => import("./views/settings/Limits"));
const VoicePage = lazy(() => import("./views/settings/Voice"));
const StoragePage = lazy(() => import("./views/settings/Storage"));
const WiringPage = lazy(() => import("./views/settings/Wiring"));
const CommandsPage = lazy(() => import("./views/Commands"));
const TemplatesPage = lazy(() => import("./views/settings/Templates"));
const SchedulesPage = lazy(() => import("./views/settings/Schedules"));
const NeedsPage = lazy(() => import("./views/Needs"));

/** /t/{id} of a driven thread opens it in the Workspace, so every old link lands in the right place. */
function ThreadOrWork() {
  const { threadId = "" } = useParams();
  const v = useV3();
  const orch = useOrch();
  const id = decodeURIComponent(threadId);
  const row = v.threads.find((t) => t.id === id);
  const d = row?.kind === "driven" ? row.drivenId || row.id : orch.threads.has(id) ? id : undefined;
  if (d) return <Navigate to={`/w/${encodeURIComponent(d)}`} replace />;
  return <ThreadPage />;
}

/** Old deep links (/session/:id, /thread/:id, /learnings, /#s=) open the thread in the workspace. */
function ToWorkspace() {
  const loc = useLocation();
  const st = useV3();
  const m = /^\/(?:session|thread)\/([^/?#]+)/.exec(loc.pathname) || /(?:^|[#&])s=([^&]+)/.exec(loc.hash || "");
  const id = m ? decodeURIComponent(m[1]) : "";
  return <Navigate to={id ? `/w/${encodeURIComponent(st.threadOf[id] || id)}` : "/w"} replace />;
}

export default function V3App() {
  const loc = useLocation();
  if (loc.pathname === "/" && /(?:^|[#&])s=/.test(loc.hash || "")) return <ToWorkspace />;
  return (
    <Routes>
      <Route
        path="/tv"
        element={
          <Suspense fallback={null}>
            <TVPage />
          </Suspense>
        }
      />
      <Route path="/menubar" element={<Menubar />} />
      <Route
        path="/float"
        element={
          <Boundary what="This window">
            <Suspense fallback={null}>
              <FloatPage />
            </Suspense>
          </Boundary>
        }
      />
      <Route
        path="/approvals"
        element={
          <Boundary what="This window">
            <Suspense fallback={null}>
              <ApprovalsPage />
            </Suspense>
          </Boundary>
        }
      />
      <Route
        path="/pair"
        element={
          <Suspense fallback={null}>
            <PairPage />
          </Suspense>
        }
      />
      <Route path="/session/*" element={<ToWorkspace />} />
      <Route path="/thread/*" element={<ToWorkspace />} />
      <Route path="/learnings" element={<ToWorkspace />} />
      <Route element={<V3Shell />}>
        <Route path="/" element={<Navigate to="/w" replace />} />
        <Route path="/desk" element={<Now />} />
        <Route path="/threads" element={<Inbox />} />
        <Route path="/control" element={<Control />} />
        <Route path="/w" element={<Workspace />} />
        <Route path="/w/new" element={<Workspace />} />
        <Route path="/w/search" element={<Workspace />} />
        <Route path="/w/:threadId" element={<Workspace />} />
        <Route path="/w/:threadId/linked" element={<Workspace />} />
        <Route path="/messages" element={<Messages />} />
        <Route path="/messages/:threadId" element={<Messages />} />
        <Route path="/inbox" element={<Navigate to="/w" replace />} />
        <Route path="/inbox/:list" element={<Inbox />} />
        <Route path="/t/:threadId" element={<ThreadOrWork />} />
        <Route path="/needs" element={<Suspense fallback={null}><NeedsPage /></Suspense>} />
        <Route path="/needs/recent" element={<Suspense fallback={null}><NeedsPage /></Suspense>} />
        <Route path="/notifications" element={<Navigate to="/needs/recent" replace />} />
        <Route path="/board" element={<Board />} />
        <Route path="/timeline" element={<Timeline />} />
        <Route path="/traces/:sessionId/:turn" element={<TracePage />} />
        <Route path="/explore" element={<Explore />} />
        <Route path="/errors" element={<Errors />} />
        <Route path="/tracing" element={<Tracing />} />
        <Route path="/monitors" element={<Monitors />} />
        <Route path="/intel" element={<Intel />} />
        <Route path="/rules" element={<Rules />} />
        <Route path="/more" element={<MorePage />} />
        <Route path="/settings" element={<MorePage />} />
        <Route
          path="/settings/devices"
          element={
            <Suspense fallback={null}>
              <DevicesPage />
            </Suspense>
          }
        />
        <Route path="/settings/machines" element={<Suspense fallback={null}><MachinesPage /></Suspense>} />
        <Route path="/m/:peer" element={<Suspense fallback={null}><PeerPage /></Suspense>} />
        <Route path="/settings/notifications" element={<Suspense fallback={null}><NotifySettingsPage /></Suspense>} />
        <Route path="/settings/limits" element={<Suspense fallback={null}><LimitsPage /></Suspense>} />
        <Route path="/settings/voice" element={<Suspense fallback={null}><VoicePage /></Suspense>} />
        <Route path="/settings/storage" element={<Suspense fallback={null}><StoragePage /></Suspense>} />
        <Route path="/settings/agents" element={<Suspense fallback={null}><WiringPage /></Suspense>} />
        <Route path="/commands" element={<Suspense fallback={null}><CommandsPage /></Suspense>} />
        <Route path="/settings/templates" element={<Suspense fallback={null}><TemplatesPage /></Suspense>} />
        <Route path="/settings/schedules" element={<Suspense fallback={null}><SchedulesPage /></Suspense>} />
        <Route path="/templates" element={<Navigate to="/settings/templates" replace />} />
        <Route path="/schedules" element={<Navigate to="/settings/schedules" replace />} />
        <Route path="*" element={<Navigate to="/w" replace />} />
      </Route>
    </Routes>
  );
}
