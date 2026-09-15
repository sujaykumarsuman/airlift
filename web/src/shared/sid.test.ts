import { describe, expect, test } from "vitest";
import { type Formatted, formatSessionId, isSessionId, looksLikeLink, sessionIdFromPaste, tokenFor } from "./sid";

/** A field value with the caret marked by "|", e.g. "abc-|d". */
function at(marked: string): Formatted {
  const caret = marked.indexOf("|");
  return { value: marked.replace("|", ""), caret };
}
const show = (f: Formatted) => f.value.slice(0, f.caret) + "|" + f.value.slice(f.caret);

/** Simulates the browser applying an edit to a formatted state, then formats. */
function type(state: string, text: string): string {
  const s = at(state);
  const raw = s.value.slice(0, s.caret) + text + s.value.slice(s.caret);
  return show(formatSessionId(raw, s.caret + text.length, true, s));
}
function backspace(state: string): string {
  const s = at(state);
  if (s.caret === 0) return state;
  const raw = s.value.slice(0, s.caret - 1) + s.value.slice(s.caret);
  return show(formatSessionId(raw, s.caret - 1, false, s));
}
function forwardDelete(state: string): string {
  const s = at(state);
  const raw = s.value.slice(0, s.caret) + s.value.slice(s.caret + 1);
  return show(formatSessionId(raw, s.caret, false, s));
}
function typeAll(text: string, start = "|"): string {
  let state = start;
  for (const ch of text) state = type(state, ch);
  return state;
}

describe("typing", () => {
  test("nine letters become a session id, the hyphens placed as each group fills", () => {
    const steps: string[] = [];
    let state = "|";
    for (const ch of "qkfmztbwp") steps.push((state = type(state, ch)));
    expect(steps).toEqual(["q|", "qk|", "qkf-|", "qkf-m|", "qkf-mz|", "qkf-mzt-|", "qkf-mzt-b|", "qkf-mzt-bw|", "qkf-mzt-bwp|"]);
  });

  test("uppercase is lowercased and anything but letters is dropped", () => {
    expect(typeAll("QKF MZT 1BWP!")).toBe("qkf-mzt-bwp|");
    expect(typeAll("ab1")).toBe("ab|");
    expect(typeAll("é")).toBe("|");
  });

  test("a typed hyphen lands in its place: never doubled, ignored mid-group", () => {
    expect(typeAll("qkf-")).toBe("qkf-|");
    expect(typeAll("qkf--mzt")).toBe("qkf-mzt-|");
    expect(typeAll("q-k-f-m")).toBe("qkf-m|");
    expect(typeAll("-")).toBe("|");
    expect(typeAll("qkf-mzt-bwp")).toBe("qkf-mzt-bwp|");
    expect(type("qkf|-mzt", "-")).toBe("qkf-|mzt"); // before a hyphen: the caret steps over it
  });

  test("a full id refuses a tenth letter wherever the caret is", () => {
    expect(type("qkf-mzt-bwp|", "x")).toBe("qkf-mzt-bwp|");
    expect(type("q|kf-mzt-bwp", "x")).toBe("q|kf-mzt-bwp");
    expect(type("qkf-mzt-bwp|", "-")).toBe("qkf-mzt-bwp|");
  });

  test("typing in the middle keeps the caret after the letter typed", () => {
    expect(type("q|kf-mzt", "x")).toBe("qx|k-fmz-t");
    expect(type("qkf|-mzt", "x")).toBe("qkf-x|mz-t"); // the letters regroup; the caret follows the x
    expect(type("|qkf", "x")).toBe("x|qk-f");
  });
});

describe("deleting", () => {
  test("backspace always removes something: the hyphen, then the letter", () => {
    expect(backspace("qkf-m|")).toBe("qkf-|");
    expect(backspace("qkf-|")).toBe("qkf|");
    expect(backspace("qkf|")).toBe("qk|");
    expect(backspace("qkf-mzt-|")).toBe("qkf-mzt|");
  });

  test("backspace from the end all the way to empty", () => {
    let state = "qkf-mzt-bwp|";
    const seen = [state];
    while (state !== "|") {
      const next = backspace(state);
      expect(next).not.toBe(state); // never stuck
      seen.push((state = next));
    }
    expect(seen.length).toBe(12); // 9 letters + 2 hyphens, then empty
  });

  test("deleting in the middle closes the gap and keeps the caret in place", () => {
    expect(backspace("qkf-mz|t-bwp")).toBe("qkf-m|tb-wp");
    expect(backspace("qkf-m|zt")).toBe("qkf-|zt");
    expect(backspace("qkf-|mzt")).toBe("qkf|-mzt"); // a middle hyphen is structure: it returns, the caret moves before it
    expect(forwardDelete("qkf|-mzt")).toBe("qkf|-mzt");
    expect(forwardDelete("qk|f-mzt")).toBe("qk|m-zt");
  });

  test("selecting everything and typing starts over", () => {
    expect(show(formatSessionId("x", 1, true, at("qkf-mzt-bwp|")))).toBe("x|");
  });

  test("a deletion never leaves a hyphen the user removed at the end", () => {
    expect(show(formatSessionId("qkf", 3, false))).toBe("qkf|");
    expect(show(formatSessionId("qkfmzt", 6, false))).toBe("qkf-mzt|");
  });
});

