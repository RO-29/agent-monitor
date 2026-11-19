// Copy and Insert text for code blocks; ios/AgentMonitor/Insert.swift is the same, both checked by testdata/insert_cases.json.

const SHELL = /^(sh|bash|zsh|fish|shell|console|terminal|shellsession|shell-session)$/i;
const PROMPT = /^\s*[$!] /;

/** The text to paste into a shell: a console transcript keeps only its commands, and "$ "/"! " prompts come off. */
export function copyableCode(code: string, lang = ""): string {
  const lines = code.replace(/\s+$/, "").split("\n");
  if (lang && !SHELL.test(lang)) return lines.join("\n");
  const prompted = lines.filter((l) => PROMPT.test(l));
  if (!prompted.length) return lines.join("\n");
  const mixed = prompted.length < lines.filter((l) => l.trim()).length;
  return (mixed ? prompted : lines).map((l) => l.replace(PROMPT, "")).join("\n");
}

/** A block with no language that starts with "! " or "$ " is shell. */
export const blockLang = (raw: string, lang: string) => lang || (PROMPT.test(raw) ? "sh" : "");

/** Box text for Insert: a one-line shell command becomes "!cmd" where the box has bash mode; longer blocks stay as they are. */
export function fillText(code: string, lang: string, bash: boolean): string {
  const c = code.trim();
  if (!bash || !SHELL.test(lang)) return c;
  const one = c.replace(/\s*\\\n\s*/g, " ");
  return one.includes("\n") ? c : "!" + one;
}

/** Insert adds to a draft; a "!" command only counts at the start of the box. */
export function withInsert(cur: string, add: string): string {
  const head = cur.replace(/\s+$/, "");
  if (!head) return add;
  return `${head}\n${add.startsWith("!") && !cur.startsWith("!") ? add.slice(1) : add}`;
}

/** The whole Insert step: raw block text and its fence language to the new box text. */
export function insertFor(raw: string, lang: string, bash: boolean, cur: string): string {
  const l = blockLang(raw, lang);
  return withInsert(cur, fillText(copyableCode(raw, l), l, bash));
}
