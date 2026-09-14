export type Join =
  | { sid: string; token: string; client?: string; needsPassword?: undefined; error?: undefined }
  | { sid: string; token?: undefined; client?: undefined; needsPassword: true; error?: undefined }
  | { error: string; sid?: undefined; token?: undefined; client?: undefined; needsPassword?: undefined };

/** Strips a path prefix ("/airlift") off a pathname, leaving a rooted "/s/…". */
function underBase(pathname: string, basePath: string): string {
  const rel = basePath && pathname.startsWith(basePath) ? pathname.slice(basePath.length) : pathname;
  return rel.startsWith("/") ? rel : "/" + rel;
}

/** Reads /s/{sid} from the path (below basePath), t= from the fragment, and an
 *  optional c= — the dashboard's client id, so the scanner it opened resumes the
 *  same participant (ADR 0022). */
export function parseJoin(pathname: string, hash: string, basePath = ""): Join {
  const m = /^\/s\/([A-Za-z0-9_-]+)\/?$/.exec(underBase(pathname, basePath));
  if (!m?.[1]) return { error: "This is not a join link: the address should look like /s/<session>#t=<token>." };
  const params = new URLSearchParams(hash.replace(/^#/, ""));
  const token = params.get("t")?.trim() ?? "";
  // No usable token: the page offers a password join (404s if none is set).
  if (!/^[A-Za-z0-9_-]{8,}$/.test(token)) return { sid: m[1], needsPassword: true };
  const client = params.get("c")?.trim() ?? "";
  return /^[0-9a-f]{16}$/.test(client) ? { sid: m[1], token, client } : { sid: m[1], token };
}

export const LAST_KEY = "airlift.lastJoin";

export interface JoinStorage {
  getItem(key: string): string | null;
  setItem(key: string, value: string): void;
}

export type Resolved = Join & { redirect?: string };

interface Saved {
  sid?: unknown;
  token?: unknown;
}

function readSaved(storage: JoinStorage | null): Saved | null {
  try {
    return JSON.parse(storage?.getItem(LAST_KEY) ?? "null") as Saved | null;
  } catch {
    return null;
  }
}

/**
 * Like parseJoin, but `/s/last` reopens the session this phone joined most
 * recently (the installed app starts there), and a successful join is
 * remembered for next time.
 */
export function resolveJoin(pathname: string, hash: string, storage: JoinStorage | null, basePath = ""): Resolved {
  if (/^\/s\/last\/?$/.test(underBase(pathname, basePath))) {
    const saved = readSaved(storage);
    if (saved && typeof saved.sid === "string" && typeof saved.token === "string") {
      // App-relative; the caller resolves it against <base href>.
      return { sid: saved.sid, token: saved.token, redirect: `s/${saved.sid}#t=${saved.token}` };
    }
    return { error: "No previous session on this phone. Scan the tower's QR code to join one." };
  }
  const join = parseJoin(pathname, hash, basePath);
  if (join.token !== undefined) {
    try {
      storage?.setItem(LAST_KEY, JSON.stringify({ sid: join.sid, token: join.token }));
    } catch {
      /* storage unavailable: nothing to remember */
    }
  }
  return join;
}
