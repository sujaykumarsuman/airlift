import { expect, test } from "vitest";
import type { Beam, State } from "../shared/types";
import { activeKey, scanJustCompleted } from "./complete";

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
