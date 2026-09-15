import { raw, type Raw } from "./dom";

// Inline stroke symbols — no icon font, no runtime dependency (CLAUDE.md: web
// runtime deps are zxing-wasm + qrcode only). Each is a 24×24 path set drawn with
// `currentColor`, so a symbol takes the colour and size of its context.
const PATHS = {
  beam: '<circle cx="12" cy="12" r="2"/><path d="M8.5 8.5a5 5 0 0 0 0 7M15.5 8.5a5 5 0 0 1 0 7M6 6a9 9 0 0 0 0 12M18 6a9 9 0 0 1 0 12"/>',
  share: '<circle cx="18" cy="5" r="2.6"/><circle cx="6" cy="12" r="2.6"/><circle cx="18" cy="19" r="2.6"/><path d="M8.4 10.7l7.2-4.4M8.4 13.3l7.2 4.4"/>',
  scan: '<path d="M4 8V5.5A1.5 1.5 0 0 1 5.5 4H8M16 4h2.5A1.5 1.5 0 0 1 20 5.5V8M20 16v2.5a1.5 1.5 0 0 1-1.5 1.5H16M8 20H5.5A1.5 1.5 0 0 1 4 18.5V16"/><path d="M4 12h16"/>',
  camera: '<path d="M4 8V5.5A1.5 1.5 0 0 1 5.5 4H8M16 4h2.5A1.5 1.5 0 0 1 20 5.5V8M20 16v2.5a1.5 1.5 0 0 1-1.5 1.5H16M8 20H5.5A1.5 1.5 0 0 1 4 18.5V16"/><circle cx="12" cy="12" r="3"/>',
  people: '<circle cx="9" cy="8" r="3.2"/><path d="M3.5 19a5.5 5.5 0 0 1 11 0"/><path d="M16 5.3a3.2 3.2 0 0 1 0 5.4M17.6 19a5.6 5.6 0 0 0-2.8-4.8"/>',
  lock: '<rect x="4.5" y="10.5" width="15" height="9" rx="2"/><path d="M8 10.5V7a4 4 0 0 1 8 0v3.5"/>',
  key: '<circle cx="8" cy="15" r="3.4"/><path d="M10.4 12.6L20 3M17 6l2 2"/>',
  bell: '<path d="M6 15V11a6 6 0 0 1 12 0v4l1.6 2.2H4.4L6 15z"/><path d="M10 20a2 2 0 0 0 4 0"/>',
  clock: '<circle cx="12" cy="12" r="8"/><path d="M12 8v4.2l2.8 1.8"/>',
  download: '<path d="M12 4v10m0 0l-4-4m4 4l4-4"/><path d="M5 18h14"/>',
  upload: '<path d="M12 20V10m0 0l-4 4m4-4l4 4"/><path d="M5 6h14"/>',
  trash: '<path d="M4 7h16M9 7V5a1 1 0 0 1 1-1h4a1 1 0 0 1 1 1v2M6.5 7l.9 12a1 1 0 0 0 1 .9h7.2a1 1 0 0 0 1-.9L17.5 7"/>',
  settings: '<circle cx="12" cy="12" r="3.2"/><path d="M12 2.6v3M12 18.4v3M21.4 12h-3M5.6 12h-3M18.4 5.6l-2.1 2.1M7.7 16.3l-2.1 2.1M18.4 18.4l-2.1-2.1M7.7 7.7L5.6 5.6"/>',
  check: '<path d="M5 12.5l4.5 4.5L19 7"/>',
  cross: '<path d="M6 6l12 12M18 6L6 18"/>',
  copy: '<rect x="9" y="9" width="11" height="11" rx="2"/><path d="M5 15V5.5A1.5 1.5 0 0 1 6.5 4H15"/>',
  home: '<path d="M4 11l8-6.5 8 6.5"/><path d="M6 9.5V19h12V9.5"/>',
  plus: '<path d="M12 5v14M5 12h14"/>',
  reopen: '<path d="M4 12a8 8 0 1 1 2.3 5.6"/><path d="M4 20v-4h4"/>',
  torch: '<path d="M9 3h6l-1 7h3l-8 11 2-8H8z"/>',
  power: '<path d="M12 3v9"/><path d="M6.6 6.6a7.5 7.5 0 1 0 10.8 0"/>',
} as const;

export type IconName = keyof typeof PATHS;

/** An inline SVG symbol as safe HTML for the `html` template tag. `cls` adds
 *  classes beside the base `ic`; `plus`-weight symbols look right a touch bolder,
 *  so `check`/`cross`/`plus` carry 2.0 stroke. */
export function icon(name: IconName, cls = ""): Raw {
  const w = name === "check" || name === "cross" || name === "plus" ? 2 : 1.75;
  return raw(
    `<svg class="ic${cls ? ` ${cls}` : ""}" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="${w}" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">${PATHS[name]}</svg>`,
  );
}
