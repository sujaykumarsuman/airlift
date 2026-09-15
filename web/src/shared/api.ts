import type { Client, ConfigKey, Created, CreateOptions, IngestResult, Info, Joined, Snapshot } from "./types";

export type FetchFn = typeof fetch;

/**
 * Resolves an app-relative path (no leading slash, e.g. "api/sessions/abc")
 * against the document's base, so every request honours the <base href> the
 * tower injects and works under any path prefix (ADR 0012).
 */
export function apiURL(path: string): string {
  return new URL(path, document.baseURI).toString();
}

/** A non-2xx answer; `message` is the server's `error` field when present. */
export class ApiError extends Error {
  constructor(
    public readonly status: number,
    message: string,
  ) {
    super(message);
    this.name = "ApiError";
  }
}

/** Every authenticated call carries the token and, once registered, the client
 *  id (ADR 0017). */
/** The resume keys this page holds, by client id: a client's proof of identity
 *  on every client-tier call (ADR 0022, amended), so a phone that comes back on
 *  a new address is still itself. Pages restore it from storage on load. */
const resumeKeys = new Map<string, string>();

export function rememberClientKey(clientId: string, key: string | undefined): void {
  if (clientId && key) resumeKeys.set(clientId, key);
}

export function clientKey(clientId: string | undefined): string | undefined {
  return clientId ? resumeKeys.get(clientId) : undefined;
}

export function clientHeaders(token: string, clientId?: string): Record<string, string> {
  const h: Record<string, string> = { Authorization: `Bearer ${token}` };
  if (clientId) {
    h["X-Airlift-Client"] = clientId;
    const key = resumeKeys.get(clientId);
    if (key) h["X-Airlift-Client-Key"] = key;
  }
  return h;
}

async function expectJSON<T>(resp: Response): Promise<T> {
  if (!resp.ok) throw new ApiError(resp.status, await errorMessage(resp));
  return (await resp.json()) as T;
}

async function errorMessage(resp: Response): Promise<string> {
  try {
    const body = (await resp.json()) as { error?: unknown };
    if (typeof body.error === "string") return body.error;
  } catch {
    /* not JSON */
  }
  return `HTTP ${resp.status}`;
}

export function getInfo(fetchFn: FetchFn = fetch): Promise<Info> {
  return fetchFn(apiURL("api/info"), { headers: { accept: "application/json" } }).then((r) => expectJSON<Info>(r));
}

export function createSession(opts: CreateOptions = {}, fetchFn: FetchFn = fetch): Promise<Created> {
  return fetchFn(apiURL("api/sessions"), {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(opts),
  }).then(async (r) => {
    const c = await expectJSON<Created>(r);
    rememberClientKey(c.client_id, c.resume_key);
    return c;
  });
}

/** Registers a client for this device. `resume` (a client id this device holds)
 *  with its `resumeKey` keeps that identity on a reload, a second tab, or a
 *  return from sleep on a new address; without a key the tower honours the id
 *  only from the same address (ADR 0022). The reply's key is remembered. */
export function registerClient(
  sid: string,
  token: string,
  opts: { name?: string; role?: string; resume?: string; resumeKey?: string } = {},
  fetchFn: FetchFn = fetch,
): Promise<Client> {
  const { resume, resumeKey, ...rest } = opts;
  const body: Record<string, string> = { ...rest };
  const key = resumeKey ?? clientKey(resume);
  if (resume && key) body.resume_key = key;
  return fetchFn(apiURL(`api/sessions/${sid}/clients`), {
    method: "POST",
    headers: { ...clientHeaders(token, resume), "Content-Type": "application/json" },
    body: JSON.stringify(body),
  }).then(async (r) => {
    const c = await expectJSON<Client>(r);
    rememberClientKey(c.client_id, c.resume_key);
    return c;
  });
}

/** Joins a password-protected session (no token needed); returns a token. */
export function joinSession(
  sid: string,
  body: { password: string; name?: string },
  fetchFn: FetchFn = fetch,
): Promise<Joined> {
  return fetchFn(apiURL(`api/sessions/${sid}/join`), {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body),
  }).then(async (r) => {
    const j = await expectJSON<Joined>(r);
    rememberClientKey(j.client_id, j.resume_key);
    return j;
  });
}

export function getSnapshot(sid: string, token: string, clientId?: string, fetchFn: FetchFn = fetch): Promise<Snapshot> {
  return fetchFn(apiURL(`api/sessions/${sid}`), { headers: clientHeaders(token, clientId), cache: "no-store" }).then(
    (r) => expectJSON<Snapshot>(r),
  );
}

export function postFrames(
  sid: string,
  token: string,
  frames: string[],
  clientId?: string,
  fetchFn: FetchFn = fetch,
): Promise<IngestResult> {
  return fetchFn(apiURL(`api/sessions/${sid}/frames`), {
    method: "POST",
    headers: { ...clientHeaders(token, clientId), "Content-Type": "application/json" },
    body: JSON.stringify({ frames }),
  }).then((r) => expectJSON<IngestResult>(r));
}

