// Chosen prompts (and their replies) of a thread as one Markdown document.
import { toSlack } from "./slack";

export interface ExportEntry {
  prompt: string;
  reply?: string;
  at?: number;
  label?: string; // e.g. "subagent: review the diff"
}

const day = (ms: number) => new Date(ms).toLocaleDateString(undefined, { year: "numeric", month: "short", day: "numeric" });
const clock = (ms: number) => new Date(ms).toLocaleTimeString(undefined, { hour: "2-digit", minute: "2-digit" });
const quote = (s: string) =>
  s
    .trim()
    .split("\n")
    .map((l) => (l ? `> ${l}` : ">"))
    .join("\n");

export function exportMarkdown(title: string, entries: ExportEntry[], replies: boolean, now = Date.now()): string {
  const parts = [`# ${title.trim() || "Thread"}`, `Exported ${day(now)}. ${entries.length} ${entries.length === 1 ? "prompt" : "prompts"}.`];
  entries.forEach((e, i) => {
    const when = e.at ? ` (${day(e.at)}, ${clock(e.at)})` : "";
    parts.push(`## Prompt ${i + 1}${e.label ? `, ${e.label}` : ""}${when}`, quote(e.prompt));
    if (replies && e.reply?.trim()) parts.push(e.reply.trim());
    if (i < entries.length - 1) parts.push("---");
  });
  return parts.join("\n\n") + "\n";
}

export const exportSlack = (title: string, entries: ExportEntry[], replies: boolean, now = Date.now()) => toSlack(exportMarkdown(title, entries, replies, now));

export function exportFileName(title: string, now = Date.now()): string {
  const slug = title.toLowerCase().replace(/[^a-z0-9]+/g, "-").replace(/^-+|-+$/g, "").slice(0, 60) || "thread";
  return `${slug}-${new Date(now).toISOString().slice(0, 10)}.md`;
}

/** Saves text as a file through a temporary object URL. */
export function downloadText(name: string, text: string, type = "text/markdown") {
  const url = URL.createObjectURL(new Blob([text], { type: `${type};charset=utf-8` }));
  const a = document.createElement("a");
  a.href = url;
  a.download = name;
  document.body.appendChild(a);
  a.click();
  a.remove();
  window.setTimeout(() => URL.revokeObjectURL(url), 1000);
}
