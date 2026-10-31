/**
 * Provider quota card: identity, loading/error state, quota body, and actions.
 * Claude observations remain visible while refreshing or after a failed read.
 *
 * Claude and Codex seats lead with the account, its plan and the two windows
 * the hub routes on; everything else sits behind one "Details" disclosure.
 */

import { useId, useState, type CSSProperties, type ReactNode } from 'react';
import type { TFunction } from 'i18next';
import { useTranslation } from 'react-i18next';
import { IconChevronDown, IconRefreshCw } from '@/components/ui/icons';
import type { ClaudeQuotaState, CodexQuotaState, ResolvedTheme } from '@/types';
import { useNow } from '@/hooks/useNow';
import {
  formatRelativeInstant,
  resolveCodexPlanType,
  resolvePlanTier,
  resolveQuotaErrorMessage,
} from '@/utils/quota';
import { getQuotaDisplayName } from '@/utils/quota/identity';
import { MINUTE_MS } from '@/utils/time/durations';
import {
  getAuthFileIcon,
  getThemeSurfaceIconBackground,
  getTypeLabel,
  isThemeSurfaceIconProvider,
} from '@/features/authFiles/constants';
import { deriveAuthFileIdentity } from '@/features/authFiles/identity';
import { bindQuotaClasses } from '../types';
import { QUOTA_ADAPTERS, type QuotaCardState } from '../providers';
import { isQuotaRefreshDisabled, type QuotaFileEntry } from '../logic';
import {
  describeSeatFreshness,
  isSeatCardType,
  readSeatPlanTier,
  splitSeatWindows,
  type SeatFreshness,
} from '../seatCard';
import { useClaudeResetGrants } from '../providers/claude/ClaudeResetGrants';
import { ClaudeResetGrantDetails } from '../providers/claude/ClaudeResetGrantDetails';
import { hasClaudeUsageData } from '../providers/claude/data';
import { getCodexPlanLabel } from '../providers/codex/planLabel';
import { hasCodexUsageData } from '../providers/codex/data';
import { useQuotaCacheReads } from '../hooks/useQuotaCacheReads';
import { SeatGauges, SeatWindowRows, type SeatWindow } from './SeatWindows';
import bodyStyles from './QuotaBody.module.scss';
import styles from './QuotaCard.module.scss';

/** Bind the quota-page styles to the shared rendering contract. */
const quotaClasses = bindQuotaClasses(bodyStyles, 'QuotaBody.module.scss');
/** Seat details reuse those classes with labels and countdowns at 4.5:1 on every theme. */
const detailClasses = bindQuotaClasses(
  {
    ...bodyStyles,
    codexPlanLabel: styles.detailsLabel,
    codexResetCreditLabel: styles.detailsCreditLabel,
    quotaMeta: styles.detailsMeta,
    quotaAmount: styles.detailsAmount,
    quotaReset: styles.detailsMuted,
    quotaResetRelative: styles.detailsRelative,
  },
  'QuotaCard.module.scss details'
);

export type QuotaCardProps = {
  entry: QuotaFileEntry;
  quota?: QuotaCardState;
  resolvedTheme: ResolvedTheme;
  canRefresh: boolean;
  resetting: boolean;
  /** Initial cascade delay; null disables entrance motion for later mounts. */
  entranceDelayMs?: number | null;
  onRefresh: () => void;
  onReset: () => void;
};

