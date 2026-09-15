import { type Formatted, type Pasted, formatSessionId, isSessionId, looksLikeLink, sessionIdFromPaste, tokenFor } from "../shared/sid";

/**
 * attachSessionIdField keeps the home page's join field in the session-id
 * shape as it is edited (shared/sid.ts does the formatting) and turns a pasted
 * share link into its id. It returns `target`, which gives the address to open
 * for a complete id — with the pasted link's token when that link was for this
 * tower and the id is unchanged — or shows why the id is not complete yet.
 */
export function attachSessionIdField(input: HTMLInputElement, error: HTMLElement, appBase: URL): { target(): string | undefined } {
  let state: Formatted = { value: input.value, caret: input.value.length };
  let pasted: Pasted | undefined;
  let composing = false;

  const clearError = () => {
    error.hidden = true;
    input.removeAttribute("aria-invalid");
  };
  const format = (insert: boolean) => {
    const f = formatSessionId(input.value, input.selectionEnd ?? input.value.length, insert, state);
    if (input.value !== f.value) input.value = f.value;
    if (document.activeElement === input && (input.selectionStart !== f.caret || input.selectionEnd !== f.caret)) {
      input.setSelectionRange(f.caret, f.caret);
    }
    state = f;
    clearError();
  };

  // The state before an edit: a full id refuses a tenth letter by going back to it.
  input.addEventListener("beforeinput", () => {
    if (!composing) state = { value: input.value, caret: input.selectionStart ?? input.value.length };
  });
  input.addEventListener("compositionstart", () => {
    composing = true;
  });
  // An edit added text when the field grew — keyboards report some deletions
  // inside a word as insertions, so the event's own label is not trusted.
  const grew = () => input.value.length > state.value.length;
  input.addEventListener("compositionend", () => {
    composing = false;
    format(grew());
  });
  input.addEventListener("input", (e) => {
    if (composing || (e as InputEvent).isComposing) return; // a keyboard's word in progress is left alone until it lands
    format(grew());
  });
  input.addEventListener("paste", (e) => {
    const text = e.clipboardData?.getData("text") ?? "";
    const found = sessionIdFromPaste(text);
    if (found) {
      e.preventDefault();
      input.value = found.sid;
      input.setSelectionRange(found.sid.length, found.sid.length);
      state = { value: found.sid, caret: found.sid.length };
      pasted = found;
      clearError();
    } else if (looksLikeLink(text)) {
      e.preventDefault(); // its letters are not a session id
      error.textContent = "That link has no session id in it.";
      error.hidden = false;
    }
  });

  return {
    target() {
      format(false);
      const sid = input.value;
      if (!isSessionId(sid)) {
        error.textContent = sid
          ? `A session id is nine letters in three groups, like qkf-mzt-bwp — ${9 - sid.replace(/-/g, "").length} to go.`
          : "Enter the session id, like qkf-mzt-bwp.";
        error.hidden = false;
        input.setAttribute("aria-invalid", "true");
        input.focus();
        return undefined;
      }
      const token = tokenFor(pasted, sid, appBase);
      return new URL(sid + (token ? `#t=${encodeURIComponent(token)}` : ""), appBase).toString();
    },
  };
}
