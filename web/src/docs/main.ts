import "../shared/style.css";
import { bindCopyButtons } from "../shared/copy";
import { enter } from "../shared/motion";

// The docs are static HTML; this wires the copy buttons and keeps the table of
// contents pointing at the section in view.
bindCopyButtons(document);

const links = [...document.querySelectorAll<HTMLAnchorElement>(".toc a[href^='#']")];
const sections = links
  .map((a) => document.getElementById(a.getAttribute("href")!.slice(1)))
  .filter((el): el is HTMLElement => el !== null);
if ("IntersectionObserver" in window && sections.length) {
  const visible = new Set<string>();
  const io = new IntersectionObserver(
    (entries) => {
      for (const e of entries) {
        if (e.isIntersecting) visible.add(e.target.id);
        else visible.delete(e.target.id);
      }
      // the first section (in document order) that is in view is the current one
      const current = sections.find((s) => visible.has(s.id))?.id;
      for (const a of links) a.classList.toggle("on", a.getAttribute("href") === `#${current}`);
    },
    { rootMargin: "-56px 0px -60% 0px", threshold: 0 },
  );
  for (const s of sections) io.observe(s);
}

enter(document.querySelector<HTMLElement>("main") ?? document.body);
