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
