/**
 * What is left of each kind of banked Claude reset, which kind was last spent,
 * the four card states, and the confirm that names the seat and the kind.
 * Rendered to markup with no browser; every fixture is invented.
 */

import { afterEach, beforeEach, describe, expect, spyOn, test } from 'bun:test';
import { readFile } from 'node:fs/promises';
import { createElement } from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import i18n from '@/i18n';
import type { ClaudeLimitResetSeat } from '@/services/api/claudeLimitResets';
import type { SeatFreshness } from '@/features/quota/seatCard';
import { ClaudeResetKindsView } from '@/features/quota/providers/claude/ClaudeResetKinds';
import {
  ClaudeResetConfirmBody,
  ClaudeResetSpendView,
} from '@/features/quota/providers/claude/ClaudeResetSpend';
import {
  claudeResetCardState,
  claudeResetSpendSeatKey,
  getClaudeResetSpend,
  openClaudeResetSpend,
} from '@/features/quota/providers/claude/claudeResetSpendState';

const FABLE_EXPIRY = '2026-12-01T00:00:00Z';
const COOLDOWN_UNTIL = '2099-01-01T00:00:00Z';
const visible: ClaudeLimitResetSeat = {
  name: 'seat-one.json',
  auth_index: 'idx-one',
  credential_generation: '1:1',
  last_good_at: '2026-11-25T11:59:48Z',
  stale: false,
  age_seconds: 12,
  receivedAt: 12_000,
  best_click_rank: null,
  rank_reason: 'holder action only',
  cedar_ember: {
    eligible: true,
    ineligible_reason: null,
    at_limit: true,
    weekly_resets_at: null,
    cooldown_until: null,
    grants: [
      {
        id: 'grant-f1',
        label: 'Launch gift',
        resets_left: 1,
        ends_at: FABLE_EXPIRY,
        type: 'weekly',
        clears: ['seven_day_fable'],
        usable_now: true,
        kind: 'fable',
      },
      {
        id: 'grant-w0',
        label: 'Winter promo',
        resets_left: 1,
        ends_at: '2026-12-10T00:00:00Z',
        type: 'full',
        clears: ['five_hour', 'seven_day'],
        usable_now: false,
        kind: 'full',
        unavailable_reason: 'not_limited',
      },
      {
        id: 'grant-u2',
        label: 'Mystery box',
        resets_left: 2,
        ends_at: null,
        type: 'bonus',
        clears: [],
        usable_now: false,
        kind: 'unknown',
      },
    ],
  },
  kinds: {
    fable: { resets_left: 1, usable_resets: 1, soonest_expiry: FABLE_EXPIRY },
    full: { resets_left: 1, usable_resets: 0, soonest_expiry: null },
    unknown: { resets_left: 2, usable_resets: 0, soonest_expiry: null },
  },
  last_spend: null,
};
const lastSpend = {
  phase: 'answered',
  operation: '0123456789abcdef0123456789abcdef',
  at: '2026-11-24T10:00:00Z',
  auth_index: 'idx-one',
  kind: 'fable',
  grant_id: 'grant-f0',
  named_grant: '',
  request_id: '00000000-0000-4000-8000-000000000009',
  retry: false,
  result: 'reset',
  resets_left_before: { fable: 2 },
  resets_left_after: { fable: 1 },
};
const identity = { primary: 'ops-one@example.test', organization: 'Example Org One', fileName: 'seat-one.json' };
const fresh: SeatFreshness = { kind: 'fresh', ageMs: 5_000 };
const idle = getClaudeResetSpend(claudeResetSpendSeatKey('http://hub.example.test:8317', 'idx-never-opened'));

