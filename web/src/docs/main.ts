import "../shared/style.css";
import { bindCopyButtons } from "../shared/copy";
import { enter } from "../shared/motion";

// The docs are static HTML; this wires the copy buttons and keeps the table of
// contents pointing at the section in view.
bindCopyButtons(document);

// The TOC links are `docs#section`, not `#section`: the tower injects a
// <base href> into every page, against which a bare fragment would resolve to
// the home page. Same-document fragment navigation still just scrolls.
const fragment = (a: HTMLAnchorElement): string => a.getAttribute("href")?.split("#")[1] ?? "";
const links = [...document.querySelectorAll<HTMLAnchorElement>(".toc a[href*='#']")];
const sections = links
  .map((a) => document.getElementById(fragment(a)))
  .filter((el): el is HTMLElement => el !== null);

// The current section is the last one whose top has passed the sticky nav —
// deterministic after a jump, unlike "first section intersecting the viewport",
// which still counts the sliver of the previous section left above the fold.
const NAV_PX = 80;
function sync(): void {
  let current = sections[0]?.id ?? "";
  for (const s of sections) {
    if (s.getBoundingClientRect().top <= NAV_PX) current = s.id;
  }
  // At the very bottom the last section may be too short to reach the nav.
  const atEnd = window.scrollY + window.innerHeight >= document.documentElement.scrollHeight - 2;
  if (atEnd && sections.length) current = sections[sections.length - 1]!.id;
  for (const a of links) a.classList.toggle("on", fragment(a) === current);
}
// Eight rects per scroll event is nothing, so no throttling — and no rAF, which
// never fires in a background tab. hashchange covers a TOC click directly.
addEventListener("scroll", sync, { passive: true });
addEventListener("hashchange", sync);
sync();

enter(document.querySelector<HTMLElement>("main") ?? document.body);
