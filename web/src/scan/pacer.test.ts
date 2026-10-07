import { expect, test } from "vitest";
import { startDecodeLoop, type Decoder, type LoopStats } from "./decoder";
import { STALL_MS, startFramePacer, type FrameCount, type PacerClock } from "./pacer";

/** A hand-driven browser: video-frame and animation-frame callbacks fire when
 *  the test says, timers when time is advanced past them. */
function fakeClock(opts: { rvfc?: boolean } = {}) {
  let t = 0;
  let videoCbs: ((presentedFrames: number) => void)[] = [];
  let animCbs: (() => void)[] = [];
  let timers: { at: number; cb: () => void }[] = [];
  let presented = 0;
  let received: number | null = null;
  const clock: PacerClock = {
    now: () => t,
    videoFrame: opts.rvfc === false ? undefined : (cb) => void videoCbs.push(cb),
    animationFrame: (cb) => void animCbs.push(cb),
    timeout: (cb, ms) => void timers.push({ at: t + ms, cb }),
    receivedFrames: () => received,
  };
  return {
    clock,
    /** What the element reports as received (getVideoPlaybackQuality). */
    set received(n: number | null) {
      received = n;
    },
    /** Moves time on, running the timers that fall due. */
    advance(ms: number) {
      const end = t + ms;
      for (;;) {
        const next = timers.filter((x) => x.at <= end).sort((a, b) => a.at - b.at)[0];
        if (!next) break;
        timers = timers.filter((x) => x !== next);
        t = next.at;
        next.cb();
      }
      t = end;
    },
    /** The camera presents a frame: every pending video-frame callback fires. */
    videoFrame() {
      presented++;
      const cbs = videoCbs;
      videoCbs = [];
      cbs.forEach((cb) => cb(presented));
    },
    /** A rendering step: every pending animation-frame callback fires. */
    animationFrame() {
      const cbs = animCbs;
      animCbs = [];
      cbs.forEach((cb) => cb());
    },
    pending: () => ({ video: videoCbs.length, anim: animCbs.length }),
  };
}

function run(f: ReturnType<typeof fakeClock>) {
  const ticks: (FrameCount | null)[] = [];
  const stop = startFramePacer(f.clock, (c) => ticks.push(c));
  return { ticks, stop };
}

test("video-frame callbacks pace the loop; no fallback while they flow", () => {
  const f = fakeClock();
  const { ticks } = run(f);
  for (let i = 0; i < 30; i++) {
    f.advance(33);
    f.videoFrame();
  }
  expect(ticks).toHaveLength(30);
  expect(ticks[29]).toEqual({ counter: "presented", frames: 30 });
  expect(f.pending()).toEqual({ video: 1, anim: 0 }); // exactly one callback outstanding, no animation frames asked for
});

test("a stall hands over to animation frames, one tick per received frame", () => {
  const f = fakeClock();
  const { ticks } = run(f);
  f.advance(STALL_MS - 1);
  expect(f.pending().anim).toBe(0);
  f.advance(STALL_MS); // the watchdog notices within half a STALL_MS more
  expect(f.pending().anim).toBe(1);
  f.received = 7;
  f.animationFrame();
  expect(ticks).toEqual([{ counter: "received", frames: 7 }]);
  f.advance(16);
  f.animationFrame(); // a display refresh without a new camera frame
  expect(ticks).toHaveLength(1);
  f.received = 8;
  f.advance(16);
  f.animationFrame();
  expect(ticks).toEqual([
    { counter: "received", frames: 7 },
    { counter: "received", frames: 8 },
  ]);
});

test("a count that never moves still ticks once per STALL_MS", () => {
  const f = fakeClock();
  const { ticks } = run(f);
  f.received = 6; // a camera that stopped delivering
  f.advance(STALL_MS * 2);
  for (let i = 0; i < 60; i++) {
    f.animationFrame();
    f.advance(16);
  }
  // ~1 s of refreshes: the first tick, then one every STALL_MS
  expect(ticks.length).toBeGreaterThanOrEqual(4);
  expect(ticks.length).toBeLessThanOrEqual(5);
});

