import { Fragment, useMemo, type ReactNode } from "react";

// Markdown for agent prose. It renders to React nodes, never to HTML, because
// the text includes tool output and pages the agent fetched.
//
// Two rules keep it from mangling code-ish prose: a `_` only opens emphasis at
// a non-word boundary (so mcp__clickstack__sql survives) and a `*` only opens
// at whitespace (so src/**/*.ts survives).

const SAFE = /^(https?:|mailto:|#|\/)/i;
const safeHref = (u: string) => (SAFE.test(u.trim()) ? u.trim() : undefined);

function Link({ href, children }: { href: string; children: ReactNode }) {
  const h = safeHref(href);
  if (!h) return <>{children}</>;
  return (
    <a href={h} target="_blank" rel="noreferrer noopener">
      {children}
    </a>
  );
}

const wordish = (c: string | undefined) => !!c && /[\w]/.test(c);

/** Inline spans: code, links, images, bold, italic, strike, bare URLs. */
function inline(src: string): ReactNode[] {
  const out: ReactNode[] = [];
  let buf = "";
  let i = 0;
  const push = (n: ReactNode) => {
    if (buf) {
      out.push(buf);
      buf = "";
    }
    out.push(<Fragment key={out.length}>{n}</Fragment>);
  };
  while (i < src.length) {
    const rest = src.slice(i);
    const prev = i > 0 ? src[i - 1] : undefined;
    let m: RegExpExecArray | null;

    // `code` — first, so nothing inside it is parsed further
    if ((m = /^(`+)(?!`)([\s\S]*?)\1(?!`)/.exec(rest))) {
      push(<code>{m[2]}</code>);
      i += m[0].length;
      continue;
    }
    // ![alt](src) and [text](url)
    if ((m = /^(!?)\[([^\]]*)\]\(\s*<?((?:[^\s>()]|\([^()]*\))*)>?(?:\s+"[^"]*")?\s*\)/.exec(rest))) {
      const [, bang, label, url] = m;
      push(bang ? <Link href={url}>{label || url}</Link> : <Link href={url}>{inline(label)}</Link>);
      i += m[0].length;
      continue;
    }
    // <https://…>
    if ((m = /^<((?:https?:\/\/|mailto:)[^>\s]+)>/.exec(rest))) {
      push(<Link href={m[1]}>{m[1]}</Link>);
      i += m[0].length;
      continue;
    }
    // ~~strike~~
    if ((m = /^~~(?=\S)([\s\S]+?)~~/.exec(rest))) {
      push(<s>{inline(m[1])}</s>);
      i += m[0].length;
      continue;
    }
    // **bold** / __bold__
    if ((m = /^(\*\*|__)(?=\S)([\s\S]+?)\1/.exec(rest)) && opens(m[1], prev)) {
      push(<strong>{inline(m[2])}</strong>);
      i += m[0].length;
      continue;
    }
    // *italic* / _italic_
    if ((m = /^(\*|_)(?=\S)([^\n]+?)\1/.exec(rest)) && opens(m[1], prev)) {
      push(<em>{inline(m[2])}</em>);
      i += m[0].length;
      continue;
    }
    // bare URL, minus any trailing sentence punctuation
    if ((m = /^(https?:\/\/[^\s<>]+)/.exec(rest))) {
      let url = m[1];
      let trail = "";
      // sentence punctuation is not part of the link, but a balanced ")" is
      for (;;) {
        const last = url[url.length - 1];
        if (!last || !".,;:!?)]}'\"".includes(last)) break;
        if (last === ")" && (url.match(/\(/g) || []).length >= (url.match(/\)/g) || []).length) break;
        trail = last + trail;
        url = url.slice(0, -1);
      }
      push(<Link href={url}>{url}</Link>);
      buf += trail;
      i += m[1].length;
      continue;
    }
    buf += src[i];
    i++;
  }
  if (buf) out.push(buf);
  return out;
}

/** `_` never opens inside a word; `*` opens only after a real boundary, so
 *  neither the `/` nor the second `*` of src/**\/*.ts can start emphasis. */
function opens(delim: string, prev: string | undefined): boolean {
  if (delim === "_" || delim === "__") return !wordish(prev);
  return prev === undefined || /[\s([{<"']/.test(prev);
}

/** A paragraph keeps its single newlines: agents lay text out deliberately. */
function para(lines: string[], key: number) {
  const nodes: ReactNode[] = [];
  lines.forEach((l, k) => {
    if (k) nodes.push(<br key={`b${k}`} />);
    nodes.push(...inline(l));
  });
  return <p key={key}>{nodes}</p>;
}

const RE_FENCE = /^\s{0,3}(```+|~~~+)\s*([\w+-]*)/;
const RE_HEAD = /^\s{0,3}(#{1,6})\s+(.*)$/;
const RE_HR = /^\s{0,3}([-*_])(\s*\1){2,}\s*$/;
const RE_QUOTE = /^\s{0,3}>\s?(.*)$/;
const RE_ITEM = /^(\s*)([-*+]|\d{1,9}[.)])\s+(.*)$/;
const RE_TSEP = /^\s*\|?[\s:-]*-[\s|:-]*\|?\s*$/;

function blocks(src: string): ReactNode[] {
  const lines = src.replace(/\r\n?/g, "\n").split("\n");
  const out: ReactNode[] = [];
  let i = 0;
  while (i < lines.length) {
    const ln = lines[i];
    if (!ln.trim()) {
      i++;
      continue;
    }
    let m: RegExpExecArray | null;

    if ((m = RE_FENCE.exec(ln))) {
      const close = m[1][0].repeat(3);
      const lang = m[2];
      const body: string[] = [];
      i++;
      while (i < lines.length && !lines[i].trimStart().startsWith(close)) body.push(lines[i++]);
      i++;
      out.push(
        <pre key={out.length} className="md-pre">
          {lang && <span className="lang">{lang}</span>}
          <code>{body.join("\n")}</code>
        </pre>,
      );
      continue;
    }
    if (RE_HR.test(ln)) {
      out.push(<hr key={out.length} />);
      i++;
      continue;
    }
    if ((m = RE_HEAD.exec(ln))) {
      const H = `h${Math.min(m[1].length + 2, 6)}` as "h3";
      out.push(<H key={out.length}>{inline(m[2])}</H>);
      i++;
      continue;
    }
    if (RE_QUOTE.test(ln)) {
      const body: string[] = [];
      while (i < lines.length && (m = RE_QUOTE.exec(lines[i]))) {
        body.push(m[1]);
        i++;
      }
      out.push(<blockquote key={out.length}>{blocks(body.join("\n"))}</blockquote>);
      continue;
    }
    // table: a header row followed by a |---|---| separator
    if (ln.includes("|") && i + 1 < lines.length && RE_TSEP.test(lines[i + 1]) && lines[i + 1].includes("-")) {
      const cells = (r: string) => r.replace(/^\s*\|/, "").replace(/\|\s*$/, "").split("|").map((c) => c.trim());
      const head = cells(ln);
      i += 2;
      const rows: string[][] = [];
      while (i < lines.length && lines[i].includes("|") && lines[i].trim()) rows.push(cells(lines[i++]));
      out.push(
        <div key={out.length} className="md-tablewrap">
          <table>
            <thead>
              <tr>{head.map((c, k) => <th key={k}>{inline(c)}</th>)}</tr>
            </thead>
            <tbody>
              {rows.map((r, k) => (
                <tr key={k}>{head.map((_, j) => <td key={j}>{inline(r[j] ?? "")}</td>)}</tr>
              ))}
            </tbody>
          </table>
        </div>,
      );
      continue;
    }
    if (RE_ITEM.test(ln)) {
      const [list, next] = parseList(lines, i);
      out.push(<div key={out.length}>{list}</div>);
      i = next;
      continue;
    }
    const body: string[] = [];
    while (i < lines.length && lines[i].trim() && !RE_FENCE.test(lines[i]) && !RE_HEAD.test(lines[i]) && !RE_QUOTE.test(lines[i]) && !RE_ITEM.test(lines[i]) && !RE_HR.test(lines[i])) {
      body.push(lines[i++]);
    }
    out.push(para(body, out.length));
  }
  return out;
}

/** One list level; deeper indents recurse. Returns the list and the next line. */
function parseList(lines: string[], start: number): [ReactNode, number] {
  const first = RE_ITEM.exec(lines[start])!;
  const baseIndent = first[1].length;
  const ordered = /\d/.test(first[2]);
  const items: ReactNode[] = [];
  let i = start;
  while (i < lines.length) {
    const m = RE_ITEM.exec(lines[i]);
    if (!m || m[1].length < baseIndent) break;
    if (m[1].length > baseIndent) {
      const [sub, next] = parseList(lines, i);
      items.push(<li key={items.length} className="sub">{sub}</li>);
      i = next;
      continue;
    }
    if (/\d/.test(m[2]) !== ordered) break;
    const body = [m[3]];
    i++;
    // continuation lines: indented further, and not a new item
    while (i < lines.length && lines[i].trim() && !RE_ITEM.test(lines[i]) && /^\s{2,}/.test(lines[i])) body.push(lines[i++].trim());
    // a nested list right under this item
    let nested: ReactNode = null;
    if (i < lines.length) {
      const n = RE_ITEM.exec(lines[i]);
      if (n && n[1].length > baseIndent) {
        const [sub, next] = parseList(lines, i);
        nested = sub;
        i = next;
      }
    }
    items.push(
      <li key={items.length}>
        {inline(body.join(" "))}
        {nested}
      </li>,
    );
  }
  const L = ordered ? "ol" : "ul";
  return [<L key="l">{items}</L>, i];
}

/** Renders agent prose. `inlineOnly` skips block structure for one-line slots. */
export function Markdown({ text, className = "", inlineOnly = false }: { text: string; className?: string; inlineOnly?: boolean }) {
  const nodes = useMemo(() => (inlineOnly ? inline(text || "") : blocks(text || "")), [text, inlineOnly]);
  const Tag = inlineOnly ? "span" : "div";
  return <Tag className={`md ${inlineOnly ? "inline" : ""} ${className}`}>{nodes}</Tag>;
}

/** Markdown stripped to plain text, for titles and one-line previews. */
export function plainText(src: string): string {
  return (src || "")
    .replace(/```[\s\S]*?```/g, " ")
    .replace(/`([^`]*)`/g, "$1")
    .replace(/!?\[([^\]]*)\]\([^)]*\)/g, "$1")
    .replace(/^\s{0,3}#{1,6}\s+/gm, "")
    .replace(/^\s{0,3}>\s?/gm, "")
    .replace(/(\*\*|__)(\S[\s\S]*?)\1/g, "$2")
    .replace(/\s+/g, " ")
    .trim();
}
