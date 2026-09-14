// Motion is CSS; this is the one hook it needs. `enter` replays the entry
// animation on an element — every `.card` under it (and the element itself when
// it is a card) rises into view. The class is dropped once the animation has
// had time to finish, so a later rebuild of the same DOM is not animated again
// unless enter() is called: snapshots re-render the dashboard every second and
// must not make it flicker.
export const ENTER_MS = 480;

export function enter(el: HTMLElement): void {
  el.classList.remove("enter");
  el.getBoundingClientRect(); // flush, so a running animation restarts
  el.classList.add("enter");
  setTimeout(() => el.classList.remove("enter"), ENTER_MS);
}
