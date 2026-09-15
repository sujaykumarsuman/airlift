import { expect, test } from "vitest";
import type { Beam, Snapshot } from "../shared/types";
import { failedStage, initialView, reduce, tick } from "./state";

const beam = (over: Partial<Beam> = {}): Beam => ({
  bid: "0000000a",
  sender_session: 10,
  name: "bundle.txt",
  state: "RECEIVING",
  total: 0,
  have: 0,
  bitmap: "",
  fps: 0,
  verdicts: { gz_sha: null, orig_sha: null, bundle: null },
  bundle: null,
  downloads: [],
  saved_path: null,
  error: null,
  ...over,
});

const place = (beams: Beam[]): Snapshot => ({
  sid: "s",
  status: "OPEN",
  relays: 0,
  beams,
  clients: [],
  terminated: null,
  terminate_at: null,
  extension: null,
  expires_at: "2026-09-09T12:00:00Z",
  reopenable: false,
  has_password: false,
  knocks: [],
  uploads: [],
});

test("empty place has no beam views", () => {
  const v = reduce(initialView, place([]), 1000);
  expect(v.beams).toHaveLength(0);
});

test("elapsed starts when a beam appears, ETA follows fps, and freezes when done", () => {
  let v = reduce(initialView, place([beam({ total: 100, have: 10, fps: 5 })]), 2000);
  expect(v.beams[0]).toMatchObject({ startedAt: 2000, elapsedMs: 0, etaSec: 18, pct: 10 });
  v = tick(v, 5000);
  expect(v.beams[0]?.elapsedMs).toBe(3000);
  v = reduce(v, place([beam({ total: 100, have: 60, fps: 0 })]), 7000);
  expect(v.beams[0]).toMatchObject({ elapsedMs: 5000, etaSec: null, pct: 60 });
  v = reduce(v, place([beam({ state: "VERIFYING", total: 100, have: 100, fps: 4 })]), 8000);
  expect(v.beams[0]?.etaSec).toBeNull();
  v = reduce(v, place([beam({ state: "READY", total: 100, have: 100 })]), 9000);
  expect(v.beams[0]).toMatchObject({ finishedAt: 9000, elapsedMs: 7000, pct: 100 });
  expect(tick(v, 20000).beams[0]?.elapsedMs).toBe(7000);
});

test("two beams keep independent clocks, keyed by bid", () => {
  const a = beam({ bid: "0000000a", total: 100, have: 20, fps: 10 });
  const b = beam({ bid: "0000000b", total: 50, have: 5, fps: 5 });
  let v = reduce(initialView, place([a, b]), 1000);
  expect(v.beams.map((bv) => bv.startedAt)).toEqual([1000, 1000]);
  // Beam A finishes; B keeps ticking.
  v = reduce(v, place([beam({ bid: "0000000a", state: "READY", total: 100, have: 100 }), { ...b, have: 25 }]), 4000);
  expect(v.beams[0]).toMatchObject({ finishedAt: 4000, elapsedMs: 3000, pct: 100 });
  v = tick(v, 6000);
  expect(v.beams[0]?.elapsedMs).toBe(3000); // A frozen
  expect(v.beams[1]?.elapsedMs).toBe(5000); // B still running
  expect(v.beams[1]?.pct).toBe(50);
});

test("failedStage picks the first failing verdict", () => {
  const ok = { ok: true, expected: "a", actual: "a" };
  const bad = { ok: false, expected: "a", actual: "b" };
  expect(failedStage(beam())).toBeNull();
  expect(failedStage(beam({ verdicts: { gz_sha: bad, orig_sha: null, bundle: null } }))).toBe("gz_sha");
  expect(failedStage(beam({ verdicts: { gz_sha: ok, orig_sha: bad, bundle: null } }))).toBe("orig_sha");
  expect(failedStage(beam({ verdicts: { gz_sha: ok, orig_sha: ok, bundle: bad } }))).toBe("bundle");
  expect(failedStage(beam({ verdicts: { gz_sha: ok, orig_sha: ok, bundle: ok } }))).toBeNull();
});

test("server timestamps win over the local clock", () => {
  const started = "2026-09-09T12:00:00Z";
  const finished = "2026-09-09T12:03:20Z";
  const now = Date.parse("2026-09-09T12:10:00Z");
  let v = reduce(initialView, place([beam({ total: 10, have: 5, started_at: started })]), now);
  expect(v.beams[0]?.startedAt).toBe(Date.parse(started));
  expect(v.beams[0]?.elapsedMs).toBe(10 * 60 * 1000);
  v = reduce(v, place([beam({ state: "READY", total: 10, have: 10, started_at: started, finished_at: finished })]), now);
  expect(v.beams[0]?.finishedAt).toBe(Date.parse(finished));
  expect(v.beams[0]?.elapsedMs).toBe(200 * 1000);
  const local = reduce(initialView, place([beam({ total: 10, have: 1, started_at: "garbage" })]), 5000);
  expect(local.beams[0]?.startedAt).toBe(5000);
});
