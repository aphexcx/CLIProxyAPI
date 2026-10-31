import { describe, expect, test } from 'bun:test';
import {
  describeSeatFreshness,
  formatResetWords,
  isSeatCardType,
  readSeatPlanTier,
  remainingPercent,
  splitSeatWindows,
} from '@/features/quota/seatCard';
import { DAY_MS, HOUR_MS, MINUTE_MS } from '@/utils/time/durations';
import type { AuthFileItem, ClaudeQuotaState } from '@/types';

const seat = (overrides: Partial<AuthFileItem>): AuthFileItem => ({
  name: 'claude-seat.json',
  type: 'claude',
  ...overrides,
});

describe('readSeatPlanTier', () => {
  test('reads the trimmed tier the hub recorded', () => {
    expect(readSeatPlanTier(seat({ plan_tier: '  Max 20x ' }))).toBe('Max 20x');
  });

  test.each([undefined, '', '   ', 20, { label: 'Max' }])('shows no chip for %p', (value) => {
    expect(readSeatPlanTier(seat({ plan_tier: value as string }))).toBeNull();
  });
});

describe('splitSeatWindows', () => {
  const ids = (windows: { id: string }[]) => windows.map((window) => window.id);

  test('leads Claude with the 5-hour and 7-day windows the hub routes on', () => {
    const { headline, rest } = splitSeatWindows('claude', [
      { id: 'seven-day-sonnet' },
      { id: 'seven-day' },
      { id: 'five-hour' },
      { id: 'seven-day-fable' },
    ]);
    expect(ids(headline)).toEqual(['five-hour', 'seven-day']);
    expect(ids(rest)).toEqual(['seven-day-sonnet', 'seven-day-fable']);
  });

  test('leads Codex with the primary and the weekly or monthly window', () => {
    const weekly = splitSeatWindows('codex', [
      { id: 'five-hour' },
      { id: 'weekly' },
      { id: 'astra-five-hour' },
    ]);
    expect(ids(weekly.headline)).toEqual(['five-hour', 'weekly']);
    expect(ids(weekly.rest)).toEqual(['astra-five-hour']);
    const team = splitSeatWindows('codex', [{ id: 'monthly' }, { id: 'code-review-five-hour' }]);
    expect(ids(team.headline)).toEqual(['monthly']);
    expect(ids(team.rest)).toEqual(['code-review-five-hour']);
  });

  test('a seat without either binding window leads with the windows it has', () => {
    const scopedOnly = splitSeatWindows('claude', [
      { id: 'seven-day-fable' },
      { id: 'seven-day-sonnet' },
      { id: 'seven-day-opus' },
    ]);
    expect(ids(scopedOnly.headline)).toEqual(['seven-day-fable', 'seven-day-sonnet']);
    expect(ids(scopedOnly.rest)).toEqual(['seven-day-opus']);
    const none = splitSeatWindows('codex', []);
    expect(none.headline).toEqual([]);
    expect(none.rest).toEqual([]);
  });

  test('only Claude and Codex cards use the seat layout', () => {
    expect(['claude', 'codex', 'kimi', 'xai'].map(isSeatCardType)).toEqual([
      true,
      true,
      false,
      false,
    ]);
  });
});

describe('remainingPercent', () => {
  test.each([
    [62, 38],
    [-5, 100],
    [130, 0],
    [null, null],
  ] as const)('%p used leaves %p', (used, left) => {
    expect(remainingPercent(used)).toBe(left);
  });
});

