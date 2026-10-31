/**
 * Seat-card presentation logic for Claude and Codex quota cards.
 *
 * React-free and SCSS-free so tests can import it directly. The card leads with
 * the seat, its plan, and the two windows the hub routes on; everything else is
 * one disclosure away.
 */

import type { AuthFileItem, ClaudeQuotaState } from '@/types';
import { DAY_MS } from '@/utils/time/durations';

export type SeatCardType = 'claude' | 'codex';

type SeatWindow = { id: string };

/** Window ids the hub routes on, in display order; Codex teams use a monthly second window. */
const HEADLINE_WINDOW_IDS: Record<SeatCardType, readonly (readonly string[])[]> = {
  claude: [['five-hour'], ['seven-day']],
  codex: [['five-hour'], ['weekly', 'monthly']],
};

export const isSeatCardType = (type: string): type is SeatCardType =>
  type === 'claude' || type === 'codex';

/** The trimmed Claude tier label from the v8 entry; null hides the chip rather than guessing. */
export const readSeatPlanTier = (file: AuthFileItem): string | null => {
  const value: unknown = file.plan_tier;
  return typeof value === 'string' && value.trim() ? value.trim() : null;
};

/** Split windows into the binding pair (in display order) and the rest (in payload order). */
export const splitSeatWindows = <T extends SeatWindow>(
  type: SeatCardType,
  windows: readonly T[]
): { headline: T[]; rest: T[] } => {
  const headline: T[] = [];
  for (const ids of HEADLINE_WINDOW_IDS[type]) {
    const match = windows.find((window) => ids.includes(window.id));
    if (match) headline.push(match);
  }
  return { headline, rest: windows.filter((window) => !headline.includes(window)) };
};

/** Remaining share of a window, clamped to 0–100; null when usage is unknown. */
export const remainingPercent = (usedPercent: number | null): number | null =>
  usedPercent === null ? null : Math.max(0, Math.min(100, 100 - usedPercent));

const startOfDay = (ms: number): number => {
  const date = new Date(ms);
  date.setHours(0, 0, 0, 0);
  return date.getTime();
};

const safeLocale = (locale?: string): string | undefined => {
  try {
    return locale ? Intl.getCanonicalLocales(locale)[0] : undefined;
  } catch {
    return undefined;
  }
};

/**
 * A reset instant in plain words, in the reader's clock: `10:06 PM` today,
 * `tomorrow 3:48 AM`, `Mon 11:53 PM` within the week, `Nov 12` beyond it.
 */
export const formatResetWords = (atMs: number, nowMs: number, locale?: string): string => {
  const lang = safeLocale(locale);
  const time = new Date(atMs).toLocaleTimeString(lang, { hour: 'numeric', minute: '2-digit' });
  const days = Math.round((startOfDay(atMs) - startOfDay(nowMs)) / DAY_MS);
  if (days <= 0) return time;
  if (days === 1) {
    const tomorrow = new Intl.RelativeTimeFormat(lang, { numeric: 'auto' }).format(1, 'day');
    return `${tomorrow} ${time}`;
  }
  if (days < 7) {
    return `${new Date(atMs).toLocaleDateString(lang, { weekday: 'short' })} ${time}`;
  }
  return new Date(atMs).toLocaleDateString(lang, { month: 'short', day: 'numeric' });
};

export type SeatFreshness =
  | { kind: 'pending' }
  | { kind: 'failed'; status: number; ageMs: number | null }
  | { kind: 'stale'; ageMs: number | null }
  | { kind: 'paused' }
  | { kind: 'queued' }
  | { kind: 'fresh'; ageMs: number | null };

type FreshnessSource = Pick<ClaudeQuotaState, 'serverCache' | 'observedAt' | 'stale'>;

/**
 * One state for the card footer, most actionable first. Age comes from the
 * server's own clock where it is known, so a skewed browser clock cannot make
 * fresh data look old.
 *
 * `polledAt` is the last successful poll, so it stays null after a failed
 * first poll and after a hub restart; a completed failure and retained stale
 * usage both outrank "waiting for the first poll".
 */
export const describeSeatFreshness = (
  quota: FreshnessSource,
  nowMs: number
): SeatFreshness | null => {
  const cache = quota.serverCache;
  const observedMs = quota.observedAt ? Date.parse(quota.observedAt) : NaN;
  const ageMs =
    cache?.ageSeconds != null
      ? Math.max(0, cache.ageSeconds * 1000 + Math.max(0, nowMs - cache.receivedAtMs))
      : Number.isFinite(observedMs)
        ? Math.max(0, nowMs - observedMs)
        : null;
  const firstPollRunning = cache
    ? !cache.polledAt && cache.refreshing && cache.lastStatus === 0
    : false;
  if (
    cache?.lastPollAt &&
    !firstPollRunning &&
    (cache.lastStatus < 200 || cache.lastStatus >= 300)
  ) {
    return { kind: 'failed', status: cache.lastStatus, ageMs };
  }
  if (quota.stale && quota.observedAt) return { kind: 'stale', ageMs };
  if (cache && !cache.polledAt) return firstPollRunning ? { kind: 'queued' } : { kind: 'pending' };
  if (quota.stale) return { kind: 'stale', ageMs };
  if (cache?.status === 'polling_paused') return { kind: 'paused' };
  if (cache?.refreshing) return { kind: 'queued' };
  if (!cache && !quota.observedAt) return null;
  return { kind: 'fresh', ageMs };
};