export async function deleteSession(
  sid: string,
  token: string,
  clientId?: string,
  hard = false,
  fetchFn: FetchFn = fetch,
): Promise<void> {
  const resp = await fetchFn(apiURL(`api/sessions/${sid}${hard ? "?hard" : ""}`), {
    method: "DELETE",
    headers: clientHeaders(token, clientId),
  });
  if (!resp.ok && resp.status !== 404) throw new ApiError(resp.status, await errorMessage(resp));
}

/** Evicts a client (session admin). */
export async function deleteClient(
  sid: string,
  token: string,
  clientId: string,
  cid: string,
  fetchFn: FetchFn = fetch,
): Promise<void> {
  const resp = await fetchFn(apiURL(`api/sessions/${sid}/clients/${cid}`), {
    method: "DELETE",
    headers: clientHeaders(token, clientId),
  });
  if (!resp.ok) throw new ApiError(resp.status, await errorMessage(resp));
}

/** Removes a beam and reclaims its files (session admin). */
export async function deleteBeam(
  sid: string,
  token: string,
  clientId: string,
  bid: string,
  fetchFn: FetchFn = fetch,
): Promise<void> {
  const resp = await fetchFn(apiURL(`api/sessions/${sid}/beams/${bid}`), {
    method: "DELETE",
    headers: clientHeaders(token, clientId),
  });
  if (!resp.ok) throw new ApiError(resp.status, await errorMessage(resp));
}

/** Marks the client active on the session (client tier, rate_ping). 204 resolves;
 *  any other status throws ApiError so the caller can classify it. */
export async function postPing(
  sid: string,
  token: string,
  clientId: string,
  fetchFn: FetchFn = fetch,
): Promise<void> {
  const resp = await fetchFn(apiURL(`api/sessions/${sid}/ping`), {
    method: "POST",
    headers: clientHeaders(token, clientId),
  });
  if (!resp.ok) throw new ApiError(resp.status, await errorMessage(resp));
}

/** Requests an extension of a TERMINATED session (client tier, rate_extension).
 *  204 resolves; any other status throws ApiError so the caller can classify it. */
export async function postExtension(
  sid: string,
  token: string,
  clientId: string,
  reason: string,
  fetchFn: FetchFn = fetch,
): Promise<void> {
  const resp = await fetchFn(apiURL(`api/sessions/${sid}/extension`), {
    method: "POST",
    headers: { ...clientHeaders(token, clientId), "Content-Type": "application/json" },
    body: JSON.stringify({ reason }),
  });
  if (!resp.ok) throw new ApiError(resp.status, await errorMessage(resp));
}

/** Session admin grants the session another hour before the max_age cap ends it
 *  (ADR 0018). 200 resolves; any other status throws ApiError. */
export async function postExtendMaxAge(
  sid: string,
  token: string,
  clientId: string,
  fetchFn: FetchFn = fetch,
): Promise<void> {
  const resp = await fetchFn(apiURL(`api/sessions/${sid}/max-age`), {
    method: "POST",
    headers: clientHeaders(token, clientId),
  });
  if (!resp.ok) throw new ApiError(resp.status, await errorMessage(resp));
}

/** Knock: ask a public session's admin to be let in — no token, no password (ADR
 *  0021). 404 means the session needs its link or does not exist. */
export function postKnock(sid: string, name: string, fetchFn: FetchFn = fetch): Promise<{ id: string; status: string }> {
  return fetchFn(apiURL(`api/sessions/${sid}/knock`), {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ name }),
  }).then((r) => expectJSON<{ id: string; status: string }>(r));
}

/** Poll a knock's admission state; `token` is present once admitted. */
export function getKnockStatus(sid: string, fetchFn: FetchFn = fetch): Promise<{ status: string; token?: string }> {
  return fetchFn(apiURL(`api/sessions/${sid}/knock`), { cache: "no-store" }).then((r) => expectJSON<{ status: string; token?: string }>(r));
}

/** Admit or deny a pending knock by id (session admin, ADR 0021). */
export async function resolveKnock(
  sid: string,
  token: string,
  clientId: string,
  kid: string,
  decision: "admit" | "deny",
  fetchFn: FetchFn = fetch,
): Promise<void> {
  const resp = await fetchFn(apiURL(`api/sessions/${sid}/knock/${kid}`), {
    method: "POST",
    headers: { ...clientHeaders(token, clientId), "Content-Type": "application/json" },
    body: JSON.stringify({ decision }),
  });
  if (!resp.ok) throw new ApiError(resp.status, await errorMessage(resp));
}

/** Approve or deny a pending direct upload by id (session admin, ADR 0023). */
export async function resolveUpload(
  sid: string,
  token: string,
  clientId: string,
  uid: string,
  decision: "approve" | "deny",
  fetchFn: FetchFn = fetch,
): Promise<void> {
  const resp = await fetchFn(apiURL(`api/sessions/${sid}/uploads/${uid}`), {
    method: "POST",
    headers: { ...clientHeaders(token, clientId), "Content-Type": "application/json" },
    body: JSON.stringify({ decision }),
  });
  if (!resp.ok) throw new ApiError(resp.status, await errorMessage(resp));
}

