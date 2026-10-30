import { afterEach, beforeEach, describe, expect, spyOn, test } from 'bun:test';
import { readFileSync } from 'node:fs';
import { createElement } from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { I18nextProvider } from 'react-i18next';
import i18n from '@/i18n';
import { normalizeClaudeUsageSnapshot } from '@/services/api/claudeUsage';
import { apiCallApi, getApiCallErrorMessage, type ApiCallResult } from '@/services/api/apiCall';
import { apiClient } from '@/services/api/client';
import {
  CLAUDE_CONFIG,
  hasClaudeUsageData,
  readClaudeQuota,
} from '@/features/quota/providers/claude/data';
import { ClaudeQuotaBody } from '@/features/quota/providers/claude/ClaudeQuotaBody';
import { buildTimelineLane } from '@/features/quota/quotaTimelineModel';
import { nextRecoveryMs } from '@/features/quota/resetSchedule';
import { QUOTA_CLASS_KEYS, bindQuotaClasses } from '@/features/quota/types';
import {
  captureQuotaCacheGeneration,
  commitIfQuotaCacheCurrent,
  useQuotaStore,
} from '@/stores/useQuotaStore';
import {
  CLAUDE_PROFILE_URL,
  CLAUDE_USAGE_URL,
  formatQuotaResetTime,
  getStatusFromError,
} from '@/utils/quota';
import { getQuotaCacheKey } from '@/utils/quota/identity';
import type { ClaudeQuotaState } from '@/types';
import fixture from './fixtures/claude-oauth-usage.json';

const translations = i18n.cloneInstance({ lng: 'en' });
const observedAt = '2026-10-10T00:00:00Z';
const file = { name: 'claude-fixture.json', type: 'claude', authIndex: 'synthetic-index' };
const classes = bindQuotaClasses(
  Object.fromEntries(QUOTA_CLASS_KEYS.map((key) => [key, key])),
  'claude-usage-test'
);
const result = (statusCode: number, body: unknown): ApiCallResult => ({
  statusCode,
  header: {},
  body,
  bodyText: JSON.stringify(body),
});
const snapshot = normalizeClaudeUsageSnapshot(fixture)!;
const quota: ClaudeQuotaState = {
  status: 'success',
  windows: [],
  extraUsage: snapshot.extraUsage,
  dollarWindows: snapshot.dollarWindows,
  observedAt,
};
const render = (state: ClaudeQuotaState) =>
  renderToStaticMarkup(
    createElement(
      I18nextProvider,
      { i18n: translations },
      createElement(ClaudeQuotaBody, { quota: state, classes })
    )
  );

