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
