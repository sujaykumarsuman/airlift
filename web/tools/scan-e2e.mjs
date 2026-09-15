// Drives a real beam through the real scanner without a phone: a throw-away
// tower, a beam of BEAM (default docs/adr) with the current defaults, the
// player's frames recorded into an MJPEG file, and headless Chrome fed that
// file as a fake camera on the scan page. Passes when the tower writes the
// beam's meta.json (READY, sha256 chain verified); prints the HUD's decode
// numbers and the time to READY.
//
//   make scan-e2e                 (builds bin/airlift first)
//   BEAM=path/to/folder make scan-e2e
//   CHROME=/path/to/chrome node web/tools/scan-e2e.mjs
//
// Only a virtual camera — a phone still decides the real frame rate — but it
// proves the whole chain (symbol size, the finder crop, the decode loop, the
// relay, the tower) with the frames the beam actually draws.
import { spawn } from "node:child_process";
import { createServer } from "node:http";
import { existsSync, mkdirSync, mkdtempSync, readdirSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const repo = resolve(dirname(fileURLToPath(import.meta.url)), "..", "..");
const bin = join(repo, "bin", "airlift");
const target = resolve(process.env.BEAM ?? join(repo, "docs", "adr"));
const CHROME =
  process.env.CHROME ??
  ["/Applications/Google Chrome.app/Contents/MacOS/Google Chrome", "/usr/bin/google-chrome", "/usr/bin/chromium", "/usr/bin/chromium-browser"].find(
    existsSync,
  );
if (!CHROME) throw new Error("no Chrome found: set CHROME=/path/to/chrome");
if (!existsSync(bin)) throw new Error("bin/airlift missing: run `make airlift` first");

const TOWER_PORT = 8807;
const PLAYER_PORT = 8806;
const CDP_REC = 9343;
const CDP_SCAN = 9344;
const DUP = 3; // each player frame repeated in the MJPEG: Chrome plays it at 30 fps, the beam default is 10
const TIMEOUT_MS = 90_000;
const tower = `http://127.0.0.1:${TOWER_PORT}`;
const tmp = mkdtempSync(join(tmpdir(), "airlift-e2e-"));
const dataDir = join(tmp, "data");
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
          setTimeout(res, 3000);
        }),
    ),
  );
  rmSync(tmp, { recursive: true, force: true, maxRetries: 10, retryDelay: 200 });
};

/** A headless Chrome with a CDP session on one fresh page. */
async function chrome(port, extraFlags, windowSize) {
  const proc = spawn(
    CHROME,
    [
      `--remote-debugging-port=${port}`,
      "--headless=new",
      "--disable-gpu",
      "--hide-scrollbars",
      "--no-first-run",
      "--no-default-browser-check",
      "--autoplay-policy=no-user-gesture-required",
      `--user-data-dir=${join(tmp, `chrome-${port}`)}`,
      `--window-size=${windowSize}`,
      ...extraFlags,
      "about:blank",
    ],
    { stdio: "ignore" },
  );
  procs.push(proc);
  await until(() => fetch(`http://127.0.0.1:${port}/json/version`).then((r) => r.ok));
  const page = await (await fetch(`http://127.0.0.1:${port}/json/new?about:blank`, { method: "PUT" })).json();
  const ws = new WebSocket(page.webSocketDebuggerUrl);
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
  return {
    send,
    go: async (url, settle = 1500) => {
      await send("Page.navigate", { url });
      await sleep(settle);
    },
    eval: async (expression) => {
      const r = await send("Runtime.evaluate", { expression, awaitPromise: true, returnByValue: true });
      return r.result?.result?.value;
    },
    jpeg: async (quality = 92) => Buffer.from((await send("Page.captureScreenshot", { format: "jpeg", quality })).result.data, "base64"),
    close: () => ws.close(),
  };
}

