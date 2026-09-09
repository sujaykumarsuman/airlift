export function listCameras(): Promise<MediaDeviceInfo[]> {
  return navigator.mediaDevices.enumerateDevices().then((all) => all.filter((d) => d.kind === "videoinput"));
}

/**
 * Opens a camera: the given device, else the rear camera; the highest
 * resolution on offer; continuous focus where the track supports it.
 */
export async function openCamera(deviceId?: string): Promise<MediaStream> {
  const video: MediaTrackConstraints = deviceId
    ? { deviceId: { exact: deviceId } }
    : { facingMode: { ideal: "environment" } };
  video.width = { ideal: 4096 };
  video.height = { ideal: 2160 };
  const stream = await navigator.mediaDevices.getUserMedia({ video, audio: false });
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

export function stopStream(stream: MediaStream): void {
  for (const t of stream.getTracks()) t.stop();
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
