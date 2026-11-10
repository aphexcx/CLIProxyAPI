import { afterEach, beforeEach, expect, spyOn, test } from 'bun:test';
import { readdir, readFile } from 'node:fs/promises';
import { join } from 'node:path';
import { createElement } from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { apiClient } from '@/services/api/client';
import {
  readClaudeLimitResets,
  matchClaudeLimitResetSeat,
  observeClaudeResetCredentials,
  invalidateClaudeLimitResets,
  type ClaudeLimitResetSeat,
} from '@/services/api/claudeLimitResets';
import { ClaudeLimitResetsView } from '@/features/quota/providers/claude/ClaudeLimitResets';
import i18n from '@/i18n';
import { authFilesApi } from '@/services/api/authFiles';

const seat: ClaudeLimitResetSeat = {
  name: 'fixture.json',
  auth_index: 'fixture',
  credential_generation: '1:1',
  last_good_at: '2026-11-10T00:00:00Z',
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
const observedAt = Date.parse(seat.last_good_at!);
beforeEach(async () => {
  await i18n.changeLanguage('en');
  invalidateClaudeLimitResets();
  mocks.push(spyOn(Date, 'now').mockReturnValue(observedAt + 12_000));
});
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

test('response latency does not skip the next minute refresh', async () => {
  let now = 1_000;
  const clock = spyOn(Date, 'now').mockImplementation(() => now);
  const revision = spyOn(apiClient, 'getConnectionRevision').mockReturnValue(100_003);
  const get = spyOn(apiClient, 'get').mockImplementation(async () => {
    now += 200;
    return { seats: [seat], ranking_rule: 'fixture' };
  });
  mocks.push(clock, revision, get);
  await readClaudeLimitResets();
  now = 61_000;
  await readClaudeLimitResets();
  expect(get).toHaveBeenCalledTimes(2);
});

test('credential list replacement drops cached observations within the minute', async () => {
  let generation = '1:1';
  const get = spyOn(apiClient, 'get').mockImplementation(async (path) =>
    path === '/credentials'
      ? { files: [{ name: seat.name, type: 'claude', credential_generation: generation }] }
      : { seats: [{ ...seat, credential_generation: generation }], ranking_rule: 'fixture' }
  );
  mocks.push(get);
  await authFilesApi.list();
  const first = await readClaudeLimitResets();
  generation = '1:2';
  await authFilesApi.list();
  expect(matchClaudeLimitResetSeat(first.seats, seat.name, generation)).toBeUndefined();
  const next = await readClaudeLimitResets();
  expect(matchClaudeLimitResetSeat(next.seats, seat.name, generation)?.credential_generation).toBe(
    '1:2'
  );
  expect(get.mock.calls.filter(([path]) => path === '/claude/limit-resets')).toHaveLength(2);
});

test('a reset read spanning a credential replacement is discarded', async () => {
  let resolve!: (value: { seats: ClaudeLimitResetSeat[]; ranking_rule: string }) => void;
  const get = spyOn(apiClient, 'get').mockImplementation(
    () =>
      new Promise((done) => {
        resolve = done;
      })
  );
  mocks.push(get);
  observeClaudeResetCredentials([{ name: seat.name, credential_generation: '2:1' }]);
  const old = readClaudeLimitResets();
  observeClaudeResetCredentials([{ name: seat.name, credential_generation: '2:2' }]);
  resolve({ seats: [seat], ranking_rule: 'old' });
  await expect(old).rejects.toThrow('obsolete credential list');
  get.mockResolvedValue({
    seats: [{ ...seat, credential_generation: '2:2' }],
    ranking_rule: 'new',
  });
  expect((await readClaudeLimitResets()).ranking_rule).toBe('new');
});

test('two failed refreshes display elapsed time from the last good read', () => {
  const clock = spyOn(Date, 'now');
  mocks.push(clock);
  for (const seconds of [72, 3_612]) {
    clock.mockReturnValue(observedAt + seconds * 1_000);
    const html = renderToStaticMarkup(createElement(ClaudeLimitResetsView, { seat, failed: true }));
    expect(html).toContain(`${seconds}s ago · stale`);
    expect(html).not.toContain('Best click');
  }
});

test('panel source has no reset claim path', async () => {
  async function scan(dir: string): Promise<void> {
    for (const entry of await readdir(dir, { withFileTypes: true })) {
      const path = join(dir, entry.name);
      if (entry.isDirectory()) await scan(path);
      else expect(await readFile(path, 'utf8')).not.toContain('reset_rate_limits');
    }
  }
  await scan('src');
});
