// Workspace (/w, the home page): one thread list and one conversation view for driven and watched threads.
import { useEffect, useMemo, useState } from "react";
import { useLocation, useNavigate, useParams, useSearchParams } from "react-router-dom";
import { useLive } from "../../../lib/ws";
import { useOrch, useOrchThread } from "../../api/orch";
import { typing } from "../../ctx";
import { useV3 } from "../../live";
import { buildRows } from "../../model";
import { DrivenChat, NotFoundChat, useTick } from "./DrivenChat";
import { NewThread } from "./NewThread";
import { buildWRows, peerRows, treeOf, visualOrder } from "./rows";
import { PeerChat } from "./PeerChat";
import { parsePeerKey, useFleet } from "../../api/fleet";
import { rowFromSession } from "./handover";
import { DrivenSide, WatchedSide } from "./Side";
import { latestSession, WatchedChat, watchedLiveness } from "./WatchedChat";
import { useWatchedFeed } from "./watched";
import { useSessionChildren } from "./children";
import { ThreadList } from "./ThreadList";
import { Boundary } from "../../components/Boundary";
import { useTermHotkey } from "./TermDock";
import { hitFromParams } from "./hit";
import { SearchView } from "./SearchView";
import { EdgeBtns } from "./barToggles";
import { barForKey, drawer, toggleBar, useLayout, wide } from "../../layout";
import "../../styles/workspace.css";
import "../../styles/p7.css";
import { useNeedsYou, drivenNeedIds } from "../../api/needsYou";

