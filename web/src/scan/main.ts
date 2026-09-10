import "../shared/style.css";
import { ApiError, eventsURL, joinSession, postExtension, postFrames, postPing, registerClient } from "../shared/api";
import { decodeBitmap, drawBitmap } from "../shared/bitmap";
import { $, html, raw } from "../shared/dom";
import { cleanupCountdown, terminateCountdown, terminatedBy, terminatedWhy } from "../shared/lifecycle";
import { bindActivity, Pinger, type PingOutcome } from "../shared/ping";
import { subscribe, type SSEStatus } from "../shared/sse";
import type { Beam, Snapshot } from "../shared/types";
import {
  activeDeviceId,
  describeCamera,
  explainCameraError,
  hasTorch,
  listCameras,
  openCamera,
  setTorch,
  stopStream,
  streamSize,
} from "./camera";
import { activeKey, scanJustCompleted } from "./complete";
import { createDecoder, startDecodeLoop, type Decoder, type LoopStats } from "./decoder";
import { resolveJoin } from "./join";
import { Relay, type RelayStats } from "./relay";

const video = $<HTMLVideoElement>("#video");
const frameCanvas = $<HTMLCanvasElement>("#frame");
const bitmapCanvas = $<HTMLCanvasElement>("#bitmap");
const progressEl = $<HTMLElement>("#progress");
const stateEl = $<HTMLElement>("#state");
const statsEl = $<HTMLElement>("#stats");
const messageEl = $<HTMLElement>("#message");
const cameraSelect = $<HTMLSelectElement>("#camera");
const startButton = $<HTMLButtonElement>("#start");
const torchButton = $<HTMLButtonElement>("#torch");
const joinForm = $<HTMLFormElement>("#join-form");
const hudEl = $<HTMLElement>("#hud");
const endedEl = $<HTMLElement>("#ended");
const warningEl = $<HTMLElement>("#warning");

function safeStorage(): Storage | null {
  try {
    return localStorage;
  } catch {
    return null;
  }
}
const basePath = new URL(document.baseURI).pathname.replace(/\/$/, "");
const join = resolveJoin(location.pathname, location.hash, safeStorage(), basePath);
if (join.redirect) history.replaceState(null, "", new URL(join.redirect, document.baseURI).toString());
if (join.error !== undefined) {
  progressEl.textContent = "✗";
  stateEl.textContent = "Not a join link";
  messageEl.textContent = join.error;
  startButton.hidden = true;
  cameraSelect.hidden = true;
  throw new Error(join.error);
}
const sid = join.sid;
let token = join.token ?? ""; // filled by a password join when the link has no token

let snap: Snapshot | null = null;
let relayStats: RelayStats | null = null;
let loopStats: LoopStats | null = null;
let decoder: Decoder | null = null;
let stream: MediaStream | null = null;
let stopLoop: (() => void) | null = null;
let stopEvents: (() => void) | null = null;
let role: "viewer" | "relay" = "viewer";
let connection: SSEStatus = "connecting";
let message = "";
let cameraLabel = "";
let wakeLock: WakeLockSentinel | null = null;
let torchOn = false;
let clientID = "";
let ownName = "";

// Lifecycle chrome (ADR 0014). A TERMINATING session stays live and shows a
// warning banner; the frozen states (TERMINATED/PENDING_REVIEW/REJECTED) stop the
// camera and show a full-screen overlay; closed/evicted are final and snapshot-
// less; a return to OPEN (an admin cancel or an accepted extension) resumes.
type Terminal = { kind: "closed" } | { kind: "evicted" };
const FROZEN = new Set(["TERMINATED", "PENDING_REVIEW", "REJECTED"]);
let terminal: Terminal | null = null;
let frozen = false; // we have stopped the camera for a frozen status
let scanComplete = false; // the beam we were feeding is fully received (ADR 0019)
let prevActiveKey = ""; // `${bid}:${state}` of the active beam last render, for the READY edge
let overlayKey = ""; // identifies the currently-rendered overlay (avoids clobbering the form)
let lifecycleTimer: ReturnType<typeof setInterval> | null = null;
let pinger: Pinger | null = null;

const relay = new Relay({
  post: (frames) => postFrames(sid, token, frames, clientID),
  onUpdate: (s) => {
    relayStats = s;
    render();
  },
});

async function doPing(): Promise<PingOutcome> {
  try {
    await postPing(sid, token, clientID);
    return "ok";
  } catch (e) {
    return e instanceof ApiError && [401, 403, 404, 409].includes(e.status) ? "stop" : "retry";
  }
}

