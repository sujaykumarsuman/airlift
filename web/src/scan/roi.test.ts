import { expect, test } from "vitest";
import { finderROI } from "./roi";

// A 390×844 phone showing a 1080×1920 portrait stream: cover scales by 844/1920
// and crops 42 px off each side; the finder is min(78vw, 44vh) = 304 px, centred.
const phone = { vw: 1080, vh: 1920, cssW: 390, cssH: 844 };
const finder = { left: 43, top: 270, width: 304, height: 304 };

test("maps the centred finder to a square crop of the frame, grown by the margin", () => {
  const roi = finderROI(phone.vw, phone.vh, phone.cssW, phone.cssH, finder);
  expect(roi).not.toBeNull();
  const { x, y, w, h } = roi!;
  // 304 CSS px is 692 frame px; a 12 % margin each side makes it ~858.
  expect(w).toBeGreaterThan(840);
  expect(w).toBeLessThan(880);
  expect(Math.abs(w - h)).toBeLessThanOrEqual(2);
  // centred horizontally in the 1080-wide frame and vertically in the 1920-tall one
  expect(Math.abs(x + w / 2 - 540)).toBeLessThan(3);
  expect(Math.abs(y + h / 2 - 960)).toBeLessThan(3);
});

test("a landscape stream on a portrait screen crops the sides, not the top", () => {
  // 1920×1080 stream shown cover in 390×844: s = 844/1080, ox is hugely negative.
  const roi = finderROI(1920, 1080, 390, 844, finder)!;
  expect(roi.y).toBeGreaterThanOrEqual(0);
  expect(roi.y + roi.h).toBeLessThanOrEqual(1080);
  expect(roi.x).toBeGreaterThan(0);
  expect(roi.x + roi.w).toBeLessThan(1920);
});

test("clamps to the frame when the finder reaches an edge", () => {
  const roi = finderROI(phone.vw, phone.vh, phone.cssW, phone.cssH, { left: -20, top: 0, width: 430, height: 430 })!;
  expect(roi.x).toBe(0);
  expect(roi.y).toBe(0);
  expect(roi.x + roi.w).toBeLessThanOrEqual(phone.vw);
  expect(roi.y + roi.h).toBeLessThanOrEqual(phone.vh);
});

test("returns null without a frame, a box or a usable finder", () => {
  expect(finderROI(0, 0, 390, 844, finder)).toBeNull();
  expect(finderROI(1080, 1920, 0, 0, finder)).toBeNull();
  expect(finderROI(1080, 1920, 390, 844, { left: 0, top: 0, width: 0, height: 0 })).toBeNull();
  expect(finderROI(1080, 1920, 390, 844, { left: 5000, top: 5000, width: 10, height: 10 })).toBeNull();
});