export function QuotaCard(props: QuotaCardProps) {
  const {
    entry,
    quota,
    resolvedTheme,
    canRefresh,
    resetting,
    entranceDelayMs,
    onRefresh,
    onReset,
  } = props;
  const { t, i18n } = useTranslation();
  const now = useNow();
  const detailsId = useId();
  const [detailsOpen, setDetailsOpen] = useState(false);
  const adapter = QUOTA_ADAPTERS[entry.type];
  const file = entry.file;
  const seatType = isSeatCardType(entry.type) ? entry.type : null;
  const claudeQuota = entry.type === 'claude' ? (quota as ClaudeQuotaState | undefined) : undefined;
  const codexQuota = entry.type === 'codex' ? (quota as CodexQuotaState | undefined) : undefined;
  const retainedClaudeUsage = hasClaudeUsageData(claudeQuota) || hasCodexUsageData(codexQuota);
  const displayName = getQuotaDisplayName(file);
  // The Codex body used to own this subscription; seat cards keep it while details are closed.
  useQuotaCacheReads(Boolean(codexQuota?.serverCache));

  // Capture the entrance delay only at mount, without a render-time ref read.
  const [mountEntranceDelayMs] = useState<number | null>(entranceDelayMs ?? null);
  const entranceStyle =
    mountEntranceDelayMs === null
      ? undefined
      : ({ '--card-delay': `${mountEntranceDelayMs}ms` } as CSSProperties);

  const status = quota?.status ?? 'idle';
  const loading = status === 'loading';
  const claudeReset = useClaudeResetGrants(
    file,
    entry.type === 'claude' && status !== 'idle',
    !canRefresh || loading || resetting,
    onRefresh
  );
  const providerType =
    entry.type === 'plugin'
      ? String(
          file.quotaProvider ?? file['quota_provider'] ?? file.provider ?? file.type ?? 'plugin'
        )
      : entry.type;
  const iconSrc = getAuthFileIcon(providerType, resolvedTheme);
  const typeLabel = getTypeLabel(t, providerType);
  const errorMessage = resolveQuotaErrorMessage(
    t,
    quota?.errorStatus,
    quota?.error || t('common.unknown_error')
  );
  // Claude banked resets join the plan chip row; their expiry list follows it.
  const showClaudeReset = entry.type === 'claude' && status === 'success';
  const claudePlanItems = showClaudeReset ? (
    <span className={quotaClasses.codexPlanItem}>
      <span className={detailClasses.codexPlanLabel}>{t('claude_reset.title')}</span>
      <span className={quotaClasses.codexPlanValue}>{claudeReset.count ?? '--'}</span>
    </span>
  ) : undefined;
  const claudeAfterPlan = showClaudeReset ? (
    <>
      <ClaudeResetGrantDetails grants={claudeReset.grants} classes={detailClasses} />
      {claudeReset.message && (
        <div role="status" className={quotaClasses.codexResetCreditsError}>
          {t(`claude_reset.${claudeReset.message}`)}
        </div>
      )}
    </>
  ) : undefined;
  const showReset =
    status === 'success' &&
    Boolean(adapter.resetQuota) &&
    quota !== undefined &&
    Boolean(adapter.canResetQuota?.(quota));

  const icon = (
    <span
      className={styles.iconWrap}
      title={typeLabel}
      style={
        isThemeSurfaceIconProvider(providerType)
          ? { background: getThemeSurfaceIconBackground(resolvedTheme) }
          : undefined
      }
    >
      {iconSrc ? (
        <img src={iconSrc} alt="" className={styles.icon} />
      ) : (
        <span className={styles.iconFallback}>{typeLabel.slice(0, 1).toUpperCase()}</span>
      )}
    </span>
  );

  const actions = (
    <>
      {entry.type === 'claude' && (
        <button
          type="button"
          className={styles.actionPill}
          disabled={claudeReset.readBlocked}
          onClick={claudeReset.refresh}
        >
          <IconRefreshCw size={13} className={claudeReset.reading ? styles.spinning : undefined} />
          {t('claude_quota.cache_check_grants')}
        </button>
      )}
      {entry.type === 'claude' && (
        <button
          type="button"
          className={styles.actionPill}
          disabled={claudeReset.blocked}
          onClick={claudeReset.confirm}
          title={t(`claude_reset.${claudeReset.buttonLabel}`)}
        >
          <IconRefreshCw size={13} className={claudeReset.busy ? styles.spinning : undefined} />
          {t(`claude_reset.${claudeReset.buttonLabel}`)}
        </button>
      )}
      {showReset && (
        <button
          type="button"
          className={styles.actionPill}
          onClick={onReset}
          disabled={!canRefresh || loading || resetting}
          title={t('codex_quota.reset_button')}
        >
          <IconRefreshCw size={13} className={resetting ? styles.spinning : undefined} />
          {t('codex_quota.reset_button')}
        </button>
      )}
      <button
        type="button"
        className={styles.actionPill}
        onClick={onRefresh}
        disabled={isQuotaRefreshDisabled(canRefresh, loading, resetting || claudeReset.busy)}
        title={t('auth_files.quota_refresh_hint')}
      >
        <IconRefreshCw size={13} className={loading ? styles.spinning : undefined} />
        {t('auth_files.quota_refresh_single')}
      </button>
    </>
  );

  const idleBody = (
    <button type="button" className={styles.idleBody} onClick={onRefresh} disabled={!canRefresh}>
      <IconRefreshCw size={15} aria-hidden="true" className={styles.idleGlyph} />
      <span className={styles.idleHint}>{t(`${adapter.i18nPrefix}.idle`)}</span>
    </button>
  );

  const skeleton = (
    <div className={styles.skeleton} aria-busy="true">
      <span className={styles.srOnly}>{t(`${adapter.i18nPrefix}.loading`)}</span>
      {[0, 1].map((row) => (
        <div key={row} className={styles.skeletonRow} aria-hidden="true">
          <span className={styles.skeletonLabel} />
          <span className={styles.skeletonTrack} />
        </div>
      ))}
    </div>
  );

  const errorStrip = (
    <div className={styles.errorStrip} role="alert">
      {t(`${adapter.i18nPrefix}.load_failed`, { message: errorMessage })}
    </div>
  );

  if (seatType) {
    const identity = deriveAuthFileIdentity(file);
    const organization =
      entry.type === 'claude' && typeof file.organization_name === 'string'
        ? file.organization_name.trim()
        : '';
    let planChip: ReactNode = null;
    if (entry.type === 'claude') {
      // Only the hub's recorded tier: the panel's own profile read infers "Free" for
      // Enterprise seats and cannot tell Max 5x from 20x, so an unknown tier shows no chip.
      const tier = readSeatPlanTier(file);
      if (tier) planChip = <span className={styles.planChip}>{tier}</span>;
    }
    if (entry.type === 'codex') {
      const planType = codexQuota?.planType ?? resolveCodexPlanType(file);
      const planLabel = getCodexPlanLabel(t, planType);
      const tier = resolvePlanTier(planType);
      if (planLabel) {
        planChip = (
          <span
            className={
              tier === 'elite'
                ? quotaClasses.elitePlanValue
                : tier === 'premium'
                  ? quotaClasses.premiumPlanValue
                  : styles.planChip
            }
          >
            {planLabel}
          </span>
        );
      }
    }

    const hasData = quota !== undefined && (retainedClaudeUsage || status === 'success');
    const windows: readonly SeatWindow[] = (hasData && (claudeQuota ?? codexQuota)?.windows) || [];
    const { headline, rest } = splitSeatWindows(seatType, windows);
    const freshness = hasData ? describeSeatFreshness(quota as ClaudeQuotaState, now) : null;
    const freshnessText = loading
      ? t(`${adapter.i18nPrefix}.loading`)
      : describeFreshnessText(freshness, t, now, i18n.resolvedLanguage);
    // Numbers from a failed, stale or paused poll stay readable but stop looking current.
    const freshnessWarn =
      freshness?.kind === 'failed' || freshness?.kind === 'stale' || freshness?.kind === 'paused';
    const nextPollAt = (quota as ClaudeQuotaState | undefined)?.serverCache?.nextPollAt;
    const observedAt = (quota as ClaudeQuotaState | undefined)?.observedAt;
    const waitingForFirstPoll = freshness?.kind === 'pending' || freshness?.kind === 'queued';

    return (
      <article
        className={`${styles.card} ${styles.seatCard} ${mountEntranceDelayMs === null ? '' : styles.cardEnter}`}
        style={entranceStyle}
      >
        <header className={styles.head}>
          {icon}
          <div className={styles.identity}>
            {/* h2: the page title is the h1 and the timeline below is an h2 sibling. */}
            <h2
              className={
                identity.kind === 'fileName'
                  ? `${styles.seatName} ${styles.seatNameMono}`
                  : styles.seatName
              }
              title={identity.fullName}
            >
              {identity.primary}
            </h2>
            {(organization || planChip) && (
              <span className={styles.seatMeta}>
                {organization && (
                  <span className={styles.seatOrg} title={organization}>
                    {organization}
                  </span>
                )}
                {planChip}
              </span>
            )}
          </div>
        </header>

        {status === 'idle' ? (
          idleBody
        ) : loading && !retainedClaudeUsage ? (
          skeleton
        ) : (
          <>
            {status === 'error' && errorStrip}
            {headline.length > 0 ? (
              <SeatGauges windows={headline} classes={quotaClasses} dimmed={freshnessWarn} />
            ) : hasData && !waitingForFirstPoll ? (
              <div className={styles.idleHint}>{t(`${adapter.i18nPrefix}.empty_windows`)}</div>
            ) : null}
          </>
        )}

        {status !== 'idle' && (
          <>
            <footer className={styles.seatFooter}>
              {/* Not a live region: fourteen cards re-announcing "Updated 2 minutes ago" is noise. */}
              <span
                aria-busy={loading || undefined}
                className={
                  freshnessWarn ? `${styles.freshness} ${styles.freshnessWarn}` : styles.freshness
                }
                title={
                  [
                    observedAt && new Date(observedAt).toLocaleString(i18n.resolvedLanguage),
                    freshness?.kind === 'failed' &&
                      t('claude_quota.cache_failed', {
                        status: freshness.status || t('claude_quota.cache_no_response'),
                      }),
                  ]
                    .filter(Boolean)
                    .join(' · ') || undefined
                }
              >
                {freshnessText}
              </span>
              <button
                type="button"
                className={styles.disclosure}
                aria-expanded={detailsOpen}
                aria-controls={detailsId}
                aria-label={`${t('quota_card.details')}: ${identity.primary}`}
                onClick={() => setDetailsOpen((open) => !open)}
              >
                {t('quota_card.details')}
                <IconChevronDown
                  size={14}
                  aria-hidden="true"
                  className={
                    detailsOpen ? `${styles.chevron} ${styles.chevronOpen}` : styles.chevron
                  }
                />
              </button>
            </footer>
            <div id={detailsId} className={styles.details} hidden={!detailsOpen}>
              {detailsOpen && (
                <>
                  <SeatWindowRows windows={rest} classes={quotaClasses} />
                  {hasData && (
                    <adapter.Body
                      quota={quota}
                      classes={detailClasses}
                      planItems={claudePlanItems}
                      afterPlan={claudeAfterPlan}
                      extrasOnly
                    />
                  )}
                  <dl className={styles.facts}>
                    {observedAt && (
                      <div className={styles.fact}>
                        <dt>{t('quota_card.polled')}</dt>
                        <dd>{new Date(observedAt).toLocaleString(i18n.resolvedLanguage)}</dd>
                      </div>
                    )}
                    {nextPollAt && (
                      <div className={styles.fact}>
                        <dt>{t('quota_card.next_poll')}</dt>
                        <dd>{new Date(nextPollAt).toLocaleString(i18n.resolvedLanguage)}</dd>
                      </div>
                    )}
                    <div className={styles.fact}>
                      <dt>{t('quota_card.file')}</dt>
                      <dd className={styles.factMono} title={identity.fullName}>
                        {identity.fullName}
                      </dd>
                    </div>
                  </dl>
                  <div className={styles.actionRow}>{actions}</div>
                </>
              )}
            </div>
          </>
        )}
      </article>
    );
  }

  return (
    <article
      className={`${styles.card} ${mountEntranceDelayMs === null ? '' : styles.cardEnter}`}
      style={entranceStyle}
    >
      <header className={styles.head}>
        {icon}
        <span className={styles.fileName} title={displayName}>
          {displayName}
        </span>
      </header>

      <div className={styles.body}>
        {status === 'idle' ? (
          idleBody
        ) : loading && !retainedClaudeUsage ? (
          skeleton
        ) : status === 'error' ? (
          errorStrip
        ) : quota && !retainedClaudeUsage ? (
          <adapter.Body
            quota={quota}
            classes={quotaClasses}
            planItems={claudePlanItems}
            afterPlan={claudeAfterPlan}
          />
        ) : !retainedClaudeUsage ? (
          <div className={styles.idleHint}>{t(`${adapter.i18nPrefix}.idle`)}</div>
        ) : null}
        {loading && retainedClaudeUsage && (
          <div role="status" aria-busy="true" className={quotaClasses.quotaMessage}>
            {t(`${adapter.i18nPrefix}.loading`)}
          </div>
        )}
        {retainedClaudeUsage && quota && (
          <adapter.Body
            quota={quota}
            classes={quotaClasses}
            planItems={claudePlanItems}
            afterPlan={claudeAfterPlan}
          />
        )}
      </div>

      {status !== 'idle' && <footer className={styles.actionRow}>{actions}</footer>}
    </article>
  );
}

/** The footer's one freshness phrase; quiet unless the numbers cannot be trusted. */
function describeFreshnessText(
  freshness: SeatFreshness | null,
  t: TFunction,
  now: number,
  locale?: string
): string {
  if (!freshness) return '';
  const age = (ageMs: number) => formatRelativeInstant(now - ageMs, now, locale);
  switch (freshness.kind) {
    case 'pending':
      return t('claude_quota.cache_pending');
    case 'failed':
      return freshness.ageMs === null
        ? t('claude_quota.cache_failed', {
            status: freshness.status || t('claude_quota.cache_no_response'),
          })
        : t('quota_card.failed', { age: age(freshness.ageMs) });
    case 'stale':
      return freshness.ageMs === null
        ? t('quota_card.stale_unknown')
        : t('quota_card.stale', { age: age(freshness.ageMs) });
    case 'paused':
      return t('claude_quota.cache_paused');
    case 'queued':
      return t('claude_quota.cache_queued');
    case 'fresh':
      return freshness.ageMs === null || freshness.ageMs < MINUTE_MS
        ? t('quota_card.updated_now')
        : t('quota_card.updated', { age: age(freshness.ageMs) });
  }
}