const t0 = Date.now();
try {
  // 1. a throw-away tower
  mkdirSync(dataDir, { recursive: true });
  procs.push(
    spawn(bin, ["tower", "--listen", `127.0.0.1:${TOWER_PORT}`, "--public_url", tower, "--admin_token", "e2e", "--data_dir", dataDir], {
      env: { ...process.env, AIRLIFT_HOME: join(tmp, "home") },
      stdio: "ignore",
    }),
  );
  await until(() => fetch(`${tower}/api/info`).then((r) => r.ok));

  // 2. the beam, with the current defaults
  const beamHTML = join(tmp, "beam.html");
  const summary = await new Promise((res, rej) => {
    const p = spawn(bin, ["beam", "--no-open", "--name", "e2e", "--out", beamHTML, target], { stdio: ["ignore", "pipe", "inherit"] });
    let out = "";
    p.stdout.on("data", (d) => (out += d));
    p.on("exit", (c) => (c === 0 ? res(out) : rej(new Error(`beam exited ${c}`))));
  });
  for (const line of summary.split("\n")) if (/chunks|mode|qr |loop|bundle|gzip/.test(line)) console.log(line);
  const player = createServer((_req, res) => {
    res.setHeader("content-type", "text/html; charset=utf-8");
    res.end(readFileSync(beamHTML));
  }).listen(PLAYER_PORT, "127.0.0.1");
  procs.push({ kill: () => player.close() });

  // 3. record the player's frames: paused, chrome hidden, the tile at half size
  //    so a version-30 symbol lands inside the scanner's viewfinder at an
  //    integer 3 px per module (the player's pitch) in a 1080p "camera" frame
  const rec = await chrome(CDP_REC, [], "1920,1080");
  await rec.go(`http://127.0.0.1:${PLAYER_PORT}/beam.html`);
  await rec.eval(`document.dispatchEvent(new KeyboardEvent('keydown', {key: ' '})); document.dispatchEvent(new KeyboardEvent('keydown', {key: 'h'})); for (let i = 0; i < 10; i++) document.getElementById('smaller').click(); 1`);
  await sleep(300);
  const total = Number((await rec.eval(`document.getElementById('frame').textContent`)).match(/\/(\d+)/)[1]);
  const frames = [];
  for (let i = 0; i < total; i++) {
    const jpg = await rec.jpeg();
    for (let d = 0; d < DUP; d++) frames.push(jpg);
    await rec.eval(`document.getElementById('next').click(); 1`);
    await sleep(40);
  }
  rec.close();
  const mjpeg = join(tmp, "beam.mjpeg");
  writeFileSync(mjpeg, Buffer.concat(frames));
  console.log(`recorded ${total} player frames × ${DUP} → ${(frames.length / 30).toFixed(1)} s of camera at 30 fps`);

  // 4. a session, and headless Chrome on its scan page with the recording as the camera
  const created = await (await fetch(`${tower}/api/sessions`, { method: "POST", headers: { "content-type": "application/json" }, body: "{}" })).json();
  const { sid, token } = created;
  const scan = await chrome(
    CDP_SCAN,
    ["--use-fake-device-for-media-stream", `--use-file-for-fake-video-capture=${mjpeg}`, "--use-fake-ui-for-media-stream"],
    "700,1100",
  );
  const tScan = Date.now();
  await scan.go(`${tower}/s/${sid}#t=${token}`, 500);
  let last = "";
  let ready = null;
  for (;;) {
    const hud = await scan.eval(
      `[document.getElementById('progress')?.textContent, document.getElementById('stats')?.textContent, document.getElementById('message')?.textContent].join(' | ')`,
    );
    if (hud && hud !== last) {
      last = hud;
      console.log(`${((Date.now() - tScan) / 1000).toFixed(1)}s  ${hud}`);
    }
    const sessDir = join(dataDir, sid);
    if (existsSync(sessDir)) {
      for (const bid of readdirSync(sessDir)) {
        const meta = join(sessDir, bid, "meta.json");
        if (existsSync(meta)) ready = { bid, meta: JSON.parse(readFileSync(meta, "utf8")) };
      }
    }
    if (ready) break;
    if (Date.now() - tScan > TIMEOUT_MS) throw new Error(`no READY after ${TIMEOUT_MS / 1000} s; last HUD: ${last}`);
    await sleep(500);
  }
  scan.close();
  console.log(`READY in ${((Date.now() - tScan) / 1000).toFixed(1)} s from opening the scanner (${((Date.now() - t0) / 1000).toFixed(1)} s in all): beam ${ready.bid} ${JSON.stringify(ready.meta.name ?? "")}`);
} finally {
  await cleanup();
}
