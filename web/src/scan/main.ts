import "../shared/style.css";
import { ApiError, eventsURL, joinSession, postExtension, postFrames, postPing, registerClient, rememberClientKey } from "../shared/api";
import { decodeBitmap } from "../shared/bitmap";
import { renderChunkMarks } from "../shared/chunks";
import { $, html, raw } from "../shared/dom";
import { icon } from "../shared/icons";
import { cleanupCountdown, terminateCountdown, terminatedBy, terminatedWhy } from "../shared/lifecycle";
import { bindActivity, Pinger, type PingOutcome } from "../shared/ping";
import { subscribe, type SSEStatus } from "../shared/sse";
import type { Beam, Snapshot } from "../shared/types";
import {
  activeDeviceId,
  describeCamera,
  explainCameraError,
  hasOtherSameFacing,
  hasTorch,
  listCameras,
  openCamera,
  setTorch,
  stopStream,
  streamFrameRate,
  streamSize,
} from "./camera";
import { activeKey, baselineIgnored, pickActive, relayCompleted, scanJustCompleted } from "./complete";
import { createDecoder, startDecodeLoop, type Decoder, type LoopStats } from "./decoder";
import { finderROI } from "./roi";
import { resolveJoin } from "./join";
import { Relay, type RelayStats } from "./relay";
import { fpsOptions, frameType, HintGate, parseFpsChoice, Smoother, speedHint, type FpsChoice, type SpeedHint } from "./speed";

const video = $<HTMLVideoElement>("#video");
const frameCanvas = $<HTMLCanvasElement>("#frame");
const chunksEl = $<HTMLElement>("#chunks");
const finderEl = $<HTMLElement>("#finder");
const progressEl = $<HTMLElement>("#progress");
const stateEl = $<HTMLElement>("#state");
const statsEl = $<HTMLElement>("#stats");
const messageEl = $<HTMLElement>("#message");
const cameraSelect = $<HTMLSelectElement>("#camera");
const fpsSelect = $<HTMLSelectElement>("#fps");
const hintEl = $<HTMLElement>("#hint");
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
function readPref(key: string): string | null {
  try {
    return safeStorage()?.getItem(key) ?? null;
  } catch {
    return null;
  }
}
function writePref(key: string, value: string): void {
  try {
    safeStorage()?.setItem(key, value);
  } catch {
    /* storage unavailable: the choice lasts this visit only */
  }
}
const basePath = new URL(document.baseURI).pathname.replace(/\/$/, "");
const join = resolveJoin(location.pathname, location.hash, safeStorage(), basePath);