describe('Claude OAuth usage API normalization', () => {
  test('preserves every extra-usage field and discovers independent dollar balances', () => {
    const normalized = normalizeClaudeUsageSnapshot(fixture)!;
    expect(normalized.fiveHour).toEqual({
      utilization: 42.5,
      resetsAt: fixture.five_hour.resets_at,
    });
    expect(normalized.sevenDay?.utilization).toBe(16);
    expect(normalized.extraUsage).toEqual({
      isEnabled: true,
      monthlyLimit: 5000,
      usedCredits: 1250,
      utilization: 25,
      currency: 'USD',
      disabledReason: null,
      userDisabled: false,
      spendLimitReached: false,
    });
    expect(normalized.dollarWindows.map((balance) => balance.key)).toEqual([
      'iguana_necktie',
      'additional_credit_balance',
    ]);
    expect(normalized.dollarWindows[0]).toMatchObject({
      limitDollars: 100,
      usedDollars: 5.043595,
      remainingDollars: 94.956405,
      resetsAt: '2026-11-01T00:00:00Z',
    });
    expect(normalized.dollarWindows[1]).toMatchObject({
      usedDollars: 0,
      remainingDollars: 20,
      utilization: 0,
      resetsAt: null,
    });
    expect(normalized).not.toHaveProperty('observedAt');
  });

  test('retains null, false, and zero without fabricating values', () => {
    const normalized = normalizeClaudeUsageSnapshot({
      five_hour: { utilization: null, resets_at: null },
      seven_day: null,
      extra_usage: {
        is_enabled: false,
        monthly_limit: 0,
        used_credits: 0,
        utilization: null,
        currency: 'EUR',
        disabled_reason: 'user_disabled',
        user_disabled: true,
        spend_limit_reached: false,
      },
      future_upstream_key: {
        limit_dollars: null,
        used_dollars: 0,
        remaining_dollars: null,
        utilization: null,
        resets_at: null,
      },
    })!;
    expect(normalized.fiveHour).toEqual({ utilization: null, resetsAt: null });
    expect(normalized.sevenDay).toBeNull();
    expect(normalized.extraUsage).toMatchObject({
      isEnabled: false,
      monthlyLimit: 0,
      usedCredits: 0,
      currency: 'EUR',
      disabledReason: 'user_disabled',
      userDisabled: true,
      spendLimitReached: false,
    });
    expect(normalized.dollarWindows[0]).toEqual({
      key: 'future_upstream_key',
      limitDollars: null,
      usedDollars: 0,
      remainingDollars: null,
      utilization: null,
      resetsAt: null,
    });
  });

  test('ignores incomplete balances and unrelated payloads', () => {
    expect(normalizeClaudeUsageSnapshot({ error: 'failed' })).toBeNull();
    expect(normalizeClaudeUsageSnapshot(null)).toBeNull();
    expect(normalizeClaudeUsageSnapshot([])).toBeNull();
    expect(
      normalizeClaudeUsageSnapshot({ five_hour: null, partial: { used_dollars: 5 } })
    ).toMatchObject({ fiveHour: null, dollarWindows: [] });
  });

  test('keeps every complete top-level dollar window even when its key is dollar_windows', () => {
    const normalized = normalizeClaudeUsageSnapshot({
      ...fixture,
      dollar_windows: { ...fixture.additional_credit_balance, remaining_dollars: 15 },
    })!;
    expect(normalized.dollarWindows.map((balance) => balance.key)).toEqual([
      'iguana_necktie',
      'additional_credit_balance',
      'dollar_windows',
    ]);
    expect(normalized.dollarWindows[2].remainingDollars).toBe(15);
  });

  test('uses only the existing authenticated raw API-call response contract', async () => {
    const body = { error: { message: 'usage unavailable' } };
    const post = spyOn(apiClient, 'post').mockResolvedValue({
      status_code: 429,
      header: {},
      body: JSON.stringify(body),
    });
    try {
      const request = { authIndex: file.authIndex, method: 'GET', url: CLAUDE_USAGE_URL };
      const response = await apiCallApi.request(request);
      expect(post).toHaveBeenCalledWith('/requests/api-call', request, undefined);
      expect(response).toEqual(result(429, body));
      expect(getApiCallErrorMessage(response)).toBe('429 usage unavailable');
    } finally {
      post.mockRestore();
    }
  });
});

