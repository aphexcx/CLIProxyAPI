/**
 * QuotaMeter rendered as markup: the default remaining-share track every provider
 * body uses, and the seat cards' used-share fill that grows left to right.
 */

import { describe, expect, test } from 'bun:test';
import { createElement } from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { QuotaMeter, type QuotaMeterProps } from '@/features/quota/components/QuotaMeter';
import { QUOTA_CLASS_KEYS, bindQuotaClasses } from '@/features/quota/types';

const classes = bindQuotaClasses(
  Object.fromEntries(QUOTA_CLASS_KEYS.map((key) => [key, key])),
  'test-host'
);

const render = (props: Omit<QuotaMeterProps, 'classes'>) =>
  renderToStaticMarkup(createElement(QuotaMeter, { classes, ...props }));

describe('QuotaMeter, remaining fill (provider bodies)', () => {
  test('draws what is left and stays decorative', () => {
    const html = render({ percent: 70 });
    expect(html).toContain('width:70%');
    expect(html).toContain('quotaBarFillHigh');
    expect(html).not.toContain('role=');
  });
});

describe('QuotaMeter, used fill (seat cards)', () => {
  const seatBar = (percent: number | null) =>
    render({
      percent,
      fill: 'used',
      ariaLabel: '5-hour limit: 38% left',
      ariaValueText: '62% used',
    });

  test('grows with the used share while the tone follows what is left', () => {
    const html = seatBar(38);
    expect(html).toContain('width:62%');
    expect(html).toContain('quotaBarFillMedium');
  });

  test('is a meter of the used share named by the window and its figure', () => {
    const html = seatBar(38);
    expect(html).toContain('role="meter"');
    expect(html).toContain('aria-valuemin="0"');
    expect(html).toContain('aria-valuemax="100"');
    expect(html).toContain('aria-valuenow="62"');
    expect(html).toContain('aria-valuetext="62% used"');
    expect(html).toContain('aria-label="5-hour limit: 38% left"');
  });

  test('an untouched window is an empty bar, an exhausted one a full red bar', () => {
    const fresh = seatBar(100);
    expect(fresh).toContain('width:0%');
    expect(fresh).toContain('aria-valuenow="0"');
    const exhausted = seatBar(0);
    expect(exhausted).toContain('width:100%');
    expect(exhausted).toContain('quotaBarFillLow');
    expect(exhausted).toContain('aria-valuenow="100"');
  });

  test('a fractional share draws exactly and announces the rounded figure', () => {
    const html = seatBar(38.5);
    expect(html).toContain('width:61.5%');
    expect(html).toContain('aria-valuenow="61"');
  });

  test('unknown usage draws an empty, unnamed track', () => {
    const html = seatBar(null);
    expect(html).toContain('width:0%');
    expect(html).not.toContain('role=');
    expect(html).toContain('aria-hidden="true"');
  });
});
