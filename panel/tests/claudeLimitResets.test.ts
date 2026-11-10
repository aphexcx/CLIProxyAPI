import { afterEach, expect, spyOn, test } from 'bun:test';
import { createElement } from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { apiClient } from '@/services/api/client';
import { readClaudeLimitResets, type ClaudeLimitResetSeat } from '@/services/api/claudeLimitResets';
import { ClaudeLimitResetsView } from '@/features/quota/providers/claude/ClaudeLimitResets';

const seat: ClaudeLimitResetSeat = {
  name: 'fixture.json',
  auth_index: 'fixture',
  stale: false,
  age_seconds: 12,
  best_click_rank: 1,
  rank_reason: 'holder action only',
  cedar_ember: {
    eligible: true,
    ineligible_reason: null,
    at_limit: true,
    weekly_resets_at: '2027-03-01T00:00:00Z',
    cooldown_until: null,
    grants: [
      {
        id: 'launch',
        label: 'Launch',
        resets_left: 2,
        ends_at: '2027-03-08T00:00:00Z',
        type: 'weekly',
        clears: ['seven_day_overage_included'],
        usable_now: true,
      },
    ],
  },
};
let mocks: { mockRestore(): void }[] = [];
afterEach(() => {
  for (const mock of mocks) mock.mockRestore();
  mocks = [];
});

test('reset column renders count, expiry, usability and holder rank without controls', () => {
  const html = renderToStaticMarkup(createElement(ClaudeLimitResetsView, { seat }));
  for (const text of [
    'Resets: 2',
    'Best click #1',
    'Launch: 2 left',
    'expires',
    'usable now',
    'holder action only',
    '12s ago',
  ])
    expect(html).toContain(text);
  expect(html).not.toContain('<button');
  expect(html).not.toContain('<form');
});

test('stale reset data stays visible with error and age but no recommendation', () => {
  const html = renderToStaticMarkup(
    createElement(ClaudeLimitResetsView, {
      seat: { ...seat, stale: true, error: 'usage poll failed (HTTP 429)' },
    })
  );
  expect(html).toContain('Resets: 2');
  expect(html).toContain('HTTP 429');
  expect(html).toContain('12s ago · stale');
  expect(html).toContain('not usable now');
  expect(html).not.toContain('Best click');
});

test('cards coalesce cache GETs and isolate management connections', async () => {
  const get = spyOn(apiClient, 'get').mockResolvedValue({ seats: [seat], ranking_rule: 'fixture' });
  const revision = spyOn(apiClient, 'getConnectionRevision').mockReturnValue(100_001);
  mocks.push(get, revision);
  const results = await Promise.all([readClaudeLimitResets(), readClaudeLimitResets()]);
  expect(results[0].seats[0].name).toBe('fixture.json');
  await readClaudeLimitResets();
  expect(get).toHaveBeenCalledTimes(1);
  expect(get).toHaveBeenCalledWith('/claude/limit-resets');
  revision.mockReturnValue(100_002);
  await readClaudeLimitResets();
  expect(get).toHaveBeenCalledTimes(2);
});
