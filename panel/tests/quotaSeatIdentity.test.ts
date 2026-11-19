import { beforeAll, describe, expect, test } from 'bun:test';
import { createElement } from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import i18n from '../src/i18n/index';
import { QuotaTimeline } from '../src/features/quota/components/QuotaTimeline';
import { filterEntriesBySearch, type QuotaFileEntry } from '../src/features/quota/logic';
import {
  resolveCodexChatgptAccountId,
  resolveCodexPlanType,
  resolveCodexSubscriptionActiveUntil,
} from '../src/utils/quota/resolvers';

// Invented identities only: the fork is public.
const ALPHA_FILE = 'claude-5e6f7a8b-shared@example.test.json';
const BETA_FILE = 'claude-1a2b3c4d-shared@example.test.json';
const CODEX_FILE = 'codex-9c0d1e2f-coder@example.test-plus.json';

const claudeBeta: QuotaFileEntry = {
  file: {
    name: BETA_FILE,
    type: 'claude',
    email: 'shared@example.test',
    organization_name: 'Org Beta',
    plan_tier: 'Max 20x',
  },
  type: 'claude',
};
const claudeAlpha: QuotaFileEntry = {
  file: {
    name: ALPHA_FILE,
    type: 'claude',
    email: 'shared@example.test',
    organization_name: 'Org Alpha',
    plan_tier: 'Team premium',
  },
  type: 'claude',
};
const codex: QuotaFileEntry = {
  file: { name: CODEX_FILE, type: 'codex', email: 'coder@example.test', plan_type: 'plus' },
  type: 'codex',
};

const WINDOW_LABEL = '7-day';
const weeklyQuota = {
  status: 'success' as const,
  windows: [
    {
      id: 'weekly',
      label: WINDOW_LABEL,
      usedPercent: 25,
      resetAtMs: new Date(2026, 7, 1, 12).getTime(),
      periodHours: 168,
    },
  ],
};

const render = (entries: QuotaFileEntry[]) =>
  renderToStaticMarkup(
    createElement(QuotaTimeline, {
      entries,
      quotaFor: () => weeklyQuota,
      resolvedTheme: 'light',
      now: new Date(2026, 6, 29, 12).getTime(),
    })
  );

/**
 * Visible text of each row's name column, in row order. CSS-module classes do not
 * render under bun, so a row is found by the provider accent on its root, and its
 * name column ends at the first limit chip (the window's label).
 */
const rowNames = (markup: string) =>
  markup
    .split('style="--provider-accent')
    .slice(1)
    .map((row) => {
      const texts = row
        .slice(row.indexOf('>') + 1)
        .replace(/<[^>]*>/g, '\n')
        .split('\n')
        .map((text) => text.trim())
        .filter(Boolean);
      return texts.slice(0, texts.indexOf(WINDOW_LABEL));
    });

/** Row titles in row order; a bar's title adds its window span (→) and is skipped. */
const rowTitles = (markup: string) =>
  Array.from(markup.matchAll(/ title="([^"]*)"/g), (match) => match[1]).filter(
    (title) => !title.includes('→')
  );

describe('quota windows table seat identity', () => {
  beforeAll(async () => {
    await i18n.changeLanguage('en');
  });

  // The page hands the table its cards' order (provider-grouped), not the table's own order.
  const renderThree = () => render([claudeBeta, claudeAlpha, codex]);

  test('names each row by email, then organization or plan, then the file name, sorted by email', () => {
    // A Claude seat's second line is its organization alone, never its plan tier.
    expect(rowNames(renderThree())).toEqual([
      ['coder@example.test', '7d', 'Plus', CODEX_FILE],
      ['shared@example.test', '7d', 'Org Alpha', ALPHA_FILE],
      ['shared@example.test', '7d', 'Org Beta', BETA_FILE],
    ]);
  });

  test('carries all three lines in the row title', () => {
    expect(rowTitles(renderThree())).toEqual([
      `coder@example.test\nPlus\n${CODEX_FILE}`,
      `shared@example.test\nOrg Alpha\n${ALPHA_FILE}`,
      `shared@example.test\nOrg Beta\n${BETA_FILE}`,
    ]);
  });

  test('adds the short id when two seats share an email and an organization', () => {
    const sameOrg = render([
      claudeBeta,
      { ...claudeAlpha, file: { ...claudeAlpha.file, organization_name: 'Org Beta' } },
    ]);
    expect(rowNames(sameOrg).map((row) => row[2])).toEqual([
      'Org Beta · 1a2b3c4d',
      'Org Beta · 5e6f7a8b',
    ]);
  });

  test('falls back to the file name, with no third line, when a seat has no email', () => {
    const noEmail = render([
      { file: { name: 'claude-legacy.json', type: 'claude' }, type: 'claude' },
    ]);
    expect(rowNames(noEmail)).toEqual([['claude-legacy.json', '7d']]);
  });
});

describe('quota page search', () => {
  const entries = [claudeBeta, claudeAlpha, codex];
  const namesFor = (query: string) =>
    filterEntriesBySearch(entries, query).map((entry) => entry.file.name);

  test('matches email, organization and file name', () => {
    expect(namesFor('coder@')).toEqual([CODEX_FILE]);
    expect(namesFor('org alpha')).toEqual([ALPHA_FILE]);
    expect(namesFor('1a2b3c4d')).toEqual([BETA_FILE]);
    expect(namesFor('shared@example')).toEqual([BETA_FILE, ALPHA_FILE]);
  });
});

describe('Codex claims in the v8 read', () => {
  const file = {
    name: CODEX_FILE,
    type: 'codex',
    codex_claims: {
      chatgpt_account_id: 'acct-example-0001',
      plan_type: 'team',
      chatgpt_subscription_active_until: '2099-01-01T00:00:00Z',
    },
  };

  test('reads the account id, plan and subscription end from codex_claims', () => {
    expect(resolveCodexChatgptAccountId(file)).toBe('acct-example-0001');
    expect(resolveCodexPlanType(file)).toBe('team');
    expect(resolveCodexSubscriptionActiveUntil(file)).toBe('2099-01-01T00:00:00Z');
  });
});