// The identity this device holds for the session (ADR 0022, amended): the
// dashboard's record when the link named its client (`c=`), else the scanner's
// own from a previous visit. Both carry the client's resume key, so a phone back
// from sleep on a new address is still the same participant.
interface Identity {
  client_id?: string;
  resumeKey?: string;
}
function readIdentity(key: string): Identity {
  try {
    return (JSON.parse(safeStorage()?.getItem(key) ?? "null") as Identity | null) ?? {};
  } catch {
    return {};
  }
}
function heldIdentity(sid: string, urlClient: string | undefined): Identity {
  const dash = readIdentity(`airlift.session.${sid}`);
  if (urlClient) return { client_id: urlClient, resumeKey: dash.client_id === urlClient ? dash.resumeKey : undefined };
  if (dash.client_id) return dash;
  return readIdentity(`airlift.scan.${sid}`);
}
function rememberIdentity(sid: string, client_id: string, resumeKey: string | undefined): void {
  rememberClientKey(client_id, resumeKey);
  try {
    safeStorage()?.setItem(`airlift.scan.${sid}`, JSON.stringify({ client_id, resumeKey }));
  } catch {
    /* storage unavailable: the tower still knows us by address for this visit */
  }
}
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
// Scan speed: the frame rate asked of the camera (kept per browser), what the
// open track reports, and the debounced hint built from the loop's numbers.
const FPS_KEY = "airlift.scan.fps";
let fpsChoice: FpsChoice = parseFpsChoice(readPref(FPS_KEY));
let camFps: { set?: number; max?: number } = {};
let otherCamera = false; // another camera faces the same way as this one
let lastFountainAt = -Infinity; // when a FOUNTAIN frame was last decoded
let hint: SpeedHint | null = null;
let startGen = 0; // bumps on every startCamera, so a superseded one backs out
const SCANNING = "Point the camera at the beam.";
let hintShown = "";
const smoother = new Smoother();
const hintGate = new HintGate();
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
let completedBid = ""; // which beam that was, for the overlay and for dismissing it
let prevActiveKey = ""; // `${bid}:${state}` of the active beam last render, for the READY edge
// A scanner adopts only beams that arrive after it opened: the ones already
// finished at its first snapshot, and the ones it has scanned and dismissed, are
// ignored, so a reopened scanner is a clean "waiting for a beam".
let ignored = new Set<string>();
let baselined = false;
const acked = new Set<string>(); // relay-reported completions already acted on
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
  finderEl.hidden = showOverlay || !stream; // the square viewfinder frames a live camera only
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
    const done = snap?.beams.find((b) => b.bid === completedBid);
    endedEl.innerHTML = html`<div class="ended-card">
      <h2>Beam received</h2>
      <p class="muted">${done?.name ? html`<b>${done.name}</b> — ` : ""}The tower has the whole beam. Close this tab, or scan another.</p>
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

// onScanAnother dismisses the received beam (it stays ignored, so the still-
// displayed loop cannot re-adopt it), zeroes the counters, and restarts the
// camera for the next beam (ADR 0019).
function onScanAnother(): void {
  if (completedBid) ignored.add(completedBid);
  completedBid = "";
  relay.resetStats();
  scanComplete = false;
  void startCamera();
}

// onCloseTab closes the scanner tab (script-closable since the dashboard's Scan
// button opened it); if the browser blocks it, tell the user they can close it.
function onCloseTab(): void {
  window.close();
  const card = endedEl.querySelector(".ended-card");
  if (card) {
    if (!card.querySelector(".close-hint")) {
      const p = document.createElement("p");
      p.className = "muted close-hint";
      p.textContent = "If the tab did not close, you can close it now.";
      card.appendChild(p);
    }
  } else {
    message = "If the tab did not close, you can close it now.";
    render();
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
  const held = heldIdentity(sid, clientID || join.client);
  void registerClient(sid, token, { role: "relay", resume: held.client_id, resumeKey: held.resumeKey })
    .then((c) => {
      clientID = c.client_id;
      rememberIdentity(sid, c.client_id, c.resume_key);
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
 *  most recently arrived, never one it ignores. A place may hold several; the
 *  scan page tracks one. */
function activeBeam(): Beam | null {
  return snap ? pickActive(snap.beams, ignored) : null;
}

// finish marks the beam this scanner fed as received: camera off, overlay on.
function finish(bid: string): void {
  acked.add(bid);
  completedBid = bid;
  scanComplete = true;
  stopCamera();
}

function render(): void {
  const beam = activeBeam();
  // The beam this scanner is feeding reaching READY (all packets received) stops
  // the camera and offers to close (ADR 0019). Two signals: the SSE transition
  // into READY from RECEIVING/VERIFYING, and the frames reply's completed_beams —
  // the latter catches a small beam that is READY before its first snapshot.
  const filled = relayCompleted(relayStats?.completed ?? [], acked);
  if (filled !== null) acked.add(filled);
  if (beam && scanJustCompleted(prevActiveKey, beam, !!stream)) finish(beam.bid);
  else if (filled !== null && stream) finish(filled);
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
    chunksEl.hidden = false; // unhide first: the marks size themselves to the row's width
    renderChunkMarks(chunksEl, beam.bid, decodeBitmap(beam.bitmap, total));
  } else {
    chunksEl.hidden = true;
  }
  const parts: string[] = [];
  if (decoder) parts.push(decoder.name + (cameraLabel ? ` · ${cameraLabel}` : ""));
  if (loopStats) {
    const rates: string[] = [];
    // The set rate is in the camera label; the delivered one shows when it falls short.
    const cam = loopStats.cameraFps;
    if (cam !== null && (!camFps.set || cam < camFps.set * 0.9)) rates.push(`camera ${cam.toFixed(0)} fps`);
    if (loopStats.distinctPerSec > 0) {
      rates.push(`beam ~${loopStats.distinctPerSec.toFixed(0)} fps · ${(loopStats.decodesPerSec / loopStats.distinctPerSec).toFixed(1)}× each`);
    }
    if (rates.length) parts.push(rates.join(" · "));
    parts.push(`${loopStats.decodesPerSec.toFixed(1)} decoded/s · ${loopStats.attemptsPerSec.toFixed(0)} tries/s · ${loopStats.lastDecodeMs.toFixed(0)} ms`);
  }
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
  // No beam to feed but the tower keeps answering "dup": the loop on screen is a
  // beam this session already has.
  const stale = !beam && !!stream && !!relayStats && relayStats.dup > 0;
  const text = beam?.error ?? (stale ? "That beam is already received — show a new one." : message);
  // The speed hint (in the top band) stands in for the message while that only
  // says to point the camera, so the bottom band stays short.
  const showHint = !!stream && !beam?.error && !stale && message === SCANNING && hint !== null;
  messageEl.textContent = text;
  messageEl.hidden = showHint;
  renderHint(showHint ? hint : null);
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
          if (!baselined) {
            ignored = baselineIgnored(snap.beams); // finished before we opened: not ours
            baselined = true;
          }
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
  otherCamera = hasOtherSameFacing(cams, active, stream?.getVideoTracks()[0]?.getSettings().facingMode);
  const current = cams.find((c) => c.deviceId === active);
  cameraLabel = current ? describeCamera(current, cams.indexOf(current)) : "";
  if (stream) cameraLabel += ` ${streamSize(stream)}`;
  if (stream && camFps.set) cameraLabel += ` · ${Math.round(camFps.set)} fps`;
  if (stream && camFps.max && camFps.set && camFps.max > camFps.set + 1) cameraLabel += ` (max ${Math.round(camFps.max)})`;
}

// fillFpsMenu offers the rates this camera reports (hidden when it reports
// none); each option says what it asks for, and the selected one what the
// camera actually runs at.
function fillFpsMenu(): void {
  const rates = fpsOptions(camFps.max);
  if (typeof fpsChoice === "number" && !rates.includes(fpsChoice)) rates.push(fpsChoice);
  rates.sort((a, b) => b - a);
  const set = camFps.set ? Math.round(camFps.set) : undefined;
  const got = (r: number) => (set && Math.abs(set - r) > 1 ? ` · got ${set}` : "");
  fpsSelect.innerHTML = html`<option value="auto" ${fpsChoice === "auto" ? raw("selected") : ""}>Auto fps${fpsChoice === "auto" && set ? ` · ${set}` : ""}</option>${rates.map(
    (r) => html`<option value="${r}" ${fpsChoice === r ? raw("selected") : ""}>${r} fps${fpsChoice === r ? got(r) : ""}</option>`,
  )}`.html;
  fpsSelect.hidden = rates.length === 0;
}

// updateHint folds one second of decode-loop numbers into the speed hint. A
// second without the beam in view starts the averages afresh rather than
// decaying them (which would count the suggested rates down to nonsense).
function updateHint(s: LoopStats): void {
  if (s.distinctPerSec < 0.5) {
    smoother.reset();
    hint = hintGate.next(null);
    return;
  }
  const settings = stream?.getVideoTracks()[0]?.getSettings();
  const m = smoother.next({
    cameraFps: s.cameraFps,
    beamFps: s.distinctPerSec,
    decodesPerSec: s.decodesPerSec,
    attemptsPerSec: s.attemptsPerSec,
    lastDecodeMs: s.lastDecodeMs,
  });
  hint = hintGate.next(
    speedHint({
      ...m,
      setFps: camFps.set,
      maxFps: camFps.max,
      size: stream ? streamSize(stream) : undefined,
      shortEdge: settings?.width && settings.height ? Math.min(settings.width, settings.height) : undefined,
      fountain: performance.now() - lastFountainAt < 5000,
      fpsMenu: !fpsSelect.hidden,
      picked: fpsChoice !== "auto",
      otherCameras: otherCamera,
    }),
  );
}

function renderHint(h: SpeedHint | null): void {
  const text = h?.text ?? "";
  if (text === hintShown) return;
  hintShown = text;
  hintEl.innerHTML = text ? html`${icon("gauge")}<span>${text}</span>`.html : "";
  hintEl.hidden = !text;
}

function resetHint(): void {
  smoother.reset();
  hintGate.reset();
  hint = null;
  renderHint(null);
}

async function startCamera(deviceId?: string): Promise<void> {
  // A pick in either menu while a start is still opening the camera starts
  // another; the newest wins and an older one releases what it opened.
  const gen = ++startGen;
  let opened: MediaStream | null = null;
  const superseded = (): boolean => {
    if (gen === startGen) return false;
    if (opened && opened !== stream) stopStream(opened);
    return true;
  };
  scanComplete = false;
  stopCamera(false);
  startButton.disabled = true;
  cameraSelect.disabled = true;
  fpsSelect.disabled = true;
  message = "Starting camera…";
  render();
  try {
    opened = await openCamera(deviceId, fpsChoice);
    if (superseded()) return;
    stream = opened;
    video.srcObject = stream;
    await video.play();
    if (superseded()) return;
    camFps = streamFrameRate(stream);
    await fillCameraList();
    fillFpsMenu();
    decoder ??= await createDecoder();
    if (superseded()) return;
    stopLoop = startDecodeLoop(
      video,
      decoder,
      frameCanvas,
      (text) => {
        relay.push(text);
        if (frameType(text) === 2) lastFountainAt = performance.now();
      },
      (s) => {
        loopStats = s;
        updateHint(s);
        render();
      },
      // The decoder reads the viewfinder's crop of the frame (the video is
      // object-fit: cover over the viewport, the finder a centred square).
      () => (finderEl.hidden ? null : finderROI(video.videoWidth, video.videoHeight, video.clientWidth, video.clientHeight, finderEl.getBoundingClientRect())),
    );
    if (role !== "relay") {
      stopEvents?.();
      stopEvents = subscribeProgress("relay");
    }
    message = SCANNING;
    startButton.hidden = true;
    torchOn = false;
    torchButton.hidden = !hasTorch(stream);
    torchButton.textContent = "Torch";
    void requestWakeLock();
  } catch (err) {
    if (superseded()) return;
    message = explainCameraError(err);
    startButton.hidden = false;
    startButton.disabled = false;
    startButton.textContent = "Start camera";
  } finally {
    if (gen === startGen) {
      cameraSelect.disabled = false;
      fpsSelect.disabled = false;
    }
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
  camFps = {};
  resetHint();
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
fpsSelect.addEventListener("change", () => {
  fpsChoice = parseFpsChoice(fpsSelect.value);
  writePref(FPS_KEY, String(fpsChoice));
  // Reopen the same camera at the new rate: applyConstraints cannot raise a
  // running camera's rate everywhere, a fresh getUserMedia can.
  void startCamera((stream ? activeDeviceId(stream) : undefined) ?? (cameraSelect.value || undefined));
});
startButton.addEventListener("click", () => void startCamera());
$<HTMLButtonElement>("#close-hud").addEventListener("click", onCloseTab);
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
    const held = heldIdentity(sid, join.client);
    const c = await registerClient(sid, token, { role: "relay", resume: held.client_id, resumeKey: held.resumeKey });
    clientID = c.client_id;
    ownName = c.name;
    rememberIdentity(sid, c.client_id, c.resume_key);
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
      rememberIdentity(sid, j.client_id, j.resume_key);
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
  // Preview the HUD for a beam of `total` chunks with `have` received (spread evenly).
  demo: (total: number, have: number) => {
    const bits = new Uint8Array(total);
    for (let i = 0; i < Math.min(have, total); i++) bits[Math.floor((i * total) / Math.max(1, have))] = 1;
    progressEl.textContent = `${have} / ${total}`;
    stateEl.textContent = "receiving";
    stateEl.dataset.state = "RECEIVING";
    chunksEl.hidden = false;
    finderEl.hidden = false;
    renderChunkMarks(chunksEl, "demo", bits);
  },
  // Preview the speed hint the numbers would produce (or a given text), without a camera.
  hint: (inputs: Parameters<typeof speedHint>[0] | string) => {
    hintShown = "";
    renderHint(typeof inputs === "string" ? { id: "beam-headroom", text: inputs } : speedHint(inputs));
  },
};
