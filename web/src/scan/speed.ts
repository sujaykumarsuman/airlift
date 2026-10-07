// Scan speed: the camera frame rate the scanner asks for, the rates it offers,
// and the hint that tells the operator which knob would make the scan faster —
// the beam's fps, the camera's fps, or another camera.

/** "auto" asks for DEFAULT_FPS and lets the browser settle (resolution first);
 *  a number is the operator's pick from the fps menu (that rate first). */
export type FpsChoice = "auto" | number;

/** What "auto" asks for. The beam's screen refreshes at 60 Hz, so 60 is every
 *  frame it can draw; a faster camera only costs exposure (light per frame). */
export const DEFAULT_FPS = 60;

/** The rates the fps menu may offer, highest first. */
const LADDER = [120, 60, 30, 24, 15];

/**
 * The frameRate constraint for a choice. "auto" is only an ideal, so a camera
 * without DEFAULT_FPS at 1080p keeps 1080p at a lower rate (the browser weighs
 * every ideal). A pick is strict — that rate even at a lower resolution — and
 * `strict = false` is the fallback for a camera that cannot meet it.
 */
export function frameRateConstraint(choice: FpsChoice, strict = true): ConstrainDouble {
  if (choice === "auto") return { ideal: DEFAULT_FPS };
  if (!strict) return { ideal: choice };
  return { min: choice * 0.9, ideal: choice, max: choice + 1 };
}

/** The menu's rates for a camera whose capabilities top out at `max` fps
 *  (none when the browser does not report it). */
export function fpsOptions(max: number | undefined): number[] {
  if (!max || !Number.isFinite(max) || max < 10) return [];
  const top = Math.round(max);
  const rates = LADDER.filter((r) => r <= top + 0.5);
  if (top <= Math.max(...LADDER) && !rates.some((r) => Math.abs(r - top) <= 2)) rates.unshift(top);
  return rates;
}

/** Parses a stored or selected choice; anything unknown is "auto". */
export function parseFpsChoice(v: string | null | undefined): FpsChoice {
  const n = Number(v);
  return v && v !== "auto" && Number.isFinite(n) && n >= 1 && n <= 240 ? Math.round(n) : "auto";
}

/** Frame rates that divide a 60 Hz refresh evenly, up to half of it: a beam at
 *  one of these shows every frame for the same number of refreshes. */
const CLEAN_FPS = [30, 20, 15, 12, 10, 6, 5, 4, 3, 2, 1];

/**
 * Reads per beam frame a suggested rate aims for. A sequential beam needs
 * every frame: a frame read once is lost to any tear or blur and waits a whole
 * pass, so 2.5 leaves a margin. A fountain beam needs only enough distinct
 * frames — any fresh packet helps — so 1.5 keeps a little slack for torn
 * captures and spends the rest on new frames.
 */
const TARGET_READS = { sequential: 2.5, fountain: 1.5 };

/** The fastest clean beam rate that `decodesPerSec` still reads `reads` times
 *  a frame. */
export function cleanBeamFps(decodesPerSec: number, reads = TARGET_READS.sequential): number {
  return CLEAN_FPS.find((f) => f * reads <= decodesPerSec) ?? 1;
}

const B45 = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ $%*+-./:";

/** The frame type (0 MANIFEST, 1 DATA, 2 FOUNTAIN — docs/PROTOCOL.md) read
 *  from a decoded string's first header bytes, or null when it is not an
 *  airlift frame. Only for the speed hint: the tower parses and checks frames. */
export function frameType(text: string): number | null {
  const word = (i: number): number => {
    const a = B45.indexOf(text.charAt(i));
    const b = B45.indexOf(text.charAt(i + 1));
    const c = B45.indexOf(text.charAt(i + 2));
    if (a < 0 || b < 0 || c < 0) return -1;
    const v = a + b * 45 + c * 45 * 45;
    return v > 0xffff ? -1 : v;
  };
  if (text.length < 6 || word(0) !== 0x414c) return null;
  const w = word(3);
  return w < 0 || w >> 8 !== 1 ? null : w & 0xff;
}

export interface SpeedInputs {
  /** Frames the camera actually delivers per second (null when unmeasured). */
  cameraFps: number | null;
  /** The rate the camera track is configured for (getSettings). */
  setFps?: number;
  /** The highest rate this camera reports (getCapabilities) — across all its
   *  formats, so not necessarily at the current resolution. */
  maxFps?: number;
  /** The track's resolution, e.g. "1920×1080", and its shorter edge. */
  size?: string;
  shortEdge?: number;
  /** Distinct beam frames decoded per second: the beam's rate as seen. */
  beamFps: number;
  /** QR strings decoded per second, repeats included. */
  decodesPerSec: number;
  /** Camera frames handed to the decoder per second. */
  attemptsPerSec: number;
  lastDecodeMs: number;
  /** Whether the beam in view is a fountain layout (FOUNTAIN frames seen). */
  fountain: boolean;
  /** Whether the fps menu is on screen. */
  fpsMenu: boolean;
  /** Whether the operator picked a rate (not "auto"): a deliberate pick is not
   *  second-guessed. */
  picked: boolean;
  /** Whether there is another camera facing the same way as this one. */
  otherCameras: boolean;
}

export interface SpeedHint {
  /** Which rule fired: the stable part, for debouncing. */
  id:
    | "picked-low-res"
    | "decoder-bound"
    | "camera-below-max"
    | "camera-starved"
    | "missing-frames"
    | "beam-headroom"
    | "lens-capped";
  text: string;
}

const r0 = (n: number) => Math.round(n).toString();
const r1 = (n: number) => (Math.round(n * 10) / 10).toString();

