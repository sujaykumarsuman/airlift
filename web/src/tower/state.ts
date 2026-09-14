import type { Beam, Snapshot } from "../shared/types";
import { isTerminal } from "../shared/types";

/** One beam's derived timing, carried across snapshots so a reloaded or
 *  ticking dashboard keeps a stable clock. */
export interface BeamView {
  beam: Beam;
  startedAt: number | null;
  finishedAt: number | null;
  elapsedMs: number;
  etaSec: number | null;
  pct: number;
}

/** What the dashboard renders: the latest place snapshot and a view per beam. */
export interface View {
  snap: Snapshot | null;
  beams: BeamView[];
}

export const initialView: View = { snap: null, beams: [] };

/** Milliseconds since the epoch for an RFC 3339 stamp, or null. */
function stamp(value: string | null | undefined): number | null {
  if (!value) return null;
  const t = Date.parse(value);
  return Number.isFinite(t) ? t : null;
}

function deriveBeam(prev: BeamView | undefined, beam: Beam, now: number): BeamView {
  // A beam exists only because its manifest arrived, so it has always started;
  // prefer the tower's stamp, else the one we first recorded, else now.
  const startedAt = stamp(beam.started_at) ?? prev?.startedAt ?? now;
  let finishedAt = stamp(beam.finished_at) ?? prev?.finishedAt ?? null;
  if (finishedAt === null && isTerminal(beam.state)) finishedAt = now;
  const remaining = beam.total - beam.have;
  return {
    beam,
    startedAt,
    finishedAt,
    elapsedMs: (finishedAt ?? now) - startedAt,
    etaSec: beam.state === "RECEIVING" && beam.fps > 0 && remaining > 0 ? remaining / beam.fps : null,
    pct: beam.total > 0 ? (100 * beam.have) / beam.total : 0,
  };
}

export function reduce(prev: View, snap: Snapshot, now: number): View {
  const byBid = new Map(prev.beams.map((bv) => [bv.beam.bid, bv]));
  return { snap, beams: snap.beams.map((beam) => deriveBeam(byBid.get(beam.bid), beam, now)) };
}

/** Advances the clock between snapshots for beams still running. */
export function tick(prev: View, now: number): View {
  if (!prev.snap || !prev.beams.some((bv) => bv.finishedAt === null)) return prev;
  const beams = prev.beams.map((bv) =>
    bv.finishedAt === null ? { ...bv, elapsedMs: now - (bv.startedAt ?? now) } : bv,
  );
  return { ...prev, beams };
}

export type Stage = "gz_sha" | "orig_sha" | "bundle";

/** The first verdict that failed, in chain order. */
export function failedStage(beam: Beam): Stage | null {
  for (const stage of ["gz_sha", "orig_sha", "bundle"] as const) {
    const v = beam.verdicts[stage];
    if (v && !v.ok) return stage;
  }
  return null;
}