const text = (html: string) =>
  html
    .replace(/<[^>]*>/g, ' ')
    .replace(/&#x27;/g, "'")
    .replace(/&quot;/g, '"')
    .replace(/&lt;/g, '<')
    .replace(/&gt;/g, '>')
    .replace(/&amp;/g, '&')
    .replace(/\s+/g, ' ');
const face = (props: { seat?: ClaudeLimitResetSeat; failed?: boolean; loaded?: boolean }) =>
  renderToStaticMarkup(createElement(ClaudeResetKindsView, { loaded: true, ...props }));
const details = (props: {
  seat?: ClaudeLimitResetSeat;
  failed?: boolean;
  loaded?: boolean;
  freshness?: SeatFreshness | null;
}) =>
  renderToStaticMarkup(
    createElement(ClaudeResetSpendView, {
      loaded: true,
      identity,
      usage: { freshness: props.freshness === undefined ? fresh : props.freshness, text: 'Updated 5 minutes ago' },
      spend: idle,
      ...props,
    })
  );
const confirmBody = (props: {
  seat?: ClaudeLimitResetSeat;
  failed?: boolean;
  kind?: 'fable' | 'weekly' | 'full' | 'session';
  freshness?: SeatFreshness | null;
  usageText?: string;
}) =>
  text(
    renderToStaticMarkup(
      createElement(ClaudeResetConfirmBody, {
        seat: props.seat,
        failed: props.failed,
        kind: props.kind ?? 'fable',
        identity,
        usage: {
          freshness: props.freshness === undefined ? fresh : props.freshness,
          text: props.usageText ?? 'Updated 5 minutes ago',
        },
        inFlight: false,
      })
    )
  );
const controlTags = (html: string) => html.match(/<button[^>]*data-reset-kind="[^"]*"[^>]*>/g) ?? [];
const control = (html: string, kind: string) =>
  controlTags(html).find((tag) => tag.includes(`data-reset-kind="${kind}"`));

let mocks: { mockRestore(): void }[] = [];
// Restore the language each test found: other files read i18n's global state.
let previousLanguage = i18n.language;
beforeEach(async () => {
  previousLanguage = i18n.language;
  await i18n.changeLanguage('en');
  mocks.push(spyOn(performance, 'now').mockReturnValue(12_000));
});
afterEach(async () => {
  for (const mock of mocks) mock.mockRestore();
  mocks = [];
  await i18n.changeLanguage(previousLanguage);
});

describe('A2: kinds in state V', () => {
  test('three lines in the fixed order with their numbers; weekly and session have none', () => {
    const shown = text(face({ seat: visible }));
    const fable = shown.indexOf('Fable reset (fable): 1 left · 1 usable now');
    const full = shown.indexOf('Full reset (full): 1 left · 0 usable now');
    const unknown = shown.indexOf('Unknown kind (unknown): 2 left · the hub does not spend this kind');
    expect(fable).toBeGreaterThanOrEqual(0);
    expect(full).toBeGreaterThan(fable);
    expect(unknown).toBeGreaterThan(full);
    expect(shown).toContain(`next expires ${new Date(FABLE_EXPIRY).toLocaleString('en')}`);
    expect(shown).toContain('provider labels: Launch gift');
    expect(shown).toContain('provider labels: Winter promo');
    expect(shown).not.toContain('Weekly reset');
    expect(shown).not.toContain('Session reset');
    expect(claudeResetCardState(visible, { loaded: true }).state).toBe('V');
  });

  test('controls: fable enabled, full disabled with its reason, unknown, weekly and session absent', () => {
    const html = details({ seat: visible });
    expect(control(html, 'fable')).toBeDefined();
    expect(control(html, 'fable')).not.toContain('disabled');
    expect(control(html, 'full')).toContain('disabled=""');
    expect(text(html)).toContain('not usable now: not_limited');
    for (const kind of ['unknown', 'weekly', 'session']) expect(control(html, kind)).toBeUndefined();
    expect(controlTags(html)).toHaveLength(2);
    const noReason = {
      ...visible,
      cedar_ember: {
        ...visible.cedar_ember!,
        grants: visible.cedar_ember!.grants.map((grant) => ({ ...grant, unavailable_reason: undefined })),
      },
    };
    expect(text(details({ seat: noReason }))).toContain('none usable now');
  });

  test('a seat in its reset cooldown disables every control with the time (R5)', () => {
    const cooling = { ...visible, cedar_ember: { ...visible.cedar_ember!, cooldown_until: COOLDOWN_UNTIL } };
    const html = details({ seat: cooling });
    expect(control(html, 'fable')).toContain('disabled=""');
    expect(text(html)).toContain(`reset cooldown until ${new Date(COOLDOWN_UNTIL).toLocaleString('en')}`);
  });

  test('a row with no auth index offers no control (the state with fewer controls)', () => {
    expect(controlTags(details({ seat: { ...visible, auth_index: '' } }))).toHaveLength(0);
  });
});

describe('A3: which kind was last used', () => {
  test('a recorded spend, a null record, and an unreadable ledger each have their own line', () => {
    const recorded = text(face({ seat: { ...visible, last_spend: lastSpend } }));
    expect(recorded).toContain(
      `Last spent: Fable reset (fable) at ${new Date(lastSpend.at).toLocaleString('en')}, the provider confirmed it`
    );
    expect(text(face({ seat: visible }))).toContain('Last spent: never through this hub');
    const unreadable = text(
      face({ seat: { ...visible, last_spend: null, last_spend_error: 'the reset spend ledger cannot be read' } })
    );
    expect(unreadable).toContain('Last spent: not known: the hub cannot read its spend record');
    expect(unreadable).not.toContain('never');
  });

  test('a sent record and a not-sent record read as R4 says', () => {
    const sent = text(face({ seat: { ...visible, last_spend: { ...lastSpend, phase: 'sent', result: 'unknown' } } }));
    expect(sent).toContain('sent, outcome not recorded');
    const notSent = text(
      face({
        seat: {
          ...visible,
          last_spend: { ...lastSpend, result: 'not_sent', reason: 'grant_expired_at_dispatch' },
        },
      })
    );
    expect(notSent).toContain('not sent; nothing spent (grant_expired_at_dispatch)');
    const unlisted = text(face({ seat: { ...visible, last_spend: { ...lastSpend, result: 'surprise_word' } } }));
    expect(unlisted).toContain('no answer from the provider; it may have been spent');
  });
});

describe('A4: states N, O and H', () => {
  test('N: a failed read or no matching row renders one plain line and no control', () => {
    expect(text(face({ failed: true }))).toContain("The hub's reset read failed");
    expect(text(face({}))).toContain("The hub's reset read has no row for this seat.");
    expect(text(face({ loaded: false }))).toContain("Waiting for the hub's reset read.");
    expect(controlTags(details({ failed: true }))).toHaveLength(0);
    expect(controlTags(details({}))).toHaveLength(0);
    expect(claudeResetCardState(undefined, { failed: true, loaded: true }).state).toBe('N');
  });

  test('O: a row with no kinds key renders one plain line and no control', () => {
    const old: ClaudeLimitResetSeat = { ...visible };
    delete old.kinds;
    delete old.last_spend;
    expect(text(face({ seat: old }))).toContain("This hub's build cannot spend a reset from the page.");
    expect(text(face({ seat: old }))).not.toContain('never');
    expect(controlTags(details({ seat: old }))).toHaveLength(0);
    expect(claudeResetCardState(old, { loaded: true }).state).toBe('O');
  });

  test('H: the hub read cannot see the grants; four controls, each marked count not known', async () => {
    const fixture = JSON.parse(
      await readFile('../internal/auth/claude/testdata/limit_resets_surface.json', 'utf8')
    );
    for (const seat of [
      { ...visible, cedar_ember: fixture.cedar_ember, kinds: {} },
      { ...visible, cedar_ember: null, kinds: {}, error: 'cedar_ember status unavailable' },
    ]) {
      const shown = text(face({ seat }));
      expect(shown).toContain("The hub's own read does not show this seat's banked resets.");
      const html = details({ seat });
      const tags = controlTags(html);
      expect(tags.map((tag) => tag.match(/data-reset-kind="([^"]*)"/)![1])).toEqual([
        'fable',
        'weekly',
        'full',
        'session',
      ]);
      for (const tag of tags) expect(tag).not.toContain('disabled');
      expect(text(html).split('count not known').length - 1).toBe(4);
      expect(claudeResetCardState(seat, { loaded: true }).state).toBe('H');
    }
    expect(text(face({ seat: { ...visible, cedar_ember: fixture.cedar_ember, kinds: {} } }))).toContain(
      'Not visible on this surface'
    );
  });
});

