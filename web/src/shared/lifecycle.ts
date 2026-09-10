import { formatDuration } from "./format";
import type { Snapshot, Termination } from "./types";

/** Milliseconds since the epoch for an RFC 3339 stamp, or null for an absent
 *  value, Go's zero time ("0001-01-01T00:00:00Z" → a negative epoch, meaning
 *  "no clock"), or garbage. */
export function instantMs(iso: string | null | undefined): number | null {
  if (!iso) return null;
  const t = Date.parse(iso);
  return Number.isFinite(t) && t > 0 ? t : null;
}

/** Who ended the session, in friendly prose. Unknown actors (Phase 7's "airlift
 *  admin") degrade gracefully. */
export function terminatedBy(t: Termination): string {
  if (t.by === "session admin") return "Ended by an admin";
  if (t.by === "system") return "Session closed";
  return `Ended by ${t.by}`;
}

/** Why the session ended, in friendly prose. */
export function terminatedWhy(t: Termination): string {
  if (t.by === "session admin" || t.reason === "terminated by session admin") {
    return "An admin ended this session.";
  }
  switch (t.reason) {
    case "idle_ttl":
      return "It closed after everyone disconnected.";
    case "inactive_ttl":
      return "It closed after a spell of inactivity.";
    case "max_age":
      return "It reached its maximum age.";
    default:
      return "The session has ended.";
  }
}

/** A countdown rendered from a deadline: its text, whether it has elapsed, and
 *  whether there is any deadline to show at all. */
export interface Countdown {
  text: string;
  done: boolean;
  hidden: boolean;
}

/** Time until a terminated session's files are removed. */
export function cleanupCountdown(t: Termination, now: number): Countdown {
  const at = instantMs(t.cleanup_at);
  if (at === null) return { text: "", done: false, hidden: true };
  const ms = at - now;
  return ms <= 0 ? { text: "", done: true, hidden: false } : { text: formatDuration(ms), done: false, hidden: false };
}

/** Time until an OPEN session next expires (its earliest clock); hidden when no
 *  clock applies. */
export function expiryCountdown(s: Snapshot, now: number): Countdown {
  const at = instantMs(s.expires_at);
  if (at === null) return { text: "", done: false, hidden: true };
  const ms = at - now;
  return { text: formatDuration(Math.max(0, ms)), done: ms <= 0, hidden: false };
}
