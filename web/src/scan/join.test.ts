import { expect, test } from "vitest";
import { LAST_KEY, parseJoin, resolveJoin } from "./join";

test("parses sid from the path and the token from the fragment", () => {
  expect(parseJoin("/s/0123456789abcdef", "#t=IArmTtXFjcbAMpWOwO6NAg")).toEqual({
    sid: "0123456789abcdef",
    token: "IArmTtXFjcbAMpWOwO6NAg",
  });
  expect(parseJoin("/s/abc/", "#t=IArmTtXFjcbAMpWOwO6NAg&x=1")).toEqual({ sid: "abc", token: "IArmTtXFjcbAMpWOwO6NAg" });
});

test("rejects a missing sid or token", () => {
  expect(parseJoin("/", "#t=IArmTtXFjcbAMpWOwO6NAg").error).toMatch(/not a join link/);
  expect(parseJoin("/s/", "#t=IArmTtXFjcbAMpWOwO6NAg").error).toMatch(/not a join link/);
  expect(parseJoin("/s/abc/extra", "#t=IArmTtXFjcbAMpWOwO6NAg").error).toBeDefined();
  expect(parseJoin("/s/abc", "").error).toMatch(/token/);
  expect(parseJoin("/s/abc", "#t=").error).toMatch(/token/);
  expect(parseJoin("/s/abc", "#t=short").error).toMatch(/token/);
  expect(parseJoin("/s/abc", "#t=has space in it!").error).toMatch(/token/);
  expect(parseJoin("/s/../x", "#t=IArmTtXFjcbAMpWOwO6NAg").error).toBeDefined();
});

function memory(): Map<string, string> & { getItem(k: string): string | null; setItem(k: string, v: string): void } {
  const m = new Map<string, string>() as Map<string, string> & { getItem(k: string): string | null; setItem(k: string, v: string): void };
  m.getItem = (k) => m.get(k) ?? null;
  m.setItem = (k, v) => void m.set(k, v);
  return m;
}

test("a join is remembered and /s/last reopens it", () => {
  const store = memory();
  expect(resolveJoin("/s/abc", "#t=IArmTtXFjcbAMpWOwO6NAg", store)).toEqual({ sid: "abc", token: "IArmTtXFjcbAMpWOwO6NAg" });
  expect(JSON.parse(store.get(LAST_KEY) ?? "")).toEqual({ sid: "abc", token: "IArmTtXFjcbAMpWOwO6NAg" });
  expect(resolveJoin("/s/last", "", store)).toEqual({
    sid: "abc",
    token: "IArmTtXFjcbAMpWOwO6NAg",
    redirect: "/s/abc#t=IArmTtXFjcbAMpWOwO6NAg",
  });
  expect(resolveJoin("/s/last/", "#t=ignored", store).sid).toBe("abc");
  expect(resolveJoin("/s/last", "", memory()).error).toMatch(/No previous session/);
  expect(resolveJoin("/s/last", "", null).error).toMatch(/No previous session/);
  const broken = memory();
  broken.set(LAST_KEY, "{not json");
  expect(resolveJoin("/s/last", "", broken).error).toBeDefined();
  expect(resolveJoin("/s/nope", "", store).error).toMatch(/token/);
  expect(JSON.parse(store.get(LAST_KEY) ?? "").sid).toBe("abc"); // a failed join does not overwrite
});