export default function Workspace() {
  const { threadId } = useParams();
  const loc = useLocation();
  const nav = useNavigate();
  const isNew = loc.pathname === "/w/new";
  const isSearch = loc.pathname === "/w/search";
  const linkedPage = /^\/w\/[^/]+\/linked\/?$/.test(loc.pathname);
  const sel = isNew || isSearch ? undefined : threadId;
  const [sp] = useSearchParams();
  const hit = useMemo(() => (sel ? hitFromParams(sp) : null), [sp, sel]);
  const orch = useOrch();
  const v = useV3();
  const live = useLive();
  const ny = useNeedsYou();
  const watchedRows = useMemo(() => buildRows(v, live, ny), [v.version, live.version, ny.version]); // eslint-disable-line react-hooks/exhaustive-deps
  const needIds = useMemo(() => drivenNeedIds(ny.items, v.approvals.values()), [ny.version, v.approvals]); // eslint-disable-line react-hooks/exhaustive-deps
  // Snoozes expire on the client too, so recompute once a minute.
  const minute = useTick(true, 60_000);
  const fleet = useFleet();
  const peerSel = parsePeerKey(sel);
  // Threads on other machines join the same list; their ids start with "@".
  const rows = useMemo(
    () => [...buildWRows(orch, watchedRows, needIds, Date.now(), Infinity, live.sessions), ...peerRows(fleet.threads)].sort((a, b) => b.at - a.at),
    [orch, watchedRows, needIds, minute, fleet.at], // eslint-disable-line react-hooks/exhaustive-deps
  );
  const tree = useMemo(() => treeOf(rows), [rows]);
  const selRow = rows.find((r) => r.id === sel);
  const bareSession = sel && !peerSel && !selRow && !orch.threads.has(sel) ? live.sessions.get(sel) : undefined;
  const watchedSel =
    peerSel ? undefined : selRow?.mode === "watched" ? selRow.watched : !orch.threads.has(sel || "") ? watchedRows.find((r) => r.id === sel) || (bareSession ? rowFromSession(bareSession) : undefined) : undefined;
  const drivenId = sel && !watchedSel && !peerSel ? sel : undefined;
  const { detail, status } = useOrchThread(drivenId);
  const wl = watchedSel ? watchedLiveness(watchedSel, live.connected) : null;
  // Session tab of a watched thread: the one picked, else the hit's session, else the newest.
  const [tab, setTab] = useState("");
  useEffect(() => setTab(""), [sel, hit?.key]);
  const members = watchedSel?.thread?.sessions || [];
  const newest = watchedSel ? latestSession(watchedSel) : "";
  const tabSid = (tab && members.includes(tab) && tab) || (hit?.session && members.includes(hit.session) && hit.session) || newest;
  const onNewest = tabSid === newest;
  const wfeed = useWatchedFeed(tabSid, watchedSel?.id || "", onNewest && !!wl?.running, onNewest && !!(wl?.owned || wl?.running));
  const kids = useSessionChildren(watchedSel ? tabSid : "", onNewest && !!wl?.running);
  // Below 1100px the right panel is a drawer opened from the top bar.
  const [sideOpen, setSideOpen] = useState(false);
  useEffect(() => setSideOpen(false), [sel]);
  const closeSide = () => setSideOpen(false);
  const lay = useLayout();
  const onSide = () => toggleBar("right");
  useEffect(() => {
    drawer.open = () => setSideOpen(true);
    return () => void (drawer.open = null);
  }, []);
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      const b = barForKey(e);
      if (!b || !wide()) return;
      e.preventDefault();
      toggleBar(b);
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, []);

  useTermHotkey();

  // j/k move through the list.
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (typing(e) || e.metaKey || e.ctrlKey || e.altKey || (e.key !== "j" && e.key !== "k")) return;
      if (!rows.length) return;
      const order = visualOrder(tree);
      const i = order.findIndex((r) => r.id === sel);
      const n = order[Math.max(0, Math.min(order.length - 1, i < 0 ? 0 : i + (e.key === "j" ? 1 : -1)))];
      if (n) nav(`/w/${encodeURIComponent(n.id)}`);
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [rows, tree, sel, nav]);

  const liveLabel = orch.mock ? "sample data" : "";
  const notice =
    orch.status === "missing" ? (
      <div className="wmissing sm">
        <b>Orchestrator API not available on this daemon</b>
        <span>Watched threads still show. Driven threads need a newer daemon.</span>
      </div>
    ) : orch.status === "error" ? (
      <div className="wmissing sm err">
        <b>Orchestrator API error</b>
        <span>{orch.error}</span>
      </div>
    ) : !orch.connected && orch.status === "ok" ? (
      <div className="wmissing sm">
        <span>Reconnecting to the orchestrator stream…</span>
      </div>
    ) : null;

  const back = () => nav("/w");
  let main: React.ReactNode;
  let side: React.ReactNode = null;
  if (isNew) main = <NewThread onBack={back} />;
  else if (isSearch) main = <SearchView onBack={back} />;
  else if (!sel)
    main = (
      <section className="wchat wempty">
        <div className="state">
          <b>Pick a thread, or start one</b>
          <div>Sessions you run in a terminal show here too.</div>
          <button className="btn" onClick={() => nav("/w/new")}>
            New thread
          </button>
        </div>
      </section>
    );
  else if (peerSel)
    main = selRow?.peer ? (
      <PeerChat key={selRow.id} row={selRow} onBack={back} />
    ) : (
      <NotFoundChat status={fleet.at ? (fleet.machines.some((m) => m.name === peerSel.machine && m.status === "online") ? "missing" : "error") : "loading"} onBack={back} />
    );
  else if (watchedSel) {
    main = <WatchedChat key={watchedSel.id} row={watchedSel} feed={wfeed} onBack={back} onSide={onSide} tab={tabSid} onTab={setTab} hit={hit && (!hit.session || hit.session === tabSid || !members.includes(hit.session)) ? hit : null} kids={kids.data} linkedPage={linkedPage} />;
    side = <WatchedSide key={"side:" + watchedSel.id} row={watchedSel} feed={wfeed.feed} onClose={closeSide} sid={tabSid} kids={kids.data} kidsMissing={kids.missing} />;
  } else if (detail) {
    main = <DrivenChat key={detail.thread.id} detail={detail} onBack={back} onSide={onSide} hit={hit} linkedPage={linkedPage} />;
    side = <DrivenSide key={"side:" + detail.thread.id} detail={detail} onClose={closeSide} />;
  } else main = <NotFoundChat status={v.threadsStatus === "loading" && status !== "missing" ? "loading" : status} onBack={back} />;

  return (
    <div className={`ws ${sel || isNew || isSearch ? "has-sel" : ""} ${side ? "" : "no-side"} ${sideOpen && side ? "side-open" : ""} ${lay.left ? "lay-l" : ""} ${lay.right ? "lay-r" : ""} ${lay.top ? "lay-t" : ""}`}>
      <ThreadList rows={rows} tree={tree} sel={sel} notice={notice} liveLabel={liveLabel} />
      <Boundary key={sel || (isNew ? "new" : isSearch ? "search" : "none")} what={isSearch ? "Search" : "This thread"} className="wchat">
        {main}
      </Boundary>
      {side}
      {sideOpen && side && <div className="wside-scrim" onClick={closeSide} aria-hidden />}
      <EdgeBtns side={!!side} />
    </div>
  );
}
