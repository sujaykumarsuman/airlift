import { expect, test } from "vitest";
import { readFileSync } from "node:fs";
import { cleanBeamFps, DEFAULT_FPS, fpsOptions, frameRateConstraint, frameType, HintGate, parseFpsChoice, Smoother, speedHint, type SpeedInputs } from "./speed";

// A balanced 60 fps phone on a 20 fps beam: each frame read ~2.7 times.
const base: SpeedInputs = {
  cameraFps: 60,
  setFps: 60,
  maxFps: 60,
  beamFps: 20,
  decodesPerSec: 54,
  attemptsPerSec: 58,
  lastDecodeMs: 23,
  size: "1920×1080",
  shortEdge: 1080,
  fountain: false,
  fpsMenu: true,
  picked: false,
  otherCameras: true,
};

test("auto asks for 60 fps as an ideal only; a pick is strict with a loose fallback", () => {
  expect(frameRateConstraint("auto")).toEqual({ ideal: DEFAULT_FPS });
  expect(frameRateConstraint("auto", false)).toEqual({ ideal: DEFAULT_FPS });
  expect(frameRateConstraint(30)).toEqual({ min: 27, ideal: 30, max: 31 });
  expect(frameRateConstraint(30, false)).toEqual({ ideal: 30 });
});

test("the fps menu offers the ladder up to the camera's max, plus a max off the ladder", () => {
  expect(fpsOptions(undefined)).toEqual([]);
  expect(fpsOptions(0)).toEqual([]);
  expect(fpsOptions(30)).toEqual([30, 24, 15]);
  expect(fpsOptions(60)).toEqual([60, 30, 24, 15]);
  expect(fpsOptions(59.94)).toEqual([60, 30, 24, 15]);
  expect(fpsOptions(90)).toEqual([90, 60, 30, 24, 15]);
  expect(fpsOptions(25)).toEqual([24, 15]); // 25 is within a hair of 24
  expect(fpsOptions(240)).toEqual([120, 60, 30, 24, 15]);
});

test("a stored or selected choice parses to a whole rate or auto", () => {
  expect(parseFpsChoice(null)).toBe("auto");
  expect(parseFpsChoice("auto")).toBe("auto");
  expect(parseFpsChoice("60")).toBe(60);
  expect(parseFpsChoice("29.97")).toBe(30);
  expect(parseFpsChoice("0")).toBe("auto");
  expect(parseFpsChoice("nope")).toBe("auto");
  expect(parseFpsChoice("1000")).toBe("auto");
});

test("the suggested beam rate is the fastest clean divisor of 60 read enough times a frame", () => {
  expect(cleanBeamFps(55)).toBe(20); // 60 fps camera, sequential (2.5×)
  expect(cleanBeamFps(28)).toBe(10); // 30 fps camera: 12 would be read only 2.3×
  expect(cleanBeamFps(30)).toBe(12);
  expect(cleanBeamFps(80)).toBe(30);
  expect(cleanBeamFps(1)).toBe(1);
  expect(cleanBeamFps(55, 1.5)).toBe(30); // fountain: any fresh frame helps
  expect(cleanBeamFps(28, 1.5)).toBe(15);
});

test("the frame type is read from the base45 header", () => {
  const dump = JSON.parse(readFileSync(new URL("../../../testdata/vectors/vectors-fountain.json", import.meta.url), "utf8")) as { frames: string[] };
  const types = new Set(dump.frames.map(frameType));
  expect(frameType(dump.frames[0]!)).toBe(0); // the manifest leads the loop
  expect(types.has(2)).toBe(true); // FOUNTAIN packets
  expect(types.has(null)).toBe(false);
  expect(frameType("")).toBeNull();
  expect(frameType("HELLO WORLD")).toBeNull();
  expect(frameType("not base45!")).toBeNull();
});

test("no beam in view, no hint; a balanced scan, no hint", () => {
  expect(speedHint({ ...base, beamFps: 0, decodesPerSec: 0 })).toBeNull();
  expect(speedHint(base)).toBeNull();
  expect(speedHint({ ...base, fountain: true, beamFps: 30, decodesPerSec: 50 })).toBeNull();
});

test("spare reads suggest a faster beam, further for a fountain beam", () => {
  const seq = speedHint({ ...base, beamFps: 10, decodesPerSec: 55 });
  expect(seq?.id).toBe("beam-headroom");
  expect(seq?.text).toContain("20 fps");
  expect(seq?.text).toContain("--fps 20");
  const fountain = speedHint({ ...base, fountain: true, beamFps: 10, decodesPerSec: 55 });
  expect(fountain?.id).toBe("beam-headroom");
  expect(fountain?.text).toContain("30 fps");
});

