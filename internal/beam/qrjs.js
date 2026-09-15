// airlift beam - the in-page QR encoder (ADR 0011, amended).
//
// The Go side picks the version and ECC for a beam and ships the plan for that
// symbol: the size, the block structure and two bitmaps - which modules are
// function patterns (occ) and which of those are dark (base). This file turns
// one frame's text into that symbol: alphanumeric data bits -> Reed-Solomon
// check bytes -> block interleave -> zig-zag placement -> the best of the eight
// masks by the ISO/IEC 18004 section 8.8.2 penalty -> format bits. It is a twin of
// rsc.io/qr/coding's Plan.Encode plus internal/beam/penalty.go and is checked
// bit for bit against them (web/src/beam/qrjs.test.ts, testdata/qr).
//
// Plain ES5 on purpose: the beam runs in whatever browser the air-gapped
// machine has. No dependencies, no network. ASCII on purpose too: this file is
// inlined into every beam, and one non-Latin-1 character would make the
// browser keep the whole script (about the page's size) as two-byte text.
function airliftQR(plan) {
  'use strict';
  var n = plan.n, N = n * n;
  var level = plan.level; // rsc.io numbering: L=0, M=1, Q=2, H=3
  var dataBytes = plan.data, checkBytes = plan.check, blocks = plan.blocks;
  var nde = Math.floor(dataBytes / blocks), extra = dataBytes % blocks;
  var ne = checkBytes / blocks;
  var countBits = plan.version <= 9 ? 9 : plan.version <= 26 ? 11 : 13;
  var ALPHA = '0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ $%*+-./:';

  // --- the plan's bitmaps: occupied (function) modules and their colour ---
  function unpack(b64) {
    var s = atob(b64), out = new Uint8Array(N);
    for (var i = 0; i < N; i++) out[i] = (s.charCodeAt(i >> 3) >> (7 - (i & 7))) & 1;
    return out;
  }
  var occ = unpack(plan.occ), base = unpack(plan.base);

  // Zig-zag placement order over the free modules: pairs of columns from the
  // right, up then down, right module before left, skipping column 6.
  var order = new Int32Array(N), free = 0;
  for (var x = n; x > 0;) {
    var y;
    for (y = n - 1; y >= 0; y--) {
      if (!occ[y * n + x - 1]) order[free++] = y * n + x - 1;
      if (!occ[y * n + x - 2]) order[free++] = y * n + x - 2;
    }
    x -= 2;
    if (x === 7) x--;
    for (y = 0; y < n; y++) {
      if (!occ[y * n + x - 1]) order[free++] = y * n + x - 1;
      if (!occ[y * n + x - 2]) order[free++] = y * n + x - 2;
    }
    x -= 2;
  }

  // The eight mask patterns over the free modules (zero elsewhere).
  var MASK = [
    function (i, j) { return (i + j) % 2 === 0; },
    function (i, j) { return i % 2 === 0; },
    function (i, j) { return j % 3 === 0; },
    function (i, j) { return (i + j) % 3 === 0; },
    function (i, j) { return (Math.floor(i / 2) + Math.floor(j / 3)) % 2 === 0; },
    function (i, j) { return (i * j) % 2 + (i * j) % 3 === 0; },
    function (i, j) { return ((i * j) % 2 + (i * j) % 3) % 2 === 0; },
    function (i, j) { return ((i * j) % 3 + (i + j) % 2) % 2 === 0; }
  ];
  var masks = [];
  for (var m = 0; m < 8; m++) {
    var mb = new Uint8Array(N);
    for (var yy = 0; yy < n; yy++) for (var xx = 0; xx < n; xx++) {
      var idx = yy * n + xx;
      if (!occ[idx] && MASK[m](yy, xx)) mb[idx] = 1;
    }
    masks.push(mb);
  }

  // Format information for each mask: 5 bits (level, mask), BCH remainder,
  // XOR 0x5412, at the two fixed sets of positions.
  var fmtPos = [];
  for (var fi = 0; fi < 15; fi++) {
    var a = fi < 6 ? [fi, 8] : fi < 8 ? [fi + 1, 8] : fi < 9 ? [8, 7] : [8, 14 - fi];
    var b = fi < 8 ? [8, n - 1 - fi] : [n - 1 - (14 - fi), 8];
    fmtPos.push([a[0] * n + a[1], b[0] * n + b[1]]);
  }
  var fmtBits = [];
  for (m = 0; m < 8; m++) {
    var fb = ((level ^ 1) << 13) | (m << 10);
    var rem = fb;
    for (var bi = 14; bi >= 10; bi--) if (rem & (1 << bi)) rem ^= 0x537 << (bi - 10);
    fb = (fb | rem) ^ 0x5412;
    fmtBits.push(fb);
  }

  // --- GF(256) with the QR polynomial, and the Reed-Solomon generator ---
  var EXP = new Uint8Array(512), LOG = new Uint8Array(256);
  var v = 1;
  for (var e = 0; e < 255; e++) {
    EXP[e] = v; LOG[v] = e;
    v <<= 1;
    if (v & 0x100) v ^= 0x11d;
  }
  for (e = 255; e < 512; e++) EXP[e] = EXP[e - 255];
  function mul(p, q) { return p === 0 || q === 0 ? 0 : EXP[LOG[p] + LOG[q]]; }
  var gen = new Uint8Array(ne + 1);
  gen[ne] = 1;
  for (e = 0; e < ne; e++) {
    var c = EXP[e];
    for (var j = 0; j < ne; j++) gen[j] = mul(gen[j], c) ^ gen[j + 1];
    gen[ne] = mul(gen[ne], c);
  }
  var lgen = new Uint16Array(ne + 1);
  for (e = 0; e <= ne; e++) lgen[e] = gen[e] === 0 ? 255 : LOG[gen[e]];

  // rs writes the ne check bytes of data[from..to) into out[at..).
  function rs(data, from, to, out, at) {
    var len = to - from, p = new Uint8Array(len + ne);
    for (var i = 0; i < len; i++) p[i] = data[from + i];
    for (i = 0; i < len; i++) {
      var ci = p[i];
      if (ci === 0) continue;
      var lc = LOG[ci];
      for (var k = 1; k <= ne; k++) if (lgen[k] !== 255) p[i + k] ^= EXP[lc + lgen[k]];
    }
    for (i = 0; i < ne; i++) out[at + i] = p[len + i];
  }

  // --- the data bit stream ---
  var codeBytes = new Uint8Array(dataBytes + checkBytes);
  var nbit;
  function write(val, bits) {
    for (var i = bits - 1; i >= 0; i--) {
      if ((val >>> i) & 1) codeBytes[nbit >> 3] |= 0x80 >> (nbit & 7);
      nbit++;
    }
  }
  function encodeData(text) {
    for (var i = 0; i < codeBytes.length; i++) codeBytes[i] = 0;
    var pad = dataBytes * 8 - (4 + countBits + Math.floor((11 * text.length + 1) / 2));
    if (pad < 0) throw new Error('airlift beam: a ' + text.length + '-character frame does not fit the symbol');
    nbit = 0;
    write(2, 4);
    write(text.length, countBits);
    var a, b;
    for (i = 0; i + 2 <= text.length; i += 2) {
      a = ALPHA.indexOf(text.charAt(i));
      b = ALPHA.indexOf(text.charAt(i + 1));
      if (a < 0 || b < 0) throw new Error('airlift beam: frame text is not base45');
      write(a * 45 + b, 11);
    }
    if (i < text.length) {
      a = ALPHA.indexOf(text.charAt(i));
      if (a < 0) throw new Error('airlift beam: frame text is not base45');
      write(a, 6);
    }
    if (pad <= 4) {
      write(0, pad);
    } else {
      write(0, 4);
      pad -= 4;
      var align = (-nbit) & 7;
      pad -= align;
      write(0, align);
      for (var k = 0, bytes = pad >> 3; k < bytes; k++) write(k % 2 === 0 ? 0xec : 0x11, 8);
    }
    // Check bytes, block by block; the last `extra` blocks carry one more byte.
    var from = 0, at = dataBytes;
    for (var bl = 0; bl < blocks; bl++) {
      var db = bl >= blocks - extra ? nde + 1 : nde;
      rs(codeBytes, from, from + db, codeBytes, at);
      from += db;
      at += ne;
    }
  }

  // Interleave: byte i of every data block in turn, then of every check block,
  // as a sequence of byte offsets into codeBytes.
  var seq = [];
  (function () {
    var starts = [], sizes = [], from = 0;
    for (var bl = 0; bl < blocks; bl++) {
      var db = bl >= blocks - extra ? nde + 1 : nde;
      starts.push(from); sizes.push(db); from += db;
    }
    for (var i = 0; i <= nde; i++) for (bl = 0; bl < blocks; bl++) if (i < sizes[bl]) seq.push(starts[bl] + i);
    for (i = 0; i < ne; i++) for (bl = 0; bl < blocks; bl++) seq.push(dataBytes + bl * ne + i);
  })();
  var totalBits = seq.length * 8;

  // --- the ISO penalty over a 0/1 matrix ---
  function penalty(g) {
    var score = 0, i, j, k, count, cur, prev, dark = 0;
    // rule 1: runs of five or more, rows then columns
    for (i = 0; i < n; i++) {
      count = 1; prev = g[i * n];
      for (j = 1; j < n; j++) {
        cur = g[i * n + j];
        if (cur === prev) { count++; continue; }
        if (count >= 5) score += 3 + (count - 5);
        count = 1; prev = cur;
      }
      if (count >= 5) score += 3 + (count - 5);
    }
    for (j = 0; j < n; j++) {
      count = 1; prev = g[j];
      for (i = 1; i < n; i++) {
        cur = g[i * n + j];
        if (cur === prev) { count++; continue; }
        if (count >= 5) score += 3 + (count - 5);
        count = 1; prev = cur;
      }
      if (count >= 5) score += 3 + (count - 5);
    }
    // rule 2: 2x2 blocks of one colour
    for (i = 0; i < n - 1; i++) for (j = 0; j < n - 1; j++) {
      cur = g[i * n + j];
      if (g[i * n + j + 1] === cur && g[(i + 1) * n + j] === cur && g[(i + 1) * n + j + 1] === cur) score += 3;
    }
    // rule 3: 1:1:3:1:1 finder-like patterns with four light modules to a side
    var PA = [1, 0, 1, 1, 1, 0, 1, 0, 0, 0, 0], PB = [0, 0, 0, 0, 1, 0, 1, 1, 1, 0, 1];
    function at(base, step, start, pat) {
      for (var t = 0; t < 11; t++) if (g[base + (start + t) * step] !== pat[t]) return false;
      return true;
    }
    for (i = 0; i < n; i++) for (k = 0; k + 11 <= n; k++) {
      if (at(i * n, 1, k, PA) || at(i * n, 1, k, PB)) score += 40;
    }
    for (j = 0; j < n; j++) for (k = 0; k + 11 <= n; k++) {
      if (at(j, n, k, PA) || at(j, n, k, PB)) score += 40;
    }
    // rule 4: dark proportion away from 50 %, in 5 % steps
    for (i = 0; i < N; i++) dark += g[i];
    var percent = dark * 100 / N;
    score += 10 * Math.floor(Math.abs(percent - 50) / 5);
    return score;
  }

  var raw = new Uint8Array(N), cand = new Uint8Array(N), best = new Uint8Array(N);

  // encode returns {mask, bits}: bits is one byte per module, row-major, 1 dark.
  // A frame longer than the symbol holds, or outside the base45 alphabet, throws.
  function encode(text) {
    encodeData(text);
    raw.set(base);
    var bit = 0;
    for (var s = 0; s < seq.length; s++) {
      var byte = codeBytes[seq[s]];
      for (var b = 7; b >= 0; b--) raw[order[bit++]] = (byte >> b) & 1;
    }
    for (; bit < free; bit++) raw[order[bit]] = 0; // remainder modules
    var bestMask = -1, bestScore = Infinity;
    for (var m = 0; m < 8; m++) {
      var mb = masks[m];
      for (var i = 0; i < N; i++) cand[i] = raw[i] ^ mb[i];
      var fb = fmtBits[m];
      for (i = 0; i < 15; i++) {
        var bitv = (fb >> i) & 1;
        cand[fmtPos[i][0]] = bitv;
        cand[fmtPos[i][1]] = bitv;
      }
      var sc = penalty(cand);
      if (sc < bestScore) {
        bestScore = sc; bestMask = m;
        best.set(cand);
      }
    }
    return { mask: bestMask, bits: new Uint8Array(best) }; // a copy: the buffers are reused
  }

  return { n: n, free: free, totalBits: totalBits, encode: encode };
}
