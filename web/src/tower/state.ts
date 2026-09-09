import type { Snapshot } from "../shared/types";
import { isTerminal } from "../shared/types";

/** What the dashboard renders: the latest snapshot plus timing derived here. */
export interface View {
  snap: Snapshot | null;
  startedAt: number | null; // first sign of frames arriving
  finishedAt: number | null; // first terminal snapshot
  elapsedMs: number;
  etaSec: number | null;
  pct: number;
}

export const initialView: View = { snap: null, startedAt: null, finishedAt: null, elapsedMs: 0, etaSec: null, pct: 0 };

export function reduce(prev: View, snap: Snapshot, now: number): View {
  let startedAt = prev.startedAt;
  if (startedAt === null && (snap.state !== "WAITING_MANIFEST" || snap.have > 0)) startedAt = now;
  let finishedAt = prev.finishedAt;
  if (finishedAt === null && isTerminal(snap.state)) finishedAt = now;
  const remaining = snap.total - snap.have;
  return {
    snap,
    startedAt,
    finishedAt,
    elapsedMs: startedAt === null ? 0 : (finishedAt ?? now) - startedAt,
    etaSec: snap.state === "RECEIVING" && snap.fps > 0 && remaining > 0 ? remaining / snap.fps : null,
    pct: snap.total > 0 ? (100 * snap.have) / snap.total : 0,
  };
}

/** Advances the clock between snapshots. */
export function tick(prev: View, now: number): View {
  if (prev.startedAt === null || prev.finishedAt !== null) return prev;
  return { ...prev, elapsedMs: now - prev.startedAt };
}

export type Stage = "gz_sha" | "orig_sha" | "bundle";

/** The first verdict that failed, in chain order. */
export function failedStage(snap: Snapshot): Stage | null {
  for (const stage of ["gz_sha", "orig_sha", "bundle"] as const) {
    const v = snap.verdicts[stage];
    if (v && !v.ok) return stage;
  }
  return null;
}

/** `/#s=<sid>&t=<token>`: a viewer joining an existing session. */
export function parseDeepLink(hash: string): { sid: string; token: string } | null {
  const p = new URLSearchParams(hash.replace(/^#/, ""));
  const sid = p.get("s")?.trim();
  const token = p.get("t")?.trim();
  return sid && token ? { sid, token } : null;
}
