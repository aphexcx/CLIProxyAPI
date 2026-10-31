import { useTranslation } from 'react-i18next';
import type { ClaudeQuotaState } from '@/types';
import { useNow } from '@/hooks/useNow';
import { useQuotaCacheReads } from '../hooks/useQuotaCacheReads';

/** Server poll diagnostics; rendering this component never starts a request. */
export function QuotaCacheStatus({
  quota,
  className,
}: {
  quota: Pick<ClaudeQuotaState, 'serverCache' | 'observedAt' | 'stale'>;
  className: string;
}) {
  const { t, i18n } = useTranslation();
  const now = useNow();
  const cache = quota.serverCache;
  useQuotaCacheReads(Boolean(cache));
  if (!cache) return null;
  const age =
    cache.ageSeconds == null
      ? null
      : Math.max(0, Math.floor(cache.ageSeconds + Math.max(0, now - cache.receivedAtMs) / 1000));
  const firstPollRunning = !cache.polledAt && cache.refreshing && cache.lastStatus === 0;
  const failed =
    cache.lastPollAt && !firstPollRunning && (cache.lastStatus < 200 || cache.lastStatus >= 300);
  return (
    <div role="status" className={className}>
      {quota.observedAt && (
        <div>
          <time dateTime={quota.observedAt}>
            {new Date(quota.observedAt).toLocaleString(i18n.resolvedLanguage)}
          </time>
          {age !== null && ` · ${t('claude_quota.cache_age', { seconds: age })}`}
        </div>
      )}
      {!cache.polledAt && <div>{t('claude_quota.cache_pending')}</div>}
      {cache.status === 'polling_paused' && <div>{t('claude_quota.cache_paused')}</div>}
      {cache.refreshing && <div>{t('claude_quota.cache_queued')}</div>}
      {cache.nextPollAt && (
        <div>
          {t('claude_quota.cache_next', {
            time: new Date(cache.nextPollAt).toLocaleString(i18n.resolvedLanguage),
          })}
        </div>
      )}
      {failed && (
        <div>
          {t('claude_quota.cache_failed', {
            status: cache.lastStatus || t('claude_quota.cache_no_response'),
          })}
        </div>
      )}
      {quota.stale && quota.observedAt && <div>{t('claude_quota.cache_stale')}</div>}
    </div>
  );
}
