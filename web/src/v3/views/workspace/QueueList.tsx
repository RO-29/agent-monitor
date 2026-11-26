// Messages that wait for an idle agent, just above the composer: the CLI's own queue, then agent-monitor's. Keys work only while the list has focus.
import { forwardRef, useEffect, useImperativeHandle, useId, useLayoutEffect, useRef, useState } from "react";
import { showToast } from "../../../app/toast";
import { overlayOpen } from "../../keys";
import { cliApi, queueApi, type CliItem, type CliView, type QItem, type QView } from "../../api/queue";

export interface QueueListHandle {
  /** Selects the newest queued message and focuses the list; false when the queue is empty. */
  selectNewest: () => boolean;
}

/** The CLI's own queue of a watched session. */
export interface CliQueueProps {
  session: string;
  /** The CLI's name, for example "Claude Code". */
  name: string;
  showAll: boolean;
  onShowAll: (all: boolean) => void;
}

interface Props {
  threadId: string;
  q: QView;
  /** Puts a message back into the composer. */
  onEdit: (text: string) => void;
  sendLabel: string;
  pausedNote?: string;
  cli?: CliQueueProps;
}

type Entry = { src: "cli"; it: CliItem } | { src: "am"; it: QItem };

const focusComposer = (el: HTMLElement | null) => el?.closest(".wchat")?.querySelector<HTMLTextAreaElement>(".wcomp textarea")?.focus();

const cliSendTitle = (name: string) => `Interrupts the current turn; ${name} then sends every queued message as the next turn (ctrl+s)`;