test("a camera running below the menu's top rate says to try it, unless the rate was picked", () => {
  const h = speedHint({ ...base, setFps: 30, cameraFps: 30, beamFps: 10, decodesPerSec: 28, attemptsPerSec: 29 });
  expect(h?.id).toBe("camera-below-max");
  expect(h?.text).toContain("60 fps");
  expect(h?.text).toContain("lower resolution");
  const picked = speedHint({ ...base, setFps: 30, cameraFps: 30, beamFps: 10, decodesPerSec: 28, attemptsPerSec: 29, picked: true });
  expect(picked).toBeNull(); // 28 reads/s on a 10 fps beam is 2.8× — balanced
  expect(speedHint({ ...base, setFps: 30, cameraFps: 30, beamFps: 10, decodesPerSec: 28, fpsMenu: false })?.id).not.toBe("camera-below-max");
  // a camera reporting 240 is offered the menu's 120, not a rate it cannot pick
  const fast = speedHint({ ...base, maxFps: 240, beamFps: 20, decodesPerSec: 54 });
  expect(fast?.id).toBe("camera-below-max");
  expect(fast?.text).toContain("120 fps");
});

test("a picked rate that cost resolution and fails to decode points back to Auto", () => {
  const h = speedHint({ ...base, picked: true, size: "1280×720", shortEdge: 720, beamFps: 10, decodesPerSec: 20, attemptsPerSec: 58 });
  expect(h?.id).toBe("picked-low-res");
  expect(h?.text).toContain("1280×720");
  expect(speedHint({ ...base, picked: true, size: "1280×720", shortEdge: 720, beamFps: 20, decodesPerSec: 54 })).toBeNull();
});

test("a lens capped at 30 fps points at another camera facing the same way", () => {
  const h = speedHint({ ...base, setFps: 30, maxFps: 30, cameraFps: 30, beamFps: 10, decodesPerSec: 27, attemptsPerSec: 29 });
  expect(h?.id).toBe("lens-capped");
  expect(speedHint({ ...base, setFps: 30, maxFps: 30, cameraFps: 30, beamFps: 10, decodesPerSec: 27, otherCameras: false })).toBeNull();
});

test("a camera starved of light says so", () => {
  const h = speedHint({ ...base, cameraFps: 31, attemptsPerSec: 30, decodesPerSec: 28, beamFps: 10 });
  expect(h?.id).toBe("camera-starved");
  expect(h?.text).toContain("31 of its 60");
});

test("a sequential beam read too few times is told to slow down, never below what it reads", () => {
  const dec = speedHint({ ...base, beamFps: 15, decodesPerSec: 14, attemptsPerSec: 14, lastDecodeMs: 120 });
  expect(dec?.id).toBe("decoder-bound");
  expect(dec?.text).toContain("120 ms");
  expect(dec?.text).toContain("lower the beam to 6 fps");
  const miss = speedHint({ ...base, beamFps: 30, decodesPerSec: 40, attemptsPerSec: 58 });
  expect(miss?.id).toBe("missing-frames");
  expect(miss?.text).toContain("1.3×");
  expect(miss?.text).toContain("20 fps");
});

test("a fountain beam is never told to slow down", () => {
  // iOS zxing at ~100 ms on the default 10 fps beam: every decode is a fresh packet
  const ios = speedHint({ ...base, fountain: true, cameraFps: 30, setFps: 30, maxFps: 30, beamFps: 8, decodesPerSec: 8.5, attemptsPerSec: 10, lastDecodeMs: 100, otherCameras: false });
  expect(ios?.id).toBe("decoder-bound");
  expect(ios?.text).not.toContain("lower");
  // a fast fountain beam read 1.3× is not "missing frames"
  expect(speedHint({ ...base, fountain: true, beamFps: 30, decodesPerSec: 40, attemptsPerSec: 58 })).toBeNull();
  // decoder-bound but reads 1.8×: a faster fountain beam still gains
  const room = speedHint({ ...base, fountain: true, cameraFps: 60, beamFps: 10, decodesPerSec: 18, attemptsPerSec: 20 });
  expect(room?.id).toBe("beam-headroom");
  expect(room?.text).toContain("12 fps");
});

test("without a measured camera rate the configured one stands in", () => {
  expect(speedHint({ ...base, cameraFps: null, beamFps: 15, decodesPerSec: 14, attemptsPerSec: 14 })?.id).toBe("decoder-bound");
});

test("the smoother averages, passes nulls through and starts fresh after reset", () => {
  const s = new Smoother(0.5);
  expect(s.next({ a: 10, b: null })).toEqual({ a: 10, b: null });
  expect(s.next({ a: 20, b: null })).toEqual({ a: 15, b: null });
  s.reset();
  expect(s.next({ a: 4, b: 2 })).toEqual({ a: 4, b: 2 });
});

test("the gate shows a rule only after it holds, refreshes its text, and lets go after it stops", () => {
  const g = new HintGate(2);
  const a1 = { id: "beam-headroom", text: "a1" } as const;
  const a2 = { id: "beam-headroom", text: "a2" } as const;
  const b = { id: "lens-capped", text: "b" } as const;
  expect(g.next(a1)).toBeNull();
  expect(g.next(a2)).toEqual(a2);
  expect(g.next(a1)).toEqual(a1); // same rule: new numbers at once
  expect(g.next(b)).toEqual(a1); // one second of another rule is not enough
  expect(g.next(a1)).toEqual(a1);
  expect(g.next(null)).toEqual(a1);
  expect(g.next(null)).toBeNull();
  g.reset();
  expect(g.next(b)).toBeNull();
});
