/** Mirrors docs/API.md. */
export type State = "RECEIVING" | "VERIFYING" | "READY" | "FAILED";

export interface Verdict {
  ok: boolean;
  expected: string;
  actual: string;
}

export interface BundleSummary {
  files: number;
  total_bytes: number;
  paths: string[];
}

/** One beam accumulating in a place, keyed by its sender u32 (bid = hex). */
export interface Beam {
  bid: string;
  sender_session: number;
  name: string;
  state: State;
  total: number;
  have: number;
  bitmap: string;
  fps: number;
  verdicts: { gz_sha: Verdict | null; orig_sha: Verdict | null; bundle: Verdict | null };
  bundle: BundleSummary | null;
  downloads: string[];
  saved_path: string | null;
  error: string | null;
  started_at?: string | null;
  finished_at?: string | null;
}

/** A place: identity, relay count and the list of beams read into it. */
export interface Snapshot {
  sid: string;
  relays: number;
  beams: Beam[];
  expires_at: string;
}

export interface Info {
  version: string;
  public_url: string;
  base_path: string;
  admin_enabled: boolean;
  caps: {
    max_gz_bytes: number;
    idle_ttl: number;
    inactive_ttl: number;
    max_age: number;
    sessions: number;
  };
}

export interface Created {
  sid: string;
  token: string;
  join_url: string;
  expires_at: string;
}

export interface IngestResult {
  accepted: number;
  dup: number;
  bad: number;
  completed_beams: string[];
}

export function isTerminal(state: State): boolean {
  return state === "READY" || state === "FAILED";
}