function startPinging(): void {
  if (pinger) return;
  pinger = new Pinger({ ping: doPing, bindActivity });
}

function stopPinging(): void {
  pinger?.stop(); // stop() releases the activity binding it owns
  pinger = null;
}

// setTerminal handles the snapshot-less finals: freeze and never resume.
function setTerminal(t: Terminal): void {
  terminal = t;
  frozen = true;
  stopCamera();
  relay.stop();
  stopPinging();
}

// syncLifecycle reacts to a fresh snapshot: freeze on a frozen status, or resume
// on a return to a live one (a cancel or an accepted extension).
function syncLifecycle(): void {
  if (terminal || !snap) return;
  const shouldFreeze = FROZEN.has(snap.status);
  if (shouldFreeze && !frozen) {
    frozen = true;
    stopCamera();
    relay.stop();
    stopPinging();
  } else if (!shouldFreeze && frozen) {
    // Reopened (a cancel, or an accepted extension): revive the relay before the
    // camera so decoded frames flow again, and restart the pinger.
    frozen = false;
    relay.resume();
    startPinging();
    void startCamera();
  }
}

function overlayActive(): boolean {
  return terminal !== null || (!!snap && FROZEN.has(snap.status));
}

// A key for the currently-shown overlay: a full re-render only when it changes,
// so the extension form's input is not clobbered by the per-second countdown.
function overlayStateKey(): string {
  if (terminal) return terminal.kind;
  if (!snap) return "";
  if (snap.reopenable) return "SUSPENDED";
  if (snap.status === "TERMINATED" || snap.status === "REJECTED") {
    const done = snap.terminated ? cleanupCountdown(snap.terminated, Date.now()).done : false;
    return `${snap.status}:${done ? "done" : "live"}`;
  }
  return snap.status;
}

function startLifecycleTimer(): void {
  if (lifecycleTimer === null) lifecycleTimer = setInterval(render, 1000);
}
function stopLifecycleTimer(): void {
  if (lifecycleTimer !== null) {
    clearInterval(lifecycleTimer);
    lifecycleTimer = null;
  }
}

// updateChrome shows the frozen/terminal overlay and the TERMINATING warning
// banner, and runs a 1 s ticker while either has a live countdown.
function updateChrome(): void {
  const showOverlay = overlayActive() || scanComplete;
  hudEl.hidden = showOverlay;
  endedEl.hidden = !showOverlay;
  if (showOverlay) {
    const key = overlayActive() ? overlayStateKey() : "COMPLETE";
    if (key !== overlayKey) {
      overlayKey = key;
      renderOverlay();
    } else if (overlayActive()) {
      patchOverlayClock();
    }
  } else {
    overlayKey = "";
  }
  const terminating = !terminal && !!snap && snap.status === "TERMINATING";
  warningEl.hidden = !terminating;
  if (terminating && snap) {
    const c = terminateCountdown(snap, Date.now());
    warningEl.innerHTML = html`Session ending${c.hidden ? "" : html` in <b class="clock">${c.text}</b>`} unless an admin cancels.`.html;
  }
  if (overlayActive() || terminating) startLifecycleTimer();
  else stopLifecycleTimer();
}

