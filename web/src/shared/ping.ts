/**
 * The activity pinger. A passive dashboard viewer or a scan page between beams
 * sends no frames, so it must ping to keep the session's inactive clock alive —
 * but only while the tab is visible and the operator is actually around. This
 * module is DOM-free (bindActivity is the one exception) so its logic is
 * unit-tested with fake timers.
 */

export interface PingConfig {
  minIntervalMs: number; // at most one ping this often
  inputWindowMs: number; // only ping within this long of the last real input
}
export const defaultPingConfig: PingConfig = { minIntervalMs: 60_000, inputWindowMs: 300_000 };

export interface PingGate {
  visible: boolean;
  now: number;
  lastInputAt: number;
  lastPingAt: number;
}

/** The pure decision: ping now? Visible, input recent, and a minute since the last. */
export function shouldPing(g: PingGate, cfg: PingConfig = defaultPingConfig): boolean {
  if (!g.visible) return false;
  if (g.now - g.lastInputAt >= cfg.inputWindowMs) return false; // input too stale
  if (g.now - g.lastPingAt < cfg.minIntervalMs) return false; // pinged too recently
  return true;
}

export type PingOutcome = "ok" | "stop" | "retry";

export interface PingerOptions {
  ping: () => Promise<PingOutcome>;
  now?: () => number; // default Date.now
  visible?: () => boolean; // default: document visible (guarded for node)
  checkMs?: number; // gate-evaluation cadence, default 20 s
  config?: PingConfig;
}

const defaultVisible = (): boolean => typeof document === "undefined" || document.visibilityState === "visible";

/** Owns exactly one interval and runs until stop(). Mirrors Relay's self-owned
 *  timer shape; visibility is sampled at each tick, so there is no listener to
 *  leak. */
export class Pinger {
  private readonly now: () => number;
  private readonly visible: () => boolean;
  private readonly cfg: PingConfig;
  private timer: ReturnType<typeof setInterval> | null;
  private lastInputAt: number;
  private lastPingAt: number;
  private inflight = false;
  private stopped = false;

  constructor(private readonly opts: PingerOptions) {
    this.now = opts.now ?? ((): number => Date.now());
    this.visible = opts.visible ?? defaultVisible;
    this.cfg = opts.config ?? defaultPingConfig;
    const t = this.now();
    this.lastInputAt = t; // a fresh load counts as recent input
    this.lastPingAt = t; // the first ping is one interval after start
    this.timer = setInterval(() => this.check(), opts.checkMs ?? 20_000);
  }

  /** Record real user input, resetting the "operator is around" window. */
  noteInput(): void {
    if (!this.stopped) this.lastInputAt = this.now();
  }

  stop(): void {
    if (this.stopped) return;
    this.stopped = true;
    if (this.timer !== null) clearInterval(this.timer);
    this.timer = null;
  }

  private check(): void {
    if (this.stopped || this.inflight) return;
    const now = this.now();
    if (!shouldPing({ visible: this.visible(), now, lastInputAt: this.lastInputAt, lastPingAt: this.lastPingAt }, this.cfg)) {
      return;
    }
    // Advance optimistically so a failing/retrying ping still keeps a strict
    // ≥ minInterval cadence (staying under rate_ping).
    this.lastPingAt = now;
    this.inflight = true;
    void this.opts
      .ping()
      .then((o) => {
        if (o === "stop") this.stop();
      })
      .catch(() => {
        /* transient; a later tick retries */
      })
      .finally(() => {
        this.inflight = false;
      });
  }
}

const activityEvents = ["pointerdown", "keydown", "touchstart"] as const;

/** Feeds real user input to a pinger; returns a detach function. The only DOM
 *  touch in this module. */
export function bindActivity(onInput: () => void, target: EventTarget = document): () => void {
  const h = (): void => onInput();
  for (const t of activityEvents) target.addEventListener(t, h, { passive: true });
  return () => {
    for (const t of activityEvents) target.removeEventListener(t, h);
  };
}
