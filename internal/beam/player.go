package beam

import (
	"encoding/json"
	"fmt"
	"html"
	"strconv"
	"strings"
)

// playerTemplate is the self-contained HTML player (ADR 0003): inline SVG whose
// single path is swapped per frame, an inline loop on requestAnimationFrame,
// and keys for pause, step, fps and fullscreen. No external references. Ported
// verbatim from sender/airlift.py; the __TOKEN__ slots are filled by
// PlayerHTML. The player never talks to anything — it preserves the air gap.
const playerTemplate = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>airlift beam · __TITLE__</title>
<style>
html,body{margin:0;height:100%;background:#fff;color:#000;overflow:hidden}
body{display:flex;flex-direction:column;
font:14px/1.5 ui-monospace,SFMono-Regular,Menlo,Consolas,monospace}
header{flex:none;display:flex;flex-wrap:wrap;gap:0 1.5em;padding:.3em 1em;color:#333}
header .hint{margin-left:auto;color:#999}
main{flex:1;min-height:0;display:flex;align-items:center;justify-content:center}
svg{display:block}
</style>
</head>
<body>
<header>
<span>__NAME__</span>
<span>session __SESSION__</span>
<span>__MODE__</span>
<span id="frame"></span>
<span id="chunk"></span>
<span id="fps"></span>
<span id="state"></span>
<span class="hint">space pause · ←/→ step · +/- fps · f fullscreen</span>
</header>
<main id="main"><svg id="qr" viewBox="0 0 __SIZE__ __SIZE__" shape-rendering="crispEdges">
<path id="path" stroke="#000" stroke-width="1" fill="none" d=""/></svg></main>
<script>
(function () {
  var FRAMES = __FRAMES__;
  var ORDER = __ORDER__;
  var N = __TOTAL__, fps = __FPS__, LABEL = __LABEL__;
  var i = 0, playing = true, acc = 0, last = null;
  var main = document.getElementById('main'), svg = document.getElementById('qr');
  var path = document.getElementById('path');
  var hFrame = document.getElementById('frame'), hChunk = document.getElementById('chunk');
  var hFps = document.getElementById('fps'), hState = document.getElementById('state');
  var KEYS = {32: ' ', 37: 'ArrowLeft', 39: 'ArrowRight', 187: '+', 61: '+', 107: '+',
              189: '-', 173: '-', 109: '-', 70: 'f'};
  function fit() {
    var s = Math.min(main.clientWidth, main.clientHeight);
    svg.style.width = s + 'px';
    svg.style.height = s + 'px';
  }
  function show() {
    var k = ORDER[i];
    path.setAttribute('d', FRAMES[k]);
    hFrame.textContent = 'frame ' + (i + 1) + '/' + ORDER.length;
    hChunk.textContent = k === 0 ? 'manifest' : LABEL + ' ' + k + '/' + N;
  }
  function status() {
    hFps.textContent = fps + ' fps · ' + (ORDER.length / fps).toFixed(1) + ' s/pass';
    hState.textContent = playing ? 'playing' : 'paused';
  }
  function step(d) {
    playing = false;
    acc = 0;
    i = (i + d + ORDER.length) % ORDER.length;
    show();
  }
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
    if (k === ' ') { playing = !playing; acc = 0; }
    else if (k === 'ArrowRight') { step(1); }
    else if (k === 'ArrowLeft') { step(-1); }
    else if (k === '+' || k === '=') { fps = Math.min(60, fps + 1); }
    else if (k === '-' || k === '_') { fps = Math.max(1, fps - 1); }
    else if (k === 'f' || k === 'F') {
      if (document.fullscreenElement) { document.exitFullscreen(); }
      else if (document.documentElement.requestFullscreen) {
        document.documentElement.requestFullscreen();
      }
    }
    else { return; }
    e.preventDefault();
    status();
  });
  window.addEventListener('resize', fit);
  fit();
  show();
  status();
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
// frames, order and label are JSON; the session is eight hex digits. The
// result is one self-contained page with no external references.
func PlayerHTML(name string, session uint32, total int, order []int, paths []string, size, fps int, fountain bool) string {
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
		"__SIZE__", strconv.Itoa(size),
		"__FRAMES__", js(paths),
		"__ORDER__", js(order),
		"__TOTAL__", strconv.Itoa(total),
		"__FPS__", strconv.Itoa(fps),
		"__LABEL__", js(label),
	).Replace(playerTemplate)
}
