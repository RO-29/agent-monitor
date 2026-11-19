// One-tap copy for commands and code blocks: shown on hover with a mouse, always on touch.
import { useEffect, useState } from "react";
import { copy } from "../../lib/clipboard";
import { showToast } from "../../app/toast";

export { copyableCode } from "../../lib/insert";

export async function copyWithToast(text: string): Promise<boolean> {
  const ok = await copy(text);
  if (!ok) showToast("Copy failed", "The browser blocked the clipboard.", { kind: "alert" });
  return ok;
}

export function CopyBtn({ text, label = "Copy", className = "" }: { text: string | (() => string); label?: string; className?: string }) {
  const [done, setDone] = useState(false);
  useEffect(() => {
    if (!done) return;
    const t = window.setTimeout(() => setDone(false), 1400);
    return () => window.clearTimeout(t);
  }, [done]);
  return (
    <button
      type="button"
      className={`wcpy ${done ? "on" : ""} ${className}`}
      aria-label={done ? "Copied" : label}
      title={label}
      onClick={async (e) => {
        e.stopPropagation();
        if (await copyWithToast(typeof text === "function" ? text() : text)) setDone(true);
      }}
    >
      {done ? "Copied" : "Copy"}
    </button>
  );
}
