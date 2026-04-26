/**
 * Date formatting utilities for financial dashboards.
 *
 * All timestamps are displayed in UTC to avoid timezone ambiguity
 * when multiple operators collaborate across regions.
 */

/**
 * Format an ISO date string to UTC with explicit "UTC" suffix.
 *
 *   formatUTC("2026-04-25T08:30:00Z") → "Apr 25, 2026, 08:30:00 UTC"
 */
export function formatUTC(isoString: string): string {
  const d = new Date(isoString);
  return (
    d.toLocaleString("en-US", {
      timeZone: "UTC",
      year: "numeric",
      month: "short",
      day: "2-digit",
      hour: "2-digit",
      minute: "2-digit",
      second: "2-digit",
      hour12: false,
    }) + " UTC"
  );
}

/**
 * Format an ISO date string to a short UTC date (no time).
 *
 *   formatDateUTC("2026-04-25T08:30:00Z") → "Apr 25, 2026"
 */
export function formatDateUTC(isoString: string): string {
  const d = new Date(isoString);
  return d.toLocaleDateString("en-US", {
    timeZone: "UTC",
    year: "numeric",
    month: "short",
    day: "2-digit",
  });
}