function renderOverlay(): void {
  if (scanComplete && !overlayActive()) {
    // The beam is fully received: camera off, offer to close or scan another.
    endedEl.innerHTML = html`<div class="ended-card">
      <h2>Beam received</h2>
      <p class="muted">The tower has the whole beam. Close this tab, or scan another.</p>
      <p class="overlay-actions">
        <button id="scan-again" class="btn" type="button">Scan another</button>
        <button id="close-tab" class="btn primary" type="button">Close</button>
      </p>
    </div>`.html;
    endedEl.querySelector<HTMLButtonElement>("#scan-again")?.addEventListener("click", onScanAnother);
    endedEl.querySelector<HTMLButtonElement>("#close-tab")?.addEventListener("click", onCloseTab);
    return;
  }
  if (terminal) {
    const closed = terminal.kind === "closed";
    endedEl.innerHTML = html`<div class="ended-card">
      <h2>${closed ? "Session closed" : "Removed"}</h2>
      <p>${closed ? "The tower has closed this session." : "You were removed from this session."}</p>
    </div>`.html;
    return;
  }
  if (!snap) return;
  if (snap.reopenable) {
    // Suspended by inactivity (ADR 0018): opening the link reopens it — no review.
    endedEl.innerHTML = html`<div class="ended-card">
      <h2>Session paused</h2>
      <p class="muted">This session closed after a spell of inactivity. Reopen it to carry on — every timer resets.</p>
      <p><button id="reopen-btn" class="btn primary" type="button">Reopen session</button></p>
    </div>`.html;
    endedEl.querySelector<HTMLButtonElement>("#reopen-btn")?.addEventListener("click", onReopen);
    return;
  }
  if (snap.status === "PENDING_REVIEW") {
    endedEl.innerHTML = html`<div class="ended-card">
      <h2>Awaiting review</h2>
      <p class="muted">Your request for more time is with the tower's administrator.</p>
      ${snap.extension?.reason ? html`<p class="muted">“${snap.extension.reason}”</p>` : ""}
    </div>`.html;
    return;
  }
  const t = snap.terminated;
  const c = t ? cleanupCountdown(t, Date.now()) : { text: "", done: true, hidden: true };
  const cleanupLine = c.hidden
    ? ""
    : c.done
      ? html`<p class="muted">The received files have been removed from the tower.</p>`
      : html`<p class="muted">The received files stay on the tower for another <b id="cleanup" class="clock">${c.text}</b>.</p>`;
  const canExtend = snap.status === "TERMINATED" && !c.done;
  const rejectedNote = snap.status === "REJECTED" && snap.extension?.note;
  endedEl.innerHTML = html`<div class="ended-card">
    <h2>${t ? terminatedBy(t) : "Session ended"}</h2>
    ${t ? html`<p>${terminatedWhy(t)}</p>` : ""}
    ${rejectedNote ? html`<p class="muted">Note: ${snap.extension!.note}</p>` : ""}
    ${cleanupLine}
    ${canExtend
      ? html`<form id="ext-form" class="ext-form">
          <input id="ext-reason" type="text" placeholder="why you need more time (optional)" />
          <button class="btn primary" type="submit">Request more time</button>
        </form>`
      : ""}
  </div>`.html;
  const form = endedEl.querySelector<HTMLFormElement>("#ext-form");
  if (form) form.addEventListener("submit", onExtensionSubmit);
}

// patchOverlayClock updates only the cleanup countdown text so the extension
// form's input is preserved across the per-second tick.
function patchOverlayClock(): void {
  if (terminal || !snap?.terminated) return;
  const c = cleanupCountdown(snap.terminated, Date.now());
  const el = endedEl.querySelector<HTMLElement>("#cleanup");
  if (el && !c.hidden) el.textContent = c.text;
}

// onScanAnother clears the completion overlay and restarts the camera for the
// next beam (ADR 0019).
function onScanAnother(): void {
  scanComplete = false;
  void startCamera();
}

// onCloseTab closes the scanner tab (script-closable since the dashboard's Scan
// button opened it); if the browser blocks it, tell the user they can close it.
function onCloseTab(): void {
  window.close();
  const card = endedEl.querySelector(".ended-card");
  if (card && !card.querySelector(".close-hint")) {
    const p = document.createElement("p");
    p.className = "muted close-hint";
    p.textContent = "If the tab did not close, you can close it now.";
    card.appendChild(p);
  }
}

// onReopen revives a session suspended by inactivity: registering reopens it
// server-side (ADR 0018), then the SSE pushes OPEN and syncLifecycle brings the
// camera and relay back.
function onReopen(): void {
  const btn = endedEl.querySelector<HTMLButtonElement>("#reopen-btn");
  if (btn) {
    btn.disabled = true;
    btn.textContent = "Reopening…";
  }
  void registerClient(sid, token, { role: "relay" })
    .then((c) => {
      clientID = c.client_id;
    })
    .catch((err) => {
      if (btn) {
        btn.disabled = false;
        btn.textContent = "Reopen session";
      }
      message = err instanceof Error ? err.message : String(err);
      render();
    });
}

function onExtensionSubmit(e: Event): void {
  e.preventDefault();
  const input = endedEl.querySelector<HTMLInputElement>("#ext-reason");
  const reason = input?.value.trim() ?? "";
  const button = endedEl.querySelector<HTMLButtonElement>("#ext-form button");
  if (button) button.disabled = true;
  void postExtension(sid, token, clientID, reason)
    .then(() => {
      /* the SSE will push PENDING_REVIEW and re-render the overlay */
    })
    .catch((err) => {
      if (button) button.disabled = false;
      const msg = err instanceof Error ? err.message : String(err);
      const note = document.createElement("p");
      note.className = "warn";
      note.textContent = `Could not request more time: ${msg}`;
      endedEl.querySelector(".ended-card")?.appendChild(note);
    });
}

