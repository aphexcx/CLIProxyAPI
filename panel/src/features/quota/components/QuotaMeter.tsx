/**
 * Quota level bar (the typed successor of the old QuotaProgressBar).
 *
 * Dataviz grammar: a thin track recedes into the background and the fill is toned
 * by what is left in three bands (>= 70 green / >= 30 amber / < 30 red). With
 * percent === null the track renders empty: unknown is never tinted (the Medium
 * class is invisible at width 0, as before).
 * `index` is written to `--meter-index` so the full-page skin can stagger row
 * entrances; the compact skin ignores it.
 *
 * Provider bodies draw what is left. Seat cards pass `fill="used"`: the fill
 * grows left to right with what was used (empty part on the right), its tone
 * still follows what is left, and with `ariaLabel` the bar is a meter of the
 * used share.
 */

import type { CSSProperties } from 'react';
import type { QuotaClassMap } from '../types';

export const QUOTA_PROGRESS_HIGH_THRESHOLD = 70;
export const QUOTA_PROGRESS_MEDIUM_THRESHOLD = 30;

export interface QuotaMeterProps {
  /** Remaining share, 0–100; null when usage is unknown. */
  percent: number | null;
  classes: QuotaClassMap;
  index?: number;
  /** What the fill draws: what is left (default) or what was used. */
  fill?: 'remaining' | 'used';
  /** Accessible name; with known usage the bar becomes a meter of the share it draws. */
  ariaLabel?: string;
  ariaValueText?: string;
}

export function QuotaMeter({
  percent,
  classes,
  index,
  fill = 'remaining',
  ariaLabel,
  ariaValueText,
}: QuotaMeterProps) {
  const clamp = (value: number, min: number, max: number) => Math.min(max, Math.max(min, value));
  const normalized = percent === null ? null : clamp(percent, 0, 100);
  const fillClass =
    normalized === null
      ? classes.quotaBarFillMedium
      : normalized >= QUOTA_PROGRESS_HIGH_THRESHOLD
        ? classes.quotaBarFillHigh
        : normalized >= QUOTA_PROGRESS_MEDIUM_THRESHOLD
          ? classes.quotaBarFillMedium
          : classes.quotaBarFillLow;
  const drawn = normalized === null ? 0 : fill === 'used' ? 100 - normalized : normalized;
  const widthPercent = Math.round(drawn * 100) / 100;
  const style: CSSProperties & { '--meter-index'?: number } = { width: `${widthPercent}%` };
  if (index !== undefined) {
    style['--meter-index'] = index;
  }

  // The value matches the rounded "% left" figure beside the bar, so the two never disagree.
  const meter =
    ariaLabel === undefined
      ? {}
      : normalized === null
        ? { 'aria-hidden': true }
        : {
            role: 'meter',
            'aria-valuemin': 0,
            'aria-valuemax': 100,
            'aria-valuenow':
              fill === 'used' ? 100 - Math.round(normalized) : Math.round(normalized),
            'aria-valuetext': ariaValueText,
            'aria-label': ariaLabel,
          };

  return (
    <div className={classes.quotaBar} {...meter}>
      <div className={`${classes.quotaBarFill} ${fillClass}`} style={style} />
    </div>
  );
}
