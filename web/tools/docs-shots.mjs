// Regenerates the docs page's screenshots (web/public/docs/*.webp) from the
// real app: it starts a throw-away tower from bin/airlift, makes a demo beam
// page, posts the frozen 24-chunk vector as frames so the dashboard shows a
// genuinely verified beam, and drives headless Chrome over the DevTools
// protocol (Node's built-in WebSocket — no npm dependency).
//
//   make docs-shots            (builds bin/airlift first)
//   CHROME=/path/to/chrome node web/tools/docs-shots.mjs
//
// Then rebuild (`make airlift`) so the new images are embedded and commit them.
import { spawn } from "node:child_process";
import { createServer } from "node:http";
import { existsSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const repo = resolve(dirname(fileURLToPath(import.meta.url)), "..", "..");
const bin = join(repo, "bin", "airlift");
const outDir = join(repo, "web", "public", "docs");
const vectors = JSON.parse(readFileSync(join(repo, "testdata", "vectors", "vectors.json"), "utf8"));
const CHROME =
  process.env.CHROME ??
  ["/Applications/Google Chrome.app/Contents/MacOS/Google Chrome", "/usr/bin/google-chrome", "/usr/bin/chromium", "/usr/bin/chromium-browser"].find(
    existsSync,
  );
if (!CHROME) throw new Error("no Chrome found: set CHROME=/path/to/chrome");
if (!existsSync(bin)) throw new Error("bin/airlift missing: run `make airlift` first");
mkdirSync(outDir, { recursive: true });

const TOWER_PORT = 8797;
const PLAYER_PORT = 8796;
const CDP_PORT = 9333;
const tower = `http://127.0.0.1:${TOWER_PORT}`;
const tmp = mkdtempSync(join(tmpdir(), "airlift-docs-"));
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const until = async (fn, ms = 10_000) => {
  const t = Date.now();
  for (;;) {
    try {
      if (await fn()) return;
    } catch {
      /* not yet */
    }
    if (Date.now() - t > ms) throw new Error("timeout waiting for a service");
    await sleep(150);
  }
};
const procs = [];
// Kill everything, wait for the processes to actually exit (Chrome keeps writing
// its profile for a moment), then remove the temp dir — with retries for the
// same reason.
const cleanup = async () => {
  await Promise.all(
    procs.map(
      (p) =>
        new Promise((res) => {
          if (typeof p.on !== "function") {
            p.kill();
            res();
            return;
          }
          if (p.exitCode !== null) {
            res();
            return;
          }
          p.once("exit", res);
          p.kill();
          setTimeout(res, 3000); // never hang on a stubborn process
        }),
    ),
  );
  rmSync(tmp, { recursive: true, force: true, maxRetries: 10, retryDelay: 200 });
};

try {
  // a throw-away tower
  procs.push(
    spawn(bin, ["tower", "--listen", `127.0.0.1:${TOWER_PORT}`, "--public_url", tower, "--admin_token", "docs"], {
      env: { ...process.env, AIRLIFT_HOME: join(tmp, "home") },
      stdio: "ignore",
    }),
  );
  await until(() => fetch(`${tower}/api/info`).then((r) => r.ok));

  // a demo beam page, served by a one-file HTTP server (a file:// page cannot be captured)
  const beamHTML = join(tmp, "demo.html");
  await new Promise((res, rej) => {
    const p = spawn(bin, ["beam", "--no-open", "--name", "docs-adr", "--out", beamHTML, join(repo, "docs", "adr")], { stdio: "ignore" });
    p.on("exit", (c) => (c === 0 ? res() : rej(new Error(`beam exited ${c}`))));
  });
  const player = createServer((_req, res) => {
    res.setHeader("content-type", "text/html; charset=utf-8");
    res.end(readFileSync(beamHTML));
  }).listen(PLAYER_PORT, "127.0.0.1");
  procs.push({ kill: () => player.close() });

  // a session holding a real, verified beam
  const created = await (await fetch(`${tower}/api/sessions`, { method: "POST", headers: { "content-type": "application/json" }, body: "{}" })).json();
  const { sid, token, client_id } = created;
  const auth = { authorization: `Bearer ${token}`, "x-airlift-client": client_id, "content-type": "application/json" };
  const ingest = await (
    await fetch(`${tower}/api/sessions/${sid}/frames`, { method: "POST", headers: auth, body: JSON.stringify({ frames: vectors.frames }) })
  ).json();
  if (!ingest.completed_beams?.length) throw new Error(`the vector did not complete a beam: ${JSON.stringify(ingest)}`);
  await sleep(800);

  // headless chrome over CDP
  procs.push(
    spawn(
      CHROME,
      [`--remote-debugging-port=${CDP_PORT}`, "--headless=new", "--disable-gpu", "--hide-scrollbars", "--no-first-run", "--no-default-browser-check", `--user-data-dir=${join(tmp, "chrome")}`, "--window-size=1280,900", "about:blank"],
      { stdio: "ignore" },
    ),
  );
  await until(() => fetch(`http://127.0.0.1:${CDP_PORT}/json/version`).then((r) => r.ok));
  const target = await (await fetch(`http://127.0.0.1:${CDP_PORT}/json/new?about:blank`, { method: "PUT" })).json();
  const ws = new WebSocket(target.webSocketDebuggerUrl);
  await new Promise((r) => (ws.onopen = r));
  const pending = new Map();
  let seq = 0;
  ws.onmessage = (m) => {
    const d = JSON.parse(m.data);
    if (d.id && pending.has(d.id)) {
      pending.get(d.id)(d);
      pending.delete(d.id);
    }
  };
  const send = (method, params = {}) =>
    new Promise((res) => {
      const id = ++seq;
      pending.set(id, res);
      ws.send(JSON.stringify({ id, method, params }));
    });
  await send("Page.enable");
  await send("Runtime.enable");
  const size = (w, h, mobile = false) => send("Emulation.setDeviceMetricsOverride", { width: w, height: h, deviceScaleFactor: 2, mobile });
  const go = async (url, settle = 1500) => {
    await send("Page.navigate", { url });
    await sleep(settle);
  };
  const evaluate = async (expression) => {
    await send("Runtime.evaluate", { expression, awaitPromise: true, returnByValue: true });
    await sleep(400);
  };
  const shot = async (file) => {
    const r = await send("Page.captureScreenshot", { format: "webp", quality: 88 });
    writeFileSync(join(outDir, file), Buffer.from(r.result.data, "base64"));
    console.log("wrote", join("web/public/docs", file));
  };

  await size(1280, 900);
  await go(`${tower}/`);
  await shot("home.webp");
  await go(`${tower}/${sid}#t=${token}`, 2200);
  await shot("dashboard.webp");
  await size(390, 844, true);
  await go(`${tower}/s/${sid}#t=${token}`);
  await evaluate(`window.airliftScan.demo(674, 418); document.getElementById('message').textContent = 'Point the camera at the beam.'; 1`);
  await shot("scanner.webp");
  await size(1280, 900);
  await go(`http://127.0.0.1:${PLAYER_PORT}/demo.html`);
  await shot("player.webp");
  ws.close();
  console.log("done — now `make airlift` to embed them, and commit web/public/docs/");
} finally {
  await cleanup();
}