test("no count from the browser: every animation frame ticks", () => {
  const f = fakeClock();
  const { ticks } = run(f);
  f.advance(STALL_MS * 2);
  for (let i = 0; i < 10; i++) {
    f.animationFrame();
    f.advance(16);
  }
  expect(ticks).toEqual(Array(10).fill(null));
});

test("a returning video-frame callback takes over at once, without a double tick", () => {
  const f = fakeClock();
  const { ticks } = run(f);
  f.advance(STALL_MS * 2);
  f.received = 10;
  f.animationFrame();
  expect(ticks).toHaveLength(1);
  f.videoFrame(); // rVFC is back
  f.received = 11;
  f.advance(16);
  f.animationFrame(); // the pending animation frame retires without ticking
  expect(ticks).toEqual([
    { counter: "received", frames: 10 },
    { counter: "presented", frames: 1 },
  ]);
  expect(f.pending()).toEqual({ video: 1, anim: 0 });
  // and the watchdog re-arms on the next stall
  f.advance(STALL_MS * 2);
  expect(f.pending().anim).toBe(1);
});

test("without requestVideoFrameCallback, animation frames drive from the start", () => {
  const f = fakeClock({ rvfc: false });
  const { ticks } = run(f);
  expect(f.pending()).toEqual({ video: 0, anim: 1 });
  f.received = 1;
  f.animationFrame();
  expect(ticks).toEqual([{ counter: "received", frames: 1 }]);
});

test("stop ends every source", () => {
  const f = fakeClock();
  const { ticks, stop } = run(f);
  f.advance(STALL_MS * 2);
  stop();
  f.videoFrame();
  f.animationFrame();
  f.advance(STALL_MS * 4);
  f.animationFrame();
  expect(ticks).toHaveLength(0);
  expect(f.pending()).toEqual({ video: 0, anim: 0 });
});

// The decode loop on the pacer: in-flight cap and the camera rate per counter.
function loop(f: ReturnType<typeof fakeClock>) {
  const stats: LoopStats[] = [];
  let calls = 0;
  const decoder: Decoder = { name: "fake", decode: () => (calls++, new Promise<string[]>(() => {})) }; // never settles
  const video = { readyState: 4 } as HTMLVideoElement;
  const stop = startDecodeLoop(video, decoder, {} as HTMLCanvasElement, () => {}, (s) => stats.push(s), undefined, f.clock);
  return { stats, stop, calls: () => calls };
}

test("decode loop: at most two decodes in flight; camera rate from presentedFrames", () => {
  const f = fakeClock();
  const l = loop(f);
  for (let i = 0; i < 31; i++) {
    f.advance(1000 / 30);
    f.videoFrame();
  }
  expect(l.calls()).toBe(2);
  expect(l.stats).toHaveLength(1);
  expect(l.stats[0]?.framesPerSec).toBeCloseTo(30, 0);
  expect(l.stats[0]?.cameraFps).toBeCloseTo(30, 0); // between the window's first and last presentedFrames
  l.stop();
});

test("decode loop: under the fallback the rate comes from received frames; null across the switch", () => {
  const f = fakeClock();
  const l = loop(f);
  f.advance(STALL_MS * 2);
  let received = 100;
  const refresh = () => {
    f.advance(1000 / 60);
    if (Math.round(f.clock.now() / (1000 / 60)) % 2 === 0) f.received = ++received; // a 30 fps camera on a 60 Hz display
    f.animationFrame();
  };
  for (let i = 0; i < 180; i++) refresh();
  const fallback = l.stats; // the first too: the rate is over the counts' span, not the window's
  expect(fallback.length).toBeGreaterThanOrEqual(3);
  for (const s of fallback) {
    expect(s.cameraFps).toBeGreaterThan(27);
    expect(s.cameraFps).toBeLessThan(33);
    expect(s.framesPerSec).toBeLessThan(33); // a frame is offered once, not per refresh
  }
  // rVFC comes back: the window that straddles the switch has no rate
  const before = l.stats.length;
  for (let i = 0; i < 40; i++) {
    f.advance(1000 / 30);
    f.videoFrame();
    f.animationFrame();
  }
  expect(l.stats.length).toBe(before + 1);
  expect(l.stats[l.stats.length - 1]?.cameraFps).toBeNull();
  l.stop();
});
