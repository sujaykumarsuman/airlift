import { frameRateConstraint, type FpsChoice } from "./speed";

export function listCameras(): Promise<MediaDeviceInfo[]> {
  return navigator.mediaDevices.enumerateDevices().then((all) => all.filter((d) => d.kind === "videoinput"));
}

/**
 * Opens a camera: the given device, else the rear camera; 1080p, which is
 * plenty for a version-30 symbol filling the viewfinder (~5 px per module)
 * and a quarter of the pixels of 4K for every frame the decoder reads; at the
 * frame rate `fps` asks for (60 by default: more reads of every beam frame, so
 * the beam can run faster); continuous focus where the track supports it. A
 * picked rate the camera cannot meet falls back to the nearest it can.
 */
export async function openCamera(deviceId?: string, fps: FpsChoice = "auto"): Promise<MediaStream> {
  const video: MediaTrackConstraints = deviceId
    ? { deviceId: { exact: deviceId } }
    : { facingMode: { ideal: "environment" } };
  video.width = { ideal: 1920 };
  video.height = { ideal: 1080 };
  video.frameRate = frameRateConstraint(fps);
  let stream: MediaStream;
  try {
    stream = await navigator.mediaDevices.getUserMedia({ video, audio: false });
  } catch (err) {
    if (fps === "auto" || !(err instanceof Error) || err.name !== "OverconstrainedError") throw err;
    video.frameRate = frameRateConstraint(fps, false);
    stream = await navigator.mediaDevices.getUserMedia({ video, audio: false });
  }
  await applyContinuousFocus(stream);
  return stream;
}

export async function applyContinuousFocus(stream: MediaStream): Promise<boolean> {
  const track = stream.getVideoTracks()[0];
  if (!track || typeof track.getCapabilities !== "function") return false;
  const caps = track.getCapabilities() as MediaTrackCapabilities & { focusMode?: string[] };
  if (!caps.focusMode?.includes("continuous")) return false;
  const focus: MediaTrackConstraintSet & { focusMode?: string } = { focusMode: "continuous" };
  try {
    await track.applyConstraints({ advanced: [focus] });
    return true;
  } catch {
    return false;
  }
}

export function activeDeviceId(stream: MediaStream): string | undefined {
  return stream.getVideoTracks()[0]?.getSettings().deviceId;
}

export function streamSize(stream: MediaStream): string {
  const s = stream.getVideoTracks()[0]?.getSettings();
  return s?.width && s.height ? `${s.width}×${s.height}` : "";
}

/** The frame rate the track runs at (`set`) and the most this camera reports
 *  (`max`); either is undefined where the browser does not say. */
export function streamFrameRate(stream: MediaStream): { set?: number; max?: number } {
  const track = stream.getVideoTracks()[0];
  if (!track) return {};
  const set = track.getSettings().frameRate;
  let max: number | undefined;
  try {
    max = typeof track.getCapabilities === "function" ? track.getCapabilities().frameRate?.max : undefined;
  } catch {
    max = undefined;
  }
  return { set: set && set > 0 ? set : undefined, max: max && max > 0 ? max : undefined };
}

export function stopStream(stream: MediaStream): void {
  for (const t of stream.getTracks()) t.stop();
}

export type Facing = "user" | "environment";

/** Which way a camera faces, from its label ("camera2 0, facing back", "Back
 *  Ultra Wide Camera", "Front Camera", "Camera 1, Facing front, …"). */
export function facingFromLabel(label: string): Facing | undefined {
  if (/\bfront\b|\buser\b|selfie/i.test(label)) return "user";
  if (/\bback\b|\brear\b|\benvironment\b/i.test(label)) return "environment";
  return undefined;
}

function facingOf(d: MediaDeviceInfo): Facing | undefined {
  try {
    const caps = (d as MediaDeviceInfo & { getCapabilities?: () => { facingMode?: string[] } }).getCapabilities?.();
    const f = caps?.facingMode?.[0];
    if (f === "user" || f === "environment") return f;
  } catch {
    /* not an InputDeviceInfo here: fall back to the label */
  }
  return facingFromLabel(d.label);
}

/**
 * Whether another camera faces the same way as the active one — the only
 * kind worth switching to for a beam. Where facing cannot be told (desktop
 * webcams), any second camera counts.
 */
export function hasOtherSameFacing(cams: MediaDeviceInfo[], activeId: string | undefined, activeFacing?: string): boolean {
  const others = cams.filter((c) => c.deviceId !== activeId);
  const active = cams.find((c) => c.deviceId === activeId);
  const facing = activeFacing === "user" || activeFacing === "environment" ? activeFacing : active ? facingOf(active) : undefined;
  if (!facing) return others.length > 0;
  return others.some((c) => facingOf(c) === facing);
}

export function describeCamera(d: MediaDeviceInfo, index: number): string {
  return d.label || `Camera ${index + 1}`;
}

export function explainCameraError(err: unknown): string {
  const name = err instanceof Error ? err.name : "";
  switch (name) {
    case "NotAllowedError":
    case "SecurityError":
      return "Camera permission was refused. Allow the camera for this site and tap Start.";
    case "NotFoundError":
    case "OverconstrainedError":
      return "No camera found: this device is viewing only.";
    case "NotReadableError":
      return "The camera is in use by another app.";
    default:
      return err instanceof Error ? `Camera error: ${err.message}` : "Camera error.";
  }
}

export function hasTorch(stream: MediaStream): boolean {
  const track = stream.getVideoTracks()[0];
  if (!track || typeof track.getCapabilities !== "function") return false;
  const caps = track.getCapabilities() as MediaTrackCapabilities & { torch?: boolean };
  return caps.torch === true;
}

export async function setTorch(stream: MediaStream, on: boolean): Promise<boolean> {
  const track = stream.getVideoTracks()[0];
  if (!track) return false;
  const torch: MediaTrackConstraintSet & { torch?: boolean } = { torch: on };
  try {
    await track.applyConstraints({ advanced: [torch] });
    return true;
  } catch {
    return false;
  }
}
