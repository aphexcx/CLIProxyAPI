import { describe, expect, test } from 'bun:test';
import { createElement } from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { QuotaTimeline } from '../src/features/quota/components/QuotaTimeline';

const FILE_NAME = 'example-seat.json';
const EMAIL = 'seat@example.test';
const KEY = 'sk-invented-chart-test-key';

function renderSeat(email?: string) {
  return renderToStaticMarkup(
    createElement(QuotaTimeline, {
      entries: [
        {
          type: 'claude',
          file: {
            name: FILE_NAME,
            type: 'claude',
            email,
            account_type: 'api_key',
            account: KEY,
            label: KEY,
          },
        },
      ],
      quotaFor: () => ({
        status: 'success',
        windows: [
          {
            id: 'weekly',
            label: '7-day',
            usedPercent: 25,
            periodHours: 168,
            resetAtMs: new Date(2026, 7, 1, 12).getTime(),
          },
        ],
      }),
      resolvedTheme: 'light',
      now: new Date(2026, 6, 29, 12).getTime(),
    })
  );
}

// Bun renders the row's inline geometry but omits CSS-module class names.
const row = '[style*="--provider-accent"]';
const nameCell = `${row} > div:first-child`;

describe('quota timeline labels for API-key credentials', () => {
  test.each([
    [FILE_NAME, undefined],
    [EMAIL, EMAIL],
  ])(
    'uses %s as its primary label and excludes the key from the chart',
    (expectedPrimary, email) => {
      const markup = renderSeat(email);
      expect(markup).not.toContain(KEY);

      const labels: string[] = [];
      new HTMLRewriter()
        .on(`${nameCell} > div:first-child span`, {
          text(chunk) {
            if (chunk.text) labels.push(chunk.text);
          },
        })
        .transform(markup);
      expect(labels[0]).toBe(expectedPrimary);
    }
  );

  test('keeps the file name visible and in the row and every window tooltip', () => {
    const fileLines: string[] = [];
    const rowTitles: (string | null)[] = [];
    const barTitles: (string | null)[] = [];
    new HTMLRewriter()
      .on(nameCell, {
        element(el) {
          rowTitles.push(el.getAttribute('title'));
        },
      })
      .on(`${nameCell} > span`, {
        text(chunk) {
          fileLines.push(chunk.text);
        },
      })
      .on(`${row} div[style*="left:"][style*="width:"]`, {
        element(el) {
          barTitles.push(el.getAttribute('title'));
        },
      })
      .transform(renderSeat(EMAIL));

    expect(fileLines).toContain(FILE_NAME);
    expect(rowTitles).toEqual([`${EMAIL}\n${FILE_NAME}`]);
    expect(barTitles.length).toBeGreaterThan(0);
    for (const title of barTitles) {
      expect(title?.split('\n')).toContain(FILE_NAME);
    }
  });
});
