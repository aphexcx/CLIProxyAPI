import { afterEach, beforeEach, expect, spyOn, test } from 'bun:test';
import { authFilesApi } from '@/services/api/authFiles';
import { apiClient } from '@/services/api/client';
import { apiCallApi } from '@/services/api/apiCall';
import { useQuotaStore } from '@/stores/useQuotaStore';
import { getQuotaCacheKey } from '@/utils/quota/identity';
import {
  subscribeQuotaCacheReads,
  QUOTA_CACHE_READ_INTERVAL_MS,
} from '@/features/quota/cachedQuotaReads';
import { CODEX_CONFIG } from '@/features/quota/providers/codex/data';
import i18n from '@/i18n';
import type { AuthFileItem, CodexCachedUsage } from '@/types';

const pending: CodexCachedUsage = {
  status: 'not_polled_yet',
  stale: true,
  polled_at: null,
  age_seconds: null,
  next_poll_at: '2026-10-30T12:01:00Z',
  refreshing: true,
  last_status: 0,
  last_poll_at: null,
};
const complete: CodexCachedUsage = {
  ...pending,
  status: 'ok',
  stale: false,
  polled_at: '2026-10-30T12:01:00Z',
  age_seconds: 0,
  last_status: 200,
  last_poll_at: '2026-10-30T12:01:00Z',
  refreshing: false,
  rate_limit: {
    primary_window: { used_percent: 42, reset_at: 1893456000, limit_window_seconds: 18000 },
  },
};
const seat: AuthFileItem = {
  name: 'codex.json',
  type: 'codex',
  authIndex: 'fixture',
  codex_usage: pending,
};
const timers = new Map<number, () => Promise<void>>();
const releases: (() => void)[] = [];
const mocks: { mockRestore(): void }[] = [];
let timerID = 0;

beforeEach(() => {
  useQuotaStore.getState().clearQuotaCache();
  mocks.push(
    spyOn(globalThis, 'setTimeout').mockImplementation(((
      fn: () => Promise<void>,
      delay: number
    ) => {
      expect(delay).toBe(QUOTA_CACHE_READ_INTERVAL_MS);
      timers.set(++timerID, fn);
      return timerID;
    }) as typeof setTimeout)
  );
  mocks.push(
    spyOn(globalThis, 'clearTimeout').mockImplementation(((id: number) => {
      timers.delete(id);
    }) as typeof clearTimeout)
  );
});
afterEach(() => {
  releases.splice(0).forEach((release) => release());
  mocks.splice(0).forEach((mock) => mock.mockRestore());
  timers.clear();
  useQuotaStore.getState().clearQuotaCache();
});
const tick = async () => {
  expect(timers.size).toBe(1);
  const [id, callback] = [...timers.entries()][0];
  timers.delete(id);
  await callback();
};

test('visible native cards share delayed cache reads and receive completed polls', async () => {
  const files = Array.from({ length: 12 }, (_, i) => ({
    ...seat,
    name: `codex-${i}.json`,
    authIndex: String(i),
  }));
  const get = spyOn(apiClient, 'get').mockResolvedValue({ files });
  const post = spyOn(apiClient, 'post');
  const upstream = spyOn(apiCallApi, 'request');
  mocks.push(get, post, upstream);
  await authFilesApi.list();
  files.forEach(() => releases.push(subscribeQuotaCacheReads()));
  expect(get).toHaveBeenCalledTimes(1);
  expect(timers.size).toBe(1);
  get.mockResolvedValue({ files: files.map((file) => ({ ...file, codex_usage: complete })) });
  await tick();
  for (const file of files) {
    expect(useQuotaStore.getState().codexQuota[getQuotaCacheKey(file)].windows[0].usedPercent).toBe(
      42
    );
  }
  expect(get).toHaveBeenCalledTimes(2);
  expect(post).not.toHaveBeenCalled();
  expect(upstream).not.toHaveBeenCalled();
  releases.splice(0).forEach((release) => release());
  expect(timers.size).toBe(0);
});

test('a queued explicit refresh is followed by a cache-only list read', async () => {
  const post = spyOn(apiClient, 'post').mockResolvedValue(pending);
  const get = spyOn(apiClient, 'get').mockResolvedValue({
    files: [{ ...seat, codex_usage: complete }],
  });
  const upstream = spyOn(apiCallApi, 'request');
  mocks.push(post, get, upstream);
  const data = await CODEX_CONFIG.fetchQuota(seat, i18n.t);
  useQuotaStore
    .getState()
    .setCodexQuota({ [getQuotaCacheKey(seat)]: CODEX_CONFIG.buildSuccessState(data) });
  releases.push(subscribeQuotaCacheReads());
  expect(
    useQuotaStore.getState().codexQuota[getQuotaCacheKey(seat)].serverCache?.polledAt
  ).toBeNull();
  await tick();
  expect(useQuotaStore.getState().codexQuota[getQuotaCacheKey(seat)].serverCache?.polledAt).toBe(
    complete.polled_at
  );
  expect(post).toHaveBeenCalledTimes(1);
  expect(upstream).not.toHaveBeenCalled();
});

test('in-flight reads coalesce new subscribers and stop after the last card leaves', async () => {
  let finish!: () => void;
  const list = spyOn(authFilesApi, 'list').mockImplementation(async () => {
    await new Promise<void>((resolve) => {
      finish = resolve;
    });
    return { files: [] };
  });
  mocks.push(list);
  releases.push(subscribeQuotaCacheReads());
  const running = tick();
  releases.push(subscribeQuotaCacheReads());
  expect(timers.size).toBe(0);
  releases.splice(0).forEach((release) => release());
  finish();
  await running;
  expect(list).toHaveBeenCalledTimes(1);
  expect(timers.size).toBe(0);
});
