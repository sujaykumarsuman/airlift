import { type Beam, isTerminal } from "../shared/types";

/** The key identifying the active beam and its state, tracked across renders so
 *  the READY edge can be detected (ADR 0019). Empty when there is no active beam. */
export function activeKey(beam: Beam | null): string {
  return beam ? `${beam.bid}:${beam.state}` : "";
}

/**
 * True when the active beam has just reached READY while our camera is running —
 * the beam we were feeding is complete, so the scanner stops and offers to close.
 *
 * It fires on the transition INTO READY from any earlier state of the *same* beam
 * (RECEIVING or VERIFYING — a beam verifies between the last packet and READY), so
 * it must not require the immediately-prior state to be RECEIVING. It does not fire
 * when the beam was already READY on the first sight of it (re-opening a finished
 * session), nor when the camera is off.
 */
export function scanJustCompleted(prevKey: string, beam: Beam | null, cameraOn: boolean): boolean {
  if (!beam || beam.state !== "READY" || !cameraOn) return false;
  const prefix = `${beam.bid}:`;
  return prevKey.startsWith(prefix) && prevKey !== `${prefix}READY`;
}

/**
 * The first beam the relay reports filled that this scanner has not yet acted on:
 * the frames THIS scanner posted completed it. A small beam can be READY before
 * its first snapshot even arrives — there is no transition for scanJustCompleted
 * to see — so the frames reply's `completed_beams` is the signal that never races.
 */
export function relayCompleted(completed: readonly string[], acked: ReadonlySet<string>): string | null {
  for (const bid of completed) {
    if (!acked.has(bid)) return bid;
  }
  return null;
}

/** The beams already finished when the scanner opened: it should not adopt them. */
export function baselineIgnored(beams: readonly Beam[]): Set<string> {
  return new Set(beams.filter((b) => isTerminal(b.state)).map((b) => b.bid));
}

/**
 * The beam this scanner is feeding: the last one still receiving, else the most
 * recently arrived — skipping beams that were finished before the scanner opened
 * or that it has scanned and dismissed, so a reopened scanner starts clean and
 * waits for a new beam rather than showing the last one's numbers.
 */
export function pickActive(beams: readonly Beam[], ignored: ReadonlySet<string>): Beam | null {
  let pick: Beam | null = null;
  for (const b of beams) {
    if (ignored.has(b.bid)) continue;
    if (b.state === "RECEIVING" || pick === null || pick.state !== "RECEIVING") pick = b;
  }
  return pick;
}
