import "../shared/style.css";
import { eventsURL, joinSession, postFrames, registerClient } from "../shared/api";
import { decodeBitmap, drawBitmap } from "../shared/bitmap";
import { $, html, raw } from "../shared/dom";
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

const relay = new Relay({
  post: (frames) => postFrames(sid, token, frames, clientID),
  onUpdate: (s) => {
    relayStats = s;
    render();
  },
});

/** A short message for a terminated session. */
function terminatedNote(s: Snapshot): string {
  return s.terminated?.reason === "terminated by session admin"
    ? "The session was ended by an admin."
    : "The session has ended (timed out).";
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
  messageEl.textContent = beam?.state === "READY" ? "Beam received. Point at the next, or stop." : (beam?.error ?? message);
  document.body.dataset.state = state;
}

function subscribeProgress(as: "viewer" | "relay"): () => void {
  role = as;
  return subscribe(
    eventsURL(sid, as),
    token,
    {
      onEvent: (ev) => {
        if (ev.event === "state" || ev.event === "terminated") {
          // A place stays open across beams; keep relaying whatever the camera
          // decodes so the operator can move on to the next beam. But a
          // TERMINATED session freezes the transfer (frames now 409), so stop
          // relaying and let the operator know.
          snap = JSON.parse(ev.data) as Snapshot;
          if (snap.status === "TERMINATED") {
            message = terminatedNote(snap);
            stopCamera();
            relay.stop();
          }
        } else if (ev.event === "closed") {
          message = "The session was closed on the tower.";
          stopCamera();
          relay.stop();
        } else if (ev.event === "evicted") {
          message = "You were removed from this session.";
          stopCamera();
          relay.stop();
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
      render();
      void startCamera();
    })
    .catch((err) => {
      message = err instanceof Error ? err.message : String(err);
      render();
    });
});

void init();

// Hardware-free testing: inject decoded strings as if the camera saw them.
(window as unknown as { airliftScan: unknown }).airliftScan = {
  relay,
  inject: (texts: string[]) => texts.map((t) => relay.push(t)),
};