export const QueueList = forwardRef<QueueListHandle, Props>(function QueueList({ threadId, q, onEdit, sendLabel, pausedNote, cli }, ref) {
  const items = q.items;
  const cv: CliView | undefined = cli && q.cli?.supported ? q.cli : undefined;
  const cliItems = cv?.items || [];
  const cliUsers = cliItems.filter((x) => x.kind === "user").length;
  const entries: Entry[] = [...cliItems.map((it) => ({ src: "cli" as const, it })), ...items.map((it) => ({ src: "am" as const, it }))];
  const [sel, setSel] = useState<number | null>(null);
  const [busyId, setBusyId] = useState("");
  const list = useRef<HTMLDivElement>(null);
  const lid = useId();

  useImperativeHandle(ref, () => ({
    selectNewest: () => {
      if (!entries.length) return false;
      setSel(entries.length - 1);
      list.current?.focus();
      return true;
    },
  }));
  useEffect(() => setSel(null), [threadId]);
  useEffect(() => {
    if (sel != null && sel >= entries.length) setSel(entries.length ? entries.length - 1 : null);
  }, [entries.length, sel]);
  useLayoutEffect(() => {
    if (sel != null) list.current?.querySelector<HTMLElement>(`[data-i="${sel}"]`)?.scrollIntoView({ block: "nearest" });
  }, [sel]);

  const keepFocus = (hadFocus: boolean, chat: HTMLElement | null) =>
    requestAnimationFrame(() => hadFocus && !list.current?.isConnected && chat?.querySelector<HTMLTextAreaElement>(".wcomp textarea")?.focus());

  const run = async (it: QItem, what: "send" | "remove") => {
    const chat = list.current?.closest<HTMLElement>(".wchat") || null;
    const hadFocus = !!list.current?.contains(document.activeElement);
    setBusyId(it.id);
    try {
      if (what === "send") await queueApi.sendNow(threadId, it.id);
      else await queueApi.remove(threadId, it.id);
    } catch (e) {
      showToast(what === "send" ? "Could not send it now" : "Could not remove it", (e as Error).message, { kind: "alert" });
    } finally {
      setBusyId("");
    }
    // The list unmounts with its last message; keep the keyboard in the composer.
    keepFocus(hadFocus, chat);
  };
  const edit = async (it: QItem) => {
    if (busyId) return; // a double click must not remove twice
    // The composer takes back only text; files would be lost without a word.
    if (it.attachments?.length) return showToast("This message has files", "Remove it and send it again with the files.", { kind: "alert" });
    const chat = list.current?.closest<HTMLElement>(".wchat") || null;
    setBusyId(it.id);
    try {
      await queueApi.remove(threadId, it.id);
      onEdit(it.text);
      setSel(null);
      requestAnimationFrame(() => chat?.querySelector<HTMLTextAreaElement>(".wcomp textarea")?.focus());
    } catch (e) {
      showToast("Could not edit it", (e as Error).message, { kind: "alert" });
    } finally {
      setBusyId("");
    }
  };
  const runCli = async (it: CliItem, what: "send-now" | "edit" | "remove") => {
    if (!cli || !cv) return;
    if (it.kind !== "user") return showToast(`${cli.name} sends its notifications by itself`, "Only your own queued messages can be sent, edited or removed here.");
    if (!cv.reachable) return showToast("Cannot reach this terminal", `Change the ${cli.name} queue in its terminal.`, { kind: "alert" });
    if (what === "remove" && !cv.canRemove) return showToast("Remove works with one queued message", cv.removeWhy || "", { kind: "alert" });
    const chat = list.current?.closest<HTMLElement>(".wchat") || null;
    const hadFocus = !!list.current?.contains(document.activeElement);
    setBusyId(it.id);
    try {
      const text = await cliApi.act(threadId, cli.session, what, it.id);
      if (what === "edit") {
        onEdit(text);
        setSel(null);
        requestAnimationFrame(() => chat?.querySelector<HTMLTextAreaElement>(".wcomp textarea")?.focus());
      } else if (what === "send-now") showToast(`${cli.name} sends the queue now`, "The current turn stopped; the queued messages go as the next turn.");
      void queueApi.load(threadId, cli.session, cli.showAll);
    } catch (e) {
      const t = what === "send-now" ? "Could not send it now" : what === "edit" ? "Could not take the queue back" : "Could not remove it";
      showToast(t, (e as Error).message, { kind: "alert" });
    } finally {
      setBusyId("");
    }
    if (what !== "edit") keepFocus(hadFocus, chat);
  };

  const hidden = cv?.hidden || 0;
  if (!entries.length && !hidden && !q.terminal?.length) return null;
  const cur = sel != null ? entries[sel] : undefined;

  const onKey = (e: React.KeyboardEvent) => {
    if (overlayOpen() || e.altKey) return;
    const k = e.key.toLowerCase();
    const mod = e.ctrlKey || e.metaKey;
    const busy = cur && (busyId === cur.it.id || (cur.src === "am" && cur.it.state === "sending"));
    let handled = true;
    if (!mod && (e.key === "ArrowUp" || k === "k")) setSel((s) => Math.max(0, (s ?? entries.length) - 1));
    else if (!mod && (e.key === "ArrowDown" || k === "j")) {
      if (sel == null || sel >= entries.length - 1) {
        setSel(null);
        focusComposer(list.current);
      } else setSel(sel + 1);
    } else if (mod && k === "s") cur && !busy && void (cur.src === "cli" ? runCli(cur.it, "send-now") : run(cur.it, "send"));
    else if (mod && k === "x") cur && !busy && void (cur.src === "cli" ? runCli(cur.it, "remove") : run(cur.it, "remove"));
    else if (!mod && e.key === "Enter") cur && !busy && void (cur.src === "cli" ? runCli(cur.it, "edit") : edit(cur.it));
    else if (e.key === "Escape") {
      setSel(null);
      focusComposer(list.current);
    } else handled = false;
    if (handled) {
      e.preventDefault();
      e.stopPropagation();
    }
  };

  const select = (i: number) => (e: React.MouseEvent) => {
    if ((e.target as HTMLElement).closest("button")) return;
    e.preventDefault();
    setSel(i);
    list.current?.focus();
  };
  const keysHint = (
    <span className="hidephone wq-keys">
      {sel == null ? (
        <>
          <kbd className="wq-k">↑</kbd> from an empty box selects
        </>
      ) : (
        <>
          <kbd className="wq-k">ctrl+s</kbd> {cur?.src === "cli" ? "send now" : sendLabel.toLowerCase()} <kbd className="wq-k">ctrl+x</kbd> remove <kbd className="wq-k">↵</kbd> edit <kbd className="wq-k">esc</kbd> back
        </>
      )}
    </span>
  );
  const cliNote = !cv
    ? ""
    : !cv.reachable
      ? `agent-monitor cannot reach this terminal, so these show here only. Change them in the terminal.`
      : cliUsers > 1
        ? `${cli!.name} gives its queue back only all at once: Edit takes all ${cliUsers} into the message box, Send now sends them all. Remove works when one message is queued.`
        : "";

  return (
    <div className="wqueue">
      {(entries.length > 0 || hidden > 0) && (
        <div
          ref={list}
          className="wq-list"
          role="listbox"
          tabIndex={sel == null ? -1 : 0}
          aria-label={`${entries.length} queued ${entries.length === 1 ? "message" : "messages"}`}
          aria-activedescendant={cur ? `${lid}-${cur.it.id}` : undefined}
          data-ownkeys
          onKeyDown={onKey}
          onBlur={(e) => !e.currentTarget.contains(e.relatedTarget as Node) && setSel(null)}
        >
          {cv && cli && (cliItems.length > 0 || hidden > 0) && (
            <>
              <div className="wq-head">
                <span>
                  {cliUsers || !cliItems.some((x) => x.kind === "bash") ? `Queued in ${cli.name} · ${cliUsers}` : "Terminal"}
                </span>
                {cliItems.length > 0 && keysHint}
              </div>
              {cliNote && <div className="wq-note">{cliNote}</div>}
              {cliItems.map((it, i) => {
                const sending = busyId === it.id;
                const sys = it.kind === "system" || it.kind === "bash";
                const bash = it.kind === "bash";
                const pos = cliItems.slice(0, i).filter((x) => x.kind !== "bash").length;
                return (
                  <div
                    key={it.id}
                    id={`${lid}-${it.id}`}
                    data-i={i}
                    role="option"
                    aria-selected={i === sel}
                    aria-label={`Queued in ${cli.name}, message ${i + 1} of ${cliItems.length}${sys ? ", notification" : ""}`}
                    className={`wq-it cli ${sys ? "ro" : ""} ${sending ? "sending" : ""}`}
                    onMouseDown={select(i)}
                  >
                    <div className="wq-b">
                      <div className="wq-meta">
                        <span className={`wq-st ${sending || (bash && it.status !== "waiting") ? "run" : ""}`}>
                          {sending ? "Working…" : bash ? (it.status === "waiting" ? "Waiting" : "Running") : sys ? "Notification" : "Queued"}
                        </span>
                        {!bash && <span className="wq-pos">{pos === 0 ? "next" : `#${pos + 1}`}</span>}
                        <span className="wq-src">{bash ? "in the terminal" : `in ${cli.name}`}</span>
                      </div>
                      <div className="wq-tx">{it.text || "(no text, for example an image)"}</div>
                      {bash && it.live && <pre className="wq-live">{it.live}</pre>}
                    </div>
                    {bash && (
                      <div className="wq-a">
                        <button
                          className="btn sm ghost"
                          onClick={() => void cliApi.forget(threadId, cli.session, it.id).catch((e: Error) => showToast("Not cleared", e.message, { kind: "alert" }))}
                          aria-label="Stop showing this command"
                          title="Stop showing this command. Use it when the terminal shows that it ended but no result arrived here."
                        >
                          Clear
                        </button>
                      </div>
                    )}
                    {!sys && (
                      <div className="wq-a">
                        <button className="btn sm ghost" disabled={sending || !cv.reachable} onClick={() => void runCli(it, "send-now")} title={cliSendTitle(cli.name)}>
                          Send now
                        </button>
                        <button
                          className="btn sm ghost"
                          disabled={sending || !cv.reachable}
                          onClick={() => void runCli(it, "edit")}
                          title={cliUsers > 1 ? `Takes all ${cliUsers} queued messages back into the message box (Enter)` : "Takes it back into the message box (Enter)"}
                        >
                          {cliUsers > 1 ? "Edit all" : "Edit"}
                        </button>
                        <button
                          className="btn sm ghost"
                          disabled={sending || !cv.canRemove}
                          onClick={() => void runCli(it, "remove")}
                          aria-label={`Remove this message from the ${cli.name} queue`}
                          title={cv.canRemove ? "Remove (ctrl+x)" : cv.removeWhy}
                        >
                          Remove
                        </button>
                      </div>
                    )}
                  </div>
                );
              })}
              {hidden > 0 && !cli.showAll && (
                <button className="wq-more" onClick={() => cli.onShowAll(true)}>
                  {hidden} {hidden === 1 ? "notification" : "notifications"} from {cli.name} also wait. Show
                </button>
              )}
              {cli.showAll && cliItems.some((x) => x.kind === "system") && (
                <button className="wq-more" onClick={() => cli.onShowAll(false)}>
                  Hide notifications
                </button>
              )}
            </>
          )}
          {items.length > 0 && (
            <div className="wq-head">
              <span>{cv && cliItems.length ? `Queued in agent-monitor · ${items.length}` : `Queue · ${items.length}`}</span>
              {!(cv && cliItems.length) && keysHint}
            </div>
          )}
          {pausedNote && items.length > 0 && <div className="wq-note">{pausedNote}</div>}
          {items.map((it, j) => {
            const i = cliItems.length + j;
            const sending = it.state === "sending" || busyId === it.id;
            const failed = it.state === "failed";
            return (
              <div
                key={it.id}
                id={`${lid}-${it.id}`}
                data-i={i}
                role="option"
                aria-selected={i === sel}
                aria-label={`Queued message ${j + 1} of ${items.length}${failed ? ", failed" : sending ? ", sending" : ""}`}
                className={`wq-it ${failed ? "fail" : ""} ${sending ? "sending" : ""}`}
                onMouseDown={select(i)}
              >
                <div className="wq-b">
                  <div className="wq-meta">
                    <span className={`wq-st ${failed ? "err" : sending ? "run" : ""}`}>{failed ? "Failed" : sending ? "Sending…" : "Queued"}</span>
                    <span className="wq-pos">{j === 0 && !failed && !cliItems.length ? "next" : `#${i + 1}`}</span>
                    {it.attachments?.length ? <span className="wq-pos">{it.attachments.length} {it.attachments.length === 1 ? "image" : "images"}</span> : null}
                  </div>
                  <div className="wq-tx">{it.text}</div>
                  {failed && it.error && <div className="wq-err">{it.error}</div>}
                  {!failed && it.error && <div className="wq-warn">{it.error}</div>}
                </div>
                <div className="wq-a">
                  <button className="btn sm ghost" disabled={sending} onClick={() => void run(it, "send")} title={`${sendLabel} (ctrl+s)`}>
                    {sendLabel}
                  </button>
                  <button className="btn sm ghost" disabled={sending} onClick={() => void edit(it)} title="Put it back in the message box (Enter)">
                    Edit
                  </button>
                  <button className="btn sm ghost" disabled={sending} onClick={() => void run(it, "remove")} aria-label="Remove this queued message" title="Remove (ctrl+x)">
                    Remove
                  </button>
                </div>
              </div>
            );
          })}
        </div>
      )}
      {!!q.terminal?.length && !cv && (
        <div className="wq-term" aria-label="Queued in the terminal">
          <div className="wq-head">
            <span>Queued in the terminal</span>
            <span className="wq-keys">as shown in the terminal</span>
          </div>
          {q.terminal.map((t, i) => (
            <div key={i} className="wq-it ro">
              <div className="wq-b">
                <div className="wq-tx">{t}</div>
              </div>
            </div>
          ))}
        </div>
      )}
    </div>
  );
});
