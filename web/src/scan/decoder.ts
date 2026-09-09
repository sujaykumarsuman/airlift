import wasmUrl from "zxing-wasm/reader/zxing_reader.wasm?url";
import type { ReaderOptions } from "zxing-wasm/reader";

export interface Decoder {
  name: string;
  decode(video: HTMLVideoElement, canvas: HTMLCanvasElement): Promise<string[]>;
}

/** Longest edge handed to the WASM decoder; modules stay far above 2 px. */
const MAX_EDGE = 1280;

/** The native BarcodeDetector when it can read QR codes, else zxing-wasm. */
export async function createDecoder(): Promise<Decoder> {
  const BD = (globalThis as { BarcodeDetector?: BarcodeDetectorCtor }).BarcodeDetector;
  if (BD) {
    try {
      if ((await BD.getSupportedFormats()).includes("qr_code")) {
        const det = new BD({ formats: ["qr_code"] });
        return {
          name: "BarcodeDetector",
          decode: async (video) => (await det.detect(video)).map((b) => b.rawValue).filter((t) => t.length > 0),
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
    decode: async (video, canvas) => {
      const image = grab(video, canvas);
      if (!image) return [];
      const results = await readBarcodes(image, options);
      return results.filter((r) => r.isValid && r.text.length > 0).map((r) => r.text);
    },
  };
}

function grab(video: HTMLVideoElement, canvas: HTMLCanvasElement): ImageData | null {
  const vw = video.videoWidth;
  const vh = video.videoHeight;
  if (!vw || !vh) return null;
  const scale = Math.min(1, MAX_EDGE / Math.max(vw, vh));
  const w = Math.round(vw * scale);
  const h = Math.round(vh * scale);
  if (canvas.width !== w || canvas.height !== h) {
    canvas.width = w;
    canvas.height = h;
  }
  const ctx = canvas.getContext("2d", { willReadFrequently: true });
  if (!ctx) return null;
  ctx.drawImage(video, 0, 0, w, h);
  return ctx.getImageData(0, 0, w, h);
}

export interface LoopStats {
  framesPerSec: number; // video frames offered
  decodesPerSec: number; // QR strings decoded
  lastDecodeMs: number;
}

/**
 * Decodes one frame at a time, paced by requestVideoFrameCallback (rAF as a
 * fallback). Returns a stop function.
 */
export function startDecodeLoop(
  video: HTMLVideoElement,
  decoder: Decoder,
  canvas: HTMLCanvasElement,
  onText: (text: string) => void,
  onStats?: (stats: LoopStats) => void,
): () => void {
  let running = true;
  let frames = 0;
  let decodes = 0;
  let lastMs = 0;
  let windowStart = performance.now();
  const next = () => {
    if (!running) return;
    if (typeof video.requestVideoFrameCallback === "function") video.requestVideoFrameCallback(() => void tick());
    else requestAnimationFrame(() => void tick());
  };
  const tick = async () => {
    if (!running) return;
    frames++;
    if (video.readyState >= 2) {
      const t0 = performance.now();
      try {
        for (const text of await decoder.decode(video, canvas)) {
          decodes++;
          onText(text);
        }
      } catch {
        /* a frame that failed to decode is just a missed frame */
      }
      lastMs = performance.now() - t0;
    }
    const now = performance.now();
    if (now - windowStart >= 1000) {
      const secs = (now - windowStart) / 1000;
      onStats?.({ framesPerSec: frames / secs, decodesPerSec: decodes / secs, lastDecodeMs: lastMs });
      frames = 0;
      decodes = 0;
      windowStart = now;
    }
    next();
  };
  next();
  return () => {
    running = false;
  };
}
