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
// on-screen controls with keys for pause, step, fps, size, fullscreen and
// hiding the chrome. The page is black and the chrome dim so the QR — black on
// a white tile with a quiet zone — is the only bright thing a camera exposes
// for. No external references; the __TOKEN__ slots are filled by PlayerHTML.
// The player never talks to anything — it preserves the air gap.
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
svg{display:block}
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
<main id="main"><div id="tile"><svg id="qr" viewBox="0 0 __SIZE__ __SIZE__" shape-rendering="crispEdges">
<path id="path" stroke="#000" stroke-width="1" fill="none" d=""/></svg></div></main>
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
(function () {
  var FRAMES = __FRAMES__;
  var ORDER = __ORDER__;
  var N = __TOTAL__, fps = __FPS__, LABEL = __LABEL__, SIZE = __SIZE__;
  var i = 0, playing = true, acc = 0, last = null, scale = 100;
  var $ = function (id) { return document.getElementById(id); };
  var main = $('main'), tile = $('tile'), svg = $('qr'), path = $('path');
  var hFrame = $('frame'), hChunk = $('chunk'), hFps = $('fps'), hState = $('state');
  var hScale = $('scale'), bPlay = $('play');
  var KEYS = {32: ' ', 37: 'ArrowLeft', 39: 'ArrowRight', 187: '+', 61: '+', 107: '+',
              189: '-', 173: '-', 109: '-', 219: '[', 221: ']', 70: 'f', 72: 'h'};
  function fit() {
    var s = Math.floor(Math.min(main.clientWidth, main.clientHeight) * scale / 100);
    var pad = Math.max(4, Math.round(s * 4 / SIZE)); // a 4-module quiet zone
    var q = Math.max(1, s - 2 * pad);
    svg.style.width = q + 'px';
    svg.style.height = q + 'px';
    tile.style.padding = pad + 'px';
    hScale.textContent = scale + '%';
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
