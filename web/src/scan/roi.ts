/** A rectangle in camera-frame pixels. */
export interface ROI {
  x: number;
  y: number;
  w: number;
  h: number;
}

/**
 * Maps the viewfinder square (CSS px, its bounding rect in the viewport) to the
 * camera frame, so the decoder reads only that region.
 *
 * The video is drawn `object-fit: cover` into a box of cssW×cssH: scaled by
 * s = max(cssW/vw, cssH/vh) and centred, so a CSS point maps to
 * ((x − ox)/s, (y − oy)/s) with ox = (cssW − vw·s)/2, oy = (cssH − vh·s)/2.
 * The region is grown by `margin` (a fraction of the finder, so a code that
 * spills a little over the corners still reads) and clamped to the frame;
 * null when nothing sensible can be cut (no frame, no box, a sliver).
 */
export function finderROI(
  vw: number,
  vh: number,
  cssW: number,
  cssH: number,
  finder: { left: number; top: number; width: number; height: number },
  margin = 0.12,
): ROI | null {
  if (!(vw > 0 && vh > 0 && cssW > 0 && cssH > 0 && finder.width > 0 && finder.height > 0)) return null;
  const s = Math.max(cssW / vw, cssH / vh);
  const ox = (cssW - vw * s) / 2;
  const oy = (cssH - vh * s) / 2;
  const mx = finder.width * margin;
  const my = finder.height * margin;
  const x0 = Math.max(0, Math.floor((finder.left - mx - ox) / s));
  const y0 = Math.max(0, Math.floor((finder.top - my - oy) / s));
  const x1 = Math.min(vw, Math.ceil((finder.left + finder.width + mx - ox) / s));
  const y1 = Math.min(vh, Math.ceil((finder.top + finder.height + my - oy) / s));
  if (x1 - x0 < 16 || y1 - y0 < 16) return null;
  return { x: x0, y: y0, w: x1 - x0, h: y1 - y0 };
}
