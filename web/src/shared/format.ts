export function formatBytes(n: number): string {
  if (n < 1024) return `${n} B`;
  const units = ["KB", "MB", "GB"];
  let v = n / 1024;
  let i = 0;
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024;
    i++;
  }
  return `${(v < 100 ? v.toFixed(1) : v.toFixed(0)).replace(/\.0$/, "")} ${units[i]}`;
}

export function formatDuration(ms: number): string {
  const s = Math.max(0, Math.round(ms / 1000));
  if (s < 60) return `${s}s`;
  const m = Math.floor(s / 60);
  const rest = s % 60;
  if (m < 60) return `${m}m ${rest.toString().padStart(2, "0")}s`;
  return `${Math.floor(m / 60)}h ${(m % 60).toString().padStart(2, "0")}m`;
}

export function hex8(n: number | null): string {
  return n === null ? "—" : (n >>> 0).toString(16).padStart(8, "0");
}

export function shortHex(h: string, n = 12): string {
  return h.length > n ? `${h.slice(0, n)}…` : h;
}
