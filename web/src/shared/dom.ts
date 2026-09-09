export function $<T extends Element>(selector: string, root: ParentNode = document): T {
  const el = root.querySelector<T>(selector);
  if (!el) throw new Error(`missing element ${selector}`);
  return el;
}

export function esc(value: unknown): string {
  return String(value).replace(/[&<>"']/g, (c) => escapes[c] ?? c);
}

const escapes: Record<string, string> = { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" };

/** Marks a string as already-safe HTML for the `html` template tag. */
export class Raw {
  constructor(public readonly html: string) {}
  toString(): string {
    return this.html;
  }
}

export const raw = (s: string): Raw => new Raw(s);

/**
 * Template tag that escapes every interpolation unless it is a `Raw` (which
 * nested `html` calls return) or an array of them.
 */
export function html(strings: TemplateStringsArray, ...values: unknown[]): Raw {
  let out = "";
  strings.forEach((s, i) => {
    out += s;
    if (i < values.length) out += piece(values[i]);
  });
  return new Raw(out);
}

function piece(v: unknown): string {
  if (v instanceof Raw) return v.html;
  if (Array.isArray(v)) return v.map(piece).join("");
  if (v === null || v === undefined || v === false) return "";
  return esc(v);
}
