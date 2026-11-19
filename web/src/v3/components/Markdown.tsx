import { copyableCode, copyWithToast } from "./CopyBtn";
import { createContext, useContext, useEffect, useMemo, useRef, useState } from "react";

// Escapes first, then adds only our own tags, so agent text can never inject markup.
const esc = (s: string) => s.replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;").replace(/"/g, "&quot;").replace(/'/g, "&#39;");

const BIDI = /[\u202A-\u202E\u2066-\u2069]/g;

function inline(raw: string): string {
  const codes: string[] = [];
  let s = raw.replace(/(`+)([^`]|[^`][\s\S]*?[^`])\1(?!`)/g, (_m, _t, c: string) => {
    codes.push(`<code>${esc(c)}</code>`);
    return `\u0000${codes.length - 1}\u0000`;
  });
  s = esc(s);
  const links: string[] = [];
  const keep = (h: string) => {
    links.push(h);
    return `\u0001${links.length - 1}\u0001`;
  };
  // The real target is the title, and its host shows as text when the link text is not the URL itself.
  s = s.replace(/\[([^\]]+)\]\((https?:\/\/[^\s)\u0000]+)\)/g, (_m, t: string, u: string) => {
    const host = /^https?:\/\/([^/?#]+)/.exec(u)?.[1] || u;
    const txt = t.replace(BIDI, "\uFFFD");
    return keep(`<a href="${u}" title="${u}" target="_blank" rel="noreferrer noopener">${txt}</a>${txt === u ? "" : `<span class="mdhost"> (${host})</span>`}`);
  });
  s = s.replace(/\[([^\]]+)\]\(([^\s)\u0000]+)\)/g, (_m, t: string, u: string) => keep(`<span class="mdref" title="${u}">${t}</span>`));
  s = s.replace(/(^|[\s(])(https?:\/\/[^\s<)\u0000]+[^\s<).,;:!?'"\u0000])/g, (_m, pre: string, u: string) => pre + keep(`<a href="${u}" target="_blank" rel="noreferrer noopener">${u}</a>`));
  s = s
    .replace(/\*\*(?=\S)([\s\S]*?\S)\*\*/g, "<strong>$1</strong>")
    .replace(/(^|[^\w])__(?=\S)([\s\S]*?\S)__(?!\w)/g, "$1<strong>$2</strong>")
    .replace(/(^|[\s(])\*(?=[^\s*])([^*\n]*?[^\s*])\*(?![\w*])/g, "$1<em>$2</em>")
    .replace(/(^|[\s(])_(?=[^\s_])([^_\n]*?[^\s_])_(?!\w)/g, "$1<em>$2</em>")
    .replace(/~~(?=\S)([\s\S]*?\S)~~/g, "<del>$1</del>");
  s = s.replace(/\u0001(\d+)\u0001/g, (_m, i: string) => links[Number(i)]);
  return s.replace(/\u0000(\d+)\u0000/g, (_m, i: string) => codes[Number(i)]);
}

const isTableSep = (l: string) => /^\s*\|?\s*:?-{2,}:?\s*(\|\s*:?-{2,}:?\s*)*\|?\s*$/.test(l) && l.includes("-");
const cells = (l: string) => l.trim().replace(/^\|/, "").replace(/\|$/, "").split(/(?<!\\)\|/).map((c) => c.trim().replace(/\\\|/g, "|"));
const LIST_RE = /^(\s*)([-*+]|\d+[.)])\s+(.*)$/;

function list(lines: string[], i: number): [string, number] {
  const base = LIST_RE.exec(lines[i])!;
  const indent = base[1].length;
  const ordered = /\d/.test(base[2]);
  const start = ordered ? parseInt(base[2], 10) : 1;
  const items: string[] = [];
  let cur: string[] | null = null;
  let sub = "";
  while (i < lines.length) {
    const l = lines[i];
    const m = LIST_RE.exec(l);
    if (m && m[1].length === indent && /\d/.test(m[2]) === ordered) {
      if (cur) items.push(inline(cur.join(" ")) + sub);
      cur = [m[3]];
      sub = "";
      i++;
    } else if (m && m[1].length > indent && cur) {
      const [h, n] = list(lines, i);
      sub += h;
      i = n;
    } else if (cur && l.trim() && /^\s{2,}/.test(l) && !m) {
      cur.push(l.trim());
      i++;
    } else break;
  }
  if (cur) items.push(inline(cur.join(" ")) + sub);
  const tag = ordered ? "ol" : "ul";
  return [`<${tag}${ordered && start !== 1 ? ` start="${start}"` : ""}>${items.map((x) => `<li>${x}</li>`).join("")}</${tag}>`, i];
}

/** Markdown to safe HTML: headings, paragraphs, emphasis, code, fenced code, lists, GFM tables, http(s) links. */
export function mdToHtml(src: string): string {
  const lines = src.replace(/\r\n?/g, "\n").split("\n");
  const out: string[] = [];
  let para: string[] = [];
  const flush = () => {
    if (para.length) out.push(`<p>${para.map(inline).join("<br>")}</p>`);
    para = [];
  };
  let i = 0;
  while (i < lines.length) {
    const l = lines[i];
    const fence = /^\s*(```+|~~~+)\s*([\w+-]*)/.exec(l);
    if (fence) {
      flush();
      const close = fence[1];
      const body: string[] = [];
      i++;
      while (i < lines.length && !lines[i].trim().startsWith(close)) body.push(lines[i++]);
      i++;
      out.push(`<div class="mdcode-w"><div class="mdcode-h"><span class="mdlang">${esc(fence[2] || "")}</span><button type="button" class="wcpy mdfill" title="Put this in the message box; it is not sent">Insert</button><button type="button" class="wcpy mdcpy" aria-label="Copy code" title="Copy code">Copy</button></div><pre class="mdcode"${fence[2] ? ` data-lang="${esc(fence[2])}"` : ""}><code>${esc(body.join("\n"))}</code></pre></div>`);
      continue;
    }
    if (!l.trim()) {
      flush();
      i++;
      continue;
    }
    const h = /^\s{0,3}(#{1,6})\s+(.*?)\s*#*\s*$/.exec(l);
    if (h) {
      flush();
      const n = Math.min(h[1].length + 2, 6);
      out.push(`<h${n} class="mdh">${inline(h[2])}</h${n}>`);
      i++;
      continue;
    }
    if (/^\s{0,3}([-*_])(\s*\1){2,}\s*$/.test(l)) {
      flush();
      out.push("<hr>");
      i++;
      continue;
    }
    if (l.includes("|") && i + 1 < lines.length && isTableSep(lines[i + 1])) {
      flush();
      const head = cells(l);
      const align = cells(lines[i + 1]).map((c) => (/^:-+:$/.test(c) ? "center" : /-:$/.test(c) ? "right" : ""));
      i += 2;
      const rows: string[][] = [];
      while (i < lines.length && lines[i].includes("|") && lines[i].trim()) rows.push(cells(lines[i++]));
      const td = (c: string, j: number, tag: string) => `<${tag}${align[j] ? ` style="text-align:${align[j]}"` : ""}>${inline(c)}</${tag}>`;
      out.push(
        `<div class="mdtable"><table><thead><tr>${head.map((c, j) => td(c, j, "th")).join("")}</tr></thead><tbody>${rows
          .map((r) => `<tr>${head.map((_, j) => td(r[j] || "", j, "td")).join("")}</tr>`)
          .join("")}</tbody></table></div>`,
      );
      continue;
    }
    if (LIST_RE.test(l)) {
      flush();
      const [html, n] = list(lines, i);
      out.push(html);
      i = n;
      continue;
    }
    if (/^\s*>/.test(l)) {
      flush();
      const q: string[] = [];
      while (i < lines.length && /^\s*>/.test(lines[i])) q.push(lines[i++].replace(/^\s*>\s?/, ""));
      out.push(`<blockquote>${mdToHtml(q.join("\n"))}</blockquote>`);
      continue;
    }
    para.push(l.trim());
    i++;
  }
  flush();
  return out.join("");
}

// Parse at most this much per render; the rest waits behind "Show more".
const PARSE_CAP = 200_000;
const THROTTLE_MS = 150;

/** The latest value, updated at most once per `ms` while it keeps changing (streaming text). */
function useThrottled(v: string, ms: number): string {
  const [out, setOut] = useState(v);
  const last = useRef(0);
  useEffect(() => {
    const wait = last.current + ms - Date.now();
    if (wait <= 0) {
      last.current = Date.now();
      setOut(v);
      return;
    }
    const t = window.setTimeout(() => {
      last.current = Date.now();
      setOut(v);
    }, wait);
    return () => window.clearTimeout(t);
  }, [v, ms]);
  return out;
}

/** The chat's input box: Insert passes the block's raw text and fence language (see lib/insert insertFor). */
export const FillCtx = createContext<((raw: string, lang: string) => void) | null>(null);

/** A click on a code block's Copy button copies the block, without shell prompts. */
async function onCodeCopy(e: React.MouseEvent, fill: ((code: string, lang: string) => void) | null) {
  const f = (e.target as HTMLElement).closest?.("button.mdfill");
  const fpre = f?.closest(".mdcode-w")?.querySelector("pre.mdcode");
  if (f && fpre && fill) {
    e.stopPropagation();
    fill(fpre.textContent || "", fpre.getAttribute("data-lang") || "");
    return;
  }
  const b = (e.target as HTMLElement).closest?.("button.mdcpy");
  const pre = b?.closest(".mdcode-w")?.querySelector("pre.mdcode");
  if (!b || !pre) return;
  e.stopPropagation();
  if (!(await copyWithToast(copyableCode(pre.textContent || "", pre.getAttribute("data-lang") || "")))) return;
  b.textContent = "Copied";
  b.classList.add("on");
  window.setTimeout(() => {
    b.textContent = "Copy";
    b.classList.remove("on");
  }, 1400);
}

/** Renders agent prose. Replies over `collapseAt` lines show the start and a "Show all" button. */
export function Markdown({ text: live, collapseAt = 30, className = "" }: { text: string; collapseAt?: number; className?: string }) {
  const fill = useContext(FillCtx);
  const text = useThrottled(live || "", THROTTLE_MS);
  const [all, setAll] = useState(false);
  const [cap, setCap] = useState(PARSE_CAP);
  const lines = useMemo(() => text.split("\n"), [text]);
  const long = collapseAt > 0 && lines.length > collapseAt;
  const src = long && !all ? lines.slice(0, collapseAt).join("\n") : text;
  const cut = src.length > cap;
  const html = useMemo(() => mdToHtml(cut ? src.slice(0, cap) : src), [src, cut, cap]);
  return (
    <div className={`md ${fill ? "can-fill " : ""}${className}`}>
      <div dangerouslySetInnerHTML={{ __html: html }} onClick={(e) => void onCodeCopy(e, fill)} />
      {cut && (
        <button className="btn sm ghost mdmore" onClick={() => setCap(cap + PARSE_CAP)}>
          Show more · {Math.ceil((src.length - cap) / 1024)} KB not shown
        </button>
      )}
      {long && !cut && (
        <button className="btn sm ghost mdmore" onClick={() => setAll(!all)}>
          {all ? "Show less" : `Show all · ${lines.length - collapseAt} more lines`}
        </button>
      )}
    </div>
  );
}
