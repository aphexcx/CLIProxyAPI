import { useEffect, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { claudeLimitResetAge, type ClaudeLimitResetSeat } from '@/services/api/claudeLimitResets';
import { ClaudeResetKindsView } from './ClaudeResetKinds';
import { useClaudeLimitResetRead } from './useClaudeLimitResetRead';

/** Observations for the holder's own click, then what is left of each kind; spend controls live in Details. */
export function ClaudeLimitResets({ name, generation }: { name: string; generation?: string }) {
  const read = useClaudeLimitResetRead(name, generation);
  return (
    <>
      <ClaudeLimitResetsView seat={read.seat} rule={read.rule} failed={read.failed} />
      <ClaudeResetKindsView seat={read.seat} failed={read.failed} loaded={read.loaded} />
    </>
  );
}

export function ClaudeLimitResetsView({
  seat,
  rule,
  failed,
}: {
  seat?: ClaudeLimitResetSeat;
  rule?: string;
  failed?: boolean;
}) {
  const { t } = useTranslation();
  const [now, setNow] = useState(() => performance.now());
  useEffect(() => {
    const timer = setInterval(() => setNow(performance.now()), 1_000);
    return () => clearInterval(timer);
  }, []);
  const age = claudeLimitResetAge(seat, now);
  const grants = seat?.cedar_ember?.grants ?? [];
  const ineligibleReason = seat?.cedar_ember?.eligible === false
    ? seat.cedar_ember.ineligible_reason
    : null;
  const reasons: Record<string, string> = {
    'holder action only': 'holder',
    'seat ineligible': 'ineligible',
    'seat disabled': 'disabled',
    'reset cooldown active': 'cooldown',
    'weekly reset unknown': 'weekly_unknown',
    'weekly reset within 24h': 'weekly_soon',
    'Fable usage unknown': 'usage_unknown',
    'below 90% Fable usage': 'below_cap',
    'no usable unexpired grant': 'no_grant',
    'reset status unavailable or stale': 'unavailable',
  };
  return (
    <div
      aria-label={t('claude_limit_resets.label')}
      title={rule ? t('claude_limit_resets.ranking_rule') : undefined}
    >
      <strong>
        {t('claude_limit_resets.count', {
          value: seat?.cedar_ember?.eligible
            ? grants.reduce((sum, grant) => sum + grant.resets_left, 0)
            : '—',
        })}
      </strong>
      {seat?.cedar_ember?.eligible && seat.best_click_rank != null && !seat.stale && !failed && (
        <span> · {t('claude_limit_resets.rank', { rank: seat.best_click_rank })}</span>
      )}
      <div>
        {failed
          ? t('claude_limit_resets.unavailable')
          : seat?.error ||
            t(`claude_limit_resets.${reasons[seat?.rank_reason ?? ''] ?? 'awaiting'}`)}
      </div>
      {ineligibleReason && (
        <div>{ineligibleReason === 'surface'
          ? t('claude_limit_resets.surface')
          : t('claude_limit_resets.ineligible_reason', { reason: ineligibleReason })}</div>
      )}
      {age != null && (
        <small>
          {t('claude_limit_resets.age', { seconds: age })}
          {seat?.stale || failed ? ` · ${t('claude_limit_resets.stale')}` : ''}
        </small>
      )}
      {grants.map((grant) => (
        <div key={grant.id}>
          {t('claude_limit_resets.grant', {
            label: grant.label || grant.id,
            count: grant.resets_left,
            expiry: grant.ends_at
              ? new Date(grant.ends_at).toLocaleString()
              : t('claude_limit_resets.unknown'),
          })}
          {' · '}
          {!failed && !seat?.stale && seat?.cedar_ember?.eligible && grant.usable_now
            ? t('claude_limit_resets.usable')
            : t('claude_limit_resets.not_usable')}
        </div>
      ))}
    </div>
  );
}
