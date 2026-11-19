// Reads the "Attached files" note (and older @path lines) that agent-monitor appends for terminal sessions.
export interface NoteFile {
  id: string;
  name: string;
  mime: string;
  url: string;
}

const HEAD = "Attached files (open them with your file tools):";
// "- /abs/uploads/<scope>/up_<hex>.<ext> (name, mime, size)" or "@/abs/uploads/<scope>/up_<hex>.<ext>"
const LINE = /^(?:- |@)(\/\S*\/uploads\/[\w-]+\/(up_[0-9a-f]{8,})\.(\w+))(?: \((.*), ([\w.+-]+\/[\w.+-]+), [^,()]+\))?\s*$/;
const MIME: Record<string, string> = { png: "image/png", jpg: "image/jpeg", jpeg: "image/jpeg", webp: "image/webp", gif: "image/gif", heic: "image/heic", pdf: "application/pdf", txt: "text/plain", md: "text/markdown", csv: "text/csv", json: "application/json" };

export function splitFileNote(text: string): { text: string; files: NoteFile[] } {
  if (!text || !text.includes("/uploads/")) return { text, files: [] };
  const keep: string[] = [];
  const files: NoteFile[] = [];
  for (const ln of text.split("\n")) {
    if (ln.trim() === HEAD) continue;
    const m = LINE.exec(ln.trim());
    if (!m) {
      keep.push(ln);
      continue;
    }
    const [, , id, ext, name, mime] = m;
    files.push({ id, name: name || `${id}.${ext}`, mime: mime || MIME[ext.toLowerCase()] || "application/octet-stream", url: `/api/orch/uploads/${id}` });
  }
  if (!files.length) return { text, files };
  return { text: keep.join("\n").replace(/\s+$/, ""), files };
}
