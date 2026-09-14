import { expect, test } from "vitest";
import type { Beam, State } from "../shared/types";
import { activeKey, baselineIgnored, pickActive, relayCompleted, scanJustCompleted } from "./complete";

const beam = (bid: string, state: State): Beam => ({
  bid,
  sender_session: 0,
  name: "f",
  state,
  total: 3,
  have: 3,
  bitmap: "",
  fps: 0,
  verdicts: { gz_sha: null, orig_sha: null, bundle: null },
  bundle: null,
  downloads: [],
  saved_path: null,
  error: null,
});

test("activeKey", () => {
  expect(activeKey(null)).toBe("");
  expect(activeKey(beam("f32bd5a8", "RECEIVING"))).toBe("f32bd5a8:RECEIVING");
});

test("fires on the transition into READY, including via VERIFYING (the regression)", () => {
  const ready = beam("f32bd5a8", "READY");
  // A beam verifies between the last packet and READY, so the prior state seen is
  // VERIFYING — this is the case that failed on the hardware run.
  expect(scanJustCompleted("f32bd5a8:VERIFYING", ready, true)).toBe(true);
  // A direct RECEIVING → READY also fires.
  expect(scanJustCompleted("f32bd5a8:RECEIVING", ready, true)).toBe(true);
});

test("does not fire without the camera, without a prior sighting, or on re-ready", () => {
  const ready = beam("f32bd5a8", "READY");
  expect(scanJustCompleted("f32bd5a8:VERIFYING", ready, false)).toBe(false); // camera off
  expect(scanJustCompleted("", ready, true)).toBe(false); // first sight already READY
  expect(scanJustCompleted("f32bd5a8:READY", ready, true)).toBe(false); // already READY last render
  expect(scanJustCompleted("beadfeed:VERIFYING", ready, true)).toBe(false); // a different beam
  expect(scanJustCompleted("f32bd5a8:RECEIVING", beam("f32bd5a8", "VERIFYING"), true)).toBe(false); // not READY yet
  expect(scanJustCompleted("f32bd5a8:RECEIVING", null, true)).toBe(false); // no beam
});

test("the relay's completed_beams completes a beam the snapshot never showed receiving", () => {
  // A 3-chunk beam: one POST fills it, and it is READY before its first snapshot.
  const acked = new Set<string>();
  expect(relayCompleted([], acked)).toBeNull();
  expect(relayCompleted(["58f89ba5"], acked)).toBe("58f89ba5");
  acked.add("58f89ba5");
  expect(relayCompleted(["58f89ba5"], acked)).toBeNull();
  expect(relayCompleted(["58f89ba5", "bc7ce2dd"], acked)).toBe("bc7ce2dd");
});

test("a reopened scanner ignores beams that were already finished", () => {
  const old = beam("58f89ba5", "READY");
  const failed = beam("deadbeef", "FAILED");
  const live = beam("bc7ce2dd", "RECEIVING");
  expect([...baselineIgnored([old, failed, live])].sort()).toEqual(["58f89ba5", "deadbeef"]);
  const ignored = baselineIgnored([old, failed]);
  expect(pickActive([old, failed], ignored)).toBeNull(); // clean: waiting for a beam
  expect(pickActive([old, failed, live], ignored)?.bid).toBe("bc7ce2dd");
  // A new beam that finished (another relay fed it) still shows; a dismissed one does not.
  const done = beam("0badf00d", "READY");
  expect(pickActive([old, done], ignored)?.bid).toBe("0badf00d");
  ignored.add("0badf00d");
  expect(pickActive([old, done], ignored)).toBeNull();
});

test("pickActive prefers the latest receiving beam, else the most recent", () => {
  const a = beam("a", "RECEIVING");
  const b = beam("b", "READY");
  const c = beam("c", "RECEIVING");
  expect(pickActive([a, b, c], new Set())?.bid).toBe("c");
  expect(pickActive([a, b], new Set())?.bid).toBe("a"); // receiving beats a later finished one
  expect(pickActive([b], new Set())?.bid).toBe("b");
  expect(pickActive([], new Set())).toBeNull();
});
