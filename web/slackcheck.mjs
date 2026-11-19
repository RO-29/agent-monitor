// Regression check for the Slack converter (src/lib/slack.ts) and the prompt export (src/lib/chatExport.ts).
import { execFileSync } from "node:child_process";
import { mkdtempSync, writeFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

const CASES = [
  // [name, input, expected output]
  ["bold", "a **live** b", "a *live* b"],
  ["bold underscores", "a __live__ b", "a *live* b"],
  ["italic star", "an *emphasis* here", "an _emphasis_ here"],
  ["italic underscore stays", "an _emphasis_ here", "an _emphasis_ here"],
  ["bold and italic", "**a** and *b*", "*a* and _b_"],
  ["strike", "~~gone~~", "~gone~"],
  ["heading", "## Findings", "*Findings*"],
  ["heading with bold", "### **Root** cause", "*Root cause*"],
  ["link", "see [the PR](https://example.com/p/1) now", "see the PR (https://example.com/p/1) now"],
  ["link same text", "[https://x.io](https://x.io)", "https://x.io"],
  ["inline code kept", "run `a **b** c` now", "run `a **b** c` now"],
  ["snake_case survives", "mcp__clickstack__sql and a_b_c", "mcp__clickstack__sql and a_b_c"],
  ["bullets", "- one\n* two\n+ three", "• one\n• two\n• three"],
  ["nested bullet", "- one\n  - deep", "• one\n    • deep"],
  ["task list", "- [ ] todo\n- [x] done", "• ☐ todo\n• ☑ done"],
  ["ordered", "1. one\n2) two", "1. one\n2. two"],
  ["fence kept, language dropped", "```ts\nconst a = **1**;\n```", "```\nconst a = **1**;\n```"],
  ["tilde fence", "~~~\nx\n~~~", "```\nx\n```"],
  ["quote", "> **note** here", "> *note* here"],
  ["hr", "a\n\n---\n\nb", "a\n\n──────────\n\nb"],
  ["table to code block", "| a | bb |\n|---|---|\n| **1** | 2 |", "```\na  bb\n-  --\n1  2\n```"],
  ["blank runs collapse", "a\n\n\n\nb", "a\n\nb"],
  ["glob survives", "src/**/*.ts", "src/**/*.ts"],
];

const dir = mkdtempSync(join(tmpdir(), "slackcheck-"));
const entry = join(process.cwd(), "src", `__slackcheck-${process.pid}.ts`);
const out = join(dir, "check.cjs");
writeFileSync(
  entry,
  `import { toSlack } from "./lib/slack";
import { exportMarkdown, exportSlack, exportFileName } from "./lib/chatExport";
let bad = 0;
const eq = (name: string, got: string, want: string) => {
  if (got !== want) { console.error(name + "\\n  want: " + JSON.stringify(want) + "\\n  got:  " + JSON.stringify(got)); bad++; }
};
const has = (name: string, got: string, want: string, yes = true) => {
  if (got.includes(want) !== yes) { console.error(name + (yes ? ", missing " : ", must not contain ") + JSON.stringify(want) + "\\n  got: " + JSON.stringify(got)); bad++; }
};
const CASES: [string, string, string][] = ${JSON.stringify(CASES)};
for (const [n, i, o] of CASES) eq(n, toSlack(i), o);
const e = [{ prompt: "fix the **bug**\\nnow", reply: "Done. See [PR](https://x.io/1).", at: Date.UTC(2026, 10, 17, 9, 30) }, { prompt: "ship it" }];
const md = exportMarkdown("My thread", e, true, Date.UTC(2026, 10, 17));
has("export title", md, "# My thread\\n");
has("export quote", md, "> fix the **bug**\\n> now");
has("export reply", md, "Done. See [PR](https://x.io/1).");
has("export count", md, "2 prompts.");
has("export no reply", exportMarkdown("t", e, false), "Done.", false);
const sl = exportSlack("My thread", e, true, Date.UTC(2026, 10, 17));
has("slack export heading", sl, "*My thread*");
has("slack export link", sl, "PR (https://x.io/1)");
has("slack export quote", sl, "> fix the *bug*");
eq("file name", exportFileName("Fix: the /t/ page!", Date.UTC(2026, 10, 17)), "fix-the-t-page-2026-11-17.md");
if (bad) { console.error(bad + " slack check(s) failed"); process.exit(1); }
console.log("slack: " + (CASES.length + 10) + " checks ok");
`,
);
try {
  execFileSync("node_modules/.bin/esbuild", [entry, "--bundle", "--platform=node", "--format=cjs", "--outfile=" + out, "--log-level=error"], { stdio: "inherit" });
  execFileSync("node", [out], { stdio: "inherit" });
} finally {
  rmSync(entry, { force: true });
  rmSync(dir, { recursive: true, force: true });
}
