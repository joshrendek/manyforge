// formatBytes renders a byte count for people: "24 bytes", "1.2 KB", "3.4 MB".
// Invalid input (NaN, negative, non-finite) renders as an em dash.
export function formatBytes(n: number): string {
  if (!Number.isFinite(n) || n < 0) return '—';
  if (n < 1024) return `${n} bytes`;
  const units = ['KB', 'MB', 'GB'];
  let v = n / 1024;
  let i = 0;
  // Choose the unit after rounding so 1048575 reads "1.0 MB", not "1024.0 KB".
  while (i < units.length - 1 && Number(v.toFixed(1)) >= 1024) {
    v /= 1024;
    i++;
  }
  return `${v.toFixed(1)} ${units[i]}`;
}
