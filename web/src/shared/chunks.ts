// Chunk marks: one thin tally tick per chunk, lit when received. A row holds one
// page of ticks (as many as fit the width), so a small beam shows whole and a
// large one pages; a minimap of pills (one per page, or per page-group past 48)
// gives the whole beam at a glance and is tappable. The view follows the newest
// chunk unless a tap pins a page for a moment. Fixed height whatever the count.

export const TICK = 3; // px
export const GAP = 4; // px
export const PITCH = TICK + GAP;
export const MAX_PIPS = 48;
export const PIN_MS = 8_000; // a tapped page stays put this long, then follows again
export const FLASH_MS = 600; // the newest tick flashes white this long

/** Ticks per row for a given width: at least 8, else as many as fit the pitch. */
export function perPage(width: number): number {
  return Math.max(8, Math.floor((width + GAP) / PITCH));
}

export function pageCount(n: number, per: number): number {
  return Math.max(1, Math.ceil(n / per));
}

export function pageOf(index: number, per: number): number {
  return Math.floor(index / per);
}

/** The newest chunk: the highest index that flipped 0→1 since `prev`; -1 if none.
 *  With no `prev` (first render) every set bit counts, so the view lands on the
 *  frontier of what has already arrived. */
export function newest(prev: Uint8Array | null, bits: Uint8Array): number {
  const comparable = prev !== null && prev.length === bits.length;
  let best = -1;
  for (let i = 0; i < bits.length; i++) {
    if (bits[i] && (!comparable || !prev[i])) best = i;
  }
  return best;
}

/** Which page to show: a recent pin wins, else the newest chunk's page (or the first). */
export function viewPage(pinned: number | null, pinnedAt: number, now: number, latest: number, per: number, pages: number): number {
  const clamp = (p: number): number => Math.min(pages - 1, Math.max(0, p));
  if (pinned !== null && now - pinnedAt < PIN_MS) return clamp(pinned);
  return latest >= 0 ? clamp(pageOf(latest, per)) : 0;
}

export interface Pip {
  from: number; // first chunk index covered
  to: number; // one past the last
  frac: number; // received fraction, 0..1
}

/** The minimap: one pill per page while there are at most `maxPips` pages, else
 *  each pill covers a run of whole pages so the count never exceeds `maxPips`. */
export function minimap(bits: Uint8Array, per: number, maxPips = MAX_PIPS): Pip[] {
  const pages = pageCount(bits.length, per);
  const pagesPerPip = Math.ceil(pages / Math.min(pages, maxPips));
  const span = pagesPerPip * per;
  const out: Pip[] = [];
  for (let from = 0; from < bits.length; from += span) {
    const to = Math.min(bits.length, from + span);
    let have = 0;
    for (let i = from; i < to; i++) have += bits[i] ?? 0;
    out.push({ from, to, frac: have / (to - from) });
  }
  return out;
}

interface Track {
  prev: Uint8Array | null;
  latest: number;
  latestAt: number;
  pinned: number | null;
  pinnedAt: number;
}

// Per-beam view state, keyed by the caller (the beam id): it survives the
// dashboard rebuilding its cards on every snapshot.
const tracks = new Map<string, Track>();

/** Renders the marks for `bits` into `root` (a `.chunks` element, laid out and
 *  visible so its width is known). Safe to call on every snapshot. */
export function renderChunkMarks(root: HTMLElement, key: string, bits: Uint8Array, now = Date.now()): void {
  let t = tracks.get(key);
  if (!t || (t.prev !== null && t.prev.length !== bits.length)) {
    t = { prev: null, latest: -1, latestAt: 0, pinned: null, pinnedAt: 0 };
    tracks.set(key, t);
  }
  const fresh = newest(t.prev, bits);
  if (fresh >= 0) {
    t.latest = fresh;
    t.latestAt = now;
  }
  t.prev = bits.slice();

  const per = perPage(root.clientWidth || 320);
  const pages = pageCount(bits.length, per);
  const page = viewPage(t.pinned, t.pinnedAt, now, t.latest, per, pages);
  const from = page * per;
  const to = Math.min(bits.length, from + per);

  let ticks = "";
  let here = 0;
  for (let i = from; i < to; i++) {
    const on = !!bits[i];
    if (on) here++;
    const flash = on && i === t.latest && now - t.latestAt < FLASH_MS;
    ticks += on ? `<i class="on${flash ? " new" : ""}"></i>` : "<i></i>";
  }

  // The minimap and pager always show, so a 3-chunk beam and a 700-chunk beam
  // read the same way on the scanner and the dashboard alike.
  const pips = minimap(bits, per)
    .map((p) => {
      const cur = from >= p.from && from < p.to;
      const pct = Math.round(p.frac * 100);
      const cls = p.frac >= 1 ? "full" : p.frac > 0 ? "part" : "none";
      const style = cls === "part" ? ` style="background:linear-gradient(90deg,var(--accent) ${pct}%,var(--chunk-off) ${pct}%)"` : "";
      return `<b class="${cls}${cur ? " cur" : ""}" data-from="${p.from}"${style} title="chunks ${p.from + 1}–${p.to} · ${pct}%"></b>`;
    })
    .join("");
  const pinnedNow = t.pinned !== null && now - t.pinnedAt < PIN_MS;
  const follow = pages > 1 ? (pinnedNow ? "<span>pinned</span>" : "<span>following</span>") : "";
  const mini = `<div class="mini">${pips}</div>`;
  const pager = `<div class="pager"><span>page <b>${page + 1}</b>/${pages}</span><span>chunks <b>${from + 1}–${to}</b></span><span><b>${here}</b>/${to - from} here</span>${follow}</div>`;
  root.innerHTML = `<div class="tally">${ticks}</div>${mini}${pager}`;

  if (pages > 1) {
    root.querySelector(".mini")?.addEventListener("click", (e) => {
      const pill = (e.target as HTMLElement).closest<HTMLElement>("b[data-from]");
      if (!pill || !t) return;
      t.pinned = pageOf(Number(pill.dataset.from), per);
      t.pinnedAt = Date.now();
      renderChunkMarks(root, key, bits, Date.now());
    });
  }
  if (fresh >= 0) {
    // clear the flash without a full re-render
    setTimeout(() => root.querySelector(".tally i.new")?.classList.remove("new"), FLASH_MS);
  }
}
