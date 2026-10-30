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
