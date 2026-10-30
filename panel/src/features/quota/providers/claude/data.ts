/**
 * Claude quota data: plan windows, extra usage, and independent dollar balances.
 * React-free and SCSS-free so fixture tests can consume the data layer directly.
 */

import type { TFunction } from 'i18next';
import type {
  AuthFileItem,
  ClaudeCachedUsage,
  ClaudeDollarWindow,
  ClaudeExtraUsage,
  ClaudeProfileResponse,
  ClaudeQuotaState,
  ClaudeQuotaWindow,
  ClaudeUsageWindow,
  ClaudeUsagePayload,
} from '@/types';
import { apiCallApi, getApiCallErrorMessage } from '@/services/api/apiCall';
import { apiClient } from '@/services/api/client';
import { normalizeClaudeUsageSnapshot } from '@/services/api/claudeUsage';
import {
  captureQuotaCacheGeneration,
  commitIfQuotaCacheCurrent,
  useQuotaStore,
} from '@/stores/useQuotaStore';
import { getQuotaCacheKey } from '@/utils/quota/identity';
import {
  CLAUDE_PROFILE_URL,
  CLAUDE_REQUEST_HEADERS,
  CLAUDE_USAGE_WINDOW_KEYS,
  claudePeriodHours,
  normalizeNumberValue,
  normalizeStringValue,
  formatQuotaResetTime,
  resolveResetMs,
  createStatusError,
  isClaudeFile,
  isDisabledAuthFile,
} from '@/utils/quota';
import { normalizeAuthIndex } from '@/utils/authIndex';
import type { QuotaProviderData } from '../types';

export type ClaudeQuotaData = {
  windows: ClaudeQuotaWindow[];
  extraUsage?: ClaudeExtraUsage | null;
  planType?: string | null;
  dollarWindows: ClaudeDollarWindow[];
  observedAt: string | null;
  stale: boolean;
  serverCache: NonNullable<ClaudeQuotaState['serverCache']>;
};

export const hasClaudeUsageData = (quota?: ClaudeQuotaState): boolean =>
  Boolean(
    quota?.serverCache ||
    quota?.observedAt ||
    quota?.windows?.length ||
    quota?.extraUsage ||
    quota?.dollarWindows?.length
  );

const findFableUsageLimit = (payload: ClaudeUsagePayload) => {
  if (!Array.isArray(payload.limits)) return null;

  const candidates = payload.limits.filter((limit) => {
    const kind = (normalizeStringValue(limit?.kind) ?? '').trim().toLowerCase();
    const modelName = (normalizeStringValue(limit?.scope?.model?.display_name) ?? '')
      .trim()
      .toLowerCase();
    const isFable = modelName === 'fable' || modelName === 'fable 5';
    return kind === 'weekly_scoped' && isFable && normalizeNumberValue(limit?.percent) !== null;
  });

  return candidates.find((limit) => limit.is_active === true) ?? candidates[0] ?? null;
};

const isDollarDenominatedWindow = (window: ClaudeUsageWindow) =>
  normalizeNumberValue(window.limit_dollars) !== null ||
  normalizeNumberValue(window.used_dollars) !== null ||
  normalizeNumberValue(window.remaining_dollars) !== null;

export const buildClaudeQuotaWindows = (
  payload: ClaudeUsagePayload,
  t: TFunction
): ClaudeQuotaWindow[] => {
  const windows: ClaudeQuotaWindow[] = [];
  const fableLimit = findFableUsageLimit(payload);

  for (const { key, id, labelKey } of CLAUDE_USAGE_WINDOW_KEYS) {
    const window = payload[key as keyof ClaudeUsagePayload];
    if (!window || typeof window !== 'object' || !('utilization' in window)) continue;
    const typedWindow = window as ClaudeUsageWindow;
    const isCreditPool = key === 'iguana_necktie' && isDollarDenominatedWindow(typedWindow);
    if (key === 'iguana_necktie' && fableLimit && !isCreditPool) continue;
    if (isCreditPool) {
      windows.push({
        id: 'cloud-session-credits',
        label: t('claude_quota.cloud_session_credits'),
        labelKey: 'claude_quota.cloud_session_credits',
        usedPercent: normalizeNumberValue(typedWindow.utilization),
        resetLabel: formatQuotaResetTime(typedWindow.resets_at ?? undefined),
        resetAtMs: resolveResetMs([typedWindow.resets_at]),
        periodHours: null,
      });
      continue;
    }
    const usedPercent = normalizeNumberValue(typedWindow.utilization);
    const resetLabel = formatQuotaResetTime(typedWindow.resets_at ?? undefined);
    windows.push({
      id,
      label: t(labelKey),
      labelKey,
      usedPercent,
      resetLabel,
      // Claude states the period nowhere in the payload, so it comes from the
      // key: `five_hour` is the rolling window, everything else is weekly.
      resetAtMs: resolveResetMs([typedWindow.resets_at]),
      periodHours: claudePeriodHours(key),
    });
  }

  if (fableLimit) {
    const usedPercent = normalizeNumberValue(fableLimit.percent);
    if (usedPercent !== null) {
      windows.push({
        id: 'seven-day-fable',
        label: t('claude_quota.seven_day_fable'),
        labelKey: 'claude_quota.seven_day_fable',
        usedPercent,
        resetLabel: formatQuotaResetTime(fableLimit.resets_at ?? undefined),
        // `weekly_scoped` is a 7-day window by definition, so the timeline can
        // place this row alongside the ones derived from the named keys.
        resetAtMs: resolveResetMs([fableLimit.resets_at]),
        periodHours: claudePeriodHours('seven_day'),
      });
    }
  }

  return windows;
};

