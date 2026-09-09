/** Mirrors docs/API.md. */
export type State = "WAITING_MANIFEST" | "RECEIVING" | "VERIFYING" | "READY" | "FAILED";

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

export interface Snapshot {
  sid: string;
  state: State;
  sender_session: number | null;
  name: string;
  total: number;
  have: number;
  bitmap: string;
  fps: number;
  relays: number;
  verdicts: { gz_sha: Verdict | null; orig_sha: Verdict | null; bundle: Verdict | null };
  bundle: BundleSummary | null;
  downloads: string[];
  dest_path: string | null;
  error: string | null;
  started_at?: string | null;
  finished_at?: string | null;
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
  have: number;
  total: number;
  state: State;
}

export function isReceiving(state: State): boolean {
  return state === "WAITING_MANIFEST" || state === "RECEIVING";
}

export function isTerminal(state: State): boolean {
  return state === "READY" || state === "FAILED";
}
