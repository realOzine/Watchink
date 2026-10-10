/**
 * Formats a video-relative time. Videos of one hour or more use `H:MM:SS`,
 * shorter ones `MM:SS` (docs/v0.md §9.4), so pass the video duration.
 */
export function formatMs(ms: number, durationMs?: number | null): string {
  const safe = Number.isFinite(ms) && ms > 0 ? ms : 0;
  const total = Math.floor(safe / 1000);
  const h = Math.floor(total / 3600);
  const m = Math.floor((total % 3600) / 60);
  const s = total % 60;
  const long = h > 0 || (durationMs != null && durationMs >= 3_600_000);
  const ss = String(s).padStart(2, '0');
  if (long) return `${h}:${String(m).padStart(2, '0')}:${ss}`;
  return `${String(m).padStart(2, '0')}:${ss}`;
}

/** Human duration for list cells: "1 小时 02 分" / "12 分 05 秒" / "45 秒". */
export function formatDuration(ms: number | null | undefined): string {
  if (ms == null || !Number.isFinite(ms) || ms < 0) return '—';
  const total = Math.round(ms / 1000);
  const h = Math.floor(total / 3600);
  const m = Math.floor((total % 3600) / 60);
  const s = total % 60;
  if (h > 0) return `${h} 小时 ${String(m).padStart(2, '0')} 分`;
  if (m > 0) return `${m} 分 ${String(s).padStart(2, '0')} 秒`;
  return `${s} 秒`;
}

/** Parses the `?t=<ms>` deep-link parameter; null when absent or invalid. */
export function parseTimeParam(value: string | null): number | null {
  if (value == null || !/^\d{1,12}$/.test(value.trim())) return null;
  return Number(value.trim());
}

/** Local date-time for an RFC 3339 instant; the raw string when unparsable. */
export function formatInstant(iso: string | null | undefined): string {
  if (!iso) return '—';
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return iso;
  const p = (n: number) => String(n).padStart(2, '0');
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}`;
}

/**
 * Index of the segment playing at `ms`: the last one whose start is <= ms.
 * Segments are sorted by start. Returns -1 before the first segment.
 */
export function findSegmentIndex(segments: readonly { start_ms: number }[], ms: number): number {
  let lo = 0;
  let hi = segments.length - 1;
  let found = -1;
  while (lo <= hi) {
    const mid = (lo + hi) >> 1;
    if (segments[mid]!.start_ms <= ms) {
      found = mid;
      lo = mid + 1;
    } else {
      hi = mid - 1;
    }
  }
  return found;
}
