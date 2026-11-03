// What a session row means: the TUI's state word and its archive rule
// (internal/tui/archive.go), so the page and the TUI group sessions alike.
import type { SessionRow } from "./api";

/** The TUI's state word: retired or moved, then stale, then presence. */
export function stateWord(s: SessionRow): string {
  if (s.status === "retired" || s.status === "moved") return s.status;
  if (s.stale) return "stale";
  return s.presence || s.status || "-";
}

export const DAY_MS = 24 * 60 * 60 * 1000;

/**
 * Retired or moved sessions enter the Archive 24 hours after their last
 * update. Active ones, an unreadable time and a time in the future stay
 * Current (archivedStamp and reclassify in archive.go).
 */
export function isArchived(s: SessionRow, now: Date): boolean {
  if (s.status !== "retired" && s.status !== "moved") return false;
  const t = new Date(s.updatedAt).getTime();
  if (Number.isNaN(t) || t > now.getTime()) return false;
  return now.getTime() - t >= DAY_MS;
}

export type SessionTab = "current" | "archive" | "all";

export function inTab(s: SessionRow, tab: SessionTab, now: Date): boolean {
  return tab === "all" || isArchived(s, now) === (tab === "archive");
}

/** Whether the viewer's own principal owns the session. An unknown owner or viewer is never "own". */
export function isOwn(s: SessionRow, principal: string): boolean {
  return principal !== "" && s.owner !== "" && s.owner === principal;
}
