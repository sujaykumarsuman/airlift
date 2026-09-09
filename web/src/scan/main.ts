import "../shared/style.css";
import { eventsURL, postFrames } from "../shared/api";
import { decodeBitmap, drawBitmap } from "../shared/bitmap";
import { $, html, raw } from "../shared/dom";
import { hex8 } from "../shared/format";
import { subscribe, type SSEStatus } from "../shared/sse";
import { isTerminal, type Snapshot } from "../shared/types";
import {
  activeDeviceId,
  describeCamera,
  explainCameraError,
  listCameras,
  openCamera,
  stopStream,
  streamSize,
} from "./camera";
import { createDecoder, startDecodeLoop, type Decoder, type LoopStats } from "./decoder";
import { parseJoin } from "./join";
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

const join = parseJoin(location.pathname, location.hash);
if (join.error !== undefined) {
  progressEl.textContent = "✗";
  stateEl.textContent = "Not a join link";
  messageEl.textContent = join.error;
  startButton.hidden = true;
  cameraSelect.hidden = true;
  throw new Error(join.error);
}
const { sid, token } = join;

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

const relay = new Relay({
  post: (frames) => postFrames(sid, token, frames),
  onUpdate: (s) => {
    relayStats = s;
    render();
  },
});

function render(): void {
  const total = snap?.total ?? 0;
  const have = snap?.have ?? relayStats?.have ?? 0;
  progressEl.textContent = total > 0 ? `${have} / ${total}` : snap ? "waiting for manifest" : "…";
  const state = snap?.state ?? "connecting";
  stateEl.textContent = state.replace("_", " ").toLowerCase();
  stateEl.dataset.state = state;
  if (snap && total > 0) {
    bitmapCanvas.hidden = false;
    drawBitmap(bitmapCanvas, decodeBitmap(snap.bitmap, total), { cell: 6, gap: 1 });
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
  if (snap?.sender_session != null) parts.push(`sender ${hex8(snap.sender_session)}`);
  if (connection !== "open") parts.push(`link: ${connection}`);
  statsEl.innerHTML = html`${parts.map((p) => html`<span>${p}</span>`)}`.html;
  messageEl.textContent = snap?.state === "READY" ? "Done. The tower has the file." : (snap?.error ?? message);
  document.body.dataset.state = state;
}

function subscribeProgress(as: "viewer" | "relay"): () => void {
  role = as;
  return subscribe(
    eventsURL(sid, as),
    token,
    {
      onEvent: (ev) => {
        if (ev.event === "state") {
          snap = JSON.parse(ev.data) as Snapshot;
          if (isTerminal(snap.state)) {
            stopCamera();
            relay.stop();
          }
        } else if (ev.event === "closed") {
          message = "The session was closed on the tower.";
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
navigator.mediaDevices?.addEventListener?.("devicechange", () => void fillCameraList());

stopEvents = subscribeProgress("viewer");
render();
void startCamera();

// Hardware-free testing: inject decoded strings as if the camera saw them.
(window as unknown as { airliftScan: unknown }).airliftScan = {
  relay,
  inject: (texts: string[]) => texts.map((t) => relay.push(t)),
};
