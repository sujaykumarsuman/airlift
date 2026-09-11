/** Unpacks the snapshot's base64 bitmap: one entry per chunk, 1 = received. */
export function decodeBitmap(b64: string, total: number): Uint8Array {
  const bits = new Uint8Array(total);
  if (!b64 || total <= 0) return bits;
  const bytes = atob(b64);
  for (let i = 0; i < total; i++) {
    const byte = bytes.charCodeAt(i >> 3);
    if (byte & (0x80 >> (i & 7))) bits[i] = 1;
  }
  return bits;
}

export interface GridOptions {
  cell?: number; // px per chunk
  gap?: number; // px between cells
  have?: string; // colour of a received chunk
  missing?: string; // colour of a missing chunk
}

/**
 * Draws the chunk grid to fit the canvas's current CSS width: as many
 * columns as fit, rows as needed. Resizes the canvas's bitmap to match.
 */
export function drawBitmap(canvas: HTMLCanvasElement, bits: Uint8Array, opts: GridOptions = {}): void {
  const cell = opts.cell ?? 8;
  const gap = opts.gap ?? 2;
  const width = Math.max(1, Math.floor(canvas.clientWidth || canvas.width || 300));
  const cols = Math.max(1, Math.min(bits.length || 1, Math.floor((width + gap) / (cell + gap))));
  const rows = Math.max(1, Math.ceil(bits.length / cols));
  const dpr = typeof devicePixelRatio === "number" ? devicePixelRatio : 1;
  const cssHeight = rows * (cell + gap) - gap;
  canvas.width = Math.round(width * dpr);
  canvas.height = Math.round(cssHeight * dpr);
  canvas.style.height = `${cssHeight}px`;
  const ctx = canvas.getContext("2d");
  if (!ctx) return;
  ctx.scale(dpr, dpr);
  ctx.clearRect(0, 0, width, cssHeight);
  const have = opts.have ?? "#35d0c0"; // accent
  const missing = opts.missing ?? "#232832"; // a dark cell, visible on the near-black ground
  for (let i = 0; i < bits.length; i++) {
    ctx.fillStyle = bits[i] ? have : missing;
    ctx.fillRect((i % cols) * (cell + gap), Math.floor(i / cols) * (cell + gap), cell, cell);
  }
}
