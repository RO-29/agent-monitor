// The second chat of the side-by-side view (/w/<id>?beside=<id2>), on screens wider than 1100 px.
import { useEffect, useState } from "react";
import { useLive } from "../../../lib/ws";
import { useOrch, useOrchThread } from "../../api/orch";
import { PaneCtx } from "../../components/EditTitle";
import type { Row } from "../../model";
import { DrivenChat, NotFoundChat } from "./DrivenChat";
import { rowFromSession } from "./handover";
import type { WRow } from "./rows";
import { latestSession, WatchedChat, watchedLiveness } from "./WatchedChat";
import { useWatchedFeed } from "./watched";
import { useSessionChildren } from "./children";

const SPLIT_MQ = "(min-width:1101px)";

/** True while the window is wide enough for two chats. */
export function useSplitFits(): boolean {
  const [fits, setFits] = useState(() => window.matchMedia(SPLIT_MQ).matches);
  useEffect(() => {
    const mq = window.matchMedia(SPLIT_MQ);
    const on = () => setFits(mq.matches);
    mq.addEventListener("change", on);
    return () => mq.removeEventListener("change", on);
  }, []);
  return fits;
}

export function BesideChat({ id, rows, watchedRows, onClose, onSwap }: { id: string; rows: WRow[]; watchedRows: Row[]; onClose: () => void; onSwap: () => void }) {
  const orch = useOrch();
  const live = useLive();
  const row = rows.find((r) => r.id === id);
  const bare = !row && !orch.threads.has(id) ? live.sessions.get(id) : undefined;
  const watched = row?.mode === "watched" ? row.watched : !orch.threads.has(id) ? watchedRows.find((r) => r.id === id) || (bare ? rowFromSession(bare) : undefined) : undefined;
  const { detail, status } = useOrchThread(watched || id.startsWith("@") ? undefined : id);
  const wl = watched ? watchedLiveness(watched, live.connected) : null;
  const sid = watched ? latestSession(watched) : "";
  const feed = useWatchedFeed(sid, watched?.id || "", !!wl?.running, !!(wl?.owned || wl?.running));
  const kids = useSessionChildren(watched ? sid : "", !!wl?.running);

  let chat: React.ReactNode;
  if (watched) chat = <WatchedChat key={watched.id} row={watched} feed={feed} tab={sid} kids={kids.data} />;
  else if (detail) chat = <DrivenChat key={detail.thread.id} detail={detail} />;
  else chat = <NotFoundChat status={id.startsWith("@") ? "missing" : status} onBack={onClose} />;
  return (
    <div className="wbeside" aria-label="Second thread">
      <div className="wbeside-h">
        <span className="muted small">Beside</span>
        <span className="grow1" />
        <button className="btn sm ghost" onClick={onSwap} title="Make this the main thread">
          Swap
        </button>
        <button className="btn sm ghost" onClick={onClose} title="Close the second thread" aria-label="Close the second thread">
          Close
        </button>
      </div>
      <PaneCtx.Provider value="beside">{chat}</PaneCtx.Provider>
    </div>
  );
}
