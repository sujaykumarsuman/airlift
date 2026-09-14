import { expect, test } from "vitest";
import { LAST_KEY, parseJoin, resolveJoin } from "./join";

test("parses sid from the path and the token from the fragment", () => {
  expect(parseJoin("/s/0123456789abcdef", "#t=IArmTtXFjcbAMpWOwO6NAg")).toEqual({
    sid: "0123456789abcdef",
    token: "IArmTtXFjcbAMpWOwO6NAg",
  });
  expect(parseJoin("/s/abc/", "#t=IArmTtXFjcbAMpWOwO6NAg&x=1")).toEqual({ sid: "abc", token: "IArmTtXFjcbAMpWOwO6NAg" });
  // c= is the dashboard's client id for the scanner to resume (ADR 0022); anything else is ignored.
  expect(parseJoin("/s/abc", "#t=IArmTtXFjcbAMpWOwO6NAg&c=0123456789abcdef")).toEqual({
    sid: "abc",
    token: "IArmTtXFjcbAMpWOwO6NAg",
    client: "0123456789abcdef",
  });
  expect(parseJoin("/s/abc", "#t=IArmTtXFjcbAMpWOwO6NAg&c=nope").client).toBeUndefined();
});

test("rejects a missing sid, and offers a password join when the token is absent", () => {
  expect(parseJoin("/", "#t=IArmTtXFjcbAMpWOwO6NAg").error).toMatch(/not a join link/);
  expect(parseJoin("/s/", "#t=IArmTtXFjcbAMpWOwO6NAg").error).toMatch(/not a join link/);
  expect(parseJoin("/s/abc/extra", "#t=IArmTtXFjcbAMpWOwO6NAg").error).toBeDefined();
  expect(parseJoin("/s/../x", "#t=IArmTtXFjcbAMpWOwO6NAg").error).toBeDefined();
  // A valid /s/{sid} with no usable token → the password-join path.
  for (const hash of ["", "#t=", "#t=short", "#t=has space in it!"]) {
    expect(parseJoin("/s/abc", hash)).toEqual({ sid: "abc", needsPassword: true });
  }
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
    redirect: "s/abc#t=IArmTtXFjcbAMpWOwO6NAg", // app-relative; resolved against <base>
  });
  expect(resolveJoin("/s/last/", "#t=ignored", store).sid).toBe("abc");
  expect(resolveJoin("/s/last", "", memory()).error).toMatch(/No previous session/);
  expect(resolveJoin("/s/last", "", null).error).toMatch(/No previous session/);
  const broken = memory();
  broken.set(LAST_KEY, "{not json");
  expect(resolveJoin("/s/last", "", broken).error).toBeDefined();
  expect(resolveJoin("/s/nope", "", store).needsPassword).toBe(true);
  expect(JSON.parse(store.get(LAST_KEY) ?? "").sid).toBe("abc"); // a token-less join does not overwrite
});

test("a path prefix is stripped before matching /s/…", () => {
  expect(parseJoin("/airlift/s/abc", "#t=IArmTtXFjcbAMpWOwO6NAg", "/airlift")).toEqual({
    sid: "abc",
    token: "IArmTtXFjcbAMpWOwO6NAg",
  });
  const store = memory();
  expect(resolveJoin("/airlift/s/abc", "#t=IArmTtXFjcbAMpWOwO6NAg", store, "/airlift").sid).toBe("abc");
  expect(resolveJoin("/airlift/s/last", "", store, "/airlift").redirect).toBe("s/abc#t=IArmTtXFjcbAMpWOwO6NAg");
});
