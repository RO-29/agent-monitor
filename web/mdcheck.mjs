// Regression check for both markdown renderers (v3/components/Markdown.tsx and lib/markdown.tsx).
// An array in "must" accepts any one item. Needs only the esbuild that vite pulls in.
import { execFileSync } from "node:child_process";
import { mkdtempSync, writeFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

const CASES = [
  // [name, input, must contain, must NOT contain]
  ["glob survives", "src/**/*.ts", ["src/**/*.ts"], ["<em>"]],
  ["snake_case survives", "mcp__clickstack__clickstack_sql", ["mcp__clickstack__clickstack_sql"], ["<em>", "<strong>"]],
  ["intraword underscore", "a_b_c", ["a_b_c"], ["<em>"]],
  ["bold", "**live now**", ["<strong>live now</strong>"], []],
  ["italic", "an *emphasis* here", ["<em>emphasis</em>"], []],
  ["inline code", "runs `9127f42c`", ["<code>9127f42c</code>"], []],
  ["md link", "[PR](https://example.com/p/1)", ['href="https://example.com/p/1"', 'rel="noreferrer noopener"'], []],
  ["bare url", "see https://example.com/x?a=1 now", ['href="https://example.com/x?a=1"'], []],
  ["url drops trailing dot", "https://example.com/a. Next", ['href="https://example.com/a"'], ['href="https://example.com/a."']],
  ["raw html is escaped", "<img src=x onerror=alert(1)>", ["&lt;img"], ["<img"]],
  ["script tag is escaped", "<script>alert(1)</script>", ["&lt;script&gt;"], ["<script>"]],
  ["fence", "```bash\nls -la\n```", ["<pre", "ls -la", "bash"], []],
  ["list", "- one\n- two", ["<ul>", "<li>one</li>"], []],
  ["ordered list", "1. one\n2. two", ["<ol>", "<li>one</li>"], []],
  ["nested list", "- one\n  - deep", ["<ul><li>one<ul><li>deep</li>"], []],
  ["table", "| a | b |\n|---|---|\n| 1 | 2 |", ["<table>", "<th>a</th>", "<td>1</td>"], []],
  ["heading", "## Findings", [["<h4>Findings</h4>", '<h4 class="mdh">Findings</h4>']], []],
  ["blockquote", "> quoted", ["<blockquote>"], []],
  ["hr", "a\n\n---\n\nb", [["<hr/>", "<hr>"]], []],
  ["strike", "~~gone~~", [["<s>gone</s>", "<del>gone</del>"]], []],
  ["newline kept", "one\ntwo", [["<br/>", "<br>"]], []],
  ["empty is safe", "", [], ["undefined", "null"]],
  ["code in link target stays quoted", "[a](https://x.example/`b`)", [], ['<code>b</code>"']],
];
// The classic renderer links balanced parens and drops unsafe link text; the Workspace one keeps unsafe targets as inert text.
const CLASSIC_CASES = [
  ["url keeps balanced paren", "https://en.wikipedia.org/wiki/Foo_(bar) end", ['href="https://en.wikipedia.org/wiki/Foo_(bar)"'], []],
  ["javascript: is inert", "[click](javascript:alert(1)) after", ["click after"], ["href", "javascript:"]],
  ["data: is inert", "[click](data:text/html,<svg>) after", [], ["href", "data:"]],
  ["vbscript: is inert", "[click](vbscript:msgbox(1))", [], ["href"]],
];
// Only the Workspace renderer: the real target is visible next to link text that is not the URL.
const V3_CASES = [
  ["link host shown", "[https://github.com/login](https://evil.example/login)", ['title="https://evil.example/login"', "(evil.example)"], []],
  ["bidi override neutralised in link text", "[a\u202Eb](https://x.example/)", ["a\uFFFDb"], ["\u202E"]],
  ["javascript: never becomes a link", "[click](javascript:alert(1)) after", [], ['href="javascript', "<a "]],
  ["data: never becomes a link", "[click](data:text/html,<svg>) after", [], ['href="data', "<a ", "<svg"]],
  ["parse cap", "x".repeat(250000), ["KB not shown"], []],
];

const dir = mkdtempSync(join(tmpdir(), "mdcheck-"));
const entry = join(process.cwd(), "src", `__mdcheck-${process.pid}.tsx`);
const out = join(dir, "check.cjs");
writeFileSync(
  entry,
  `import { renderToStaticMarkup } from "react-dom/server";
import { Markdown as Classic } from "./lib/markdown";
import { Markdown as Workspace } from "./v3/components/Markdown";
let bad = 0;
const run = (label, R, cases) => {
  for (const [name, src, must, mustNot] of cases) {
    const html = renderToStaticMarkup(<R text={src} />);
    for (const m of must) {
      const any = Array.isArray(m) ? m : [m];
      if (!any.some((x) => html.includes(x))) { console.error(label + ": " + name + ", missing " + JSON.stringify(m) + "\\n  got: " + html.slice(0, 400)); bad++; }
    }
    for (const m of mustNot) if (html.includes(m)) { console.error(label + ": " + name + ", must not contain " + JSON.stringify(m) + "\\n  got: " + html.slice(0, 400)); bad++; }
  }
};
const CASES = ${JSON.stringify(CASES)};
run("lib/markdown", Classic, [...CASES, ...${JSON.stringify(CLASSIC_CASES)}]);
run("v3 Markdown", Workspace, [...CASES, ...${JSON.stringify(V3_CASES)}]);
if (bad) { console.error(bad + " markdown check(s) failed"); process.exit(1); }
console.log("markdown: " + (CASES.length * 2 + ${CLASSIC_CASES.length + V3_CASES.length}) + " checks ok on both renderers");
`,
);
try {
  execFileSync("node_modules/.bin/esbuild", [entry, "--bundle", "--platform=node", "--format=cjs", "--jsx=automatic", "--outfile=" + out, "--log-level=error"], { stdio: "inherit" });
  execFileSync("node", [out], { stdio: "inherit" });
} finally {
  rmSync(entry, { force: true });
  rmSync(dir, { recursive: true, force: true });
}