describe('Claude quota usage reads', () => {
  const cached = {
    ...fixture,
    status: 'ok' as const,
    stale: false,
    polled_at: observedAt,
    age_seconds: 12,
    next_poll_at: '2026-10-10T00:01:00Z',
    refreshing: false,
    last_status: 200,
    last_poll_at: observedAt,
  };
  const originalGet = apiClient.get;
  const originalPost = apiClient.post;
  const originalRequest = apiCallApi.request;
  afterEach(() => {
    apiClient.get = originalGet;
    apiClient.post = originalPost;
    apiCallApi.request = originalRequest;
    useQuotaStore.getState().clearQuotaCache();
  });

  test('reads the native cache without an API-call and preserves the server timestamp', async () => {
    const get = spyOn(apiClient, 'get').mockResolvedValue(cached);
    const apiCall = spyOn(apiCallApi, 'request');
    const data = await readClaudeQuota(file, translations.t);
    expect(get).toHaveBeenCalledWith('/credentials/quota', {
      params: { auth_index: file.authIndex },
    });
    expect(apiCall).not.toHaveBeenCalled();
    expect(data.windows.map((window) => window.id)).toEqual([
      'five-hour',
      'seven-day',
      'seven-day-sonnet',
    ]);
    expect(data.dollarWindows).toEqual(snapshot.dollarWindows);
    expect(data.extraUsage?.usedCredits).toBe(1250);
    expect(data.observedAt).toBe(observedAt);
    expect(data.planType).toBeUndefined();
    expect(CLAUDE_CONFIG.buildSuccessState(data)).toMatchObject({
      status: 'success',
      stale: false,
    });
  });

  test('explicit refresh queues a POST, with profile metadata fetched separately', async () => {
    const post = spyOn(apiClient, 'post').mockResolvedValue({ ...cached, refreshing: true });
    const profile = spyOn(apiCallApi, 'request').mockResolvedValue(
      result(200, { account: { has_claude_max: true } })
    );
    const data = await CLAUDE_CONFIG.fetchQuota(file, translations.t);
    expect(post).toHaveBeenCalledWith('/credentials/quota', { auth_index: file.authIndex });
    expect(profile).not.toHaveBeenCalled();
    expect(data.serverCache.refreshing).toBe(true);
    expect(data.observedAt).toBe(observedAt);
    const enriched = await CLAUDE_CONFIG.enrichQuota!(file, data, translations.t);
    expect(enriched.planType).toBe('plan_max');
    expect(enriched.observedAt).toBe(observedAt);
    expect(profile).toHaveBeenCalledWith(expect.objectContaining({ url: CLAUDE_PROFILE_URL }));
  });

  test.each(['read', 'refresh'] as const)(
    'coalesces concurrent %s requests for a seat',
    async (mode) => {
      const get = spyOn(apiClient, 'get').mockResolvedValue(cached);
      const post = spyOn(apiClient, 'post').mockResolvedValue(cached);
      const fetch = mode === 'read' ? readClaudeQuota : CLAUDE_CONFIG.fetchQuota;
      const results = await Promise.all(
        Array.from({ length: 8 }, () => fetch(file, translations.t))
      );
      expect(results).toHaveLength(8);
      expect(mode === 'read' ? get : post).toHaveBeenCalledTimes(1);
      await fetch(file, translations.t);
      expect(mode === 'read' ? get : post).toHaveBeenCalledTimes(2);
    }
  );

  test('does not coalesce a queued refresh into an ordinary read', async () => {
    const get = spyOn(apiClient, 'get').mockResolvedValue(cached);
    const post = spyOn(apiClient, 'post').mockResolvedValue({ ...cached, refreshing: true });
    const [read, refresh] = await Promise.all([
      readClaudeQuota(file, translations.t),
      CLAUDE_CONFIG.fetchQuota(file, translations.t),
    ]);
    expect(get).toHaveBeenCalledTimes(1);
    expect(post).toHaveBeenCalledTimes(1);
    expect(read.serverCache.refreshing).toBe(false);
    expect(refresh.serverCache.refreshing).toBe(true);
  });

  test('preserves partial credit pools and their recovery instants', async () => {
    for (const window of [
      { utilization: 41, resets_at: fixture.seven_day.resets_at },
      { utilization: 5, remaining_dollars: 95, resets_at: fixture.seven_day.resets_at },
    ]) {
      apiClient.get = async <T>() =>
        ({
          ...cached,
          five_hour: null,
          seven_day: null,
          seven_day_sonnet: null,
          additional_credit_balance: null,
          iguana_necktie: window,
        }) as T;
      const data = await readClaudeQuota(file, translations.t);
      expect(data.windows).toHaveLength(1);
      expect(data.windows[0].id).toBe(
        'remaining_dollars' in window ? 'cloud-session-credits' : 'seven-day-fable'
      );
      expect(data.dollarWindows).toEqual([]);
    }
  });

  test('rejects malformed responses rather than erasing last-good values', async () => {
    for (const body of [null, '', 'not-json', [], { error: 'no usage' }, fixture]) {
      apiClient.get = async <T>() => body as T;
      await expect(readClaudeQuota(file, translations.t)).rejects.toThrow(
        translations.t('claude_quota.empty_windows')
      );
    }
  });

  test('retains last-good values after a management failure and replaces them on recovery', async () => {
    apiClient.get = async <T>() => cached as T;
    const store = useQuotaStore.getState();
    const key = getQuotaCacheKey(file);
    const good = CLAUDE_CONFIG.buildSuccessState(await readClaudeQuota(file, translations.t));
    store.setClaudeQuota({ [key]: good });
    store.setClaudeQuota({ [key]: CLAUDE_CONFIG.buildLoadingState() });
    apiClient.get = async () => {
      throw Object.assign(new Error('request failed'), { status: 502 });
    };
    try {
      await readClaudeQuota(file, translations.t);
      throw new Error('Expected rejection');
    } catch (error) {
      store.setClaudeQuota({
        [key]: CLAUDE_CONFIG.buildErrorState((error as Error).message, getStatusFromError(error)),
      });
    }
    const retained = useQuotaStore.getState().claudeQuota[key];
    expect(retained).toMatchObject({ status: 'error', errorStatus: 502, stale: true, observedAt });
    expect(retained.windows).toBe(good.windows);
    expect(retained.serverCache).toBe(good.serverCache);
    apiClient.get = async <T>() =>
      ({ ...cached, polled_at: '2026-10-10T00:01:00Z', last_poll_at: '2026-10-10T00:01:00Z' }) as T;
    const recovered = CLAUDE_CONFIG.buildSuccessState(await readClaudeQuota(file, translations.t));
    store.setClaudeQuota({ [key]: recovered });
    expect(useQuotaStore.getState().claudeQuota[key]).toBe(recovered);
    expect(recovered).toMatchObject({ status: 'success', stale: false });
  });

  test('shares the cached snapshot with recovery sorting and the timeline', async () => {
    apiClient.get = async <T>() => cached as T;
    const state = CLAUDE_CONFIG.buildSuccessState(await readClaudeQuota(file, translations.t));
    useQuotaStore.getState().setClaudeQuota({ [getQuotaCacheKey(file)]: state });
    expect(nextRecoveryMs('claude', state, Date.parse(observedAt))).toBe(
      Date.parse('2026-10-15T09:00:00Z')
    );
    expect(
      buildTimelineLane({
        provider: 'claude',
        name: file.name,
        displayName: file.name,
        quota: state,
        maxPeriodHours: 14 * 24,
      }).anchorMs
    ).toBe(Date.parse('2026-10-15T09:00:00Z'));
  });
});

