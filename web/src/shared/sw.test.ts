import { readFileSync } from "node:fs";
import { expect, test } from "vitest";

const src = readFileSync(new URL("../../public/sw.js", import.meta.url), "utf8");

test("the service worker parses and never touches the API", () => {
  expect(() => new Function(src)).not.toThrow();
  expect(src).toMatch(/startsWith\("\/api\/"\)/);
  expect(src).toMatch(/\/ca\.crt/);
  expect(src).toMatch(/req\.method !== "GET"/);
});