/** True for the 403 {error:"evicted"} an evicted address receives. */
export function isEvicted(err: unknown): boolean {
  return err instanceof ApiError && err.status === 403 && err.message === "evicted";
}

export function eventsURL(sid: string, role: "relay" | "viewer"): string {
  return apiURL(`api/sessions/${sid}/events${role === "relay" ? "?role=relay" : ""}`);
}

/** Downloads go through fetch so the token can travel in the header. Each
 *  download names its beam by bid (a place may hold several). */
export async function fetchDownload(
  sid: string,
  token: string,
  bid: string,
  as: string,
  clientId?: string,
  fetchFn: FetchFn = fetch,
): Promise<{ blob: Blob; filename: string }> {
  const query = `beam=${encodeURIComponent(bid)}&as=${encodeURIComponent(as)}`;
  const resp = await fetchFn(apiURL(`api/sessions/${sid}/download?${query}`), {
    headers: clientHeaders(token, clientId),
  });
  if (!resp.ok) throw new ApiError(resp.status, await errorMessage(resp));
  return { blob: await resp.blob(), filename: parseFilename(resp.headers.get("Content-Disposition"), as) };
}

// ---- admin surface (ADR 0014): the admin_token is the Bearer on every call ----

export function adminEventsURL(): string {
  return apiURL("api/admin/events");
}

/** Probes the admin surface with a token; the config dump doubles as the login
 *  check (200 = valid, 401 = wrong, 404 = admin disabled). */
export function getAdminConfig(token: string, fetchFn: FetchFn = fetch): Promise<{ keys: ConfigKey[] }> {
  return fetchFn(apiURL("api/admin/config"), { headers: clientHeaders(token), cache: "no-store" }).then((r) =>
    expectJSON<{ keys: ConfigKey[] }>(r),
  );
}

/** Merges live-key changes into the overrides file and returns the fresh dump. */
export function patchAdminConfig(token: string, changes: Record<string, string>, fetchFn: FetchFn = fetch): Promise<{ keys: ConfigKey[] }> {
  return fetchFn(apiURL("api/admin/config"), {
    method: "PATCH",
    headers: { ...clientHeaders(token), "Content-Type": "application/json" },
    body: JSON.stringify({ changes }),
  }).then((r) => expectJSON<{ keys: ConfigKey[] }>(r));
}

async function adminAction(method: string, path: string, token: string, body: unknown, fetchFn: FetchFn): Promise<void> {
  const resp = await fetchFn(apiURL(path), {
    method,
    headers: body === undefined ? clientHeaders(token) : { ...clientHeaders(token), "Content-Type": "application/json" },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  if (!resp.ok) throw new ApiError(resp.status, await errorMessage(resp));
}

/** Terminate a session: with a warning countdown, or immediately (now). */
export function adminTerminate(token: string, sid: string, now: boolean, fetchFn: FetchFn = fetch): Promise<void> {
  return adminAction("DELETE", `api/admin/sessions/${sid}${now ? "?now" : ""}`, token, undefined, fetchFn);
}
export function adminCancel(token: string, sid: string, fetchFn: FetchFn = fetch): Promise<void> {
  return adminAction("POST", `api/admin/sessions/${sid}/cancel-termination`, token, undefined, fetchFn);
}
export function adminReview(token: string, sid: string, decision: "accept" | "reject", note: string, fetchFn: FetchFn = fetch): Promise<void> {
  return adminAction("POST", `api/admin/sessions/${sid}/review`, token, { decision, note }, fetchFn);
}
export function adminEvict(token: string, sid: string, cid: string, fetchFn: FetchFn = fetch): Promise<void> {
  return adminAction("DELETE", `api/admin/sessions/${sid}/clients/${cid}`, token, undefined, fetchFn);
}

/** Downloads a beam as the operator (no activity is marked). */
export async function fetchAdminDownload(
  token: string,
  sid: string,
  bid: string,
  as: string,
  fetchFn: FetchFn = fetch,
): Promise<{ blob: Blob; filename: string }> {
  const query = `beam=${encodeURIComponent(bid)}&as=${encodeURIComponent(as)}`;
  const resp = await fetchFn(apiURL(`api/admin/sessions/${sid}/download?${query}`), { headers: clientHeaders(token) });
  if (!resp.ok) throw new ApiError(resp.status, await errorMessage(resp));
  return { blob: await resp.blob(), filename: parseFilename(resp.headers.get("Content-Disposition"), as) };
}

/** Reads `filename` (or RFC 5987 `filename*`) out of a Content-Disposition header. */
export function parseFilename(disposition: string | null, fallback: string): string {
  if (!disposition) return fallback;
  const star = /filename\*=(?:UTF-8|utf-8)''([^;]+)/.exec(disposition);
  if (star?.[1]) {
    try {
      return decodeURIComponent(star[1]);
    } catch {
      /* fall through */
    }
  }
  const plain = /filename="?([^";]+)"?/.exec(disposition);
  return plain?.[1]?.trim() || fallback;
}