/**
 * The one change most likely to make this scan faster, or null when the
 * numbers say nothing useful (no beam in view, or already balanced).
 *
 * Reads per beam frame — decodes over distinct frames — is the measure. A
 * sequential beam wants every frame read about 2.5 times: far more is room for
 * a faster beam, under 1.5 means some frames are probably missed and wait a
 * whole pass. A fountain beam's speed is the distinct frames read per second,
 * whichever they are, so it is only ever told to go faster — a slower one can
 * only deliver fewer.
 */
export function speedHint(s: SpeedInputs): SpeedHint | null {
  if (s.beamFps < 0.5 || s.decodesPerSec <= 0) return null;
  const reads = s.decodesPerSec / s.beamFps;
  const camFps = s.cameraFps ?? s.setFps ?? 0;
  const capped = s.maxFps !== undefined && s.maxFps < 45;
  const topFps = fpsOptions(s.maxFps)[0]; // the fastest rate the menu offers
  const target = s.fountain ? TARGET_READS.fountain : TARGET_READS.sequential;

  // A picked rate that cost resolution and now decodes poorly: Auto keeps 1080p.
  if (s.picked && s.shortEdge !== undefined && s.shortEdge < 1000 && s.attemptsPerSec > 0 && s.decodesPerSec < s.attemptsPerSec * 0.5) {
    return {
      id: "picked-low-res",
      text: `This rate runs at ${s.size ?? "a lower resolution"} and most frames fail to decode — Auto in the fps menu keeps 1080p.`,
    };
  }
  // A decoder that cannot keep up with the camera: more camera frames are
  // wasted, and a sequential beam should slow to what it can read. (A fountain
  // beam read well over once a frame still gains from going faster: that falls
  // to the headroom rule below.)
  if (reads < (s.fountain ? 1.3 : 2) && camFps > 0 && s.attemptsPerSec < camFps * 0.7) {
    const why = `Decoding is the limit here (${r0(s.lastDecodeMs)} ms a frame)`;
    if (s.fountain) return { id: "decoder-bound", text: `${why}: a faster beam or camera will not help.` };
    const fit = Math.min(cleanBeamFps(s.decodesPerSec, 2), Math.floor(s.beamFps));
    return { id: "decoder-bound", text: `${why}: lower the beam to ${fit} fps so no frame waits a whole pass.` };
  }
  if (s.fpsMenu && !s.picked && topFps !== undefined && s.setFps !== undefined && topFps >= 45 && topFps >= s.setFps * 1.4) {
    return {
      id: "camera-below-max",
      text: `This camera lists ${topFps} fps (now ${r0(s.setFps)}), probably at a lower resolution — try it in the fps menu.`,
    };
  }
  if (s.cameraFps !== null && s.setFps !== undefined && s.setFps > 0 && s.cameraFps < s.setFps * 0.75) {
    return {
      id: "camera-starved",
      text: `The camera gets ${r0(s.cameraFps)} of its ${r0(s.setFps)} fps — too dark: turn the beam screen's brightness up.`,
    };
  }
  if (!s.fountain && reads < 1.5) {
    const fit = Math.min(cleanBeamFps(s.decodesPerSec, 2), Math.floor(s.beamFps));
    const also = capped && s.otherCameras ? ", or switch camera" : "";
    return {
      id: "missing-frames",
      text: `Frames are read only ${r1(reads)}× — some wait a whole pass. Lower the beam to ${fit} fps${also}.`,
    };
  }
  const fit = cleanBeamFps(s.decodesPerSec, target);
  if (fit > s.beamFps * 1.15) {
    return {
      id: "beam-headroom",
      text: `The beam can go faster: set it to ${fit} fps (+ on the beam page, or --fps ${fit}).`,
    };
  }
  if (capped && s.otherCameras) {
    return {
      id: "lens-capped",
      text: `This lens tops out at ${r0(s.maxFps!)} fps; another rear camera may do 60 — try the camera menu.`,
    };
  }
  return null;
}

/** Exponential smoothing of the per-second loop numbers, so one noisy second
 *  (a window boundary splitting a frame's reads) does not flip the hint. */
export class Smoother {
  private v: Record<string, number> = {};
  constructor(private readonly alpha = 0.5) {}
  next<T extends Record<string, number | null>>(sample: T): T {
    const out: Record<string, number | null> = {};
    for (const [k, x] of Object.entries(sample)) {
      if (x === null) {
        out[k] = null;
        continue;
      }
      const prev = this.v[k];
      const y = prev === undefined ? x : prev + this.alpha * (x - prev);
      this.v[k] = y;
      out[k] = y;
    }
    return out as T;
  }
  reset(): void {
    this.v = {};
  }
}

/** Shows a hint only once the same rule has fired `hold` windows running, and
 *  drops it only after `hold` windows without it: a hint that changes every
 *  second is noise. */
export class HintGate {
  private shown: SpeedHint | null = null;
  private pending: SpeedHint["id"] | "none" | null = null;
  private count = 0;
  constructor(private readonly hold = 2) {}
  next(h: SpeedHint | null): SpeedHint | null {
    const id = h?.id ?? "none";
    if ((this.shown?.id ?? "none") === id) {
      this.shown = h; // same rule: refresh its numbers
      this.pending = null;
      this.count = 0;
      return this.shown;
    }
    if (this.pending === id) this.count++;
    else {
      this.pending = id;
      this.count = 1;
    }
    if (this.count >= this.hold) {
      this.shown = h;
      this.pending = null;
      this.count = 0;
    }
    return this.shown;
  }
  reset(): void {
    this.shown = null;
    this.pending = null;
    this.count = 0;
  }
}
