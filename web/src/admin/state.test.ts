import { expect, test } from "vitest";
import type { AdminRow, Beam, Status } from "../shared/types";
import { downloadableBeams, reduceAdmin, rowLabel } from "./state";

const row = (sid: string, status: Status, over: Partial<AdminRow> = {}): AdminRow => ({
  sid,
  status,
  relays: 0,
  beams: [],
  clients: [],
  terminated: null,
  terminate_at: null,
  extension: null,
  expires_at: "2026-09-10T00:00:00Z",
  reopenable: false,
  has_password: false,
  knocks: [],
  uploads: [],
  label: "",
  addresses: {},
  ...over,
});

const beam = (bid: string, over: Partial<Beam> = {}): Beam => ({
  bid,
  sender_session: 1,
  name: "f",
  state: "READY",
  total: 1,
  have: 1,
  bitmap: "",
  fps: 0,
  verdicts: { gz_sha: null, orig_sha: null, bundle: null },
  bundle: null,
  downloads: ["raw"],
  saved_path: null,
  error: null,
  ...over,
});

test("pending reviews sort first, order otherwise preserved", () => {
  const v = reduceAdmin([row("a", "OPEN"), row("b", "PENDING_REVIEW"), row("c", "TERMINATED"), row("d", "PENDING_REVIEW")]);
  expect(v.rows.map((r) => r.sid)).toEqual(["b", "d", "a", "c"]);
  expect(v.pending.map((r) => r.sid)).toEqual(["b", "d"]);
});

test("rowLabel falls back to the id", () => {
  expect(rowLabel(row("x", "OPEN", { label: "release" }))).toBe("release");
  expect(rowLabel(row("x", "OPEN"))).toBe("session x");
});

test("downloadableBeams lists only READY beams with a download form", () => {
  const s = row("x", "TERMINATED", {
    beams: [beam("b1"), beam("b2", { state: "RECEIVING", downloads: [] }), beam("b3", { downloads: [] })],
  });
  expect(downloadableBeams(s).map((b) => b.bid)).toEqual(["b1"]);
});
