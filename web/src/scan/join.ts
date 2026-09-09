export type Join = { sid: string; token: string; error?: undefined } | { error: string; sid?: undefined; token?: undefined };

/** Reads /s/{sid} from the path and t= from the fragment. */
export function parseJoin(pathname: string, hash: string): Join {
  const m = /^\/s\/([A-Za-z0-9_-]+)\/?$/.exec(pathname);
  if (!m?.[1]) return { error: "This is not a join link: the address should look like /s/<session>#t=<token>." };
  const params = new URLSearchParams(hash.replace(/^#/, ""));
  const token = params.get("t")?.trim() ?? "";
  if (!/^[A-Za-z0-9_-]{8,}$/.test(token)) {
    return { error: "The join link has no session token after #t=. Scan the tower's QR code again." };
  }
  return { sid: m[1], token };
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
export function resolveJoin(pathname: string, hash: string, storage: JoinStorage | null): Resolved {
  if (/^\/s\/last\/?$/.test(pathname)) {
    const saved = readSaved(storage);
    if (saved && typeof saved.sid === "string" && typeof saved.token === "string") {
      return { sid: saved.sid, token: saved.token, redirect: `/s/${saved.sid}#t=${saved.token}` };
    }
    return { error: "No previous session on this phone. Scan the tower's QR code to join one." };
  }
  const join = parseJoin(pathname, hash);
  if (join.error === undefined) {
    try {
      storage?.setItem(LAST_KEY, JSON.stringify({ sid: join.sid, token: join.token }));
    } catch {
      /* storage unavailable: nothing to remember */
    }
  }
  return join;
}