const normalizeFlagValue = (value: unknown): boolean | undefined => {
  if (value === undefined || value === null) return undefined;
  if (typeof value === 'boolean') return value;
  if (typeof value === 'number') return value !== 0;
  if (typeof value === 'string') {
    const trimmed = value.trim().toLowerCase();
    if (['true', '1', 'yes', 'y', 'on'].includes(trimmed)) return true;
    if (['false', '0', 'no', 'n', 'off'].includes(trimmed)) return false;
  }
  return undefined;
};

const parseClaudeProfilePayload = (payload: unknown): ClaudeProfileResponse | null => {
  if (payload === undefined || payload === null) return null;
  if (typeof payload === 'string') {
    const trimmed = payload.trim();
    if (!trimmed) return null;
    try {
      return JSON.parse(trimmed) as ClaudeProfileResponse;
    } catch {
      return null;
    }
  }
  if (typeof payload === 'object') {
    return payload as ClaudeProfileResponse;
  }
  return null;
};

export const resolveClaudePlanType = (profile: ClaudeProfileResponse | null): string | null => {
  if (!profile) return null;

  const organizationType = normalizeStringValue(
    profile.organization?.organization_type
  )?.toLowerCase();
  const subscriptionStatus = normalizeStringValue(
    profile.organization?.subscription_status
  )?.toLowerCase();

  if (organizationType === 'claude_team' && subscriptionStatus === 'active') {
    return 'plan_team';
  }

  // Account flags include personal subscriptions even for a Team-scoped token.
  const hasClaudeMax = normalizeFlagValue(profile.account?.has_claude_max);
  if (hasClaudeMax) return 'plan_max';

  const hasClaudePro = normalizeFlagValue(profile.account?.has_claude_pro);
  if (hasClaudePro) return 'plan_pro';

  if (hasClaudeMax === false && hasClaudePro === false) return 'plan_free';

  return null;
};

export const buildCachedClaudeQuota = (
  payload: ClaudeCachedUsage,
  t: TFunction,
  receivedAtMs = Date.now()
): ClaudeQuotaData => {
  if (
    !payload ||
    !['ok', 'not_polled_yet', 'polling_paused'].includes(payload.status) ||
    typeof payload.stale !== 'boolean' ||
    (payload.polled_at !== null &&
      (typeof payload.polled_at !== 'string' || !Number.isFinite(Date.parse(payload.polled_at))))
  ) {
    throw new Error(t('claude_quota.empty_windows'));
  }
  const snapshot = normalizeClaudeUsageSnapshot(payload);
  if (payload.polled_at && !snapshot) {
    throw new Error(t('claude_quota.empty_windows'));
  }
  const dollarWindows = snapshot?.dollarWindows ?? [];
  return {
    windows: buildClaudeQuotaWindows(payload, t).filter(
      (window) =>
        window.id !== 'cloud-session-credits' ||
        !dollarWindows.some((balance) => balance.key === 'iguana_necktie')
    ),
    extraUsage: snapshot?.extraUsage,
    dollarWindows,
    observedAt: payload.polled_at,
    stale: payload.stale,
    serverCache: {
      status: payload.status,
      ageSeconds: payload.age_seconds,
      receivedAtMs,
      nextPollAt: payload.next_poll_at,
      refreshing: payload.refreshing,
      lastStatus: payload.last_status,
      lastPollAt: payload.last_poll_at,
    },
  };
};

