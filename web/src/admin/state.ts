import type { AdminRow } from "../shared/types";

/** The admin view derived from the sessions list: pending reviews first (they
 *  need the operator's attention), then the rest in the server's order. */
export interface AdminView {
  rows: AdminRow[]; // every session, pending-review first
  pending: AdminRow[]; // just the sessions awaiting a review decision
}

export function reduceAdmin(sessions: AdminRow[]): AdminView {
  const pending = sessions.filter((s) => s.status === "PENDING_REVIEW");
  const rest = sessions.filter((s) => s.status !== "PENDING_REVIEW");
  return { rows: [...pending, ...rest], pending };
}

/** A short, human label for a session row (its label, else its short id). */
export function rowLabel(s: AdminRow): string {
  return s.label || `session ${s.sid}`;
}

/** The beams of a row that can be downloaded (READY, with at least one form). */
export function downloadableBeams(s: AdminRow): { bid: string; name: string; downloads: string[] }[] {
  return s.beams
    .filter((b) => b.state === "READY" && b.downloads.length > 0)
    .map((b) => ({ bid: b.bid, name: b.name || "(unnamed)", downloads: b.downloads }));
}
