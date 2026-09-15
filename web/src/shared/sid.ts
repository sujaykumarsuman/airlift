// Session ids (ADR 0020) are three groups of three lowercase letters joined by
// hyphens: qkf-mzt-bwp. The join field keeps whatever is typed or pasted in
// that shape as it happens — letters only, lowercased, at most nine, the
// hyphens placed by the field — so a person never has to think about them.
// Pure functions only; tower/main.ts wires them to the input.

export const SID_LETTERS = 9;
const GROUP = 3;
const SID_RE = /^[a-z]{3}-[a-z]{3}-[a-z]{3}$/;

/** A complete session id. */
export function isSessionId(s: string): boolean {
  return SID_RE.test(s);
}

/** The field's value and caret after formatting. */
export interface Formatted {
  value: string;
  caret: number;
}

/** The letters a–z in s, lowercased, in order. */
function lettersOf(s: string): string {
  return s.toLowerCase().replace(/[^a-z]/g, "");
}

/**
 * formatSessionId turns the field's state after an edit into its formatted
 * state.
 *
 * - raw, caret: the value and caret position the browser left after the edit.
 * - insert: the edit added text (typing, paste, drop) rather than removing it.
 * - prev: the formatted state before the edit, when known.
 *
 * Letters are kept (lowercased, at most nine) and everything else dropped; the
 * hyphens are placed between groups. A hyphen after a complete first or second
 * group is shown while typing at the end — the separator appears as the group
 * fills, and a typed hyphen there lands in its place — and is kept on deleting
 * only if one is still there, so backspace always removes something. With the
 * field full, a keystroke that would add a tenth letter changes nothing. The
 * caret stays after the same number of letters, stepping past a hyphen it
 * reached by typing or by deleting back to it.
 */
export function formatSessionId(raw: string, caret: number, insert: boolean, prev?: Formatted): Formatted {
  const all = lettersOf(raw);
  if (insert && prev && lettersOf(prev.value).length >= SID_LETTERS && all.length > SID_LETTERS) {
    return prev; // full: the keystroke would push a letter out
  }
  const kept = all.slice(0, SID_LETTERS);
  const at = Math.max(0, Math.min(caret, raw.length));
  const before = Math.min(lettersOf(raw.slice(0, at)).length, kept.length);

  const groups: string[] = [];
  for (let i = 0; i < kept.length; i += GROUP) groups.push(kept.slice(i, i + GROUP));
  let value = groups.join("-");

  const boundary = kept.length > 0 && kept.length < SID_LETTERS && kept.length % GROUP === 0;
  if (boundary) {
    const lastLetter = raw.search(/[a-zA-Z][^a-zA-Z]*$/);
    const tail = lastLetter < 0 ? "" : raw.slice(lastLetter + 1);
    if ((insert && before === kept.length) || tail.includes("-")) value += "-";
  }

  let pos = before + Math.floor(Math.max(0, before - 1) / GROUP);
  if (value[pos] === "-" && (insert || raw[at - 1] === "-")) pos++;
  return { value, caret: Math.min(pos, value.length) };
}

/** What a paste into the field names: a session id, and the token and link when it was a share link. */
export interface Pasted {
  sid: string;
  token?: string;
  url?: URL; // the pasted link, so a caller can keep the token only for its own tower
}

/**
 * sessionIdFromPaste finds a session id in pasted text that is more than the
 * id itself — a share link (`https://…/qkf-mzt-bwp#t=…`), a scanner link, or a
 * sentence containing one — preferring the link's own path. The token comes
 * back only from a link's fragment. It returns undefined for text with no
 * id-shaped run, which the field then formats letter by letter.
 */
export function sessionIdFromPaste(text: string): Pasted | undefined {
  const trimmed = text.trim();
  try {
    const url = new URL(trimmed);
    if (url.protocol === "http:" || url.protocol === "https:") {
      const segs = url.pathname.split("/").filter(Boolean);
      const fromPath = [...segs].reverse().find((s) => isSessionId(s.toLowerCase()));
      const frag = new URLSearchParams(url.hash.replace(/^#/, ""));
      const sid = fromPath?.toLowerCase() ?? frag.get("s")?.toLowerCase();
      if (sid && isSessionId(sid)) {
        const token = frag.get("t")?.trim();
        return token ? { sid, token, url } : { sid, url };
      }
      return undefined;
    }
  } catch {
    // not a URL: look for an id in the text
  }
  const m = /(?:^|[^a-z])([a-z]{3}-[a-z]{3}-[a-z]{3})(?![a-z])/i.exec(trimmed);
  return m ? { sid: m[1]!.toLowerCase() } : undefined;
}

/**
 * tokenFor is the pasted link's token when that link is for this tower — the
 * same origin, under the app's base path — and undefined otherwise: a token for
 * another tower's session is never sent to this one.
 */
export function tokenFor(pasted: Pasted | undefined, sid: string, appBase: URL): string | undefined {
  if (!pasted?.token || !pasted.url || pasted.sid !== sid) return undefined;
  const sameTower = pasted.url.origin === appBase.origin && pasted.url.pathname.startsWith(appBase.pathname);
  return sameTower ? pasted.token : undefined;
}

/** Whether pasted text looks like a link, which letter-by-letter formatting would only mangle. */
export function looksLikeLink(text: string): boolean {
  return /^[a-z][a-z0-9+.-]*:\/\//i.test(text.trim()) || /^[\w.-]+\.[a-z]{2,}\//i.test(text.trim());
}