/** Commit each seat independently so a mutation of one file cannot poison another. */
export const seedClaudeQuota = (
  files: AuthFileItem[],
  generation: ReturnType<typeof captureQuotaCacheGeneration>,
  t: TFunction
): void => {
  for (const file of files) {
    if (!isClaudeFile(file) || !file.claude_usage) continue;
    let data: ClaudeQuotaData;
    try {
      data = buildCachedClaudeQuota(file.claude_usage, t);
    } catch {
      // A malformed list entry must not erase a previously useful quota snapshot.
      continue;
    }
    commitIfQuotaCacheCurrent(
      generation,
      () => {
        useQuotaStore.getState().setClaudeQuota((previous) => ({
          ...previous,
          [getQuotaCacheKey(file)]: CLAUDE_CONFIG.buildSuccessState({
            ...data,
            planType: previous[getQuotaCacheKey(file)]?.planType,
          }),
        }));
      },
      file.name
    );
  }
};

const usageReads = new Map<string, Promise<ClaudeCachedUsage>>();

const fetchCachedClaudeQuota = async (
  file: AuthFileItem,
  t: TFunction,
  refresh: boolean
): Promise<ClaudeQuotaData> => {
  const rawAuthIndex = file['auth_index'] ?? file.authIndex;
  const authIndex = normalizeAuthIndex(rawAuthIndex);
  if (!authIndex) {
    throw new Error(t('claude_quota.missing_auth_index'));
  }

  const generation = captureQuotaCacheGeneration(file.name);
  const planType = useQuotaStore.getState().claudeQuota[getQuotaCacheKey(file)]?.planType;
  const key = JSON.stringify([
    apiClient.getConnectionRevision(),
    generation.cacheGeneration,
    generation.fileGenerations[file.name] ?? 0,
    getQuotaCacheKey(file),
    authIndex,
    refresh,
  ]);
  let request = usageReads.get(key);
  if (!request) {
    request = refresh
      ? apiClient.post<ClaudeCachedUsage>('/credentials/quota', { auth_index: authIndex })
      : apiClient.get<ClaudeCachedUsage>('/credentials/quota', {
          params: { auth_index: authIndex },
        });
    usageReads.set(key, request);
  }
  try {
    return { ...buildCachedClaudeQuota(await request, t), planType };
  } finally {
    if (usageReads.get(key) === request) usageReads.delete(key);
  }
};

export const readClaudeQuota = (file: AuthFileItem, t: TFunction) =>
  fetchCachedClaudeQuota(file, t, false);

/** Profile metadata is optional enrichment of an explicit refresh, never a list/render read. */
const enrichClaudeQuota = async (
  file: AuthFileItem,
  data: ClaudeQuotaData
): Promise<ClaudeQuotaData> => {
  const authIndex = normalizeAuthIndex(file['auth_index'] ?? file.authIndex);
  if (!authIndex) return data;
  const result = await apiCallApi.request({
    authIndex,
    method: 'GET',
    url: CLAUDE_PROFILE_URL,
    header: { ...CLAUDE_REQUEST_HEADERS },
  });
  if (result.statusCode < 200 || result.statusCode >= 300) {
    throw createStatusError(getApiCallErrorMessage(result), result.statusCode);
  }
  const planType = resolveClaudePlanType(parseClaudeProfilePayload(result.body ?? result.bodyText));
  return planType ? { ...data, planType } : data;
};

export const CLAUDE_CONFIG: QuotaProviderData<ClaudeQuotaState, ClaudeQuotaData> = {
  type: 'claude',
  i18nPrefix: 'claude_quota',
  filterFn: (file) => isClaudeFile(file) && !isDisabledAuthFile(file),
  fetchQuota: (file, t) => fetchCachedClaudeQuota(file, t, true),
  enrichQuota: enrichClaudeQuota,
  storeSelector: (state) => state.claudeQuota,
  storeSetter: 'setClaudeQuota',
  buildLoadingState: () => ({ status: 'loading', windows: [] }),
  buildSuccessState: (data) => ({
    status: 'success',
    windows: data.windows,
    extraUsage: data.extraUsage,
    planType: data.planType,
    dollarWindows: data.dollarWindows,
    observedAt: data.observedAt,
    stale: data.stale,
    serverCache: data.serverCache,
  }),
  buildErrorState: (message, status) => ({
    status: 'error',
    windows: [],
    error: message,
    errorStatus: status,
  }),
};
