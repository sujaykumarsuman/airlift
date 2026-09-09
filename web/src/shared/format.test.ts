import { expect, test } from "vitest";
import { parseFilename } from "./api";
import { formatBytes, formatDuration, hex8, shortHex } from "./format";
import { cyrb53 } from "./hash";
import { esc, html, raw } from "./dom";

test("formats", () => {
  expect(formatBytes(512)).toBe("512 B");
  expect(formatBytes(14212)).toBe("13.9 KB");
  expect(formatBytes(20 * 1024 * 1024)).toBe("20 MB");
  expect(formatDuration(4200)).toBe("4s");
  expect(formatDuration(65 * 1000)).toBe("1m 05s");
  expect(formatDuration(3 * 3600 * 1000 + 7 * 60 * 1000)).toBe("3h 07m");
  expect(hex8(577090037)).toBe("2265b1f5");
  expect(hex8(null)).toBe("—");
  expect(shortHex("80fc59ff8fa97bb7f5eb8b59", 12)).toBe("80fc59ff8fa9…");
});

test("content-disposition filenames", () => {
  expect(parseFilename('attachment; filename="bundle-base64.zip"', "x")).toBe("bundle-base64.zip");
  expect(parseFilename("attachment; filename=plain.txt", "x")).toBe("plain.txt");
  expect(parseFilename("attachment; filename*=UTF-8''caf%C3%A9.txt", "x")).toBe("café.txt");
  expect(parseFilename(null, "raw")).toBe("raw");
});

test("hash is stable and spreads", () => {
  expect(cyrb53("airlift")).toBe(cyrb53("airlift"));
  expect(cyrb53("airlift")).not.toBe(cyrb53("airlifT"));
  const seen = new Set<number>();
  for (let i = 0; i < 5000; i++) seen.add(cyrb53(`frame-${i}`));
  expect(seen.size).toBe(5000);
});

test("html template escapes unless raw", () => {
  expect(esc(`<a href="x">'&'</a>`)).toBe("&lt;a href=&quot;x&quot;&gt;&#39;&amp;&#39;&lt;/a&gt;");
  expect(html`<b>${"<i>"}</b>${raw("<u>ok</u>")}${["<", raw(">")]}${html`<p>${"&"}</p>`}${null}${false}`.html).toBe(
    "<b>&lt;i&gt;</b><u>ok</u>&lt;><p>&amp;</p>",
  );
});
