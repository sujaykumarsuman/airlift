import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { bindActivity, defaultPingConfig, Pinger, shouldPing, type PingOutcome } from "./ping";

const { minIntervalMs: MIN, inputWindowMs: WIN } = defaultPingConfig;

beforeEach(() => {
  vi.useFakeTimers();
  vi.setSystemTime(0);
});
afterEach(() => vi.useRealTimers());

test("shouldPing: visible, recent input, a minute since the last", () => {
  const base = { visible: true, now: 10 * MIN, lastInputAt: 10 * MIN, lastPingAt: 0 };
  expect(shouldPing(base)).toBe(true);
  expect(shouldPing({ ...base, visible: false })).toBe(false);
  expect(shouldPing({ ...base, lastInputAt: base.now - WIN })).toBe(false); // exactly stale
  expect(shouldPing({ ...base, lastInputAt: base.now - WIN + 1 })).toBe(true);
  expect(shouldPing({ ...base, lastPingAt: base.now - MIN + 1 })).toBe(false); // < a minute
  expect(shouldPing({ ...base, lastPingAt: base.now - MIN })).toBe(true); // exactly a minute
});

function make(over: Partial<ConstructorParameters<typeof Pinger>[0]> = {}) {
  const ping = vi.fn(async (): Promise<PingOutcome> => "ok");
  const p = new Pinger({ ping, checkMs: 15_000, visible: () => true, ...over });
  return { p, ping };
}

test("first ping at +60s, then once a minute", async () => {
  const { p, ping } = make();
  await vi.advanceTimersByTimeAsync(45_000);
  expect(ping).toHaveBeenCalledTimes(0);
  await vi.advanceTimersByTimeAsync(15_000);
  expect(ping).toHaveBeenCalledTimes(1); // t=60
  await vi.advanceTimersByTimeAsync(60_000);
  expect(ping).toHaveBeenCalledTimes(2); // t=120
  p.stop();
});

test("skips while hidden, resumes when visible", async () => {
  let vis = false;
  const { p, ping } = make({ visible: () => vis });
  await vi.advanceTimersByTimeAsync(120_000);
  expect(ping).not.toHaveBeenCalled();
  vis = true;
  await vi.advanceTimersByTimeAsync(15_000);
  expect(ping).toHaveBeenCalledTimes(1);
  p.stop();
});

test("stops pinging once input goes stale (> 5 min)", async () => {
  const { p, ping } = make(); // input fixed at t=0
  await vi.advanceTimersByTimeAsync(300_000); // pings at 60,120,180,240; 300 is stale
  expect(ping).toHaveBeenCalledTimes(4);
  await vi.advanceTimersByTimeAsync(300_000);
  expect(ping).toHaveBeenCalledTimes(4);
  p.stop();
});

test("noteInput keeps it alive across the window", async () => {
  const { p, ping } = make();
  await vi.advanceTimersByTimeAsync(240_000); // 4 pings by t=240
  p.noteInput(); // fresh input
  await vi.advanceTimersByTimeAsync(60_000); // t=300, input age 60 → a 5th ping
  expect(ping).toHaveBeenCalledTimes(5);
  p.stop();
});

test("stops for good on a 'stop' outcome", async () => {
  const ping = vi.fn(async (): Promise<PingOutcome> => "stop");
  new Pinger({ ping, checkMs: 15_000, visible: () => true });
  await vi.advanceTimersByTimeAsync(60_000);
  expect(ping).toHaveBeenCalledTimes(1);
  await vi.advanceTimersByTimeAsync(300_000);
  expect(ping).toHaveBeenCalledTimes(1);
});

test("a failure keeps the strict 60s cadence (optimistic advance)", async () => {
  const ping = vi.fn(async (): Promise<PingOutcome> => {
    throw new Error("net");
  });
  const p = new Pinger({ ping, checkMs: 15_000, visible: () => true });
  await vi.advanceTimersByTimeAsync(60_000);
  expect(ping).toHaveBeenCalledTimes(1);
  await vi.advanceTimersByTimeAsync(45_000);
  expect(ping).toHaveBeenCalledTimes(1); // no fast retry
  await vi.advanceTimersByTimeAsync(15_000);
  expect(ping).toHaveBeenCalledTimes(2); // the next minute
  p.stop();
});

test("stop() cancels the interval", async () => {
  const { p, ping } = make();
  p.stop();
  await vi.advanceTimersByTimeAsync(300_000);
  expect(ping).not.toHaveBeenCalled();
});

test("stop() releases the activity binding it owns", () => {
  let bound = 0;
  let detached = 0;
  const { p } = make({
    bindActivity: () => {
      bound++;
      return () => detached++;
    },
  });
  expect(bound).toBe(1);
  p.stop();
  expect(detached).toBe(1);
  p.stop(); // idempotent: no double-detach
  expect(detached).toBe(1);
});

test("a 'stop' outcome self-stops AND releases the activity binding", async () => {
  let detached = 0;
  const ping = vi.fn(async (): Promise<PingOutcome> => "stop");
  new Pinger({ ping, checkMs: 15_000, visible: () => true, bindActivity: () => () => detached++ });
  await vi.advanceTimersByTimeAsync(60_000); // first ping → "stop" → self-stop
  expect(ping).toHaveBeenCalledTimes(1);
  expect(detached).toBe(1); // the listener is not orphaned by a self-stop
});

test("bindActivity feeds input and detaches cleanly", () => {
  const target = new EventTarget();
  let n = 0;
  const off = bindActivity(() => n++, target);
  target.dispatchEvent(new Event("pointerdown"));
  target.dispatchEvent(new Event("keydown"));
  expect(n).toBe(2);
  off();
  target.dispatchEvent(new Event("touchstart"));
  expect(n).toBe(2);
});
