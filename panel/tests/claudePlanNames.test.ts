import { expect, test } from 'bun:test';
import { readFileSync } from 'node:fs';
import { deriveAuthFileIdentity } from '@/features/authFiles/identity';
import { resolveClaudePlanType } from '@/features/quota/providers/claude/data';
import { readSeatPlanTier } from '@/features/quota/seatCard';

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
  ['default_claude_max_120x', 'plan_max'],
  ['default_claude_max_25x', 'plan_max'],
  ['default_claude_max_future', 'plan_max'],
  [undefined, 'plan_max'],
] as const)('Max account with tier %s', (rate_limit_tier, expected) => {
  expect(
    resolveClaudePlanType({
      account: { has_claude_max: true, has_claude_pro: true },
      organization: { organization_type: 'claude_max', rate_limit_tier },
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

test.each([
  ['claude_team', 'canceled'],
  ['claude_team', undefined],
  ['claude_enterprise', 'active'],
  ['unknown', 'active'],
  [undefined, undefined],
] as const)(
  'ignores the tier of organization %s with status %s',
  (organization_type, subscription_status) => {
    expect(
      resolveClaudePlanType({
        account: { has_claude_max: true },
        organization: {
          organization_type,
          subscription_status,
          rate_limit_tier: 'default_claude_max_20x',
        },
      })
    ).toBe('plan_max');
  }
);

test.each([
  [' Max 20x ', 'Ultra'],
  ['Max 5x', 'Max'],
  ['Ultra', 'Ultra'],
  ['Max', 'Max'],
  ['Max 200x', 'Max 200x'],
] as const)('displays the stored Claude label %s on both cards', (plan_tier, expected) => {
  const file = { name: 'seat.json', type: 'claude', organization_name: 'Example Org', plan_tier };
  expect(deriveAuthFileIdentity(file).secondary).toBe(`Example Org · ${expected}`);
  expect(readSeatPlanTier(file)).toBe(expected);
});

test('keeps the Codex plan_type display when it resembles a legacy Claude label', () => {
  const file = { name: 'seat.json', type: 'codex', plan_type: 'Max 20x' };
  expect(deriveAuthFileIdentity(file).secondary).toBe('Max 20x');
});

test.each([[true, 'plan_pro'], [false, 'plan_free']] as const)(
  'keeps the personal plan when Max is false and Pro is %s',
  (has_claude_pro, expected) => {
    expect(resolveClaudePlanType({ account: { has_claude_max: false, has_claude_pro } })).toBe(
      expected
    );
  }
);
