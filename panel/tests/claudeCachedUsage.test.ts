import { afterEach, beforeEach, describe, expect, spyOn, test } from 'bun:test';
import { createElement } from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { I18nextProvider } from 'react-i18next';
import i18n from '@/i18n';
import { authFilesApi } from '@/services/api/authFiles';
import { apiClient } from '@/services/api/client';
import { apiCallApi } from '@/services/api/apiCall';
import { useQuotaStore } from '@/stores/useQuotaStore';
import { getQuotaCacheKey } from '@/utils/quota/identity';
import { CLAUDE_CONFIG, readClaudeQuota } from '@/features/quota/providers/claude/data';
import { ClaudeQuotaBody } from '@/features/quota/providers/claude/ClaudeQuotaBody';
import { QUOTA_CLASS_KEYS, bindQuotaClasses } from '@/features/quota/types';
import type { AuthFileItem, AuthFilesResponse, ClaudeCachedUsage } from '@/types';

const translations = i18n.cloneInstance({ lng: 'en' });
const polledAt = '2026-10-30T12:00:00Z';
const usage: ClaudeCachedUsage = {
  status: 'ok',
  stale: false,
  polled_at: polledAt,
  age_seconds: 12,
  next_poll_at: '2026-10-30T12:01:00Z',
  refreshing: false,
  last_status: 200,
  last_poll_at: polledAt,
  five_hour: { utilization: 42, resets_at: '2026-10-30T16:00:00Z' },
  seven_day: { utilization: 25, resets_at: '2026-11-01T12:00:00Z' },
};
const seat: AuthFileItem = {
  name: 'fixture-seat.json',
  type: 'claude',
  authIndex: 'fixture-index',
  claude_usage: usage,
};
const classes = bindQuotaClasses(
  Object.fromEntries(QUOTA_CLASS_KEYS.map((key) => [key, key])),
  'test'
);
const state = (file = seat) => useQuotaStore.getState().claudeQuota[getQuotaCacheKey(file)];
const render = (file = seat) =>
  renderToStaticMarkup(
    createElement(
      I18nextProvider,
      { i18n: translations },
      createElement(ClaudeQuotaBody, { quota: state(file), classes })
    )
  );
const originalGet = apiClient.get;
const originalPost = apiClient.post;
const originalRequest = apiCallApi.request;
const originalRevision = apiClient.getConnectionRevision;

beforeEach(() => useQuotaStore.getState().clearQuotaCache());
afterEach(() => {
  apiClient.get = originalGet;
  apiClient.post = originalPost;
  apiCallApi.request = originalRequest;
  apiClient.getConnectionRevision = originalRevision;
  useQuotaStore.getState().clearQuotaCache();
});

