import { createContext, useContext, useEffect, useRef, useState } from "react";
import { showToast } from "../../app/toast";

const RENAME = "am-rename";

/** Which chat a component sits in when two show side by side. */
export const PaneCtx = createContext<"main" | "beside">("main");

/** Opens the title editor of the chat in that pane (from a command menu). */
export const startRename = (pane: "main" | "beside" = "main") => window.dispatchEvent(new CustomEvent(RENAME, { detail: pane }));

/** The chat title; a double-click or startRename() edits it. Enter saves, Esc cancels. */
export function EditTitle({ title, save }: { title: string; save: (t: string) => Promise<unknown> }) {
  const [edit, setEdit] = useState<string | null>(null);
  const ending = useRef(false); // Enter then blur must save once
  // The saved title shows until the server list has it.
  const [saved, setSaved] = useState<string | null>(null);
  const pane = useContext(PaneCtx);
  useEffect(() => {
    const on = (e: Event) => {
      if ((e as CustomEvent).detail !== pane) return;
      ending.current = false;
      setEdit(saved ?? title);
    };
    window.addEventListener(RENAME, on);
    return () => window.removeEventListener(RENAME, on);
  }, [title, saved, pane]);
  useEffect(() => {
    if (saved !== null && saved === title) setSaved(null);
  }, [title, saved]);
  useEffect(() => {
    if (saved === null) return;
    const t = setTimeout(() => setSaved(null), 30_000);
    return () => clearTimeout(t);
  }, [saved]);

  const shown = saved ?? title;
  if (edit === null)
    return (
      <b className="trunc wtitle-ed" title={`${shown} (double-click to rename)`} onDoubleClick={() => {
          ending.current = false;
          setEdit(shown);
        }}>
        {shown}
      </b>
    );
  const done = async (keep: boolean) => {
    if (ending.current) return;
    ending.current = true;
    const v = edit.trim();
    setEdit(null);
    if (!keep || !v || v === shown) return;
    setSaved(v);
    try {
      await save(v);
    } catch (e) {
      setSaved(null);
      showToast("Rename failed", (e as Error).message, { kind: "alert" });
    }
  };
  return (
    <input
      className="wtitle-in"
      autoFocus
      value={edit}
      maxLength={120}
      aria-label="Title"
      onFocus={(e) => e.currentTarget.select()}
      onChange={(e) => setEdit(e.target.value)}
      onKeyDown={(e) => {
        if (e.key === "Enter") void done(true);
        else if (e.key === "Escape") void done(false);
      }}
      onBlur={() => void done(true)}
    />
  );
}
