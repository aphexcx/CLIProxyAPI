import { useTranslation } from 'react-i18next';
import { claudeLimitResetAge, type ClaudeLimitResetSeat } from '@/services/api/claudeLimitResets';
import {
  claudeResetCardState,
  claudeResetKindLines,
  claudeResetKindName,
  claudeResetLastSpendText,
  type ClaudeResetKindLine,
} from './claudeResetSpendState';
import { useClaudeResetClock } from './useClaudeLimitResetRead';

/** One line per kind: in words with its code, what is left, the next expiry, the provider's labels. */
export function ClaudeResetKindLines({ lines }: { lines: ClaudeResetKindLine[] }) {
  const { t, i18n } = useTranslation();
  return (
    <>
      {lines.map((line) => {
        const parts = [
          line.spendable
            ? t('claude_reset_spend.kind_counts', {
                left: line.summary.resets_left,
                usable: line.summary.usable_resets,
              })
            : t('claude_reset_spend.kind_not_spent_by_hub', { left: line.summary.resets_left }),
        ];
        if (line.spendable && line.summary.soonest_expiry)
          parts.push(
            t('claude_reset_spend.kind_expiry', {
              time: new Date(line.summary.soonest_expiry).toLocaleString(i18n.resolvedLanguage),
            })
          );
        if (line.labels.length)
          parts.push(t('claude_reset_spend.kind_labels', { labels: line.labels.join(', ') }));
        return (
          <div key={line.code} data-reset-kind-line={line.code}>
            {`${claudeResetKindName(t, line.code)}: ${parts.join(' · ')}`}
          </div>
        );
      })}
    </>
  );
}

/** The card face: what is left of each kind and which kind was last spent. No controls. */
export function ClaudeResetKindsView({
  seat,
  failed,
  loaded,
}: {
  seat?: ClaudeLimitResetSeat;
  failed?: boolean;
  loaded?: boolean;
}) {
  const { t, i18n } = useTranslation();
  const clock = useClaudeResetClock();
  const card = claudeResetCardState(seat, { failed, loaded });
  const last = seat ? claudeResetLastSpendText(t, seat, i18n.resolvedLanguage) : undefined;
  const age = claudeLimitResetAge(seat, clock.mono);
  let body;
  if (card.state === 'N') body = <div>{t(`claude_reset_spend.state_${card.why}`)}</div>;
  else if (card.state === 'O') body = <div>{t('claude_reset_spend.state_old_hub')}</div>;
  else if (card.state === 'H') {
    const reason = seat?.cedar_ember
      ? seat.cedar_ember.ineligible_reason === 'surface'
        ? t('claude_limit_resets.surface')
        : seat.cedar_ember.ineligible_reason
          ? t('claude_limit_resets.ineligible_reason', { reason: seat.cedar_ember.ineligible_reason })
          : ''
      : seat?.error || '';
    body = (
      <div>
        {t('claude_reset_spend.state_hidden')}
        {reason && ` ${reason}`}
      </div>
    );
  } else {
    const lines = claudeResetKindLines(seat?.kinds, seat?.cedar_ember?.grants);
    body = (
      <>
        {lines.length ? (
          <ClaudeResetKindLines lines={lines} />
        ) : (
          <div>{t('claude_reset_spend.kinds_none')}</div>
        )}
        {(seat?.stale || failed) && (
          <small>
            {age == null
              ? t('claude_reset_spend.counts_stale_no_age')
              : t('claude_reset_spend.counts_stale', { seconds: age })}
          </small>
        )}
      </>
    );
  }
  return (
    <div aria-label={t('claude_reset_spend.kinds_label')}>
      {body}
      {last && <div>{t('claude_reset_spend.last_spent', { value: last.text })}</div>}
    </div>
  );
}