describe('LEAVE: shapes between states take the state with fewer controls', () => {
  test('kinds null reads as an older hub; an unlisted kind code gets a line and no control', () => {
    expect(claudeResetCardState({ ...visible, kinds: null }, { loaded: true }).state).toBe('O');
    expect(controlTags(details({ seat: { ...visible, kinds: null } }))).toHaveLength(0);
    const extra = {
      ...visible,
      kinds: { ...visible.kinds, zeta_kind: { resets_left: 3, usable_resets: 3, soonest_expiry: null } },
    };
    const shown = text(face({ seat: extra }));
    expect(shown.indexOf('Unknown kind (zeta_kind): 3 left · the hub does not spend this kind')).toBeGreaterThan(
      shown.indexOf('Unknown kind (unknown)')
    );
    expect(control(details({ seat: extra }), 'zeta_kind')).toBeUndefined();
  });

  test('usage never read warns in the confirm as not read yet', () => {
    expect(confirmBody({ seat: visible, freshness: null, usageText: '' })).toContain(
      "This card's usage is old (not read yet)."
    );
  });
});

describe('A5: stale numbers', () => {
  const STALE_COUNTS =
    "These counts are old. The hub reads this seat's resets fresh before it spends, and refuses if it cannot.";
  const STALE_USAGE = 'The hub does NOT read usage again before it spends';

  test('a stale row keeps its numbers, its age, the word stale and its controls', () => {
    for (const props of [{ seat: { ...visible, stale: true } }, { seat: visible, failed: true }]) {
      const shown = text(face(props));
      expect(shown).toContain('Fable reset (fable): 1 left · 1 usable now');
      expect(shown).toContain('12s ago');
      expect(shown).toContain('stale');
      const html = details(props);
      expect(control(html, 'fable')).toBeDefined();
      expect(control(html, 'fable')).not.toContain('disabled');
      expect(confirmBody(props)).toContain(STALE_COUNTS);
    }
    expect(confirmBody({ seat: visible })).not.toContain(STALE_COUNTS);
  });

  test('usage that is failed, stale, paused, pending or queued is named in the confirm; fresh is not', () => {
    const old: SeatFreshness[] = [
      { kind: 'failed', status: 429, ageMs: 600_000 },
      { kind: 'stale', ageMs: 7_200_000 },
      { kind: 'paused' },
      { kind: 'pending' },
      { kind: 'queued' },
    ];
    for (const freshness of old) {
      const shown = confirmBody({ seat: visible, freshness, usageText: `usage ${freshness.kind}` });
      expect(shown).toContain(STALE_USAGE);
      expect(shown).toContain(`This card's usage is old (usage ${freshness.kind}).`);
    }
    expect(confirmBody({ seat: visible, freshness: fresh })).not.toContain(STALE_USAGE);
  });
});

