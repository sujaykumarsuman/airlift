import type { Beam } from "../shared/types";

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