/** The beam the scanner is feeding now: the last one still receiving, else the
 *  most recently arrived. A place may hold several; the scan page tracks one. */
function activeBeam(): Beam | null {
  if (!snap || snap.beams.length === 0) return null;
  for (let i = snap.beams.length - 1; i >= 0; i--) {
    if (snap.beams[i]!.state === "RECEIVING") return snap.beams[i]!;
  }
  return snap.beams[snap.beams.length - 1]!;
}

function render(): void {
  const beam = activeBeam();
  // The beam this scanner is feeding reaching READY (all packets received) stops
  // the camera and offers to close (ADR 0019). Fires on the transition into READY
  // from RECEIVING or VERIFYING, while our camera is running.
  if (scanJustCompleted(prevActiveKey, beam, !!stream)) {
    scanComplete = true;
    stopCamera();
  }
  prevActiveKey = activeKey(beam);
  updateChrome();
  if (overlayActive() || scanComplete) return; // an overlay owns the screen; skip the live HUD
  const total = beam?.total ?? 0;
  const have = beam?.have ?? 0;
  progressEl.textContent = total > 0 ? `${have} / ${total}` : snap ? "waiting for a beam" : "…";
  const state = beam?.state ?? (snap ? "waiting" : "connecting");
  stateEl.textContent = state.toLowerCase();
  stateEl.dataset.state = state;
  if (beam && total > 0) {
    bitmapCanvas.hidden = false;
    drawBitmap(bitmapCanvas, decodeBitmap(beam.bitmap, total), { cell: 6, gap: 1 });
  } else {
    bitmapCanvas.hidden = true;
  }
  const parts: string[] = [];
  if (decoder) parts.push(decoder.name + (cameraLabel ? ` · ${cameraLabel}` : ""));
  if (loopStats) parts.push(`${loopStats.decodesPerSec.toFixed(1)} decoded/s · ${loopStats.lastDecodeMs.toFixed(0)} ms`);
  if (relayStats) {
    parts.push(`sent ${relayStats.sent} · new ${relayStats.accepted} · dup ${relayStats.dup + (relayStats.seen - relayStats.unique)} · bad ${relayStats.bad}`);
    if (relayStats.buffered > 0 || relayStats.failures > 0) {
      parts.push(`buffered ${relayStats.buffered}${relayStats.failures ? ` · retrying (${relayStats.lastError ?? "network"})` : ""}`);
    }
  }
  if (ownName) parts.push(`you: ${ownName}`);
  if (snap && snap.beams.length > 1) parts.push(`${snap.beams.length} beams`);
  if (beam) parts.push(`beam ${beam.bid}`);
  if (connection !== "open") parts.push(`link: ${connection}`);
  statsEl.innerHTML = html`${parts.map((p) => html`<span>${p}</span>`)}`.html;
  messageEl.textContent = beam?.error ?? message;
  document.body.dataset.state = state;
}

function subscribeProgress(as: "viewer" | "relay"): () => void {
  role = as;
  return subscribe(
    eventsURL(sid, as),
    token,
    {
      onEvent: (ev) => {
        // closed/evicted are final and snapshot-less; every other event carries a
        // snapshot (state/terminating/terminated/rejected/reopened) and drives the
        // freeze/resume decision. A place stays open across beams, and a warned
        // (TERMINATING) session keeps relaying; only a frozen status stops it.
        if (ev.event === "closed") {
          setTerminal({ kind: "closed" });
        } else if (ev.event === "evicted") {
          setTerminal({ kind: "evicted" });
        } else {
          snap = JSON.parse(ev.data) as Snapshot;
          syncLifecycle();
        }
        render();
      },
      onStatus: (status, detail) => {
        connection = status;
        if (status === "stopped" && detail?.startsWith("HTTP")) message = `Session not found (${detail}). Scan the tower's QR code again.`;
        render();
      },
    },
    { clientId: clientID },
  );
}

async function fillCameraList(): Promise<void> {
  const cams = await listCameras();
  const active = stream ? activeDeviceId(stream) : undefined;
  cameraSelect.innerHTML = html`${cams.map(
    (c, i) => html`<option value="${c.deviceId}" ${c.deviceId === active ? raw("selected") : ""}>${describeCamera(c, i)}</option>`,
  )}`.html;
  cameraSelect.hidden = cams.length < 2;
  const current = cams.find((c) => c.deviceId === active);
  cameraLabel = current ? describeCamera(current, cams.indexOf(current)) : "";
  if (stream) cameraLabel += ` ${streamSize(stream)}`;
}

