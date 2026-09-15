package beam

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"html"
	"strconv"
	"strings"
)

// qrJS is the in-page QR encoder (ADR 0011, amended): the frames travel as
// their base45 text and the page renders each symbol itself, so a beam costs
// about the payload's size rather than a pre-rendered picture per frame.
//
//go:embed qrjs.js
var qrJS string

// playerTemplate is the self-contained HTML player (ADR 0003): a canvas the
// inline encoder paints per frame at an exact integer pixel pitch, an inline
// loop on requestAnimationFrame, on-screen controls with keys for pause, step,
// fps, size, fullscreen and hiding the chrome. The page is black and the
// chrome dim so the QR — black on a white tile with a quiet zone — is the only
// bright thing a camera exposes for. No external references; the __TOKEN__
// slots are filled by PlayerHTML. The player never talks to anything — it
// preserves the air gap.
const playerTemplate = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>airlift beam · __TITLE__</title>
<style>
html,body{margin:0;height:100%;background:#000;color:#8a929c;overflow:hidden}
body{display:flex;flex-direction:column;-webkit-user-select:none;user-select:none;
font:13px/1.4 ui-monospace,SFMono-Regular,Menlo,Consolas,monospace}
header,footer{flex:none;display:flex;flex-wrap:wrap;align-items:center;gap:.35em 1.25em;padding:.55em 1em;color:#6b7480}
header b{color:#c9d0d8;font-weight:600}
header .hint{margin-left:auto;color:#4a525c}
main{flex:1;min-height:0;display:flex;align-items:center;justify-content:center}
#tile{background:#fff;line-height:0}
canvas{display:block}
footer{justify-content:center;gap:.5em .9em}
button{font:inherit;color:#aab2bb;background:#0f1216;border:1px solid #262b33;border-radius:7px;min-width:2.6em;height:2.4em;padding:0 .75em;cursor:pointer}
button:hover{border-color:#3a414b;color:#e6e9ee}
button.on{border-color:#35d0c0;color:#35d0c0}
.grp{display:inline-flex;align-items:center;gap:.35em}
.grp span{min-width:3.4em;text-align:center;color:#c9d0d8}
#state{color:#35d0c0;min-width:5em}
body.bare header,body.bare footer{display:none}
@media (max-width:600px){header .hint{display:none}}
</style>
</head>
<body>
<header>
<b>airlift beam</b>
<span>__NAME__</span>
<span>session __SESSION__</span>
<span>__MODE__</span>
<span id="frame"></span>
<span id="chunk"></span>
<span class="hint">space pause · ←/→ step · +/- fps · [/] size · f fullscreen · h hide chrome</span>
</header>
<main id="main"><div id="tile"><canvas id="qr"></canvas></div></main>
<footer>
<button id="prev" title="step back (left)">&#9664;</button>
<button id="play" class="on" title="pause / play (space)">pause</button>
<button id="next" title="step forward (right)">&#9654;</button>
<span class="grp"><button id="slower" title="slower (-)">&minus;</button><span id="fps"></span><button id="faster" title="faster (+)">+</button></span>
<span class="grp"><button id="smaller" title="smaller ([)">&minus;</button><span id="scale"></span><button id="bigger" title="bigger (])">+</button></span>
<button id="full" title="fullscreen (f)">fullscreen</button>
<button id="hide" title="hide the chrome (h)">hide</button>
<span id="state"></span>
</footer>
<script>
__QRJS__
(function () {
  var PLAN = __PLAN__;
  var FRAMES = __FRAMES__;
  var ORDER = __ORDER__;
  var N = __TOTAL__, fps = __FPS__, LABEL = __LABEL__;
  var i = 0, playing = true, acc = 0, last = null, scale = 100;
  var $ = function (id) { return document.getElementById(id); };
  var main = $('main'), tile = $('tile'), canvas = $('qr'), ctx = canvas.getContext('2d');
  var hFrame = $('frame'), hChunk = $('chunk'), hFps = $('fps'), hState = $('state');
  var hScale = $('scale'), bPlay = $('play');
  var KEYS = {32: ' ', 37: 'ArrowLeft', 39: 'ArrowRight', 187: '+', 61: '+', 107: '+',
              189: '-', 173: '-', 109: '-', 219: '[', 221: ']', 70: 'f', 72: 'h'};
  var qr = airliftQR(PLAN), n = qr.n, M = n * n;
  var off = document.createElement('canvas');
  off.width = n;
  off.height = n;
  var octx = off.getContext('2d'), img = octx.createImageData(n, n);
  var cache = new Array(FRAMES.length); // packed symbols, encoded once each
  var shown = -1; // the frame on the tile
  var now = function () { return window.performance && performance.now ? performance.now() : Date.now(); };
  function symbol(k) {
    var packed = cache[k];
    if (!packed) {
      var bits = qr.encode(FRAMES[k]).bits;
      packed = new Uint8Array((M + 7) >> 3);
      for (var j = 0; j < M; j++) { if (bits[j]) { packed[j >> 3] |= 0x80 >> (j & 7); } }
      cache[k] = packed;
    }
    return packed;
  }
  function paint(k) {
    var packed = symbol(k), d = img.data;
    for (var j = 0, p = 0; j < M; j++, p += 4) {
      var v = (packed[j >> 3] >> (7 - (j & 7))) & 1 ? 0 : 255;
      d[p] = v; d[p + 1] = v; d[p + 2] = v; d[p + 3] = 255;
    }
    octx.putImageData(img, 0, 0);
    ctx.imageSmoothingEnabled = false;
    ctx.webkitImageSmoothingEnabled = false;
    ctx.mozImageSmoothingEnabled = false;
    ctx.msImageSmoothingEnabled = false;
    ctx.fillStyle = '#fff';
    ctx.fillRect(0, 0, canvas.width, canvas.height);
    ctx.drawImage(off, 0, 0, canvas.width, canvas.height); // integer upscale: every module the same size
    shown = k;
  }
  function fit() {
    var dpr = window.devicePixelRatio || 1;
    var s = Math.floor(Math.min(main.clientWidth, main.clientHeight) * scale / 100);
    var k = Math.max(1, Math.floor(s * dpr / (n + 8))); // device pixels per module, 4-module quiet zone each side
    canvas.width = n * k;
    canvas.height = n * k;
    canvas.style.width = (n * k / dpr) + 'px';
    canvas.style.height = (n * k / dpr) + 'px';
    tile.style.padding = (4 * k / dpr) + 'px';
    hScale.textContent = scale + '%';
    if (shown >= 0) { paint(shown); }
  }
  function show() {
    var k = ORDER[i];
    paint(k);
    hFrame.textContent = 'frame ' + (i + 1) + '/' + ORDER.length;
    hChunk.textContent = k === 0 ? 'manifest' : LABEL + ' ' + k + '/' + N;
  }
  // warm encodes the loop ahead of playback in short idle slices, so a pass
  // never waits on the encoder; a frame shown before its turn is encoded there.
  var warmAt = 0;
  function warm() {
    var t0 = now();
    while (warmAt < ORDER.length && now() - t0 < 8) { symbol(ORDER[warmAt++]); }
    if (warmAt < ORDER.length) { setTimeout(warm, 0); }
  }
  function status() {
    hFps.textContent = fps + ' fps · ' + (ORDER.length / fps).toFixed(1) + ' s/pass';
    hState.textContent = playing ? 'playing' : 'paused';
    bPlay.textContent = playing ? 'pause' : 'play';
    bPlay.className = playing ? 'on' : '';
  }
  function step(d) {
    playing = false;
    acc = 0;
    i = (i + d + ORDER.length) % ORDER.length;
    show();
    status();
  }
  function toggle() { playing = !playing; acc = 0; status(); }
  function setFps(d) { fps = Math.min(60, Math.max(1, fps + d)); status(); }
  function setScale(d) { scale = Math.min(100, Math.max(40, scale + d)); fit(); }
  function full() {
    if (document.fullscreenElement) { document.exitFullscreen(); }
    else if (document.documentElement.requestFullscreen) {
      document.documentElement.requestFullscreen();
    }
  }
  function bare() { document.body.className = document.body.className ? '' : 'bare'; fit(); }
  function tick(ts) {
    if (last !== null && playing) {
      acc += ts - last;
      var period = 1000 / fps;
      if (acc >= period) {
        acc = Math.min(acc - period, period);
        i = (i + 1) % ORDER.length;
        show();
      }
    }
    last = ts;
    window.requestAnimationFrame(tick);
  }
  document.addEventListener('keydown', function (e) {
    var k = e.key || KEYS[e.keyCode];
    if (k === 'Left') { k = 'ArrowLeft'; }
    if (k === 'Right') { k = 'ArrowRight'; }
    if (k === 'Spacebar') { k = ' '; }
    if (k === ' ') { toggle(); }
    else if (k === 'ArrowRight') { step(1); }
    else if (k === 'ArrowLeft') { step(-1); }
    else if (k === '+' || k === '=') { setFps(1); }
    else if (k === '-' || k === '_') { setFps(-1); }
    else if (k === ']') { setScale(5); }
    else if (k === '[') { setScale(-5); }
    else if (k === 'f' || k === 'F') { full(); }
    else if (k === 'h' || k === 'H') { bare(); }
    else { return; }
    e.preventDefault();
  });
  $('prev').onclick = function () { step(-1); };
  $('next').onclick = function () { step(1); };
  $('play').onclick = toggle;
  $('slower').onclick = function () { setFps(-1); };
  $('faster').onclick = function () { setFps(1); };
  $('smaller').onclick = function () { setScale(-5); };
  $('bigger').onclick = function () { setScale(5); };
  $('full').onclick = full;
  $('hide').onclick = bare;
  window.addEventListener('resize', fit);
  document.addEventListener('fullscreenchange', fit);
  fit();
  show();
  status();
  warm();
  window.requestAnimationFrame(tick);
  if (navigator.wakeLock && navigator.wakeLock.request) {
    var lock = function () { navigator.wakeLock.request('screen').catch(function () {}); };
    lock();
    document.addEventListener('visibilitychange', function () {
      if (!document.hidden) { lock(); }
    });
  }
})();
</script>
</body>
</html>
`

// PlayerHTML fills the player template. name and title are HTML-escaped; the
// plan, frames (their base45 text), order and label are JSON; the session is
// eight hex digits; the encoder is inlined. The result is one self-contained
// page with no external references.
func PlayerHTML(name string, session uint32, total int, order []int, frames []string, plan *PlayerPlan, fps int, fountain bool) string {
	js := func(v any) string {
		b, _ := json.Marshal(v)
		return strings.ReplaceAll(string(b), "</", "<\\/")
	}
	mode, label := "sequential", "chunk"
	if fountain {
		mode, label = "fountain", "packet"
	}
	return strings.NewReplacer(
		"__TITLE__", html.EscapeString(name),
		"__NAME__", html.EscapeString(name),
		"__SESSION__", fmt.Sprintf("%08x", session),
		"__MODE__", mode,
		"__QRJS__", strings.TrimSpace(qrJS),
		"__PLAN__", js(plan),
		"__FRAMES__", js(frames),
		"__ORDER__", js(order),
		"__TOTAL__", strconv.Itoa(total),
		"__FPS__", strconv.Itoa(fps),
		"__LABEL__", js(label),
	).Replace(playerTemplate)
}
