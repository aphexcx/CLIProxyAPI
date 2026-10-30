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
import { CODEX_CONFIG, readCodexQuota } from '@/features/quota/providers/codex/data';
import { enrichQuotaInBackground } from '@/features/quota/quotaEnrichment';
import type { QuotaAdapter } from '@/features/quota/providers';
import { CodexQuotaBody } from '@/features/quota/providers/codex/CodexQuotaBody';
import { QUOTA_CLASS_KEYS, bindQuotaClasses } from '@/features/quota/types';
import type { AuthFileItem, AuthFilesResponse, CodexCachedUsage } from '@/types';

const translations = i18n.cloneInstance({ lng: 'en' });
const polledAt = '2026-10-30T12:00:00Z';
const usage: CodexCachedUsage = {
  status: 'ok',
  stale: false,
  polled_at: polledAt,
  age_seconds: 12,
  next_poll_at: '2026-10-30T12:01:00Z',
  refreshing: false,
  last_status: 200,
  last_poll_at: polledAt,
  rate_limit: {
    primary_window: { used_percent: 42, reset_at: 1893456000, limit_window_seconds: 18000 },
    secondary_window: { used_percent: 25, reset_at: 1893456000, limit_window_seconds: 604800 },
  },
};
const seat: AuthFileItem = {
  name: 'codex-fixture.json',
  type: 'codex',
  authIndex: 'fixture',
  codex_usage: usage,
};
const restarted: CodexCachedUsage = {
  status: 'not_polled_yet',
  stale: true,
  polled_at: null,
  age_seconds: null,
  next_poll_at: '2026-10-30T12:02:00Z',
  refreshing: false,
  last_status: 0,
  last_poll_at: null,
};
const classes = bindQuotaClasses(
  Object.fromEntries(QUOTA_CLASS_KEYS.map((key) => [key, key])),
  'test'
);
const state = (file = seat) => useQuotaStore.getState().codexQuota[getQuotaCacheKey(file)];
const render = (file = seat) =>
  renderToStaticMarkup(
    createElement(
      I18nextProvider,
      { i18n: translations },
      createElement(CodexQuotaBody, { quota: state(file), classes })
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

describe('Codex cached usage', () => {
  test('one list seeds every card with no per-seat or render request', async () => {
    const files = Array.from({ length: 12 }, (_, index) => ({
      ...seat,
      name: `codex-${index}.json`,
      authIndex: String(index),
    }));
    const get = spyOn(apiClient, 'get').mockResolvedValue({ files });
    const post = spyOn(apiClient, 'post');
    const upstream = spyOn(apiCallApi, 'request');
    await authFilesApi.list();
    for (const file of files) {
      expect(state(file).observedAt).toBe(polledAt);
      const html = render(file);
      expect(html).toContain('58%');
      expect(html).toContain('75%');
      expect(html).toContain(`dateTime="${polledAt}"`);
      expect(html).toContain('seconds old');
    }
    expect(get).toHaveBeenCalledTimes(1);
    expect(post).not.toHaveBeenCalled();
    expect(upstream).not.toHaveBeenCalled();
  });

  test('never-polled, queued and paused states render without an invented observation', async () => {
    spyOn(apiClient, 'get').mockResolvedValue({
      files: [
        { ...seat, codex_usage: { ...restarted, refreshing: true, status: 'polling_paused' } },
      ],
    });
    await authFilesApi.list();
    expect(state().observedAt).toBeNull();
    const html = render();
    expect(html).toContain('Waiting for the first');
    expect(html).toContain('paused');
    expect(html).toContain('Next eligible');
    expect(html).not.toContain('<time');
  });

  test('failed polls retain windows and show failure, age and next slot', async () => {
    spyOn(apiClient, 'get').mockResolvedValue({
      files: [{ ...seat, codex_usage: { ...usage, stale: true, last_status: 429 } }],
    });
    await authFilesApi.list();
    const html = render();
    expect(html).toContain('58%');
    expect(html).toContain('429');
    expect(html).toContain('stale');
    expect(html).toContain('seconds old');
  });

  test('refresh posts to the shared cache; pure reads get it without optional upstream calls', async () => {
    const post = spyOn(apiClient, 'post').mockResolvedValue({ ...usage, refreshing: true });
    const get = spyOn(apiClient, 'get').mockResolvedValue(usage);
    const upstream = spyOn(apiCallApi, 'request');
    const refreshed = await CODEX_CONFIG.fetchQuota(seat, translations.t);
    expect(refreshed.serverCache?.refreshing).toBe(true);
    expect(refreshed.observedAt).toBe(polledAt);
    expect(post).toHaveBeenCalledWith('/credentials/quota', { auth_index: 'fixture' });
    await readCodexQuota(seat, translations.t);
    expect(get).toHaveBeenCalledWith('/credentials/quota', { params: { auth_index: 'fixture' } });
    expect(upstream).not.toHaveBeenCalled();
  });

  test('native cache errors never fall back to a live usage request', async () => {
    spyOn(apiClient, 'post').mockRejectedValue(
      Object.assign(new Error('missing'), { status: 404 })
    );
    spyOn(apiClient, 'get').mockRejectedValue(Object.assign(new Error('missing'), { status: 404 }));
    const upstream = spyOn(apiCallApi, 'request');
    await expect(CODEX_CONFIG.fetchQuota(seat, translations.t)).rejects.toThrow();
    await expect(
      readCodexQuota({ ...seat, codex_usage: undefined }, translations.t)
    ).rejects.toThrow();
    expect(upstream).not.toHaveBeenCalled();
  });

  test('optional credits stay explicit and are refreshed after consuming a reset', async () => {
    const post = spyOn(apiClient, 'post').mockImplementation(async (url) => {
      if (url === '/credentials/quota') return usage;
      return { status: 'ok', auth_index: 'fixture', models: [] };
    });
    const urls: string[] = [];
    spyOn(apiCallApi, 'request').mockImplementation(async (request) => {
      urls.push(request.url);
      const body = request.url.endsWith('/consume')
        ? { code: 'reset', windows_reset: 1 }
        : { available_count: 0, credits: [] };
      return { statusCode: 200, header: {}, body, bodyText: JSON.stringify(body) };
    });
    const data = await CODEX_CONFIG.resetQuota!(seat, translations.t);
    expect(data.rateLimitResetCreditsAvailableCount).toBe(0);
    expect(urls).toHaveLength(2);
    expect(urls[0]).toContain('/consume');
    expect(urls[1]).not.toContain('/consume');
    expect(urls.some((url) => url.endsWith('/wham/usage'))).toBe(false);
    expect(post).toHaveBeenCalledWith('/credentials/quota', { auth_index: 'fixture' });
  });

  test.each(['refresh', 'reset', 'invalidated'])(
    'list reads preserve explicit metadata without rolling back usage: %s',
    async (operation) => {
      const get = spyOn(apiClient, 'get').mockResolvedValue({ files: [seat] });
      await authFilesApi.list();
      spyOn(apiClient, 'post').mockImplementation(async (url) =>
        url === '/credentials/quota' ? usage : { status: 'ok', auth_index: 'fixture', models: [] }
      );
      const credits = Promise.withResolvers<Awaited<ReturnType<typeof apiCallApi.request>>>();
      const started = Promise.withResolvers<void>();
      spyOn(apiCallApi, 'request').mockImplementation(async (request) => {
        if (request.url.endsWith('/consume')) {
          return { statusCode: 200, header: {}, body: { code: 'reset' }, bodyText: '' };
        }
        started.resolve();
        return credits.promise;
      });
      const setQuota = useQuotaStore.getState().setCodexQuota;
      const key = getQuotaCacheKey(seat);
      let pending: Promise<void>;
      if (operation === 'reset') {
        pending = CODEX_CONFIG.resetQuota!(seat, translations.t).then((data) => {
          setQuota({ [key]: CODEX_CONFIG.buildSuccessState(data) });
        });
      } else {
        const data = await CODEX_CONFIG.fetchQuota(seat, translations.t);
        const success = CODEX_CONFIG.buildSuccessState(data);
        setQuota({ [key]: success });
        pending = enrichQuotaInBackground(
          CODEX_CONFIG as unknown as QuotaAdapter,
          seat,
          data,
          success,
          translations.t
        );
      }
      await started.promise;
      get.mockResolvedValue({
        files: [
          {
            ...seat,
            codex_usage: {
              ...usage,
              polled_at: '2026-10-30T12:01:00Z',
              last_poll_at: '2026-10-30T12:01:00Z',
              rate_limit: {
                primary_window: {
                  used_percent: 99,
                  reset_at: 1893456000,
                  limit_window_seconds: 18000,
                },
              },
            },
          },
        ],
      });
      await authFilesApi.list();
      const currentWindows = state().windows;
      if (operation === 'invalidated') useQuotaStore.getState().clearQuotaCache([seat.name]);
      const body =
        operation === 'reset'
          ? { available_count: 0, credits: [] }
          : {
              credits: [
                {
                  id: 'fixture',
                  status: 'available',
                  reset_type: 'codex_rate_limits',
                  expires_at: '2030-01-01T00:00:00Z',
                },
              ],
            };
      credits.resolve({ statusCode: 200, header: {}, body, bodyText: '' });
      await pending;
      if (operation === 'invalidated') {
        expect(state()).toBeUndefined();
      } else {
        expect(state().windows).toBe(currentWindows);
        expect(state().observedAt).toBe('2026-10-30T12:01:00Z');
        expect(state().rateLimitResetCreditsAvailableCount).toBe(operation === 'reset' ? 0 : 1);
        expect(CODEX_CONFIG.canResetQuota!(state())).toBe(operation !== 'reset');
        await authFilesApi.list();
        expect(state().rateLimitResetCreditsAvailableCount).toBe(operation === 'reset' ? 0 : 1);
      }
    }
  );

  test.each(['http', 'network', 'invalid', 'zero'])(
    'native reset-credit summary survives optional details failure: %s',
    async (failure) => {
      spyOn(apiClient, 'post').mockResolvedValue({
        ...usage,
        rate_limit_reset_credits: { available_count: 2, applicable_available_count: 0 },
      });
      spyOn(apiCallApi, 'request').mockImplementation(async () => {
        if (failure === 'network') throw new Error('offline');
        return {
          statusCode: failure === 'http' ? 503 : 200,
          header: {},
          body: failure === 'zero' ? { available_count: 0, credits: [] } : {},
          bodyText: '',
        };
      });
      const data = await CODEX_CONFIG.fetchQuota(seat, translations.t);
      const enriched = await CODEX_CONFIG.enrichQuota!(seat, data, translations.t);
      const quota = CODEX_CONFIG.buildSuccessState(enriched);
      expect(quota.rateLimitResetCreditsAvailableCount).toBe(failure === 'zero' ? 0 : 2);
      expect(quota.rateLimitResetCreditsApplicableAvailableCount).toBe(0);
      expect(CODEX_CONFIG.canResetQuota!(quota)).toBe(failure !== 'zero');
      expect(Boolean(quota.rateLimitResetCreditsError)).toBe(failure !== 'zero');
    }
  );

  test.each([
    { credits: [] },
    {
      credits: [
        {
          id: 'spent',
          status: 'consumed',
          reset_type: 'codex_rate_limits',
          expires_at: '2030-01-01',
        },
      ],
    },
  ])('empty available-credit details confirm zero without a count: %j', async ({ credits }) => {
    spyOn(apiClient, 'post').mockResolvedValue({
      ...usage,
      rate_limit_reset_credits: { available_count: 1 },
    });
    spyOn(apiCallApi, 'request').mockResolvedValue({
      statusCode: 200,
      header: {},
      body: { credits },
      bodyText: '',
    });
    const data = await CODEX_CONFIG.fetchQuota(seat, translations.t);
    const quota = CODEX_CONFIG.buildSuccessState(
      await CODEX_CONFIG.enrichQuota!(seat, data, translations.t)
    );
    expect(quota.rateLimitResetCreditsAvailableCount).toBe(0);
    expect(CODEX_CONFIG.canResetQuota!(quota)).toBe(false);
  });

  test.each(['details', 'cooldown'])(
    'successful consumption invalidates credits before a failed follow-up: %s',
    async (failure) => {
      const cachedUsage = {
        ...usage,
        rate_limit_reset_credits: { available_count: 1, applicable_available_count: 1 },
      };
      spyOn(apiClient, 'get').mockResolvedValue({ files: [{ ...seat, codex_usage: cachedUsage }] });
      spyOn(apiClient, 'post').mockImplementation(async (url) => {
        if (url === '/credentials/quota') return cachedUsage;
        if (failure === 'cooldown') throw new Error('cooldown unavailable');
        return { status: 'ok', auth_index: 'fixture', models: [] };
      });
      let consumed = false;
      spyOn(apiCallApi, 'request').mockImplementation(async (request) => {
        if (request.url.endsWith('/consume')) {
          consumed = true;
          return { statusCode: 200, header: {}, body: { code: 'reset' }, bodyText: '' };
        }
        return {
          statusCode: consumed ? 503 : 200,
          header: {},
          body: { available_count: 1, credits: [] },
          bodyText: '',
        };
      });
      const save = (data: Awaited<ReturnType<typeof CODEX_CONFIG.fetchQuota>>) =>
        useQuotaStore.getState().setCodexQuota({
          [getQuotaCacheKey(seat)]: CODEX_CONFIG.buildSuccessState(data),
        });
      await authFilesApi.list();
      const before = await CODEX_CONFIG.fetchQuota(seat, translations.t);
      save(await CODEX_CONFIG.enrichQuota!(seat, before, translations.t));
      expect(CODEX_CONFIG.canResetQuota!(state())).toBe(true);
      if (failure === 'cooldown') {
        await expect(CODEX_CONFIG.resetQuota!(seat, translations.t)).rejects.toThrow();
      } else {
        save(await CODEX_CONFIG.resetQuota!(seat, translations.t));
      }
      expect(state().rateLimitResetCreditsAvailableCount).toBeNull();
      expect(CODEX_CONFIG.canResetQuota!(state())).toBe(false);
      await authFilesApi.list();
      const after = await CODEX_CONFIG.fetchQuota(seat, translations.t);
      save(await CODEX_CONFIG.enrichQuota!(seat, after, translations.t));
      expect(CODEX_CONFIG.canResetQuota!(state())).toBe(false);
    }
  );

  test.each(['before-response', 'before-commit'])(
    'credit confirmation protects newer polls observed %s',
    async (timing) => {
      let cachedUsage = {
        ...usage,
        rate_limit_reset_credits: { available_count: 1, applicable_available_count: 1 },
      };
      spyOn(apiClient, 'get').mockImplementation(async () => ({
        files: [{ ...seat, codex_usage: cachedUsage }],
      }));
      spyOn(apiClient, 'post').mockImplementation(async () => cachedUsage);
      const details = Promise.withResolvers<Awaited<ReturnType<typeof apiCallApi.request>>>();
      const request = spyOn(apiCallApi, 'request').mockReturnValueOnce(details.promise);
      const save = (data: Awaited<ReturnType<typeof CODEX_CONFIG.fetchQuota>>) =>
        useQuotaStore.getState().setCodexQuota({
          [getQuotaCacheKey(seat)]: CODEX_CONFIG.buildSuccessState(data),
        });
      await authFilesApi.list();
      const oldData = await CODEX_CONFIG.fetchQuota(seat, translations.t);
      const pending = CODEX_CONFIG.enrichQuota!(seat, oldData, translations.t);
      const zero = { statusCode: 200, header: {}, body: { available_count: 0 }, bodyText: '' };
      if (timing === 'before-commit') {
        details.resolve(zero);
        await pending;
      }
      cachedUsage = {
        ...cachedUsage,
        polled_at: '2026-10-30T12:02:00Z',
        last_poll_at: '2026-10-30T12:02:00Z',
      };
      await authFilesApi.list();
      details.resolve(zero);
      save(await pending);
      expect(state().serverCache?.polledAt).toBe(cachedUsage.polled_at);
      expect(state().rateLimitResetCreditsAvailableCount).toBe(0);

      for (let attempt = 0; attempt < 2; attempt++) {
        request.mockResolvedValue({ statusCode: 503, header: {}, body: {}, bodyText: '' });
        const current = await CODEX_CONFIG.fetchQuota(seat, translations.t);
        save(await CODEX_CONFIG.enrichQuota!(seat, current, translations.t));
        expect(state().rateLimitResetCreditsAvailableCount).toBe(0);
        expect(CODEX_CONFIG.canResetQuota!(state())).toBe(false);
        // Reusing older usage data for a successful details read must not lower
        // the poll timestamp protected by the newer credit observation.
        request.mockResolvedValue(zero);
        save(await CODEX_CONFIG.enrichQuota!(seat, oldData, translations.t));
      }
    }
  );

  test.each(['confirmation-first', 'fallback-first'])(
    'credit confirmation wins over a failed newer read: %s',
    async (order) => {
      const cachedUsage = {
        ...usage,
        rate_limit_reset_credits: { available_count: 1, applicable_available_count: 1 },
      };
      spyOn(apiClient, 'get').mockResolvedValue({ files: [{ ...seat, codex_usage: cachedUsage }] });
      spyOn(apiClient, 'post').mockResolvedValue(cachedUsage);
      const save = (data: Awaited<ReturnType<typeof CODEX_CONFIG.fetchQuota>>) =>
        useQuotaStore.getState().setCodexQuota({
          [getQuotaCacheKey(seat)]: CODEX_CONFIG.buildSuccessState(data),
        });
      await authFilesApi.list();
      const data = await CODEX_CONFIG.fetchQuota(seat, translations.t);
      const confirmed = Promise.withResolvers<Awaited<ReturnType<typeof apiCallApi.request>>>();
      const request = spyOn(apiCallApi, 'request').mockReturnValueOnce(confirmed.promise);
      const earlier = CODEX_CONFIG.enrichQuota!(seat, data, translations.t);
      request.mockResolvedValue({ statusCode: 503, header: {}, body: {}, bodyText: '' });
      const fallback = await CODEX_CONFIG.enrichQuota!(seat, data, translations.t);
      expect(fallback.rateLimitResetCreditsAvailableCount).toBe(1);
      if (order === 'fallback-first') save(fallback);
      confirmed.resolve({
        statusCode: 200,
        header: {},
        body: { available_count: 0 },
        bodyText: '',
      });
      save(await earlier);
      expect(state().rateLimitResetCreditsAvailableCount).toBe(0);
      if (order === 'confirmation-first') save(fallback);
      expect(state().rateLimitResetCreditsAvailableCount).toBe(0);
      expect(CODEX_CONFIG.canResetQuota!(state())).toBe(false);
    }
  );

  test('a failed details read cannot restore consumed credits from the same cached poll', async () => {
    let cachedUsage = {
      ...usage,
      rate_limit_reset_credits: { available_count: 1, applicable_available_count: 1 },
    };
    spyOn(apiClient, 'get').mockImplementation(async () => ({
      files: [{ ...seat, codex_usage: cachedUsage }],
    }));
    spyOn(apiClient, 'post').mockImplementation(async (url) =>
      url === '/credentials/quota'
        ? cachedUsage
        : { status: 'ok', auth_index: 'fixture', models: [] }
    );
    let failDetails = false;
    spyOn(apiCallApi, 'request').mockImplementation(async (request) => ({
      statusCode: failDetails ? 503 : 200,
      header: {},
      body: request.url.endsWith('/consume')
        ? { code: 'reset' }
        : { available_count: 0, applicable_available_count: 0, credits: [] },
      bodyText: '',
    }));
    const save = (data: Awaited<ReturnType<typeof CODEX_CONFIG.fetchQuota>>) =>
      useQuotaStore.getState().setCodexQuota({
        [getQuotaCacheKey(seat)]: CODEX_CONFIG.buildSuccessState(data),
      });
    await authFilesApi.list();
    save(await CODEX_CONFIG.resetQuota!(seat, translations.t));
    expect(state().rateLimitResetCreditsAvailableCount).toBe(0);
    failDetails = true;
    for (let attempt = 0; attempt < 2; attempt++) {
      await authFilesApi.list();
      const data = await CODEX_CONFIG.fetchQuota(seat, translations.t);
      save(await CODEX_CONFIG.enrichQuota!(seat, data, translations.t));
      expect(state().rateLimitResetCreditsAvailableCount).toBe(0);
      expect(CODEX_CONFIG.canResetQuota!(state())).toBe(false);
      expect(state().rateLimitResetCreditsError).not.toBe('');
    }
    cachedUsage = {
      ...cachedUsage,
      polled_at: '2026-10-30T12:02:00Z',
      last_poll_at: '2026-10-30T12:02:00Z',
      rate_limit_reset_credits: { available_count: 1, applicable_available_count: 1 },
    };
    await authFilesApi.list();
    const inFlight = await CODEX_CONFIG.fetchQuota(seat, translations.t);
    save(await CODEX_CONFIG.enrichQuota!(seat, inFlight, translations.t));
    expect(state().rateLimitResetCreditsAvailableCount).toBe(0);
    expect(CODEX_CONFIG.canResetQuota!(state())).toBe(false);
    // Only the following serialized poll is guaranteed to have started after
    // confirmation; the first completion could contain pre-redemption data.
    cachedUsage = {
      ...cachedUsage,
      polled_at: '2026-10-30T12:03:00Z',
      last_poll_at: '2026-10-30T12:03:00Z',
      rate_limit_reset_credits: { available_count: 2, applicable_available_count: 1 },
    };
    await authFilesApi.list();
    const newer = await CODEX_CONFIG.fetchQuota(seat, translations.t);
    save(await CODEX_CONFIG.enrichQuota!(seat, newer, translations.t));
    expect(state().rateLimitResetCreditsAvailableCount).toBe(2);
    expect(CODEX_CONFIG.canResetQuota!(state())).toBe(true);
  });

  test('manual refresh replaces native metadata and consuming the final credit removes reset', async () => {
    const get = spyOn(apiClient, 'get').mockResolvedValue({ files: [seat] });
    await authFilesApi.list();
    const post = spyOn(apiClient, 'post').mockResolvedValue(usage);
    let available = 2;
    spyOn(apiCallApi, 'request').mockImplementation(async (request) => {
      if (request.url.endsWith('/consume')) {
        available = 0;
        return { statusCode: 200, header: {}, body: { code: 'reset' }, bodyText: '' };
      }
      const body = request.url.endsWith('/wham/usage')
        ? usage
        : { available_count: available, credits: [] };
      return { statusCode: 200, header: {}, body, bodyText: '' };
    });
    const save = (data: Awaited<ReturnType<typeof CODEX_CONFIG.fetchQuota>>) =>
      useQuotaStore.getState().setCodexQuota({
        [getQuotaCacheKey(seat)]: CODEX_CONFIG.buildSuccessState(data),
      });
    const cached = await CODEX_CONFIG.fetchQuota(seat, translations.t);
    save(await CODEX_CONFIG.enrichQuota!(seat, cached, translations.t));
    expect(state().rateLimitResetCreditsAvailableCount).toBe(2);
    const nativeSequence = state().optionalMetadataSequence!;
    expect(nativeSequence).toBeGreaterThan(0);

    const manualSeat = { ...seat, codex_usage: undefined };
    get.mockResolvedValue({ files: [manualSeat] });
    await authFilesApi.list();
    post.mockImplementation(async (url) => {
      if (url === '/credentials/quota') {
        throw Object.assign(new Error('polling disabled'), { status: 404 });
      }
      return { status: 'ok', auth_index: 'fixture', models: [] };
    });
    available = 1;
    save(await CODEX_CONFIG.fetchQuota(manualSeat, translations.t));
    expect(state().rateLimitResetCreditsAvailableCount).toBe(1);
    expect(state().optionalMetadataSequence).toBeGreaterThan(nativeSequence);
    expect(CODEX_CONFIG.canResetQuota!(state())).toBe(true);

    save(await CODEX_CONFIG.resetQuota!(manualSeat, translations.t));
    expect(state().rateLimitResetCreditsAvailableCount).toBe(0);
    expect(state().rateLimitResetCredits).toEqual([]);
    expect(CODEX_CONFIG.canResetQuota!(state())).toBe(false);
  });

  test('a later explicit metadata read wins over an older enrichment', async () => {
    spyOn(apiClient, 'get').mockResolvedValue({ files: [seat] });
    await authFilesApi.list();
    spyOn(apiClient, 'post').mockResolvedValue(usage);
    const first = Promise.withResolvers<Awaited<ReturnType<typeof apiCallApi.request>>>();
    const second = Promise.withResolvers<Awaited<ReturnType<typeof apiCallApi.request>>>();
    spyOn(apiCallApi, 'request')
      .mockReturnValueOnce(first.promise)
      .mockReturnValueOnce(second.promise);
    const data = await CODEX_CONFIG.fetchQuota(seat, translations.t);
    const key = getQuotaCacheKey(seat);
    const success = CODEX_CONFIG.buildSuccessState(data);
    useQuotaStore.getState().setCodexQuota({ [key]: success });
    const adapter = CODEX_CONFIG as unknown as QuotaAdapter;
    const older = enrichQuotaInBackground(adapter, seat, data, success, translations.t);
    const newer = enrichQuotaInBackground(adapter, seat, data, success, translations.t);
    second.resolve({
      statusCode: 200,
      header: {},
      body: { available_count: 0, credits: [] },
      bodyText: '',
    });
    await newer;
    first.resolve({
      statusCode: 200,
      header: {},
      body: { available_count: 2, credits: [] },
      bodyText: '',
    });
    await older;
    expect(state().rateLimitResetCreditsAvailableCount).toBe(0);
  });

  test('a delayed list cannot undo a newer server restart snapshot', async () => {
    let resolveOld!: (value: AuthFilesResponse) => void;
    const old = new Promise<AuthFilesResponse>((resolve) => {
      resolveOld = resolve;
    });
    const get = spyOn(apiClient, 'get').mockResolvedValue({ files: [seat] });
    await authFilesApi.list();
    get.mockReturnValueOnce(old);
    const pending = authFilesApi.list();
    get.mockResolvedValue({ files: [{ ...seat, codex_usage: restarted }] });
    await authFilesApi.list();
    resolveOld({ files: [seat] });
    await pending;
    expect(state().serverCache?.polledAt).toBeNull();
    expect(state().windows).toHaveLength(2);
    expect(state().stale).toBe(true);
  });

  test('cache invalidation fences delayed list responses', async () => {
    let resolveList!: (value: AuthFilesResponse) => void;
    spyOn(apiClient, 'get').mockReturnValue(
      new Promise<AuthFilesResponse>((resolve) => {
        resolveList = resolve;
      })
    );
    const pending = authFilesApi.list();
    useQuotaStore.getState().clearQuotaCache([seat.name]);
    resolveList({ files: [seat] });
    await pending;
    expect(state()).toBeUndefined();
  });

  test('loading and failed explicit reads keep cached windows', async () => {
    spyOn(apiClient, 'get').mockResolvedValue({ files: [seat] });
    await authFilesApi.list();
    const update = (value: ReturnType<typeof CODEX_CONFIG.buildLoadingState>) =>
      useQuotaStore
        .getState()
        .setCodexQuota((prev) => ({ ...prev, [getQuotaCacheKey(seat)]: value }));
    update(CODEX_CONFIG.buildLoadingState());
    expect(state().windows).toHaveLength(2);
    update(CODEX_CONFIG.buildErrorState('offline', 503));
    expect(state().windows).toHaveLength(2);
    expect(state().stale).toBe(true);
  });
});
