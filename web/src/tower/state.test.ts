import { expect, test } from "vitest";
import type { Snapshot } from "../shared/types";
import { failedStage, initialView, parseDeepLink, reduce, tick } from "./state";

const base: Snapshot = {
  sid: "s",
  state: "WAITING_MANIFEST",
  sender_session: null,
  name: "",
  total: 0,
  have: 0,
  bitmap: "",
  fps: 0,
  relays: 0,
  verdicts: { gz_sha: null, orig_sha: null, bundle: null },
  bundle: null,
  downloads: [],
  dest_path: null,
  error: null,
  expires_at: "2026-09-09T12:00:00Z",
};

test("elapsed starts with the first frames, ETA follows fps, and freezes when done", () => {
  let v = reduce(initialView, base, 1000);
  expect(v).toMatchObject({ startedAt: null, elapsedMs: 0, etaSec: null, pct: 0 });
  v = reduce(v, { ...base, state: "RECEIVING", total: 100, have: 10, fps: 5 }, 2000);
  expect(v).toMatchObject({ startedAt: 2000, elapsedMs: 0, etaSec: 18, pct: 10 });
  v = tick(v, 5000);
  expect(v.elapsedMs).toBe(3000);
  v = reduce(v, { ...base, state: "RECEIVING", total: 100, have: 60, fps: 0 }, 7000);
  expect(v).toMatchObject({ elapsedMs: 5000, etaSec: null, pct: 60 });
  v = reduce(v, { ...base, state: "VERIFYING", total: 100, have: 100, fps: 4 }, 8000);
  expect(v.etaSec).toBeNull();
  v = reduce(v, { ...base, state: "READY", total: 100, have: 100 }, 9000);
  expect(v).toMatchObject({ finishedAt: 9000, elapsedMs: 7000, pct: 100 });
  expect(tick(v, 20000).elapsedMs).toBe(7000);
});

test("failedStage picks the first failing verdict", () => {
  const ok = { ok: true, expected: "a", actual: "a" };
  const bad = { ok: false, expected: "a", actual: "b" };
  expect(failedStage(base)).toBeNull();
  expect(failedStage({ ...base, verdicts: { gz_sha: bad, orig_sha: null, bundle: null } })).toBe("gz_sha");
  expect(failedStage({ ...base, verdicts: { gz_sha: ok, orig_sha: bad, bundle: null } })).toBe("orig_sha");
  expect(failedStage({ ...base, verdicts: { gz_sha: ok, orig_sha: ok, bundle: bad } })).toBe("bundle");
  expect(failedStage({ ...base, verdicts: { gz_sha: ok, orig_sha: ok, bundle: ok } })).toBeNull();
});

test("deep links", () => {
  expect(parseDeepLink("#s=abc&t=tok")).toEqual({ sid: "abc", token: "tok" });
  expect(parseDeepLink("#t=tok&s=abc")).toEqual({ sid: "abc", token: "tok" });
  expect(parseDeepLink("#s=abc")).toBeNull();
  expect(parseDeepLink("")).toBeNull();
});
