import type { Client, Created, CreateOptions, IngestResult, Info, Joined, Snapshot } from "./types";

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
export function clientHeaders(token: string, clientId?: string): Record<string, string> {
  const h: Record<string, string> = { Authorization: `Bearer ${token}` };
  if (clientId) h["X-Airlift-Client"] = clientId;
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
  }).then((r) => expectJSON<Created>(r));
}

/** Registers (or returns) the client bound to the caller's address. */
export function registerClient(
  sid: string,
  token: string,
  opts: { name?: string; role?: string } = {},
  fetchFn: FetchFn = fetch,
): Promise<Client> {
  return fetchFn(apiURL(`api/sessions/${sid}/clients`), {
    method: "POST",
    headers: { ...clientHeaders(token), "Content-Type": "application/json" },
    body: JSON.stringify(opts),
  }).then((r) => expectJSON<Client>(r));
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
  }).then((r) => expectJSON<Joined>(r));
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
  fetchFn: FetchFn = fetch,
): Promise<void> {
  const resp = await fetchFn(apiURL(`api/sessions/${sid}`), {
    method: "DELETE",
    headers: clientHeaders(token, clientId),
  });
  if (!resp.ok && resp.status !== 404) throw new ApiError(resp.status, await errorMessage(resp));
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
