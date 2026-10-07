// The decode loop's clock: one tick per camera frame, from
// requestVideoFrameCallback, with a watchdog for when those callbacks stop.

/** A camera frame counter reading; readings of different counters do not compare. */
export interface FrameCount {
  /** "presented": requestVideoFrameCallback's presentedFrames. "received": the
   *  element's getVideoPlaybackQuality().totalVideoFrames, read by the fallback. */
  counter: "presented" | "received";
  frames: number;
}

/** The browser calls the pacer makes; injectable, so the scheduling is testable without a DOM. */
export interface PacerClock {
  now(): number;
  /** A one-shot callback on the video's next frame; absent without requestVideoFrameCallback. */
  videoFrame?: (cb: (presentedFrames: number) => void) => void;
  animationFrame(cb: () => void): void;
  timeout(cb: () => void, ms: number): void;
  /** Frames the element has received so far; null when the browser does not say. */
  receivedFrames(): number | null;
}

export function videoClock(video: HTMLVideoElement): PacerClock {
  return {
    now: () => performance.now(),
    videoFrame:
      typeof video.requestVideoFrameCallback === "function"
        ? (cb) => void video.requestVideoFrameCallback((_now, meta) => cb(meta.presentedFrames))
        : undefined,
    animationFrame: (cb) => void requestAnimationFrame(cb),
    timeout: (cb, ms) => void setTimeout(cb, ms),
    // 0 is a browser that does not count (or no frame yet): either way, no reading.
    receivedFrames: () => (typeof video.getVideoPlaybackQuality === "function" ? video.getVideoPlaybackQuality().totalVideoFrames || null : null),
  };
}

/** No video-frame callback for this long and the watchdog stops waiting for one. */
export const STALL_MS = 250;
const WATCH_MS = STALL_MS / 2;

/**
 * Calls onFrame once per camera frame, paced by requestVideoFrameCallback.
 * When no callback has come for STALL_MS (a browser whose callbacks stall
 * while the video plays, or one without them), animation frames drive onFrame
 * instead, each offering a frame only once the element has received a new one
 * (or STALL_MS on, should its count never move), and hand back the moment a
 * video-frame callback arrives, so a frame is never offered by both. The tick
 * carries the frame count it was paced by. Returns a stop function.
 */
export function startFramePacer(clock: PacerClock, onFrame: (count: FrameCount | null) => void): () => void {
  let running = true;
  // A start counts as a callback, so the first one gets STALL_MS to arrive.
  let lastVideoFrame = clock.videoFrame ? clock.now() : -Infinity;
  let animating = false;
  let lastFallback = -Infinity;
  let lastReceived: number | null = null;

  const requestVideoFrame = (): void =>
    clock.videoFrame?.((presentedFrames) => {
      if (!running) return;
      lastVideoFrame = clock.now();
      requestVideoFrame();
      onFrame({ counter: "presented", frames: presentedFrames });
    });

  const animate = (): void => {
    if (!running) return;
    const t = clock.now();
    if (t - lastVideoFrame < STALL_MS) {
      animating = false; // video-frame callbacks are back: they pace the loop again
      return;
    }
    clock.animationFrame(animate);
    const received = clock.receivedFrames();
    if (received !== null && received === lastReceived && t - lastFallback < STALL_MS) return;
    lastReceived = received;
    lastFallback = t;
    onFrame(received === null ? null : { counter: "received", frames: received });
  };

  const watch = (): void => {
    if (!running) return;
    if (!animating && clock.now() - lastVideoFrame >= STALL_MS) {
      animating = true;
      clock.animationFrame(animate);
    }
    clock.timeout(watch, WATCH_MS);
  };

  requestVideoFrame();
  watch();
  return () => {
    running = false;
  };
}
