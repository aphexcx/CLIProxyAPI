import { useEffect, useState, useSyncExternalStore } from 'react';
import { useTranslation } from 'react-i18next';
import { apiClient } from '@/services/api/client';
import {
  readClaudeLimitResets,
  matchClaudeLimitResetSeat,
  getClaudeResetCredentialRevision,
  subscribeClaudeResetCredentials,
  claudeLimitResetAge,
  type ClaudeLimitResetSeat,
} from '@/services/api/claudeLimitResets';

/** Observations for the holder's own click; deliberately no claim controls. */
export function ClaudeLimitResets({ name, generation }: { name: string; generation?: string }) {
  const [result, setResult] = useState<{
    seat?: ClaudeLimitResetSeat;
    rule?: string;
    failed?: boolean;
    revision: number;
    name: string;
    generation?: string;
    credentialRevision: number;
  }>();
  const revision = apiClient.getConnectionRevision();
  const credentialRevision = useSyncExternalStore(
    subscribeClaudeResetCredentials,
    getClaudeResetCredentialRevision,
    getClaudeResetCredentialRevision
  );
  useEffect(() => {
    let active = true;
    const load = async () => {
      try {
        const response = await readClaudeLimitResets();
        if (active && apiClient.getConnectionRevision() === revision) {
          setResult({
            seat: matchClaudeLimitResetSeat(response.seats, name, generation),
            rule: response.ranking_rule,
            revision,
            name,
            generation,
            credentialRevision,
          });
        }
      } catch {
        if (active)
          setResult((previous) => ({
            ...(previous?.revision === revision &&
            previous.name === name &&
            previous.generation === generation &&
            previous.credentialRevision === credentialRevision
              ? previous
              : {}),
            failed: true,
            revision,
            name,
            generation,
            credentialRevision,
          }));
      }
    };
    void load();
    const timer = setInterval(() => void load(), 60_000);
    return () => {
      active = false;
      clearInterval(timer);
    };
  }, [name, revision, generation, credentialRevision]);
  const current =
    result?.revision === revision &&
    result.name === name &&
    result.generation === generation &&
    result.credentialRevision === credentialRevision
      ? result
      : undefined;
  return (
    <ClaudeLimitResetsView seat={current?.seat} rule={current?.rule} failed={current?.failed} />
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
