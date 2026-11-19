// Markdown to Slack mrkdwn, for pasting an agent reply into Slack.

const FENCE = /^\s*(`{3,}|~{3,})/;
const TABLE_SEP = /^\s*\|?\s*:?-{2,}:?\s*(\|\s*:?-{2,}:?\s*)*\|?\s*$/;
const cells = (l: string) =>
  l
    .trim()
    .replace(/^\|/, "")
    .replace(/\|$/, "")
    .replace(/\\\|/g, "\u0002")
    .split("|")
    .map((c) => c.trim().replace(/\u0002/g, "|"));

/** Inline spans: code stays, links become text (url) since Slack's box does not parse <url|text> on paste, ** and __ become *, single * becomes _. */
export function slackInline(raw: string): string {
  const keep: string[] = [];
  const hold = (s: string) => `\u0000${keep.push(s) - 1}\u0000`;
  let s = raw.replace(/(`+)([^`]|[^`][\s\S]*?[^`])\1(?!`)/g, (m) => hold(m));
  s = s.replace(/!?\[([^\]]*)\]\(<?([^\s)>]+)>?(?:\s+"[^"]*")?\)/g, (_m, t: string, u: string) => hold(t && t !== u ? `${t} (${u})` : u));
  s = s.replace(/<(https?:\/\/[^\s>]+)>/g, (_m, u: string) => hold(u));
  s = s
    .replace(/\*\*(?=\S)([\s\S]*?\S)\*\*/g, "\u0001$1\u0001")
    .replace(/(^|[^\w])__(?=\S)([\s\S]*?\S)__(?!\w)/g, "$1\u0001$2\u0001")
    .replace(/(^|[\s(])\*(?=[^\s*])([^*\n]*?[^\s*])\*(?![\w*])/g, "$1_$2_")
    .replace(/~~(?=\S)([\s\S]*?\S)~~/g, "~$1~")
    .replace(/\u0001/g, "*");
  return s.replace(/\u0000(\d+)\u0000/g, (_m, i: string) => keep[Number(i)]);
}

const strip = (s: string) => s.replace(/\*\*|__/g, "").replace(/`/g, "");

function tableBlock(rows: string[][]): string {
  const n = Math.max(...rows.map((r) => r.length));
  const w = Array.from({ length: n }, (_, i) => Math.max(...rows.map((r) => (r[i] || "").length)));
  const line = (r: string[]) => w.map((x, i) => (r[i] || "").padEnd(x)).join("  ").trimEnd();
  const out = [line(rows[0]), w.map((x) => "-".repeat(x)).join("  "), ...rows.slice(1).map(line)];
  return "```\n" + out.join("\n") + "\n```";
}

/** Converts one Markdown document to Slack mrkdwn. */
export function toSlack(md: string): string {
  const src = md.replace(/\r\n?/g, "\n").split("\n");
  const out: string[] = [];
  for (let i = 0; i < src.length; i++) {
    const l = src[i];
    const f = FENCE.exec(l);
    if (f) {
      const body: string[] = [];
      let j = i + 1;
      while (j < src.length && !src[j].trim().startsWith(f[1])) body.push(src[j++]);
      out.push("```\n" + body.join("\n") + "\n```");
      i = j;
      continue;
    }
    if (l.includes("|") && i + 1 < src.length && TABLE_SEP.test(src[i + 1]) && src[i + 1].includes("-")) {
      const rows = [cells(l).map(strip)];
      let j = i + 2;
      while (j < src.length && src[j].includes("|") && src[j].trim()) rows.push(cells(src[j++]).map(strip));
      out.push(tableBlock(rows));
      i = j - 1;
      continue;
    }
    const h = /^\s{0,3}#{1,6}\s+(.*?)\s*#*\s*$/.exec(l);
    if (h) {
      out.push(`*${slackInline(strip(h[1]))}*`);
      continue;
    }
    if (/^\s{0,3}([-*_])(\s*\1){2,}\s*$/.test(l)) {
      out.push("──────────");
      continue;
    }
    const li = /^(\s*)([-*+])\s+(?:\[([ xX])\]\s+)?(.*)$/.exec(l);
    if (li) {
      const pad = " ".repeat(Math.floor(li[1].replace(/\t/g, "  ").length / 2) * 4);
      const box = li[3] ? (li[3] === " " ? "☐ " : "☑ ") : "";
      out.push(`${pad}• ${box}${slackInline(li[4])}`);
      continue;
    }
    const ol = /^(\s*)(\d+)[.)]\s+(.*)$/.exec(l);
    if (ol) {
      const pad = " ".repeat(Math.floor(ol[1].length / 2) * 4);
      out.push(`${pad}${ol[2]}. ${slackInline(ol[3])}`);
      continue;
    }
    const q = /^\s*>\s?(.*)$/.exec(l);
    if (q) {
      out.push(`> ${slackInline(q[1])}`.trimEnd());
      continue;
    }
    out.push(slackInline(l));
  }
  return out.join("\n").replace(/\n{3,}/g, "\n\n").trim();
}

/** Markdown to plain text: the Slack conversion without its markers, fences or backticks. */
export function toPlain(md: string): string {
  const out: string[] = [];
  let inFence = false;
  for (const line of toSlack(md).split("\n")) {
    if (/^```/.test(line)) {
      inFence = !inFence;
      continue;
    }
    if (inFence) {
      out.push(line);
      continue;
    }
    const keep: string[] = [];
    let s = line.replace(/`([^`]+)`/g, (_m, c: string) => `\u0000${keep.push(c) - 1}\u0000`);
    s = s
      .replace(/(^|[\s(>•])\*(?=\S)([^*\n]*?\S)\*(?=[\s).,;:!?]|$)/g, "$1$2")
      .replace(/(^|[\s(>•])_(?=\S)([^_\n]*?\S)_(?=[\s).,;:!?]|$)/g, "$1$2")
      .replace(/(^|[\s(>•])~(?=\S)([^~\n]*?\S)~(?=[\s).,;:!?]|$)/g, "$1$2")
      .replace(/^> ?/, "");
    out.push(s.replace(/\u0000(\d+)\u0000/g, (_m, i: string) => keep[Number(i)]));
  }
  return out.join("\n").trim();
}
