import { cyrb53 } from "../shared/hash";
import type { IngestResult } from "../shared/types";

export interface RelayStats {
  seen: number; // strings pushed
  unique: number; // after dedup
  sent: number; // frames the server acknowledged
  accepted: number;
  dup: number;
  bad: number;
  buffered: number; // waiting to be sent
  inflight: boolean;
  failures: number; // consecutive failed POSTs
  lastError: string | null;
  completed: string[]; // bids the server reported filled, in the order seen
}

export interface RelayOptions {
  post: (frames: string[]) => Promise<IngestResult>;
  flushMs?: number; // default 100
  maxBatch?: number; // default 50
  backoffMs?: number[]; // retry delays after failures
  onUpdate?: (stats: RelayStats) => void;
}

const defaultBackoff = [500, 1000, 2000, 4000, 5000];

/**
 * The stateless relay: dedup by string hash, batch every 100 ms or 50
 * frames, POST, and on failure keep buffering and retry with backoff. A place
 * may hold many beams, so the relay never latches "done" — it keeps feeding
 * whatever the camera decodes until stop(); the tower sorts frames into beams.
 */
export class Relay {
  readonly stats: RelayStats = {
    seen: 0,
    unique: 0,
    sent: 0,
    accepted: 0,
    dup: 0,
    bad: 0,
    buffered: 0,
    inflight: false,
    failures: 0,
    lastError: null,
    completed: [],
  };
  private readonly seen = new Set<number>();
  private readonly queue: string[] = [];
  private timer: ReturnType<typeof setTimeout> | null = null;
  private flushing: Promise<void> | null = null;
  private stopped = false;
  private readonly flushMs: number;
  private readonly maxBatch: number;
  private readonly backoff: number[];

  constructor(private readonly opts: RelayOptions) {
    this.flushMs = opts.flushMs ?? 100;
    this.maxBatch = opts.maxBatch ?? 50;
    this.backoff = opts.backoffMs ?? defaultBackoff;
  }

  /** Returns true when the string was new for this session. */
  push(text: string): boolean {
    if (this.stopped) return false;
    this.stats.seen++;
    const h = cyrb53(text);
    if (this.seen.has(h)) {
      this.emit();
      return false;
    }
    this.seen.add(h);
    this.stats.unique++;
    this.queue.push(text);
    this.stats.buffered = this.queue.length;
    if (this.queue.length >= this.maxBatch) void this.flush();
    else this.schedule(this.flushMs, false);
    this.emit();
    return true;
  }

  /** Sends everything queued, in batches; resolves when the queue is empty or a POST failed. */
  flush(): Promise<void> {
    if (!this.flushing) {
      this.flushing = this.drain().finally(() => {
        this.flushing = null;
      });
    }
    return this.flushing;
  }

  stop(): void {
    this.stopped = true;
    if (this.timer !== null) clearTimeout(this.timer);
    this.timer = null;
  }

  /** Zeroes the counters for a fresh scan. The dedup set stays — a frame already
   *  relayed is never re-sent — and so does `completed`, which the scanner acks. */
  resetStats(): void {
    Object.assign(this.stats, { seen: 0, unique: 0, sent: 0, accepted: 0, dup: 0, bad: 0, failures: 0, lastError: null });
    this.emit();
  }

  /** Re-arms a stopped relay for a reopened session (an accepted extension),
   *  keeping the dedup set so already-relayed frames are not re-sent; any queued
   *  remainder is flushed on the next tick. */
  resume(): void {
    if (!this.stopped) return;
    this.stopped = false;
    if (this.queue.length > 0) this.schedule(this.flushMs, false);
  }

  private schedule(ms: number, replace: boolean): void {
    if (this.stopped) return;
    if (this.timer !== null) {
      if (!replace) return;
      clearTimeout(this.timer);
    }
    this.timer = setTimeout(() => {
      this.timer = null;
      void this.flush();
    }, ms);
  }

  /** Sends one batch, then more only while full batches are waiting; a
   *  partial remainder waits for the next tick so POSTs stay ≤ 10/s (the
 *  tower meters 30 POSTs a second per address). */
  private async drain(): Promise<void> {
    let first = true;
    while (!this.stopped && this.queue.length > 0 && (first || this.queue.length >= this.maxBatch)) {
      first = false;
      const batch = this.queue.slice(0, this.maxBatch);
      this.stats.inflight = true;
      this.emit();
      let res: IngestResult;
      try {
        res = await this.opts.post(batch);
      } catch (err) {
        this.stats.failures++;
        this.stats.lastError = err instanceof Error ? err.message : String(err);
        this.stats.inflight = false;
        this.stats.buffered = this.queue.length;
        this.emit();
        this.schedule(this.backoff[Math.min(this.stats.failures - 1, this.backoff.length - 1)] ?? 5000, true);
        return;
      }
      this.queue.splice(0, batch.length);
      this.stats.sent += batch.length;
      this.stats.accepted += res.accepted;
      this.stats.dup += res.dup;
      this.stats.bad += res.bad;
      for (const bid of res.completed_beams) {
        if (!this.stats.completed.includes(bid)) this.stats.completed.push(bid);
      }
      this.stats.failures = 0;
      this.stats.lastError = null;
      this.stats.inflight = false;
      this.stats.buffered = this.queue.length;
      this.emit();
    }
    if (!this.stopped && this.queue.length > 0) this.schedule(this.flushMs, false);
  }

  private emit(): void {
    this.opts.onUpdate?.(this.stats);
  }
}
