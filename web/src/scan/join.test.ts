import { expect, test } from "vitest";
import { parseJoin } from "./join";

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
