import { readFileSync } from "node:fs";
import { expect, test } from "vitest";
import { readBarcodes } from "zxing-wasm/reader";

// The beam player's inline QR encoder (internal/beam/qrjs.js, embedded into
// every beam by Go) against the Go reference encoder's symbols
// (testdata/qr/matrices.json, `go test ./internal/beam -run TestQRFixtureCurrent -update`).
// The beam is offline and shares no build with web/, so the file is loaded as
// the browser will see it: plain script text, evaluated.

interface Plan {
  version: number;
  level: number;
  n: number;
  data: number;
  check: number;
  blocks: number;
  occ: string;
  base: string;
}
interface Encoded {
  mask: number;
  bits: Uint8Array;
}
interface Encoder {
  n: number;
  free: number;
  totalBits: number;
  encode(text: string): Encoded;
}
interface Fixture {
  version: number;
  n: number;
  occ: string;
  base: string;
  levels: { level: string; data: number; check: number; blocks: number; cases: { text: string; mask: number; bits: string }[] }[];
}

const src = readFileSync(new URL("../../../internal/beam/qrjs.js", import.meta.url), "utf8");
const airliftQR = new Function(`${src}\nreturn airliftQR;`)() as (plan: Plan) => Encoder;
const fixture = JSON.parse(readFileSync(new URL("../../../testdata/qr/matrices.json", import.meta.url), "utf8")) as Fixture[];
const LEVELS = ["L", "M", "Q", "H"];

function unpack(b64: string, n: number): Uint8Array {
  const raw = Buffer.from(b64, "base64");
  const out = new Uint8Array(n * n);
  for (let i = 0; i < n * n; i++) out[i] = (raw[i >> 3]! >> (7 - (i & 7))) & 1;
  return out;
}

function plans(): { plan: Plan; level: string; cases: Fixture["levels"][number]["cases"] }[] {
  return fixture.flatMap((v) =>
    v.levels.map((l) => ({
      level: l.level,
      cases: l.cases,
      plan: { version: v.version, level: LEVELS.indexOf(l.level), n: v.n, data: l.data, check: l.check, blocks: l.blocks, occ: v.occ, base: v.base },
    })),
  );
}

test("the fixture covers the default beam symbol and every level", () => {
  const seen = new Set(plans().map((p) => `${p.plan.version}${p.level}`));
  for (const want of ["30M", "30L", "30H", "1M", "40L", "10Q"]) expect(seen.has(want)).toBe(true);
  expect(plans().reduce((n, p) => n + p.cases.length, 0)).toBeGreaterThan(80);
});

test("the plan accounts for every free module", () => {
  for (const { plan } of plans()) {
    const qr = airliftQR(plan);
    expect(qr.n).toBe(plan.n);
    expect(qr.totalBits).toBe((plan.data + plan.check) * 8);
    expect(qr.free - qr.totalBits).toBeGreaterThanOrEqual(0);
    expect(qr.free - qr.totalBits).toBeLessThanOrEqual(7);
  }
});

test("every reference symbol is reproduced bit for bit, mask included", () => {
  let checked = 0;
  for (const { plan, level, cases } of plans()) {
    const qr = airliftQR(plan);
    for (const c of cases) {
      const got = qr.encode(c.text);
      const want = unpack(c.bits, plan.n);
      expect(got.mask, `v${plan.version} ${level} ${c.text.length} chars: mask`).toBe(c.mask);
      let diff = -1;
      for (let i = 0; i < want.length && diff < 0; i++) if (got.bits[i] !== want[i]) diff = i;
      expect(diff, `v${plan.version} ${level} ${c.text.length} chars: first differing module`).toBe(-1);
      checked++;
    }
  }
  expect(checked).toBeGreaterThan(80);
});

test("a frame that does not fit the symbol, or is not base45, is refused, not truncated", () => {
  const { plan, cases } = plans().find((p) => p.plan.version === 1 && p.level === "M")!;
  const qr = airliftQR(plan);
  const longest = cases.reduce((a, b) => (a.text.length > b.text.length ? a : b)).text;
  expect(() => qr.encode(longest + "A")).toThrow(/does not fit/);
  expect(() => qr.encode("ABC#")).toThrow(/not base45/);
  expect(() => qr.encode("abc")).toThrow(/not base45/);
  // and the encoder is intact afterwards
  const c = cases[0]!;
  expect(qr.encode(c.text).mask).toBe(c.mask);
});

test("a result is the caller's own: the next encode does not overwrite it", () => {
  const { plan, cases } = plans().find((p) => p.plan.version === 30 && p.level === "M")!;
  const qr = airliftQR(plan);
  const first = qr.encode(cases[0]!.text);
  const copy = Uint8Array.from(first.bits);
  qr.encode(cases[1]!.text);
  expect(first.bits).toEqual(copy);
});

// A symbol as the camera sees it: white quiet zone, pitch px per module.
function raster(bits: Uint8Array, n: number, pitch: number): ImageData {
  const w = (n + 8) * pitch;
  const data = new Uint8ClampedArray(w * w * 4).fill(255);
  for (let y = 0; y < n; y++)
    for (let x = 0; x < n; x++) {
      if (!bits[y * n + x]) continue;
      for (let dy = 0; dy < pitch; dy++)
        for (let dx = 0; dx < pitch; dx++) {
          const p = (((y + 4) * pitch + dy) * w + (x + 4) * pitch + dx) * 4;
          data[p] = data[p + 1] = data[p + 2] = 0;
        }
    }
  return { data, width: w, height: w, colorSpace: "srgb" } as ImageData;
}

test("a real decoder reads the encoder's symbols back, tiny to version 40", async () => {
  const picks = plans().filter((p) => ["1M", "10Q", "30M", "40L"].includes(`${p.plan.version}${p.level}`));
  expect(picks.length).toBe(4);
  for (const { plan, cases } of picks) {
    const qr = airliftQR(plan);
    for (const c of [cases[0]!, cases[cases.length - 1]!]) {
      const { bits } = qr.encode(c.text);
      const found = await readBarcodes(raster(bits, plan.n, 4), { formats: ["QRCode"], tryHarder: true });
      expect(found.length, `v${plan.version} ${c.text.length} chars`).toBe(1);
      expect(found[0]!.text).toBe(c.text);
    }
  }
}, 30_000);
