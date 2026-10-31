/**
 * Seat-card usage windows: the binding pair as large "% left" figures, and the
 * remaining windows as compact rows inside the card's details. Both read the
 * same normalized windows and say their resets in words ("Resets 10:06 PM").
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
  return { label, left, reset, t };
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
  const { label, left, reset, t } = useWindowText(window);
  // Judged on the shown figure, so "0% left" always reads as exhausted.
  const exhausted = left !== null && Math.round(left) <= 0;
  // An empty fill draws nothing, so an exhausted window tints its track instead.
  const meterClasses = exhausted
    ? { ...classes, quotaBar: `${classes.quotaBar} ${styles.trackExhausted}` }
    : classes;
  return (
    <div className={styles.gauge}>
      <span className={styles.gaugeLabel}>{label}</span>
      <span className={styles.gaugeFigure}>
        <span className={styles.gaugeNumber}>{left === null ? '—' : `${Math.round(left)}%`}</span>
        {left !== null && <span className={styles.gaugeUnit}>{t('quota_card.left')}</span>}
      </span>
      <QuotaMeter percent={left} classes={meterClasses} index={index} />
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

/** The binding windows, side by side at every width; dimmed when the data is not current. */
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
  const { label, left, reset, t } = useWindowText(window);
  return (
    <div className={styles.windowRow}>
      <div className={styles.windowRowHead}>
        <span className={styles.windowRowLabel} title={label}>
          {label}
        </span>
        <span className={styles.windowRowFigure}>
          {left === null ? '—' : `${Math.round(left)}% ${t('quota_card.left')}`}
        </span>
      </div>
      <QuotaMeter percent={left} classes={classes} />
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
