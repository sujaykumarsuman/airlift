import type { Created, IngestResult, Info, Snapshot } from "./types";

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

export function authHeaders(token: string): Record<string, string> {
  return { Authorization: `Bearer ${token}` };
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

export function createSession(fetchFn: FetchFn = fetch): Promise<Created> {
  return fetchFn(apiURL("api/sessions"), { method: "POST" }).then((r) => expectJSON<Created>(r));
}

export function getSnapshot(sid: string, token: string, fetchFn: FetchFn = fetch): Promise<Snapshot> {
  return fetchFn(apiURL(`api/sessions/${sid}`), { headers: authHeaders(token), cache: "no-store" }).then((r) =>
    expectJSON<Snapshot>(r),
  );
}

export function postFrames(
  sid: string,
  token: string,
  frames: string[],
  fetchFn: FetchFn = fetch,
): Promise<IngestResult> {
  return fetchFn(apiURL(`api/sessions/${sid}/frames`), {
    method: "POST",
    headers: { ...authHeaders(token), "Content-Type": "application/json" },
    body: JSON.stringify({ frames }),
  }).then((r) => expectJSON<IngestResult>(r));
}

export async function deleteSession(sid: string, token: string, fetchFn: FetchFn = fetch): Promise<void> {
  const resp = await fetchFn(apiURL(`api/sessions/${sid}`), { method: "DELETE", headers: authHeaders(token) });
  if (!resp.ok && resp.status !== 404) throw new ApiError(resp.status, await errorMessage(resp));
}

export function eventsURL(sid: string, role: "relay" | "viewer"): string {
  return apiURL(`api/sessions/${sid}/events${role === "relay" ? "?role=relay" : ""}`);
}

/** Downloads go through fetch so the token can travel in the header. */
export async function fetchDownload(
  sid: string,
  token: string,
  as: string,
  fetchFn: FetchFn = fetch,
): Promise<{ blob: Blob; filename: string }> {
  const resp = await fetchFn(apiURL(`api/sessions/${sid}/download?as=${encodeURIComponent(as)}`), {
    headers: authHeaders(token),
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
