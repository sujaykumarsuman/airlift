import { expect, test } from "vitest";
import { minimap, newest, pageCount, pageOf, perPage, PIN_MS, viewPage } from "./chunks";

test("a row holds as many ticks as fit the pitch, never fewer than 8", () => {
  expect(perPage(360)).toBe(52); // (360 + 4) / 7
  expect(perPage(700)).toBe(100);
  expect(perPage(0)).toBe(8);
});

test("pages and page lookup", () => {
  expect(pageCount(674, 56)).toBe(13);
  expect(pageCount(12, 56)).toBe(1);
  expect(pageCount(0, 56)).toBe(1);
  expect(pageOf(0, 56)).toBe(0);
  expect(pageOf(55, 56)).toBe(0);
  expect(pageOf(56, 56)).toBe(1);
});

test("newest is the highest chunk that just flipped on", () => {
  const prev = Uint8Array.from([1, 0, 1, 0, 0]);
  expect(newest(prev, Uint8Array.from([1, 1, 1, 0, 1]))).toBe(4);
  expect(newest(prev, Uint8Array.from([1, 1, 1, 0, 0]))).toBe(1);
  expect(newest(prev, prev)).toBe(-1);
  // no previous frame: everything received counts, so the view lands on the frontier
  expect(newest(null, Uint8Array.from([1, 0, 1, 0, 0]))).toBe(2);
  // a different length is a different beam: compare from scratch
  expect(newest(Uint8Array.from([1]), Uint8Array.from([1, 0, 1]))).toBe(2);
});

test("the view follows the newest chunk unless a recent pin holds it", () => {
  const per = 56;
  const pages = 13;
  expect(viewPage(null, 0, 1000, 300, per, pages)).toBe(5); // 300 / 56
  expect(viewPage(null, 0, 1000, -1, per, pages)).toBe(0);
  expect(viewPage(2, 1000, 1000 + PIN_MS - 1, 300, per, pages)).toBe(2);
  expect(viewPage(2, 1000, 1000 + PIN_MS, 300, per, pages)).toBe(5); // pin expired
  expect(viewPage(99, 1000, 1000, 300, per, pages)).toBe(12); // clamped
});

test("minimap: one pill per page, aggregating past the cap", () => {
  const bits = new Uint8Array(674);
  for (let i = 0; i < 300; i++) bits[i] = 1; // first ~5.4 pages full
  const pips = minimap(bits, 56);
  expect(pips).toHaveLength(13);
  expect(pips[0]).toEqual({ from: 0, to: 56, frac: 1 });
  expect(pips[5]?.frac).toBeCloseTo((300 - 280) / 56);
  expect(pips[12]).toEqual({ from: 672, to: 674, frac: 0 });

  const big = new Uint8Array(10_000); // 200 pages of 50 → 48 max pills → 5 pages each → 40 pills
  const grouped = minimap(big, 50);
  expect(grouped.length).toBeLessThanOrEqual(48);
  expect(grouped).toHaveLength(40);
  expect(grouped[0]).toEqual({ from: 0, to: 250, frac: 0 });
});
