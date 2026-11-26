import { expect, test } from 'bun:test';
import { readFileSync } from 'node:fs';
import { resolveClaudePlanType } from '@/features/quota/providers/claude/data';

test.each(['en', 'ko', 'ru', 'vi', 'zh-CN', 'zh-TW'])('Claude plan names in %s', (locale) => {
  const { claude_quota } = JSON.parse(readFileSync(`src/i18n/locales/${locale}.json`, 'utf8'));
  expect(claude_quota.plan_max5).toBe('Max');
  expect(claude_quota.plan_max20).toBe('Ultra');
  expect(Object.values(claude_quota).join(' ')).not.toMatch(/20x|5x/i);
});

test.each([
  ['default_claude_max_20x', 'plan_max20'],
  ['default_claude_max_5x', 'plan_max5'],
  [' DEFAULT_CLAUDE_MAX_20X ', 'plan_max20'],
  ['default_claude_max_future', 'plan_max'],
  [undefined, 'plan_max'],
] as const)('Max account with tier %s', (rate_limit_tier, expected) => {
  expect(
    resolveClaudePlanType({
      account: { has_claude_max: true, has_claude_pro: true },
      organization: { rate_limit_tier },
    })
  ).toBe(expected);
});

test('prefers an active Team over a personal Max tier', () => {
  expect(
    resolveClaudePlanType({
      account: { has_claude_max: true },
      organization: {
        organization_type: 'claude_team',
        subscription_status: 'active',
        rate_limit_tier: 'default_claude_max_20x',
      },
    })
  ).toBe('plan_team');
});

test.each([[true, 'plan_pro'], [false, 'plan_free']] as const)(
  'keeps the personal plan when Max is false and Pro is %s',
  (has_claude_pro, expected) => {
    expect(resolveClaudePlanType({ account: { has_claude_max: false, has_claude_pro } })).toBe(
      expected
    );
  }
);
