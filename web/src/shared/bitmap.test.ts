import { expect, test } from "vitest";
import { decodeBitmap } from "./bitmap";

test("decodes MSB-first bit-packed base64", () => {
  // 0b10100000 0b01000000 → chunks 0, 2 and 9 present out of 10
  const b64 = btoa(String.fromCharCode(0b10100000, 0b01000000));
  expect(Array.from(decodeBitmap(b64, 10))).toEqual([1, 0, 1, 0, 0, 0, 0, 0, 0, 1]);
  expect(Array.from(decodeBitmap("", 3))).toEqual([0, 0, 0]);
  expect(decodeBitmap("/w==", 0).length).toBe(0);
  expect(Array.from(decodeBitmap("/w==", 8))).toEqual([1, 1, 1, 1, 1, 1, 1, 1]);
});