async function startCamera(deviceId?: string): Promise<void> {
  scanComplete = false;
  stopCamera(false);
  startButton.disabled = true;
  message = "Starting camera…";
  render();
  try {
    stream = await openCamera(deviceId);
    video.srcObject = stream;
    await video.play();
    await fillCameraList();
    decoder ??= await createDecoder();
    stopLoop = startDecodeLoop(video, decoder, frameCanvas, (text) => relay.push(text), (s) => {
      loopStats = s;
      render();
    });
    if (role !== "relay") {
      stopEvents?.();
      stopEvents = subscribeProgress("relay");
    }
    message = "Point the camera at the beam.";
    startButton.hidden = true;
    torchOn = false;
    torchButton.hidden = !hasTorch(stream);
    torchButton.textContent = "Torch";
    void requestWakeLock();
  } catch (err) {
    message = explainCameraError(err);
    startButton.hidden = false;
    startButton.disabled = false;
    startButton.textContent = "Start camera";
  }
  render();
}

function stopCamera(final = true): void {
  stopLoop?.();
  stopLoop = null;
  if (stream) stopStream(stream);
  stream = null;
  torchButton.hidden = true;
  video.srcObject = null;
  loopStats = null;
  if (final) void relay.flush();
}

async function requestWakeLock(): Promise<void> {
  try {
    wakeLock = (await navigator.wakeLock?.request("screen")) ?? null;
  } catch {
    wakeLock = null;
  }
}

document.addEventListener("visibilitychange", () => {
  if (document.visibilityState === "visible" && stream && !wakeLock) void requestWakeLock();
  if (document.visibilityState === "hidden") wakeLock = null;
});
cameraSelect.addEventListener("change", () => void startCamera(cameraSelect.value));
startButton.addEventListener("click", () => void startCamera());
torchButton.addEventListener("click", () => {
  if (!stream) return;
  void setTorch(stream, !torchOn).then((ok) => {
    if (ok) torchOn = !torchOn;
    torchButton.textContent = torchOn ? "Torch off" : "Torch";
  });
});
if (import.meta.env.PROD && "serviceWorker" in navigator) {
  const scope = new URL("./", document.baseURI).pathname; // "/" or "/airlift/"
  navigator.serviceWorker.register(new URL("sw.js", document.baseURI), { scope }).catch(() => {
    /* the page works without it; it only loses offline reloads */
  });
}
navigator.mediaDevices?.addEventListener?.("devicechange", () => void fillCameraList());

// With a token in the link we register straight away; without one, the session
// is password-protected, so we show a join form first.
async function init(): Promise<void> {
  if (token) {
    await register();
    return;
  }
  message = "This session needs a password to join.";
  joinForm.hidden = false;
  startButton.hidden = true;
  render();
}

// register binds a client to this address and starts watching + scanning.
async function register(): Promise<void> {
  try {
    const c = await registerClient(sid, token, { role: "relay" });
    clientID = c.client_id;
    ownName = c.name;
  } catch (err) {
    message = err instanceof Error ? err.message : String(err);
    render();
    return;
  }
  stopEvents = subscribeProgress("viewer");
  startPinging();
  render();
  void startCamera();
}

joinForm.addEventListener("submit", (e) => {
  e.preventDefault();
  const password = $<HTMLInputElement>("#join-password").value;
  const name = $<HTMLInputElement>("#join-name").value.trim();
  message = "Joining…";
  render();
  void joinSession(sid, { password, name: name || undefined })
    .then((j) => {
      token = j.token;
      clientID = j.client_id;
      ownName = j.name;
      joinForm.hidden = true;
      message = "";
      stopEvents = subscribeProgress("viewer");
      startPinging();
      render();
      void startCamera();
    })
    .catch((err) => {
      message = err instanceof Error ? err.message : String(err);
      render();
    });
});

window.addEventListener("pagehide", () => {
  stopPinging();
  stopLifecycleTimer();
  void relay.flush();
});

void init();

// Hardware-free testing: inject decoded strings as if the camera saw them.
(window as unknown as { airliftScan: unknown }).airliftScan = {
  relay,
  inject: (texts: string[]) => texts.map((t) => relay.push(t)),
};
