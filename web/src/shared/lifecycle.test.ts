import { expect, test } from "vitest";
import { cleanupCountdown, expiryCountdown, instantMs, terminatedBy, terminatedWhy } from "./lifecycle";
import type { Snapshot, Termination } from "./types";

const term = (o: Partial<Termination> = {}): Termination => ({
  by: "system",
  reason: "inactive_ttl",
  at: "2026-09-10T00:00:00Z",
  cleanup_at: "2026-09-10T00:10:00Z",
  ...o,
});

test("friendly who/why", () => {
  expect(terminatedBy(term({ by: "session admin" }))).toBe("Ended by an admin");
  expect(terminatedBy(term({ by: "airlift admin" }))).toBe("Ended by airlift admin"); // Phase-7 fallback
  expect(terminatedWhy(term({ by: "session admin", reason: "terminated by session admin" }))).toMatch(/admin ended/);
  expect(terminatedWhy(term({ reason: "idle_ttl" }))).toMatch(/disconnected/);
  expect(terminatedWhy(term({ reason: "inactive_ttl" }))).toMatch(/inactivity/);
  expect(terminatedWhy(term({ reason: "max_age" }))).toMatch(/maximum age/);
});

test("instantMs guards Go zero-time and garbage", () => {
  expect(instantMs("2026-09-10T00:00:00Z")).toBe(Date.parse("2026-09-10T00:00:00Z"));
  expect(instantMs("0001-01-01T00:00:00Z")).toBeNull();
  expect(instantMs("garbage")).toBeNull();
  expect(instantMs("")).toBeNull();
  expect(instantMs(null)).toBeNull();
});

test("cleanup countdown counts down, then is done", () => {
  const now = Date.parse("2026-09-10T00:00:00Z");
  expect(cleanupCountdown(term(), now)).toEqual({ text: "10m 00s", done: false, hidden: false });
  expect(cleanupCountdown(term(), Date.parse("2026-09-10T00:10:00Z")).done).toBe(true); // boundary
  expect(cleanupCountdown(term({ cleanup_at: "0001-01-01T00:00:00Z" }), now).hidden).toBe(true);
});

test("expiry countdown, hidden for the zero time", () => {
  const s = (e: string): Snapshot => ({ expires_at: e }) as unknown as Snapshot;
  expect(expiryCountdown(s("2026-09-10T00:05:00Z"), Date.parse("2026-09-10T00:00:00Z")).text).toBe("5m 00s");
  expect(expiryCountdown(s("0001-01-01T00:00:00Z"), 0).hidden).toBe(true);
});
