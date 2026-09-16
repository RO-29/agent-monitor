// Regression guard for the hand-written markdown renderer (src/lib/markdown.tsx).
// It parses tool output and fetched pages, so the escaping cases matter.
// Run by build.sh; needs no test framework, only the esbuild vite already pulls in.
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
  ["url keeps balanced paren", "https://en.wikipedia.org/wiki/Foo_(bar) end", ['href="https://en.wikipedia.org/wiki/Foo_(bar)"'], []],
  ["url drops trailing dot", "https://example.com/a. Next", ['href="https://example.com/a"'], ['href="https://example.com/a."']],
  ["javascript: is inert", "[click](javascript:alert(1)) after", ["click after"], ["href", "javascript:"]],
  ["data: is inert", "[click](data:text/html,<svg>) after", [], ["href", "data:"]],
  ["vbscript: is inert", "[click](vbscript:msgbox(1))", [], ["href"]],
  ["raw html is escaped", "<img src=x onerror=alert(1)>", ["&lt;img"], ["<img"]],
  ["script tag is escaped", "<script>alert(1)</script>", ["&lt;script&gt;"], ["<script>"]],
  ["fence", "```bash\nls -la\n```", ["<pre", "ls -la", "bash"], []],
  ["list", "- one\n- two", ["<ul>", "<li>one</li>"], []],
  ["ordered list", "1. one\n2. two", ["<ol>", "<li>one</li>"], []],
  ["nested list", "- one\n  - deep", ["<ul><li>one<ul><li>deep</li>"], []],
  ["table", "| a | b |\n|---|---|\n| 1 | 2 |", ["<table>", "<th>a</th>", "<td>1</td>"], []],
  ["heading", "## Findings", ["<h4>Findings</h4>"], []],
  ["blockquote", "> quoted", ["<blockquote>"], []],
  ["hr", "a\n\n---\n\nb", ["<hr/>"], []],
  ["strike", "~~gone~~", ["<s>gone</s>"], []],
  ["newline kept", "one\ntwo", ["<br/>"], []],
  ["empty is safe", "", [], ["undefined", "null"]],
];

const dir = mkdtempSync(join(tmpdir(), "mdcheck-"));
const entry = join(process.cwd(), "src", `__mdcheck-${process.pid}.tsx`);
const out = join(dir, "check.cjs");
writeFileSync(
  entry,
  `import { renderToStaticMarkup } from "react-dom/server";
import { Markdown } from "./lib/markdown";
const CASES = ${JSON.stringify(CASES)};
let bad = 0;
for (const [name, src, must, mustNot] of CASES) {
  const html = renderToStaticMarkup(<Markdown text={src} />);
  for (const m of must) if (!html.includes(m)) { console.error("markdown: " + name + " — missing " + JSON.stringify(m) + "\\n  got: " + html); bad++; }
  for (const m of mustNot) if (html.includes(m)) { console.error("markdown: " + name + " — must not contain " + JSON.stringify(m) + "\\n  got: " + html); bad++; }
}
if (bad) { console.error(bad + " markdown check(s) failed"); process.exit(1); }
console.log("markdown: " + CASES.length + " checks ok");
`,
);
try {
  execFileSync("node_modules/.bin/esbuild", [entry, "--bundle", "--platform=node", "--format=cjs", "--jsx=automatic", "--outfile=" + out, "--log-level=error"], { stdio: "inherit" });
  execFileSync("node", [out], { stdio: "inherit" });
} finally {
  rmSync(entry, { force: true });
  rmSync(dir, { recursive: true, force: true });
}
