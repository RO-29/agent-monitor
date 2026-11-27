// /needs: what waits for you now, and the recent alerts, in one place on every device.
import { lazy, Suspense } from "react";
import { Link, useLocation } from "react-router-dom";
import { useNeedsYou } from "../api/needsYou";
import { useNotify } from "../api/notify";

const Waiting = lazy(() => import("./Approvals"));
const Recent = lazy(() => import("./Notifications"));

export default function NeedsPage() {
  const recent = useLocation().pathname.startsWith("/needs/recent");
  const ny = useNeedsYou();
  const nt = useNotify();
  return (
    <div className="needs-page">
      <div className="nt-f needs-tabs" role="tablist" aria-label="Needs you">
        <Link to="/needs" role="tab" aria-selected={!recent} className={recent ? "" : "on"}>
          Waiting{ny.count > 0 && <b>{ny.count}</b>}
        </Link>
        <Link to="/needs/recent" role="tab" aria-selected={recent} className={recent ? "on" : ""}>
          Recent{nt.unread > 0 && <b>{nt.unread}</b>}
        </Link>
      </div>
      <Suspense fallback={null}>{recent ? <Recent /> : <Waiting embedded />}</Suspense>
    </div>
  );
}
