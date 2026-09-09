import { expect, test } from "vitest";
import { VERSION } from "./version";

test("noop", () => {
  // Placeholder so vitest runs from Phase 0.
  expect(VERSION).toBeTruthy();
});
