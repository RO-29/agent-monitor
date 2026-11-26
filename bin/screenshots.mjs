// Captures the web app with its sample data (?mock=1) in headless Chrome, for the site and README.
// Usage: node bin/screenshots.mjs <base url> <out dir>. bin/screenshots.sh runs it on the build Mac.
import { spawn } from "node:child_process";
import { mkdirSync, writeFileSync } from "node:fs";

const [base = "http://127.0.0.1:5199", out = "/tmp/am-shots"] = process.argv.slice(2);
const CHROME = "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome";
const SHOTS = [
  { name: "workspace", path: "/w/cmt" },
  { name: "approval", path: "/w/tok" },
  { name: "error", path: "/w/bad" },
  { name: "desk", path: "/desk" },
];
const SIZES = [
  { tag: "desktop", width: 1440, height: 900, mobile: false },
  { tag: "phone", width: 390, height: 844, mobile: true },
];
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

mkdirSync(out, { recursive: true });
const port = 9334;
const chrome = spawn(CHROME, ["--headless=new", "--disable-gpu", `--remote-debugging-port=${port}`, `--user-data-dir=/tmp/am-shots-profile-${Date.now()}`, "about:blank"], { stdio: "ignore" });
let tabs = [];
for (let i = 0; i < 50 && !tabs.length; i++) {
  try {
    tabs = (await (await fetch(`http://127.0.0.1:${port}/json`)).json()).filter((t) => t.type === "page");
  } catch {}
  await sleep(200);
}
const ws = new WebSocket(tabs[0].webSocketDebuggerUrl);
await new Promise((r) => (ws.onopen = r));
let seq = 0;
const wait = new Map();
ws.onmessage = (m) => {
  const d = JSON.parse(m.data);
  if (d.id && wait.has(d.id)) wait.get(d.id)(d.result), wait.delete(d.id);
};
const send = (method, params = {}) => new Promise((r) => { const i = ++seq; wait.set(i, r); ws.send(JSON.stringify({ id: i, method, params })); });

let n = 0;
try {
  for (const theme of ["dark", "light"]) {
    await send("Emulation.setEmulatedMedia", { features: [{ name: "prefers-color-scheme", value: theme }] });
    for (const size of SIZES) {
      await send("Emulation.setDeviceMetricsOverride", { width: size.width, height: size.height, deviceScaleFactor: 2, mobile: size.mobile });
      for (const s of SHOTS) {
        await send("Page.navigate", { url: `${base}${s.path}?mock=1` });
        await sleep(2500); // the sample data loads as a separate chunk
        const shot = await send("Page.captureScreenshot", { format: "png" });
        const file = `${out}/${s.name}-${size.tag}-${theme}.png`;
        writeFileSync(file, Buffer.from(shot.data, "base64"));
        n++;
      }
    }
  }
} finally {
  ws.close();
  chrome.kill();
}
console.log(`${n} screenshots in ${out}`);
