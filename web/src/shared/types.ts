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

/** One participant of a place, as it appears in the snapshot. */
export interface ClientSummary {
  client_id: string;
  name: string;
  roles: string[];
  session_admin: boolean;
  connected: boolean;
  last_active: string;
}

/** How and when a session was terminated (ADR 0013). */
export interface Termination {
  by: string;
  reason: string;
  at: string;
  cleanup_at: string;
}

/** The five lifecycle states (ADR 0013/0014). Live() = OPEN | TERMINATING. */
export type Status = "OPEN" | "TERMINATING" | "TERMINATED" | "PENDING_REVIEW" | "REJECTED";

/** A client's request to keep a TERMINATED session alive, and its review (ADR 0014). */
export interface Extension {
  by: string;
  reason: string;
  at: string;
  decision?: string; // "accept" | "reject", once reviewed
  note?: string; // the reviewer's note
  decided_at?: string;
}

/** A place: identity, lifecycle status, relay count, beams and clients. */
export interface Snapshot {
  sid: string;
  status: Status;
  relays: number;
  beams: Beam[];
  clients: ClientSummary[];
  terminated: Termination | null;
  terminate_at: string | null; // set only while TERMINATING (the warning deadline)
  extension: Extension | null;
  expires_at: string;
  reopenable: boolean; // suspended by inactivity — opening the link revives it (ADR 0018)
}

/** One row of GET /api/admin/config (a setting's value, source and mutability). */
export interface ConfigKey {
  name: string;
  value: string;
  source: string;
  live: boolean;
}

/** One session row in the admin list: the snapshot plus operator-only fields. */
export interface AdminRow extends Snapshot {
  label: string;
  addresses: Record<string, string>;
}

export interface Info {
  version: string;
  public_url: string;
  base_path: string;
  admin_enabled: boolean;
  caps: {
    max_gz_bytes: number;
    idle_ttl: number;
    max_age: number;
    sessions: number;
  };
}

export interface Created {
  sid: string;
  token: string;
  join_url: string;
  expires_at: string;
  client_id: string;
  name: string;
}

/** Options for POST /api/sessions; every field is optional. */
export interface CreateOptions {
  label?: string;
  password?: string;
  joiners_admin?: boolean;
  max_gz_bytes?: number;
  idle_ttl?: number; // seconds
}

/** The response to registering a client (POST .../clients). */
export interface Client {
  client_id: string;
  name: string;
  session_admin: boolean;
  roles: string[];
}

/** The response to a password join (POST .../join). */
export interface Joined {
  token: string;
  client_id: string;
  name: string;
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
