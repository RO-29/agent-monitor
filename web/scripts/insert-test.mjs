// Runs testdata/insert_cases.json against src/lib/insert.ts: npm run test:insert
import { readFileSync } from "node:fs";
import { insertFor } from "../src/lib/insert.ts";

const cases = JSON.parse(readFileSync(new URL("../../testdata/insert_cases.json", import.meta.url), "utf8"));
let bad = 0;
for (const c of cases) {
  const got = insertFor(c.raw, c.lang, c.bash, c.cur);
  if (got !== c.want) {
    bad++;
    console.log(`FAIL ${c.name}\n  got  ${JSON.stringify(got)}\n  want ${JSON.stringify(c.want)}`);
  }
}
console.log(bad ? `${bad} of ${cases.length} failed` : `all ${cases.length} insert cases passed`);
process.exit(bad ? 1 : 0);