describe('formatResetWords', () => {
  // A fixed local noon keeps "today" and "tomorrow" stable in any timezone.
  const noon = new Date(2026, 9, 30, 12, 0).getTime();
  const time = (ms: number) =>
    new Date(ms).toLocaleTimeString('en', { hour: 'numeric', minute: '2-digit' });

  test('says only the time for a reset later today', () => {
    const at = noon + 2 * HOUR_MS + 13 * MINUTE_MS;
    expect(formatResetWords(at, noon, 'en')).toBe(time(at));
    expect(formatResetWords(at, noon, 'en')).not.toMatch(/\d{4}|\//);
  });

  test('says tomorrow for the next calendar day', () => {
    const at = noon + 15 * HOUR_MS;
    expect(formatResetWords(at, noon, 'en')).toBe(`tomorrow ${time(at)}`);
  });

  test('names the weekday within the week', () => {
    const at = noon + 3 * DAY_MS;
    const weekday = new Date(at).toLocaleDateString('en', { weekday: 'short' });
    expect(formatResetWords(at, noon, 'en')).toBe(`${weekday} ${time(at)}`);
  });

  test('gives the date beyond the week', () => {
    expect(formatResetWords(noon + 13 * DAY_MS, noon, 'en')).toBe('Nov 12');
  });

  test('never words a past reset as a time later today', () => {
    const yesterday = noon - 15 * HOUR_MS;
    expect(formatResetWords(yesterday, noon, 'en')).toBe(`yesterday ${time(yesterday)}`);
    expect(formatResetWords(noon - 3 * DAY_MS, noon, 'en')).toBe('Oct 27');
  });

  test('falls back to the default locale for an unknown language tag', () => {
    expect(() => formatResetWords(noon + HOUR_MS, noon, 'not a locale')).not.toThrow();
  });
});

describe('describeSeatFreshness', () => {
  const now = Date.parse('2026-10-31T00:00:00Z');
  const cache = (overrides: Partial<NonNullable<ClaudeQuotaState['serverCache']>> = {}) => ({
    status: 'ok' as const,
    polledAt: '2026-10-30T23:59:13Z',
    ageSeconds: 47,
    receivedAtMs: now,
    requestSequence: 1,
    nextPollAt: '2026-10-31T00:01:13Z',
    refreshing: false,
    lastStatus: 200,
    lastPollAt: '2026-10-30T23:59:13Z',
    ...overrides,
  });
  const quota = (serverCache = cache(), stale = false) => ({
    serverCache,
    observedAt: serverCache.polledAt,
    stale,
  });

  test('a healthy poll is fresh, aged by the server clock', () => {
    expect(describeSeatFreshness(quota(), now + 3_000)).toEqual({ kind: 'fresh', ageMs: 50_000 });
  });

  test('a failed last poll outranks staleness', () => {
    expect(describeSeatFreshness(quota(cache({ lastStatus: 429 }), true), now)).toEqual({
      kind: 'failed',
      status: 429,
      ageMs: 47_000,
    });
  });

  test('stale data says so with its age', () => {
    expect(describeSeatFreshness(quota(cache({ ageSeconds: 7200 }), true), now)).toEqual({
      kind: 'stale',
      ageMs: 7_200_000,
    });
  });

  test('a seat never polled waits, or reports the first poll in flight', () => {
    const never = cache({ polledAt: null, lastPollAt: null, ageSeconds: null, lastStatus: 0 });
    expect(
      describeSeatFreshness({ serverCache: never, observedAt: null, stale: true }, now)
    ).toEqual({ kind: 'pending' });
    expect(
      describeSeatFreshness(
        { serverCache: { ...never, refreshing: true }, observedAt: null, stale: true },
        now
      )
    ).toEqual({ kind: 'queued' });
  });

  test('a failed first poll is a failure, not a wait', () => {
    const failedFirst = cache({
      polledAt: null,
      ageSeconds: null,
      lastStatus: 429,
      lastPollAt: '2026-10-30T23:59:50Z',
    });
    expect(
      describeSeatFreshness({ serverCache: failedFirst, observedAt: null, stale: true }, now)
    ).toEqual({ kind: 'failed', status: 429, ageMs: null });
  });

  test('usage retained across a hub restart is stale, then failed if the next poll fails', () => {
    // After a restart the server has not polled yet; the panel keeps the last windows.
    const restarted = cache({
      status: 'not_polled_yet',
      polledAt: null,
      ageSeconds: null,
      lastStatus: 0,
      lastPollAt: null,
    });
    const retained = { observedAt: '2026-10-30T23:00:00Z', stale: true };
    expect(describeSeatFreshness({ ...retained, serverCache: restarted }, now)).toEqual({
      kind: 'stale',
      ageMs: 3_600_000,
    });
    expect(
      describeSeatFreshness(
        { ...retained, serverCache: { ...restarted, lastPollAt: '2026-10-30T23:59:00Z' } },
        now
      )
    ).toEqual({ kind: 'failed', status: 0, ageMs: 3_600_000 });
  });

  test('paused and queued polls are named', () => {
    // The server marks every paused seat stale; a failed last poll does not hide the pause.
    const paused = cache({ status: 'polling_paused', nextPollAt: null });
    expect(describeSeatFreshness(quota(paused, true), now)?.kind).toBe('paused');
    expect(describeSeatFreshness(quota({ ...paused, lastStatus: 401 }, true), now)?.kind).toBe(
      'paused'
    );
    const neverPolled = { ...paused, polledAt: null, ageSeconds: null, lastPollAt: null };
    expect(
      describeSeatFreshness({ serverCache: neverPolled, observedAt: null, stale: true }, now)?.kind
    ).toBe('paused');
    expect(describeSeatFreshness(quota(cache({ refreshing: true })), now)?.kind).toBe('queued');
  });

  test('a manual read without the server cache ages from its observation', () => {
    expect(
      describeSeatFreshness({ observedAt: '2026-10-30T23:58:00Z', stale: false }, now)
    ).toEqual({ kind: 'fresh', ageMs: 120_000 });
    expect(describeSeatFreshness({ observedAt: null, stale: false }, now)).toBeNull();
  });
});
