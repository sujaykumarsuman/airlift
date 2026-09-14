import wasmUrl from "zxing-wasm/reader/zxing_reader.wasm?url";
import type { ReaderOptions } from "zxing-wasm/reader";
import type { ROI } from "./roi";

export interface Decoder {
  name: string;
  /** Decodes one camera frame: the `roi` crop of it when given, else the whole frame. */
  decode(video: HTMLVideoElement, canvas: HTMLCanvasElement, roi: ROI | null): Promise<string[]>;
}

/** Longest edge handed to a decoder. A finder crop bigger than this is scaled
 *  down first: a version-30 symbol filling the finder keeps ~5 px per module at
 *  1080p (more from a larger stream), and the decoders' cost scales with pixels,
 *  not with what is on them. */
const MAX_EDGE = 1024;

/** The native BarcodeDetector when it can read QR codes, else zxing-wasm. */
export async function createDecoder(): Promise<Decoder> {
  const BD = (globalThis as { BarcodeDetector?: BarcodeDetectorCtor }).BarcodeDetector;
  if (BD) {
    try {
      if ((await BD.getSupportedFormats()).includes("qr_code")) {
        const det = new BD({ formats: ["qr_code"] });
        return {
          name: "BarcodeDetector",
          decode: async (video, _canvas, roi) => {
            // The crop is cut on the GPU; only it crosses to the detector.
            const crop = roi ? await cropBitmap(video, roi) : null;
            try {
              return (await det.detect(crop ?? video)).map((b) => b.rawValue).filter((t) => t.length > 0);
            } finally {
              crop?.close();
            }
          },
        };
      }
    } catch {
      /* fall through to WASM */
    }
  }
  const { prepareZXingModule, readBarcodes } = await import("zxing-wasm/reader");
  await prepareZXingModule({
    overrides: {
      locateFile: (path: string, prefix: string) => (path.endsWith(".wasm") ? wasmUrl : prefix + path),
    },
    fireImmediately: true,
  });
  const options: ReaderOptions = {
    formats: ["QRCode"],
    tryHarder: false,
    tryRotate: false,
    tryInvert: false,
    maxNumberOfSymbols: 1,
  };
  return {
    name: "zxing-wasm",
    decode: async (video, canvas, roi) => {
      const image = grab(video, canvas, roi); // copied out synchronously, so the canvas is free again at once
      if (!image) return [];
      const results = await readBarcodes(image, options);
      return results.filter((r) => r.isValid && r.text.length > 0).map((r) => r.text);
    },
  };
}

/** The finder crop as an ImageBitmap scaled to at most MAX_EDGE; null when the
 *  browser cannot crop (the caller then reads the whole frame). */
async function cropBitmap(video: HTMLVideoElement, roi: ROI): Promise<ImageBitmap | null> {
  const scale = Math.min(1, MAX_EDGE / Math.max(roi.w, roi.h));
  try {
    return await createImageBitmap(video, roi.x, roi.y, roi.w, roi.h, {
      resizeWidth: Math.max(1, Math.round(roi.w * scale)),
      resizeHeight: Math.max(1, Math.round(roi.h * scale)),
      resizeQuality: "low",
    });
  } catch {
    return null;
  }
}

/** Draws the crop (or the whole frame) into the canvas at ≤ MAX_EDGE and reads it back. */
function grab(video: HTMLVideoElement, canvas: HTMLCanvasElement, roi: ROI | null): ImageData | null {
  const vw = video.videoWidth;
  const vh = video.videoHeight;
  if (!vw || !vh) return null;
  const sx = roi?.x ?? 0;
  const sy = roi?.y ?? 0;
  const sw = roi?.w ?? vw;
  const sh = roi?.h ?? vh;
  const scale = Math.min(1, MAX_EDGE / Math.max(sw, sh));
  const w = Math.max(1, Math.round(sw * scale));
  const h = Math.max(1, Math.round(sh * scale));
  if (canvas.width !== w || canvas.height !== h) {
    canvas.width = w;
    canvas.height = h;
  }
  const ctx = canvas.getContext("2d", { willReadFrequently: true });
  if (!ctx) return null;
  ctx.drawImage(video, sx, sy, sw, sh, 0, 0, w, h);
  return ctx.getImageData(0, 0, w, h);
}

export interface LoopStats {
  framesPerSec: number; // camera frames offered
  attemptsPerSec: number; // frames handed to the decoder
  decodesPerSec: number; // QR strings decoded
  lastDecodeMs: number;
}

/** Decodes at most this many frames at once: the native detector runs out of
 *  process, so a second frame can be in flight while the first is read. */
const MAX_IN_FLIGHT = 2;
/** Once the crop has gone this long without a hit, every other attempt reads
 *  the whole frame instead, so a code held outside the square still scans (at
 *  half rate); the crop takes over again the moment it hits. */
const FULL_FRAME_AFTER_MS = 1000;

/**
 * Feeds camera frames to the decoder, paced by requestVideoFrameCallback (rAF
 * as a fallback): every new frame is offered, up to MAX_IN_FLIGHT are decoded
 * at a time, the rest are dropped. `roi` gives the finder crop for a frame
 * (null to read all of it). Returns a stop function; a decode still in flight
 * when it is called is discarded.
 */
export function startDecodeLoop(
  video: HTMLVideoElement,
  decoder: Decoder,
  canvas: HTMLCanvasElement,
  onText: (text: string) => void,
  onStats?: (stats: LoopStats) => void,
  roi?: () => ROI | null,
): () => void {
  let running = true;
  let frames = 0;
  let attempts = 0;
  let decodes = 0;
  let lastMs = 0;
  let inFlight = 0;
  let lastCropHit = performance.now();
  let windowStart = performance.now();
  const schedule = () => {
    if (!running) return;
    if (typeof video.requestVideoFrameCallback === "function") video.requestVideoFrameCallback(() => tick());
    else requestAnimationFrame(() => tick());
  };
  const tick = () => {
    if (!running) return;
    schedule(); // the next camera frame is wanted whatever happens to this one
    frames++;
    if (video.readyState >= 2 && inFlight < MAX_IN_FLIGHT) {
      const now = performance.now();
      let region = roi?.() ?? null;
      const cropDry = now - lastCropHit > FULL_FRAME_AFTER_MS;
      if (region && cropDry && attempts % 2 === 1) region = null;
      inFlight++;
      attempts++;
      const cropped = region !== null;
      decoder
        .decode(video, canvas, region)
        .then(
          (texts) => {
            if (!running) return;
            for (const text of texts) {
              decodes++;
              if (cropped) lastCropHit = performance.now();
              onText(text);
            }
          },
          () => {
            /* a frame that failed to decode is just a missed frame */
          },
        )
        .finally(() => {
          inFlight--;
          lastMs = performance.now() - now;
        });
    }
    const now = performance.now();
    if (now - windowStart >= 1000) {
      const secs = (now - windowStart) / 1000;
      onStats?.({ framesPerSec: frames / secs, attemptsPerSec: attempts / secs, decodesPerSec: decodes / secs, lastDecodeMs: lastMs });
      frames = 0;
      attempts = 0;
      decodes = 0;
      windowStart = now;
    }
  };
  schedule();
  return () => {
    running = false;
  };
}
