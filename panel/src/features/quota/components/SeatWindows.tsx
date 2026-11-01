/**
 * Seat-card usage windows: the at-a-glance windows stacked one per line, and the
 * remaining windows as compact rows inside the card's details. Every bar fills
 * left to right with what was used, with the "% left" figure beside it; both read
 * the same normalized windows and say their resets in words ("Resets 10:06 PM").
 */

import { useTranslation } from 'react-i18next';
import type { ClaudeQuotaWindow, CodexQuotaWindow } from '@/types';
import { useNow } from '@/hooks/useNow';
import { formatResetWords, remainingPercent } from '../seatCard';
import type { QuotaClassMap } from '../types';
import { QuotaMeter } from './QuotaMeter';
import styles from './QuotaCard.module.scss';

export type SeatWindow = ClaudeQuotaWindow | CodexQuotaWindow;

function useWindowText(window: SeatWindow) {
  const { t, i18n } = useTranslation();
  const now = useNow();
  const label = window.labelKey
    ? t(window.labelKey, ('labelParams' in window ? window.labelParams : undefined) ?? {})
    : window.label;
  const left = remainingPercent(window.usedPercent);
  const resetAt = window.resetAtMs;
  // Stale data can carry a reset that already passed; say so instead of a future time.
  const reset =
    typeof resetAt === 'number' && Number.isFinite(resetAt)
      ? t(resetAt > now ? 'quota_card.resets' : 'quota_card.reset_passed', {
          time: formatResetWords(resetAt, now, i18n.resolvedLanguage),
        })
      : null;
  const figure = left === null ? null : `${Math.round(left)}% ${t('quota_card.left')}`;
  // The bar is a meter of the used share; its name carries the window and the figure.
  const meter = {
    fill: 'used' as const,
    ariaLabel: figure === null ? label : `${label}: ${figure}`,
    ariaValueText:
      left === null
        ? undefined
        : t('claude_quota.usage_utilization', { percent: 100 - Math.round(left) }),
  };
  return { label, left, figure, reset, meter, t };
}

function SeatGauge({
  window,
  classes,
  index,
}: {
  window: SeatWindow;
  classes: QuotaClassMap;
  index: number;
}) {
  const { label, left, reset, meter, t } = useWindowText(window);
  // Judged on the shown figure, so "0% left" always reads as exhausted.
  const exhausted = left !== null && Math.round(left) <= 0;
  // A rounded "0% left" can leave a sliver unfilled, so an exhausted track is tinted too.
  const meterClasses = {
    ...classes,
    quotaBar: exhausted
      ? `${classes.quotaBar} ${styles.gaugeTrack} ${styles.trackExhausted}`
      : `${classes.quotaBar} ${styles.gaugeTrack}`,
  };
  return (
    <div className={styles.gauge}>
      <span className={styles.gaugeLabel}>{label}</span>
      <QuotaMeter percent={left} classes={meterClasses} index={index} {...meter} />
      <span className={styles.gaugeFigure}>
        <span className={styles.gaugeNumber}>{left === null ? '—' : `${Math.round(left)}%`}</span>
        {left !== null && <span className={styles.gaugeUnit}>{t('quota_card.left')}</span>}
      </span>
      {reset && (
        <span
          className={exhausted ? `${styles.gaugeReset} ${styles.gaugeResetNow}` : styles.gaugeReset}
        >
          {reset}
        </span>
      )}
    </div>
  );
}

/** The at-a-glance windows, one per line in display order; dimmed when the data is not current. */
export function SeatGauges({
  windows,
  classes,
  dimmed = false,
}: {
  windows: readonly SeatWindow[];
  classes: QuotaClassMap;
  dimmed?: boolean;
}) {
  return (
    <div className={dimmed ? `${styles.gauges} ${styles.gaugesDimmed}` : styles.gauges}>
      {windows.map((window, index) => (
        <SeatGauge key={window.id} window={window} classes={classes} index={index} />
      ))}
    </div>
  );
}

function SeatWindowRow({ window, classes }: { window: SeatWindow; classes: QuotaClassMap }) {
  const { label, left, figure, reset, meter } = useWindowText(window);
  return (
    <div className={styles.windowRow}>
      <div className={styles.windowRowHead}>
        <span className={styles.windowRowLabel} title={label}>
          {label}
        </span>
        <span className={styles.windowRowFigure}>{figure ?? '—'}</span>
      </div>
      <QuotaMeter percent={left} classes={classes} {...meter} />
      {reset && <span className={styles.windowRowReset}>{reset}</span>}
    </div>
  );
}

/** Every other window, in payload order, for the details section. */
export function SeatWindowRows({
  windows,
  classes,
}: {
  windows: readonly SeatWindow[];
  classes: QuotaClassMap;
}) {
  if (windows.length === 0) return null;
  return (
    <div className={styles.windowRows}>
      {windows.map((window) => (
        <SeatWindowRow key={window.id} window={window} classes={classes} />
      ))}
    </div>
  );
}
