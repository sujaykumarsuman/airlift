import { afterEach, beforeEach, expect, test, vi } from "vitest";
import type { IngestResult } from "../shared/types";
import { Relay } from "./relay";

const ok = (frames: string[], completed: string[] = []): IngestResult => ({
  accepted: frames.length,
  dup: 0,
  bad: 0,
  completed_beams: completed,
});

beforeEach(() => vi.useFakeTimers());
afterEach(() => vi.useRealTimers());

test("dedups by content and batches after 250 ms", async () => {
  const post = vi.fn(async (frames: string[]) => ok(frames));
  const relay = new Relay({ post });
  expect(relay.push("A")).toBe(true);
  expect(relay.push("A")).toBe(false);
  expect(relay.push("B")).toBe(true);
  expect(relay.stats).toMatchObject({ seen: 3, unique: 2, buffered: 2, sent: 0 });
  await vi.advanceTimersByTimeAsync(249);
  expect(post).not.toHaveBeenCalled();
  await vi.advanceTimersByTimeAsync(1);
  expect(post).toHaveBeenCalledTimes(1);
  expect(post.mock.calls[0]?.[0]).toEqual(["A", "B"]);
  expect(relay.stats).toMatchObject({ sent: 2, accepted: 2, buffered: 0, inflight: false });
  expect(relay.push("A")).toBe(false); // still remembered after sending
});

test("flushes immediately at 50 frames and keeps batches at 50", async () => {
  const post = vi.fn(async (frames: string[]) => ok(frames));
  const relay = new Relay({ post });
  for (let i = 0; i < 120; i++) relay.push(`F${i}`);
  await vi.advanceTimersByTimeAsync(0);
  expect(post.mock.calls.map((c) => c[0].length)).toEqual([50, 50]);
  expect(relay.stats.buffered).toBe(20);
  await vi.advanceTimersByTimeAsync(250);
  expect(post.mock.calls.map((c) => c[0].length)).toEqual([50, 50, 20]);
  expect(relay.stats.sent).toBe(120);
});

test("keeps buffering and retries with backoff; nothing is dropped", async () => {
  let failures = 3;
  const post = vi.fn(async (frames: string[]) => {
    if (failures-- > 0) throw new Error("network");
    return ok(frames);
  });
  const updates: number[] = [];
  const relay = new Relay({ post, backoffMs: [500, 1000, 2000], onUpdate: (s) => updates.push(s.buffered) });
  relay.push("A");
  relay.push("B");
  await vi.advanceTimersByTimeAsync(250);
  expect(post).toHaveBeenCalledTimes(1);
  expect(relay.stats).toMatchObject({ failures: 1, lastError: "network", buffered: 2, sent: 0 });
  relay.push("C"); // arrives while waiting to retry
  await vi.advanceTimersByTimeAsync(499);
  expect(post).toHaveBeenCalledTimes(1);
  await vi.advanceTimersByTimeAsync(1);
  expect(post).toHaveBeenCalledTimes(2); // retry after 500 ms
  expect(relay.stats.failures).toBe(2);
  await vi.advanceTimersByTimeAsync(1000);
  expect(post).toHaveBeenCalledTimes(3); // then 1000 ms
  expect(relay.stats.failures).toBe(3);
  await vi.advanceTimersByTimeAsync(2000);
  expect(post).toHaveBeenCalledTimes(4); // then 2000 ms, and this one succeeds
  expect(post.mock.calls[3]?.[0]).toEqual(["A", "B", "C"]);
  expect(relay.stats).toMatchObject({ failures: 0, lastError: null, buffered: 0, sent: 3 });
  expect(Math.max(...updates)).toBe(3);
});

test("records completed beams but keeps relaying (a place holds many)", async () => {
  const post = vi.fn(async (frames: string[]) => ok(frames, frames.includes("A") ? ["000000a1"] : []));
  const relay = new Relay({ post });
  relay.push("A");
  await vi.advanceTimersByTimeAsync(250);
  expect(relay.stats.completed).toEqual(["000000a1"]);
  // A beam finishing does not stop the relay: the next beam's frames still go.
  expect(relay.push("B")).toBe(true);
  await vi.advanceTimersByTimeAsync(250);
  expect(post).toHaveBeenCalledTimes(2);
  expect(post.mock.calls[1]?.[0]).toEqual(["B"]);
  expect(relay.stats.completed).toEqual(["000000a1"]); // not duplicated
});

test("stop cancels pending sends", async () => {
  const post = vi.fn(async (frames: string[]) => ok(frames));
  const relay = new Relay({ post });
  relay.push("A");
  relay.stop();
  await vi.advanceTimersByTimeAsync(1000);
  expect(post).not.toHaveBeenCalled();
});

test("resume re-arms a stopped relay for a reopened session, keeping the dedup set", async () => {
  const post = vi.fn(async (frames: string[]) => ok(frames));
  const relay = new Relay({ post });
  relay.push("A");
  relay.stop();
  expect(relay.push("B")).toBe(false); // frozen: dropped
  relay.resume();
  expect(relay.push("C")).toBe(true); // relaying again
  expect(relay.push("A")).toBe(false); // still remembered across the freeze
  await vi.advanceTimersByTimeAsync(250);
  expect(post).toHaveBeenCalledTimes(1);
  expect(post.mock.calls[0]?.[0]).toEqual(["A", "C"]); // the queued A plus the new C
});
