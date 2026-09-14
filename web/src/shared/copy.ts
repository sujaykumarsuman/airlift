// Copy buttons: `<button data-copy>` copies the text of the nearest `code`
// (or the element `data-copy` names) and shows a tick for a moment.
const TICK = '<svg class="ic" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M5 12.5l4.5 4.5L19 7"/></svg>';

export function bindCopyButtons(root: ParentNode): void {
  root.querySelectorAll<HTMLButtonElement>("button[data-copy]").forEach((btn) => {
    if (btn.dataset.copyBound) return;
    btn.dataset.copyBound = "1";
    btn.addEventListener("click", () => {
      const target = btn.dataset.copy ? document.getElementById(btn.dataset.copy) : btn.parentElement?.querySelector("code");
      const text = target?.textContent?.trim() ?? "";
      if (!text) return;
      void navigator.clipboard?.writeText(text).then(() => {
        const was = btn.innerHTML;
        btn.innerHTML = TICK;
        btn.classList.add("copied");
        setTimeout(() => {
          btn.innerHTML = was;
          btn.classList.remove("copied");
        }, 1200);
      });
    });
  });
}