describe('A6: the confirm names the seat and the kind', () => {
  test('email, organization, file, kind and code, what is left with its age, expiry, last spend, cannot be undone', () => {
    const shown = confirmBody({ seat: { ...visible, last_spend: lastSpend } });
    for (const line of [
      'ops-one@example.test',
      'Example Org One',
      'seat-one.json',
      'Fable reset (fable)',
      '1 left · 1 usable now · read 12s ago',
      new Date(FABLE_EXPIRY).toLocaleString('en'),
      'seven_day_fable',
      `Fable reset (fable) at ${new Date(lastSpend.at).toLocaleString('en')}, the provider confirmed it`,
      'This cannot be undone.',
    ])
      expect(shown).toContain(line);
  });

  test('state H says the count is not known', () => {
    const hidden = { ...visible, cedar_ember: null, kinds: {} };
    expect(confirmBody({ seat: hidden, kind: 'session' })).toContain(
      "not known: the hub's read does not show this seat's banked resets"
    );
  });

  test('the open confirm has Cancel and one button naming the act; opening it posts nothing', () => {
    const calls: unknown[] = [];
    const key = claudeResetSpendSeatKey('http://hub.example.test:8317', 'idx-one');
    openClaudeResetSpend(
      key,
      { auth_index: 'idx-one', kind: 'fable' },
      {
        post: (...args) => {
          calls.push(args);
          return Promise.resolve({});
        },
        makeKey: () => 'fixture-key',
      }
    );
    const html = renderToStaticMarkup(
      createElement(ClaudeResetSpendView, {
        seat: visible,
        loaded: true,
        identity,
        usage: { freshness: fresh, text: 'Updated just now' },
        spend: getClaudeResetSpend(key),
      })
    );
    const shown = text(html);
    expect(shown).toContain('Spend one Fable reset?');
    expect(shown).toContain('Cancel');
    expect(shown).toContain('Spend one Fable reset');
    expect(shown).toContain('This cannot be undone.');
    expect(calls).toHaveLength(0);
  });
});

describe('A16: the copy is in six files', () => {
  test('every key of the new block is in every locale, with the same placeholders', async () => {
    const locales = ['en', 'ko', 'ru', 'vi', 'zh-CN', 'zh-TW'];
    const blocks: Record<string, Record<string, string>> = {};
    for (const locale of locales) {
      const json = JSON.parse(await readFile(`src/i18n/locales/${locale}.json`, 'utf8'));
      blocks[locale] = json.claude_reset_spend;
      expect(blocks[locale]).toBeDefined();
    }
    const keys = Object.keys(blocks.en).sort();
    expect(keys.length).toBeGreaterThan(20);
    const placeholders = (value: string) => (value.match(/{{\s*\w+\s*}}/g) ?? []).sort().join(',');
    for (const locale of locales) {
      expect(Object.keys(blocks[locale]).sort()).toEqual(keys);
      for (const key of keys) {
        expect(typeof blocks[locale][key]).toBe('string');
        expect(blocks[locale][key].trim().length).toBeGreaterThan(0);
        expect(placeholders(blocks[locale][key])).toBe(placeholders(blocks.en[key]));
      }
    }
  });
});