describe("pasting plain text", () => {
  test("letters with spaces, dashes or case are fitted; more than nine keeps the first nine", () => {
    expect(show(formatSessionId("QKF MZT BWP", 11, true, at("|")))).toBe("qkf-mzt-bwp|");
    expect(show(formatSessionId("qkf_mzt_bwp_xyz", 15, true, at("|")))).toBe("qkf-mzt-bwp|");
    expect(show(formatSessionId("qkfmzt", 6, true, at("|")))).toBe("qkf-mzt-|");
  });
});

describe("the invariant, for every edit", () => {
  test("any raw text and caret gives a well-formed prefix of an id with the caret inside it", () => {
    const alphabet = ["a", "Z", "-", " ", "1", "é"];
    let seed = 7;
    const rand = (n: number) => ((seed = (seed * 1103515245 + 12345) % 2147483648), seed % n);
    for (let i = 0; i < 5000; i++) {
      const len = rand(16);
      let raw = "";
      for (let j = 0; j < len; j++) raw += alphabet[rand(alphabet.length)];
      const caret = rand(raw.length + 1);
      const f = formatSessionId(raw, caret, rand(2) === 1);
      expect(f.value).toMatch(/^([a-z]{0,3}|[a-z]{3}-[a-z]{0,3}|[a-z]{3}-[a-z]{3}-[a-z]{0,3})$/);
      expect(f.caret).toBeGreaterThanOrEqual(0);
      expect(f.caret).toBeLessThanOrEqual(f.value.length);
      expect(formatSessionId(f.value, f.caret, false).value).toBe(f.value); // formatting is stable
    }
  });
});

describe("links and ids in pasted text", () => {
  test("a share link gives its id and token; other links their id", () => {
    const pick = (text: string) => {
      const p = sessionIdFromPaste(text);
      return p && { sid: p.sid, token: p.token };
    };
    expect(pick("https://projects.sujaykumar.dev/airlift/qkf-mzt-bwp#t=abc_DEF-123")).toEqual({ sid: "qkf-mzt-bwp", token: "abc_DEF-123" });
    expect(pick(" https://projects.sujaykumar.dev/airlift/QKF-MZT-BWP ")).toEqual({ sid: "qkf-mzt-bwp", token: undefined });
    expect(pick("http://127.0.0.1:8443/s/qkf-mzt-bwp#t=tok&c=0123")).toEqual({ sid: "qkf-mzt-bwp", token: "tok" });
    expect(pick("https://h/airlift/#s=qkf-mzt-bwp&t=tok")).toEqual({ sid: "qkf-mzt-bwp", token: "tok" });
    expect(sessionIdFromPaste("https://h/airlift/docs")).toBeUndefined();
  });

  test("an id inside a sentence is found; a run of letters is left to the formatter", () => {
    expect(sessionIdFromPaste("join me in qkf-mzt-bwp please")).toEqual({ sid: "qkf-mzt-bwp" });
    expect(sessionIdFromPaste("xqkf-mzt-bwp")).toBeUndefined();
    expect(sessionIdFromPaste("qkfmztbwp")).toBeUndefined();
  });

  test("a pasted token is kept only for this tower's link and only while the id is unchanged", () => {
    const base = new URL("https://projects.sujaykumar.dev/airlift/");
    const ours = sessionIdFromPaste("https://projects.sujaykumar.dev/airlift/qkf-mzt-bwp#t=tok");
    expect(tokenFor(ours, "qkf-mzt-bwp", base)).toBe("tok");
    expect(tokenFor(ours, "qkf-mzt-bwq", base)).toBeUndefined();
    expect(tokenFor(sessionIdFromPaste("https://other.example/airlift/qkf-mzt-bwp#t=tok"), "qkf-mzt-bwp", base)).toBeUndefined();
    expect(tokenFor(sessionIdFromPaste("https://projects.sujaykumar.dev/elsewhere/qkf-mzt-bwp#t=tok"), "qkf-mzt-bwp", base)).toBeUndefined();
    expect(tokenFor(undefined, "qkf-mzt-bwp", base)).toBeUndefined();
  });

  test("link detection", () => {
    expect(looksLikeLink("https://example.com/x")).toBe(true);
    expect(looksLikeLink("projects.sujaykumar.dev/airlift/abc")).toBe(true);
    expect(looksLikeLink("qkf mzt bwp")).toBe(false);
  });

  test("isSessionId", () => {
    expect(isSessionId("qkf-mzt-bwp")).toBe(true);
    for (const bad of ["qkf-mzt-bw", "QKF-MZT-BWP", "qkfmztbwp", "qkf-mzt-bwp-", " qkf-mzt-bwp"]) expect(isSessionId(bad)).toBe(false);
  });
});