describe('Claude usage snapshot rendering and retention', () => {
  beforeEach(() => {
    useQuotaStore.getState().clearQuotaCache();
  });

  test('offers read-only refresh for every non-idle auth-file quota, including Claude', () => {
    // This host imports SCSS modules, so cover the action wiring here and
    // verify its interaction with the production build in the browser.
    const source = readFileSync(
      new URL('../src/features/authFiles/components/AuthFileQuotaSection.tsx', import.meta.url),
      'utf8'
    );
    // Idle quota is refreshed via the clickable idle prompt; avoid a duplicate button.
    expect(source).toContain("const showRefreshQuotaAction = quotaStatus !== 'idle';");
    expect(source).toContain('{showRefreshQuotaAction && (');
    expect(source).toContain('<div className={styles.quotaCardActions}>');
    expect(source).toContain('onClick={() => void refreshQuotaForFile()}');
    expect(source).toContain("disabled={!canRefreshQuota || quotaStatus === 'loading'}");
    expect(source).toContain("loading={quotaStatus === 'loading'}");
    expect(source).toContain("{t('auth_files.quota_refresh_single')}");
    expect(source).toContain("if (quota?.status === 'loading' || resettingQuota) return;");
    expect(source).toContain(
      'const canRefreshQuota = !disableControls && !file.disabled && !resettingQuota;'
    );
    expect(source).toContain('{resetQuotaAction}');
    expect(CLAUDE_CONFIG).not.toHaveProperty('resetQuota');
  });

  test('renders enabled extra usage in minor units and each balance in dollars', () => {
    const markup = render(quota);
    expect(markup).toContain('Extra Usage');
    expect(markup).toContain('Enabled');
    expect(markup).toContain('Spent $12.50 / Limit $50.00');
    expect(markup).toContain('25% used');
    expect(markup).toContain('iguana_necktie');
    expect(markup).toContain('$94.96 remaining');
    expect(markup).toContain('Spent $5.04 / Limit $100.00');
    expect(markup).toContain(formatQuotaResetTime(fixture.iguana_necktie.resets_at));
    expect(markup).toContain('additional_credit_balance');
    expect(markup).toContain('$20.00 remaining');
    expect(markup).toContain('Spent $0.00 / Limit $20.00');
    expect(markup).toContain('Not provided');
    expect(markup).not.toContain('No quota data available');
  });

  test('extra usage and balances share the window-row typography', () => {
    const markup = render({ ...quota, planType: 'plan_max' });
    // Same anatomy as plan windows: label + mono telemetry header, never bare reset text.
    expect(markup).toContain(
      '<div class="quotaRowHeader"><span class="quotaModel">Extra Usage</span><div class="quotaMeta"><span class="quotaPercent">Enabled</span><span class="quotaResetRelative">25% used</span>'
    );
    expect(markup).toContain(
      '<span class="quotaModel" title="iguana_necktie">iguana_necktie</span><div class="quotaMeta"><span class="quotaPercent">$94.96 remaining</span>'
    );
    expect(markup).toContain('<div class="quotaMeta"><span class="quotaReset">Resets</span>');
    expect(markup).not.toMatch(/<div class="quotaRow"><span class="quotaReset">/);
    expect(markup).not.toContain('codexResetCreditLabel');
    expect(markup).toContain(
      '<span class="codexPlanItem"><span class="codexPlanLabel">Plan</span>'
    );
  });

  test('host chips join the plan row and host content follows it', () => {
    const markup = renderToStaticMarkup(
      createElement(
        I18nextProvider,
        { i18n: translations },
        createElement(ClaudeQuotaBody, {
          quota: { ...quota, planType: 'plan_max' },
          classes,
          planItems: createElement('span', { 'data-slot': 'chip' }),
          afterPlan: createElement('div', { 'data-slot': 'after' }),
        })
      )
    );
    const plan = markup.indexOf('class="codexPlan"');
    const chip = markup.indexOf('data-slot="chip"');
    const after = markup.indexOf('data-slot="after"');
    const extra = markup.indexOf('Extra Usage');
    expect(plan).toBeGreaterThanOrEqual(0);
    expect(chip).toBeGreaterThan(plan);
    expect(after).toBeGreaterThan(chip);
    expect(extra).toBeGreaterThan(after);
    // Without a plan type the host chips still get a plan row.
    const chipOnly = renderToStaticMarkup(
      createElement(
        I18nextProvider,
        { i18n: translations },
        createElement(ClaudeQuotaBody, {
          quota,
          classes,
          planItems: createElement('span', { 'data-slot': 'chip' }),
        })
      )
    );
    expect(chipOnly).toContain('<div class="codexPlan"><span data-slot="chip"></span></div>');
  });

  test('renders disabled extra usage, reasons, and zero amounts', () => {
    const markup = render({
      ...quota,
      extraUsage: {
        ...snapshot.extraUsage!,
        isEnabled: false,
        usedCredits: 0,
        monthlyLimit: 0,
        utilization: 0,
        userDisabled: true,
        spendLimitReached: true,
        disabledReason: 'user_disabled',
      },
    });
    expect(markup).toContain('Disabled');
    expect(markup).toContain('Spent $0.00 / Limit $0.00');
    expect(markup).toContain('Disabled by user');
    expect(markup).toContain('Spending limit reached');
    expect(markup).toContain('Reason: user_disabled');
  });

  test('shows the last observation time when a retained snapshot is stale', () => {
    const markup = render({ ...quota, status: 'error', stale: true });
    expect(markup).toContain('role="status"');
    expect(markup).toContain('Showing last successful check:');
    expect(markup).toContain(new Date(observedAt).toLocaleString('en'));
    expect(markup).toContain('$94.96 remaining');
  });

  test('keeps the last good data during loading and errors, then recovers', () => {
    const store = useQuotaStore.getState();
    store.setClaudeQuota({ [file.name]: quota });
    store.setClaudeQuota({ [file.name]: CLAUDE_CONFIG.buildLoadingState() });
    let retained = useQuotaStore.getState().claudeQuota[file.name];
    expect(retained.status).toBe('loading');
    expect(retained.extraUsage).toEqual(quota.extraUsage);
    expect(retained.dollarWindows).toEqual(quota.dollarWindows);
    store.setClaudeQuota({ [file.name]: CLAUDE_CONFIG.buildErrorState('network failed', 502) });
    retained = useQuotaStore.getState().claudeQuota[file.name];
    expect(retained.status).toBe('error');
    expect(retained.stale).toBe(true);
    expect(retained.observedAt).toBe(observedAt);
    expect(retained.dollarWindows).toEqual(quota.dollarWindows);
    const recovered = { ...quota, observedAt: '2026-10-10T01:00:00Z', stale: false };
    store.setClaudeQuota({ [file.name]: recovered });
    expect(useQuotaStore.getState().claudeQuota[file.name]).toEqual(recovered);
  });

  test('does not retain balances across file replacement or session invalidation', () => {
    const store = useQuotaStore.getState();
    store.setClaudeQuota({ [file.name]: quota });
    const pending = captureQuotaCacheGeneration(file.name);
    store.clearQuotaCache([file.name]);
    expect(
      commitIfQuotaCacheCurrent(pending, () => store.setClaudeQuota({ [file.name]: quota }))
    ).toBe(false);
    store.setClaudeQuota({ [file.name]: CLAUDE_CONFIG.buildErrorState('new account read failed') });
    expect(hasClaudeUsageData(useQuotaStore.getState().claudeQuota[file.name])).toBe(false);
    store.setClaudeQuota({ [file.name]: quota });
    const previousSession = captureQuotaCacheGeneration(file.name);
    store.clearQuotaCache();
    expect(
      commitIfQuotaCacheCurrent(previousSession, () => store.setClaudeQuota({ [file.name]: quota }))
    ).toBe(false);
    expect(useQuotaStore.getState().claudeQuota).toEqual({});
  });
});
