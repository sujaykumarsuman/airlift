import { readFileSync } from "node:fs";
import { expect, test } from "vitest";

const src = readFileSync(new URL("../../public/sw.js", import.meta.url), "utf8");

test("the service worker parses, derives its base, and never touches the API", () => {
  expect(() => new Function(src)).not.toThrow();
  expect(src).toMatch(/const BASE = new URL\("\.\/", self\.location\)\.pathname/);
  expect(src).toMatch(/startsWith\(BASE \+ "api\/"\)/);
  expect(src).not.toMatch(/ca\.crt/);
  expect(src).toMatch(/req\.method !== "GET"/);
});