describe('Claude list ingestion and no-click rendering', () => {
  test('one list read seeds every seat and renders windows without per-seat requests', async () => {
    const files = Array.from({ length: 12 }, (_, index) => ({
      ...seat,
      name: `fixture-${index}.json`,
      authIndex: String(index),
    }));
    const get = spyOn(apiClient, 'get').mockResolvedValue({ files });
    const post = spyOn(apiClient, 'post');
    const apiCall = spyOn(apiCallApi, 'request');
    await authFilesApi.list();
    for (const file of files) {
      expect(state(file).observedAt).toBe(polledAt);
      expect(state(file).serverCache?.ageSeconds).toBe(12);
      const html = render(file);
      expect(html).toContain('58%');
      expect(html).toContain('75%');
      expect(html).toContain(`dateTime="${polledAt}"`);
      expect(html).toContain('seconds old');
      expect(html).not.toContain('Click here');
    }
    expect(get).toHaveBeenCalledTimes(1);
    expect(get).toHaveBeenCalledWith('/credentials', undefined);
    expect(post).not.toHaveBeenCalled();
    expect(apiCall).not.toHaveBeenCalled();
  });

  test('a never-polled seat shows pending and its next eligible slot without invented usage', async () => {
    const pending = {
      ...seat,
      claude_usage: {
        ...usage,
        status: 'not_polled_yet' as const,
        stale: true,
        polled_at: null,
        age_seconds: null,
        last_poll_at: null,
        last_status: 0,
        five_hour: null,
        seven_day: null,
      },
    };
    spyOn(apiClient, 'get').mockResolvedValue({ files: [pending] });
    await authFilesApi.list();
    expect(state().observedAt).toBeNull();
    const html = render();
    expect(html).toContain('Waiting for the first usage poll');
    expect(html).toContain('Next eligible poll:');
    expect(html).not.toContain('seconds old');
    expect(html).not.toContain('0%');
    expect(html).not.toContain('Last poll failed');
    expect(html).not.toContain('No quota data');
  });

  test('a failed server poll retains the last windows, original poll time and failure line', async () => {
    const get = spyOn(apiClient, 'get').mockResolvedValue({ files: [seat] });
    await authFilesApi.list();
    get.mockResolvedValue({
      files: [
        {
          ...seat,
          claude_usage: {
            ...usage,
            stale: true,
            age_seconds: 82,
            last_status: 429,
            last_poll_at: '2026-10-30T12:01:00Z',
          },
        },
      ],
    });
    await authFilesApi.list();
    expect(state().observedAt).toBe(polledAt);
    expect(state().serverCache?.ageSeconds).toBe(82);
    const html = render();
    expect(html).toContain('58%');
    expect(html).toContain('75%');
    expect(html).toContain('Last poll failed (429)');
    expect(html).toContain('Cached usage is stale');
  });

  test('queued and paused states do not imply a successful refresh', async () => {
    spyOn(apiClient, 'get').mockResolvedValue({
      files: [
        {
          ...seat,
          claude_usage: {
            ...usage,
            status: 'polling_paused',
            stale: true,
            refreshing: true,
            next_poll_at: null,
          },
        },
      ],
    });
    await authFilesApi.list();
    expect(render()).toContain('Polling paused');
    expect(render()).toContain('Refresh queued or in progress');
    expect(state().observedAt).toBe(polledAt);
    expect(render()).not.toContain('Last poll failed');
  });

  test('transport failures in the poll loop remain distinct from no poll yet', async () => {
    spyOn(apiClient, 'get').mockResolvedValue({
      files: [
        {
          ...seat,
          claude_usage: {
            ...usage,
            stale: true,
            last_status: 0,
            last_poll_at: '2026-10-30T12:01:00Z',
          },
        },
      ],
    });
    await authFilesApi.list();
    expect(render()).toContain('Last poll failed (no HTTP response)');
    expect(render()).toContain('58%');
  });

  test('optional profile metadata survives subsequent list updates', async () => {
    spyOn(apiClient, 'get').mockResolvedValue({ files: [seat] });
    await authFilesApi.list();
    useQuotaStore
      .getState()
      .setClaudeQuota({ [getQuotaCacheKey(seat)]: { ...state(), planType: 'plan_max' } });
    await authFilesApi.list();
    expect(state().planType).toBe('plan_max');
  });

  test('missing or malformed cache fields leave existing usage intact', async () => {
    const get = spyOn(apiClient, 'get').mockResolvedValue({ files: [seat] });
    await authFilesApi.list();
    const previous = state();
    for (const claude_usage of [undefined, null, {}, { ...usage, polled_at: 'invalid' }]) {
      get.mockResolvedValue({ files: [{ ...seat, claude_usage }] });
      await authFilesApi.list();
      expect(state()).toBe(previous);
    }
  });

  test('older list responses cannot overwrite a later successful poll', async () => {
    const get = spyOn(apiClient, 'get').mockResolvedValue({
      files: [
        {
          ...seat,
          claude_usage: {
            ...usage,
            polled_at: '2026-10-30T12:02:00Z',
            last_poll_at: '2026-10-30T12:02:00Z',
            five_hour: { ...usage.five_hour, utilization: 60 },
          },
        },
      ],
    });
    await authFilesApi.list();
    get.mockResolvedValue({ files: [seat] });
    await authFilesApi.list();
    expect(state().observedAt).toBe('2026-10-30T12:02:00Z');
    expect(state().windows[0].usedPercent).toBe(60);
  });

  test.each(['cache', 'connection', 'file'] as const)(
    'rejects list ingestion after %s invalidation',
    async (kind) => {
      const deferred = Promise.withResolvers<AuthFilesResponse>();
      spyOn(apiClient, 'get').mockReturnValue(deferred.promise);
      let revision = 1;
      spyOn(apiClient, 'getConnectionRevision').mockImplementation(() => revision);
      const request = authFilesApi.list();
      if (kind === 'connection') revision += 1;
      else useQuotaStore.getState().clearQuotaCache(kind === 'file' ? [seat.name] : undefined);
      deferred.resolve({ files: [seat] });
      await request;
      expect(state()).toBeUndefined();
    }
  );

  test('file invalidation allows unrelated seats from the same list to commit', async () => {
    const deferred = Promise.withResolvers<AuthFilesResponse>();
    spyOn(apiClient, 'get').mockReturnValue(deferred.promise);
    const request = authFilesApi.list();
    useQuotaStore.getState().clearQuotaCache([seat.name]);
    const other = { ...seat, name: 'other.json' };
    deferred.resolve({ files: [seat, other] });
    await request;
    expect(state()).toBeUndefined();
    expect(state(other).windows).toHaveLength(2);
  });
});

describe('Claude coalescing isolation', () => {
  test.each(['cache', 'connection', 'file'] as const)(
    'does not reuse a request after %s invalidation',
    async (kind) => {
      const deferred = Promise.withResolvers<ClaudeCachedUsage>();
      const get = spyOn(apiClient, 'get').mockReturnValue(deferred.promise);
      let revision = 1;
      spyOn(apiClient, 'getConnectionRevision').mockImplementation(() => revision);
      const first = readClaudeQuota(seat, translations.t);
      if (kind === 'connection') revision += 1;
      else useQuotaStore.getState().clearQuotaCache(kind === 'file' ? [seat.name] : undefined);
      const second = readClaudeQuota(seat, translations.t);
      expect(get).toHaveBeenCalledTimes(2);
      deferred.resolve(usage);
      await Promise.all([first, second]);
    }
  );

  test('a failed request is released so the next read can recover', async () => {
    const get = spyOn(apiClient, 'get').mockRejectedValue(new Error('offline'));
    await expect(readClaudeQuota(seat, translations.t)).rejects.toThrow('offline');
    get.mockResolvedValue(usage);
    expect((await readClaudeQuota(seat, translations.t)).observedAt).toBe(polledAt);
    expect(get).toHaveBeenCalledTimes(2);
  });

  test('profile failure does not invalidate successful cached usage', async () => {
    spyOn(apiClient, 'post').mockResolvedValue(usage);
    spyOn(apiCallApi, 'request').mockRejectedValue(new Error('profile unavailable'));
    const data = await CLAUDE_CONFIG.fetchQuota(seat, translations.t);
    await expect(CLAUDE_CONFIG.enrichQuota!(seat, data, translations.t)).rejects.toThrow(
      'profile unavailable'
    );
    expect(CLAUDE_CONFIG.buildSuccessState(data).windows).toHaveLength(2);
  });
});
