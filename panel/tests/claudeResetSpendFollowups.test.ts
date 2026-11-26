/**
 * Four follow-ups to the reset-spend confirm: after "not known" only the hub's
 * own record of the key settles the click; a confirm that starts to close
 * sends nothing; focus comes back to the seat when the confirm closes; and the
 * press checks that the seat is still the sign-in the confirm opened on.
 *
 * Every fixture is invented; fakes stand in for the hub. Exports these fixes
 * add are reached through namespace imports, so a case that needs one fails
 * on its own before they exist, and every other case runs.
 */

import { afterEach, beforeEach, describe, expect, spyOn, test } from 'bun:test';
import axios, { AxiosError, type AxiosAdapter } from 'axios';
import { readdir, readFile } from 'node:fs/promises';
import { join } from 'node:path';
import { createElement } from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import i18n from '@/i18n';
import { apiClient } from '@/services/api/client';
import * as reads from '@/services/api/claudeLimitResets';
import type { ClaudeLimitResetSeat } from '@/services/api/claudeLimitResets';
import * as api from '@/services/api/claudeResetSpend';
import type { ClaudeResetSpendBody, ClaudeResetSpendPost } from '@/services/api/claudeResetSpend';
import * as spend from '@/features/quota/providers/claude/claudeResetSpendState';
import type { ClaudeResetSpendKind } from '@/features/quota/providers/claude/claudeResetSpendState';
import * as views from '@/features/quota/providers/claude/ClaudeResetSpend';

const HUB = 'http://hub-followups.example.test:8317';
const IN_FLIGHT =
  'a spend with this Idempotency-Key is in flight and its outcome is not recorded yet: deliver it again once it has answered';

type Call = {
  path: string;
  body: unknown;
  config: { headers: Record<string, string>; timeout: number };
};
type Reply = () => Promise<unknown>;

/** A fake hub: each call takes the next reply; with none left it never answers. */
function fakeHub(replies: Reply[] = [], log?: string[]) {
  const calls: Call[] = [];
  const post: ClaudeResetSpendPost = (path, body, config) => {
    calls.push({ path, body, config });
    log?.push('post');
    const next = replies.shift();
    return next ? next() : new Promise(() => {});
  };
  return { post, calls };
}

/** A fake of the check's read: each call takes the next reply; with none left it never answers. */
function fakeRead(replies: Reply[] = [], log?: string[]) {
  let count = 0;
  const read = () => {
    count += 1;
    log?.push('read');
    const next = replies.shift();
    return next ? next() : new Promise<unknown>(() => {});
  };
  return { read, count: () => count };
}

/** What the page's client rejects with: an ApiError with the status and body. */
const refusal = (status: number, data: unknown): Reply => () =>
  Promise.reject(Object.assign(new Error(`HTTP ${status}`), { name: 'ApiError', status, data }));
/** A timeout or a network error: the client's error carries no status. */
const noAnswer: Reply = () =>
  Promise.reject(
    Object.assign(new Error('timeout of 120000ms exceeded'), { name: 'ApiError', code: 'ECONNABORTED' })
  );
const ok = (data: unknown): Reply => () => Promise.resolve(data);

let seatCounter = 0;
/** A fresh seat for each case, so no state crosses tests. */
function freshSeat(kind: ClaudeResetSpendKind = 'fable') {
  seatCounter += 1;
  const authIndex = `idx-follow-${seatCounter}`;
  const body: ClaudeResetSpendBody = { auth_index: authIndex, kind };
  return {
    key: spend.claudeResetSpendSeatKey(HUB, authIndex),
    body,
    fileName: `seat-follow-${seatCounter}.json`,
  };
}
let keyCounter = 0;
const countingKeys = () => `follow-key-${++keyCounter}`;
const keyOf = (call: Call) => call.config.headers['Idempotency-Key'];

const text = (html: string) =>
  html
    .replace(/<[^>]*>/g, ' ')
    .replace(/&#x27;/g, "'")
    .replace(/&quot;/g, '"')
    .replace(/&lt;/g, '<')
    .replace(/&gt;/g, '>')
    .replace(/&amp;/g, '&')
    .replace(/\s+/g, ' ');

const SEAT_NAME = 'ops-one@example.test';
const HEADING = `Fable reset (fable) on ${SEAT_NAME}`;
const outcomeHtml = (key: string) =>
  renderToStaticMarkup(
    createElement(views.ClaudeResetOutcomeView, {
      spend: spend.getClaudeResetSpend(key),
      seatName: SEAT_NAME,
    })
  );
const outcomeText = (key: string) => text(outcomeHtml(key)).trim();

/** The dictated English of SCOPE 1 and SCOPE 4. */
const DICTATED = {
  notFromRecord: (status: number) =>
    `The hub answered this ask with HTTP ${status}, but not from its record of this spend. Whether the reset was spent is still not known.`,
  wordsLead: "The hub's words about this ask only:",
  hold: 'Do not spend again on this seat yet. Press "Ask the hub again" once the hub\'s problem is fixed. If it cannot be fixed, check this account\'s usage page at the provider before any new spend.',
  changed:
    "This seat's sign-in changed after this confirm opened, so it may now be another account. Nothing was sent. Close this and open a new confirm from the card.",
  readFailed:
    "The hub's reset read failed, so the page could not check that this seat is still the same sign-in. Nothing was sent. Press Spend to try again.",
};
/** The page's own lines, as en.json words them. */
const en = (key: string, options?: Record<string, unknown>) => i18n.t(`claude_reset_spend.${key}`, options);

let mocks: { mockRestore(): void }[] = [];
let invalidations = 0;
let releaseListener: () => void = () => {};
// Restore the language each test found: other files read i18n's global state.
let previousLanguage = i18n.language;
beforeEach(async () => {
  previousLanguage = i18n.language;
  await i18n.changeLanguage('en');
  invalidations = 0;
  releaseListener = reads.subscribeClaudeResetCredentials(() => {
    invalidations += 1;
  });
});
afterEach(async () => {
  releaseListener();
  for (const mock of mocks) mock.mockRestore();
  mocks = [];
  await i18n.changeLanguage(previousLanguage);
});

/** Open a confirm on a fresh seat, send it, and let the first delivery go unanswered. */
async function notKnown(replies: Reply[]) {
  const seat = freshSeat();
  const hub = fakeHub([noAnswer, ...replies]);
  expect(spend.openClaudeResetSpend(seat.key, seat.body, { post: hub.post, makeKey: countingKeys })).toBe(true);
  await spend.confirmClaudeResetSpend(seat.key);
  expect(spend.getClaudeResetSpend(seat.key).outcome?.type).toBe('no_answer');
  return { ...seat, hub };
}

describe('A2: a refused second ask keeps "not known"', () => {
  type Row = { name: string; reply: Reply; status: number; error?: string; cure?: string };
  const LEDGER_ERROR =
    'the spend ledger cannot be read or trusted, so earlier claims cannot be ruled out (fixture problem); this request sent nothing';
  const LEDGER_CURE =
    'stop the other hub process that uses this log directory, or give each hub its own log directory; then restart this hub';
  const rows: Row[] = [
    {
      name: '503 a ledger the hub does not trust, with ledger_refusal and cure',
      reply: refusal(503, {
        error: LEDGER_ERROR,
        ledger_refusal: 'ledger_held_by_another_process',
        ledger: '/fixture/logs/claude-limit-reset-spends.jsonl',
        cure: LEDGER_CURE,
      }),
      status: 503,
      error: LEDGER_ERROR,
      cure: LEDGER_CURE,
    },
    {
      name: '403 remote management disabled',
      reply: refusal(403, { error: 'remote management disabled' }),
      status: 403,
      error: 'remote management disabled',
    },
    { name: '404 with a body that is not JSON', reply: refusal(404, '404 page not found'), status: 404 },
    {
      name: '503 the ledger has no directory',
      reply: refusal(503, { error: 'the spend ledger has no directory; nothing spent' }),
      status: 503,
      error: 'the spend ledger has no directory; nothing spent',
    },
    {
      name: "409 the seat's reset cooldown is active",
      reply: refusal(409, { error: "the seat's reset cooldown is active; nothing spent" }),
      status: 409,
      error: "the seat's reset cooldown is active; nothing spent",
    },
    {
      name: '502 fresh grants read failed, upstream_status 429',
      reply: refusal(502, { error: 'fresh grants read failed; nothing spent', upstream_status: 429 }),
      status: 502,
      error: 'fresh grants read failed; nothing spent',
    },
    {
      name: '404 with JSON, Claude seat not found',
      reply: refusal(404, { error: 'Claude seat not found' }),
      status: 404,
      error: 'Claude seat not found',
    },
    {
      name: '400 with JSON',
      reply: refusal(400, { error: 'unknown reset kind', kinds_allowed: ['fable', 'weekly', 'full', 'session'] }),
      status: 400,
      error: 'unknown reset kind',
    },
    { name: '500 that is not JSON', reply: refusal(500, 'Internal Server Error'), status: 500 },
    { name: '200 that is not JSON', reply: ok('<html>proxy page</html>'), status: 200 },
    { name: '200 of {}', reply: ok({}), status: 200 },
    {
      name: "200 object with a result and another seat's auth_index",
      reply: ok({
        auth_index: 'idx-another-seat',
        kind: 'fable',
        grant_id: 'grant-f9',
        request_id: '00000000-0000-4000-8000-0000000000a9',
        retry: false,
        result: 'reset',
      }),
      status: 200,
    },
  ];
  for (const row of rows) {
    test(row.name, async () => {
      const seat = await notKnown([row.reply, noAnswer]);
      invalidations = 0;
      await spend.askClaudeResetSpendAgain(seat.key);
      const entry = spend.getClaudeResetSpend(seat.key);
      const html = outcomeHtml(seat.key);
      const shown = outcomeText(seat.key);
      console.log(`A2 | ${row.name} | busy=${spend.claudeResetSpendBusy(entry)} | ${shown}`);
      expect(spend.claudeResetSpendBusy(entry)).toBe(true);
      expect(entry.inFlight).toBe(false);
      expect(entry.delivery?.key).toBe(keyOf(seat.hub.calls[0]));
      expect(entry.delivery?.body).toEqual(seat.body);
      // The heading, the dictated lines, the hub's words and cure as sent, and the control: nothing else.
      const expected = [
        HEADING,
        DICTATED.notFromRecord(row.status),
        row.error && `${DICTATED.wordsLead} ${row.error}`,
        row.cure && `${en('cure')} ${row.cure}`,
        DICTATED.hold,
        en('ask_again'),
      ]
        .filter(Boolean)
        .join(' ');
      expect(shown).toBe(expected);
      if (row.error) expect(shown).toContain(row.error);
      for (const never of [
        en('not_spent'),
        en('no_route'),
        en('no_answer'),
        en('http_not_known', { status: row.status }),
        en('upstream_429'),
        en('dismiss'),
      ])
        expect(shown).not.toContain(never);
      expect(html).toContain('role="alert"');
      // An HTTP answer drops the cached read, as before.
      expect(invalidations).toBe(1);
      // No new confirm, so no new key, opens on the seat.
      expect(
        spend.openClaudeResetSpend(seat.key, { ...seat.body, kind: 'weekly' }, {
          post: seat.hub.post,
          makeKey: countingKeys,
        })
      ).toBe(false);
      // One more ask delivers the same key and body.
      await spend.askClaudeResetSpendAgain(seat.key);
      expect(seat.hub.calls).toHaveLength(3);
      expect(new Set(seat.hub.calls.map(keyOf)).size).toBe(1);
      for (const call of seat.hub.calls) expect(call.body).toEqual(seat.body);
    });
  }
});

describe("A3: the hub's record settles it", () => {
  type Row = {
    name: string;
    reply: (body: ClaudeResetSpendBody) => Reply;
    type: 'answer' | 'refusal';
    expects: string[];
  };
  const rows: Row[] = [
    {
      name: 'a 200 replay of a reset',
      reply: () =>
        ok({
          replayed: true,
          auth_index: 'idx-fixture',
          kind: 'fable',
          grant_id: 'grant-f1',
          request_id: '00000000-0000-4000-8000-000000000001',
          retry: false,
          result: 'reset',
          at: '2026-11-26T05:00:00Z',
          resets_left_before: { fable: 1 },
          resets_left_after: { fable: 0 },
        }),
      type: 'answer',
      expects: ['The provider confirmed it.', 'This is the recorded outcome of the same click. Nothing new was sent.'],
    },
    {
      name: 'a 409 replay of a claim the hub did not send',
      reply: () =>
        refusal(409, {
          replayed: true,
          error: 'the claim was not sent; nothing spent',
          reason: 'credential_changed_at_dispatch',
          result: 'not_sent',
        }),
      type: 'refusal',
      expects: ['Not spent', 'the claim was not sent; nothing spent'],
    },
    {
      name: "a 200 first answer naming this delivery's own seat and kind, with a request id",
      reply: (body) =>
        ok({
          auth_index: body.auth_index,
          kind: body.kind,
          grant_id: 'grant-f1',
          request_id: '00000000-0000-4000-8000-000000000002',
          retry: false,
          result: 'reset',
          kinds_after: { fable: { resets_left: 0, usable_resets: 0, soonest_expiry: null } },
        }),
      type: 'answer',
      expects: ['The provider confirmed it.', 'Left now:'],
    },
  ];
  for (const row of rows) {
    test(row.name, async () => {
      const seat = freshSeat();
      const hub = fakeHub([noAnswer, row.reply(seat.body)]);
      spend.openClaudeResetSpend(seat.key, seat.body, { post: hub.post, makeKey: countingKeys });
      await spend.confirmClaudeResetSpend(seat.key);
      await spend.askClaudeResetSpendAgain(seat.key);
      const entry = spend.getClaudeResetSpend(seat.key);
      const shown = outcomeText(seat.key);
      console.log(`A3 | ${row.name} | busy=${spend.claudeResetSpendBusy(entry)} | ${shown}`);
      expect(entry.outcome?.type).toBe(row.type);
      expect(spend.claudeResetSpendBusy(entry)).toBe(false);
      for (const line of row.expects) expect(shown).toContain(line);
      expect(shown).toContain(en('dismiss'));
      expect(shown).not.toContain(en('ask_again'));
      expect(shown).not.toContain(DICTATED.wordsLead);
    });
  }
});

describe('A4: no answer, a key in flight and a 401 stay "not known" as before', () => {
  const rows: { name: string; reply: Reply; type: string }[] = [
    { name: 'no answer again', reply: noAnswer, type: 'no_answer' },
    { name: 'the 409 that says the key is in flight', reply: refusal(409, { error: IN_FLIGHT }), type: 'key_in_flight' },
    { name: 'a 401', reply: refusal(401, { error: 'Invalid management key' }), type: 'no_answer' },
  ];
  for (const row of rows) {
    test(row.name, async () => {
      const seat = await notKnown([row.reply, noAnswer]);
      await spend.askClaudeResetSpendAgain(seat.key);
      const entry = spend.getClaudeResetSpend(seat.key);
      const shown = outcomeText(seat.key);
      console.log(`A4 | ${row.name} | type=${entry.outcome?.type} | ${shown}`);
      expect(entry.outcome?.type).toBe(row.type);
      expect(spend.claudeResetSpendBusy(entry)).toBe(true);
      expect(shown).toContain(en('no_answer'));
      expect(shown).toContain(en('ask_again'));
      expect(shown).not.toContain(en('dismiss'));
      await spend.askClaudeResetSpendAgain(seat.key);
      expect(seat.hub.calls).toHaveLength(3);
      expect(new Set(seat.hub.calls.map(keyOf)).size).toBe(1);
    });
  }
});

describe('A6: Spend is dead while the confirm closes (the store)', () => {
  test('a confirm marked closing sends nothing however often it is pressed; the close ends it and its key', async () => {
    const seat = freshSeat();
    const hub = fakeHub([ok({ replayed: true, result: 'reset' })]);
    const check = fakeRead([ok({ seats: [], ranking_rule: 'fixture' })]);
    spend.openClaudeResetSpend(seat.key, seat.body, { post: hub.post, makeKey: countingKeys });
    const first = spend.getClaudeResetSpend(seat.key).delivery?.key;
    spend.markClaudeResetSpendClosing(seat.key);
    expect(spend.getClaudeResetSpend(seat.key).closing).toBe(true);
    const file = { name: seat.fileName, credential_generation: '1:1' };
    await Promise.all([
      spend.confirmClaudeResetSpend(seat.key),
      spend.confirmClaudeResetSpend(seat.key),
      spend.confirmClaudeResetSpend(seat.key),
      spend.confirmClaudeResetSpendChecked(seat.key, { post: hub.post, read: check.read, file }),
      spend.confirmClaudeResetSpendChecked(seat.key, { post: hub.post, read: check.read, file }),
      spend.confirmClaudeResetSpendChecked(seat.key, { post: hub.post, read: check.read, file }),
    ]);
    const entry = spend.getClaudeResetSpend(seat.key);
    console.log(`A6 | marked closing, six presses | posts=${hub.calls.length} | reads=${check.count()} | inFlight=${entry.inFlight}`);
    expect(hub.calls).toHaveLength(0);
    expect(check.count()).toBe(0);
    expect(entry.inFlight).toBe(false);
    expect(entry.confirmOpen).toBe(true);
    // The dialog's own close, at the end of its fade.
    spend.cancelClaudeResetSpend(seat.key);
    expect(spend.getClaudeResetSpend(seat.key)).toEqual({ confirmOpen: false, inFlight: false });
    expect(spend.openClaudeResetSpend(seat.key, seat.body, { post: hub.post, makeKey: countingKeys })).toBe(true);
    const second = spend.getClaudeResetSpend(seat.key).delivery?.key;
    console.log(`A6 | the next confirm | first key=${first} | second key=${second}`);
    expect(second).not.toBe(first);
    expect(spend.getClaudeResetSpend(seat.key).closing).toBeFalsy();
  });

  test('a confirm whose send is in flight is not marked closing', async () => {
    const seat = freshSeat();
    const hub = fakeHub();
    spend.openClaudeResetSpend(seat.key, seat.body, { post: hub.post, makeKey: countingKeys });
    void spend.confirmClaudeResetSpend(seat.key);
    await Promise.resolve();
    spend.markClaudeResetSpendClosing(seat.key);
    const entry = spend.getClaudeResetSpend(seat.key);
    expect(entry.inFlight).toBe(true);
    expect(entry.closing).toBeFalsy();
    expect(hub.calls).toHaveLength(1);
  });
});

const visibleSeat = (name: string, authIndex: string, generation: string): ClaudeLimitResetSeat => ({
  name,
  auth_index: authIndex,
  credential_generation: generation,
  last_good_at: '2026-11-26T05:59:48Z',
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
        ends_at: '2026-12-01T00:00:00Z',
        type: 'weekly',
        clears: ['seven_day_fable'],
        usable_now: true,
        kind: 'fable',
      },
    ],
  },
  kinds: { fable: { resets_left: 1, usable_resets: 1, soonest_expiry: '2026-12-01T00:00:00Z' } },
  last_spend: null,
});
const OPENED_IDENTITY = { primary: 'ops-one@example.test', organization: 'Example Org One' };
const PRESENT_IDENTITY = { primary: 'ops-two@example.test', organization: 'Example Org Two' };
const usage = { freshness: { kind: 'fresh' as const, ageMs: 5_000 }, text: 'Updated just now' };

function renderView(key: string, row: ClaudeLimitResetSeat | undefined, identity: { primary: string; organization: string; fileName: string }) {
  return renderToStaticMarkup(
    createElement(views.ClaudeResetSpendView, {
      seat: row,
      loaded: true,
      identity,
      usage,
      spend: spend.getClaudeResetSpend(key),
    })
  );
}
/** The confirm's two footer buttons, as drawn. */
function confirmButtons(html: string) {
  const footer = html.slice(html.indexOf('modal-footer'));
  return {
    cancel: footer.match(/<button[^>]*class="btn btn-secondary"[^>]*>/)?.[0] ?? '',
    spend: footer.match(/<button[^>]*class="btn btn-danger"[^>]*>/)?.[0] ?? '',
  };
}

describe('A7: Spend is dead while the confirm closes (the markup)', () => {
  test('a confirm marked closing draws Spend and Cancel disabled; the same confirm not marked draws both live', () => {
    const seat = freshSeat();
    const row = visibleSeat(seat.fileName, seat.body.auth_index, '1:1');
    const identity = { ...OPENED_IDENTITY, fileName: seat.fileName };
    spend.openClaudeResetSpend(seat.key, seat.body, { post: fakeHub().post, makeKey: countingKeys });
    const live = confirmButtons(renderView(seat.key, row, identity));
    expect(live.spend).not.toBe('');
    expect(live.cancel).not.toBe('');
    expect(live.spend).not.toContain('disabled=""');
    expect(live.cancel).not.toContain('disabled=""');
    spend.markClaudeResetSpendClosing(seat.key);
    const closing = confirmButtons(renderView(seat.key, row, identity));
    console.log(`A7 | not marked | ${live.cancel} ${live.spend}`);
    console.log(`A7 | marked closing | ${closing.cancel} ${closing.spend}`);
    expect(closing.spend).toContain('disabled=""');
    expect(closing.cancel).toContain('disabled=""');
  });

  // The dialog restarts a close in its fade whenever its callbacks change identity, and marking
  // the confirm closing re-renders the view. Only a DOM shows the close itself; this pins the wire.
  test('the dialog gets close callbacks that never change, so marking it closing does not restart its close', async () => {
    const view = await readFile(join('src', 'features', 'quota', 'providers', 'claude', 'ClaudeResetSpend.tsx'), 'utf8');
    expect(view).toContain('onClose={closeConfirm}');
    expect(view).toContain('onCloseStart={confirmClosing}');
    expect(view).toMatch(/const closeConfirm = useCallback\([^\n]*, \[\]\);/);
    expect(view).toMatch(/const confirmClosing = useCallback\([^\n]*, \[\]\);/);
  });
});

describe('A8: where focus goes when the confirm closes', () => {
  type Row = {
    closed: string;
    outcome: api.ClaudeResetSpendOutcome | undefined;
    kindButton: 'live' | 'disabled' | 'gone';
    target: string;
  };
  const answer = { type: 'answer', answer: { result: 'reset', request_id: 'r-1' } } as const;
  const refused = { type: 'refusal', status: 409, refusal: { error: 'fixture refusal; nothing spent' } } as const;
  const lost = { type: 'no_answer' } as const;
  const rows: Row[] = [];
  for (const closed of ['Cancel', 'Escape', 'the X', 'a confirm the check refused, then closed']) {
    rows.push({ closed, outcome: undefined, kindButton: 'live', target: 'kind_button' });
    rows.push({ closed, outcome: undefined, kindButton: 'gone', target: 'heading' });
    rows.push({ closed, outcome: undefined, kindButton: 'disabled', target: 'heading' });
  }
  for (const [closed, outcome] of [
    ['a send with a known answer', answer],
    ['a send with a refusal', refused],
    ['a send with no answer', lost],
  ] as const) {
    for (const kindButton of ['live', 'disabled', 'gone'] as const)
      rows.push({ closed, outcome, kindButton, target: 'outcome' });
  }
  for (const row of rows) {
    test(`${row.closed}, the kind's button ${row.kindButton}`, () => {
      const target = spend.claudeResetFocusAfterClose({ outcome: row.outcome, kindButton: row.kindButton });
      console.log(`A8 | ${row.closed} | kind button ${row.kindButton} | focus=${target}`);
      expect(target).toBe(row.target);
      // Only elements of this seat's own section: never the page body, never another seat.
      expect(['kind_button', 'heading', 'outcome']).toContain(target);
    });
  }

  test('the heading and the outcome block take focus by script only; the kind buttons keep data-reset-kind', async () => {
    const seat = freshSeat();
    const hub = fakeHub([noAnswer]);
    spend.openClaudeResetSpend(seat.key, seat.body, { post: hub.post, makeKey: countingKeys });
    await spend.confirmClaudeResetSpend(seat.key);
    const html = renderView(seat.key, visibleSeat(seat.fileName, seat.body.auth_index, '1:1'), {
      ...OPENED_IDENTITY,
      fileName: seat.fileName,
    });
    const heading = html.match(/<h3[^>]*>[^<]*<\/h3>/)?.[0] ?? '';
    const block = html.match(/<div[^>]*role="alert"[^>]*>/)?.[0] ?? '';
    const kindButton = html.match(/<button[^>]*data-reset-kind="fable"[^>]*>/)?.[0] ?? '';
    console.log(`A8 | markup | ${heading} | ${block.slice(0, 60)} | ${kindButton}`);
    expect(heading).toContain('Banked resets');
    expect(heading).toContain('tabindex="-1"');
    expect(block).toContain('tabindex="-1"');
    expect(kindButton).not.toBe('');
    expect(kindButton).not.toContain('tabindex');
  });
});

describe('A9: the press checks the sign-in', () => {
  const readOf = (...rows: ClaudeLimitResetSeat[]) => ok({ seats: rows, ranking_rule: 'fixture' });
  /** A confirm opened on a row with generation "1:1", as the card opens it. */
  function openChecked(log?: string[]) {
    const seat = freshSeat();
    const hub = fakeHub([ok({ replayed: true, result: 'reset' })], log);
    const opened = spend.openClaudeResetSpend(seat.key, seat.body, {
      post: hub.post,
      makeKey: countingKeys,
      file: { hub: HUB, name: seat.fileName },
      opened: {
        fileName: seat.fileName,
        generation: '1:1',
        identity: { ...OPENED_IDENTITY, fileName: seat.fileName },
      },
    });
    expect(opened).toBe(true);
    return { ...seat, hub, key0: spend.getClaudeResetSpend(seat.key).delivery?.key };
  }
  type Row = {
    name: string;
    card: string;
    read: (fileName: string, authIndex: string) => Reply;
  };
  const refusedRows: Row[] = [
    {
      name: 'G1: the read lists the file with generation 2:1 (another account took its place)',
      card: '1:1',
      read: (file, index) => readOf(visibleSeat(file, index, '2:1')),
    },
    {
      name: 'the read lists the file with another auth_index',
      card: '1:1',
      read: (file) => readOf(visibleSeat(file, 'idx-elsewhere', '1:1')),
    },
    {
      name: 'the read has no row of that file name',
      card: '1:1',
      read: () => readOf(visibleSeat('seat-someone-else.json', 'idx-someone-else', '1:1')),
    },
    {
      name: "G1b: the card's file entry carries 2:1 while the read still says 1:1",
      card: '2:1',
      read: (file, index) => readOf(visibleSeat(file, index, '1:1')),
    },
  ];
  for (const row of refusedRows) {
    test(row.name, async () => {
      const seat = openChecked();
      const check = fakeRead([row.read(seat.fileName, seat.body.auth_index), row.read(seat.fileName, seat.body.auth_index)]);
      const file = { name: seat.fileName, credential_generation: row.card };
      await spend.confirmClaudeResetSpendChecked(seat.key, { post: seat.hub.post, read: check.read, file });
      const entry = spend.getClaudeResetSpend(seat.key);
      // The card now shows another account at that file; the confirm keeps what it opened with.
      const html = renderView(seat.key, undefined, { ...PRESENT_IDENTITY, fileName: seat.fileName });
      const shown = text(html);
      const buttons = confirmButtons(html);
      console.log(
        `A9 | ${row.name} | posts=${seat.hub.calls.length} | reads=${check.count()} | open=${entry.confirmOpen} | spend disabled=${buttons.spend.includes('disabled=""')} | invalidations=${invalidations}`
      );
      expect(seat.hub.calls).toHaveLength(0);
      expect(entry.confirmOpen).toBe(true);
      expect(entry.inFlight).toBe(false);
      expect(buttons.spend).toContain('disabled=""');
      expect(buttons.cancel).not.toContain('disabled=""');
      expect(shown).toContain(DICTATED.changed);
      expect(html).toMatch(/<p role="alert">[^<]*sign-in changed/);
      expect(shown).toContain(`Seat ${OPENED_IDENTITY.primary}`);
      expect(shown).toContain(`Organization ${OPENED_IDENTITY.organization}`);
      expect(shown).toContain(`File ${seat.fileName}`);
      expect(shown).not.toContain(PRESENT_IDENTITY.primary);
      expect(shown).not.toContain(PRESENT_IDENTITY.organization);
      // A refusal by the check drops the page's cached read, so the card reads again.
      expect(invalidations).toBe(1);
      // Nothing is sent under that key later either.
      const good = fakeRead([readOf(visibleSeat(seat.fileName, seat.body.auth_index, '1:1'))]);
      await spend.confirmClaudeResetSpendChecked(seat.key, {
        post: seat.hub.post,
        read: good.read,
        file: { name: seat.fileName, credential_generation: '1:1' },
      });
      await spend.confirmClaudeResetSpend(seat.key);
      expect(seat.hub.calls).toHaveLength(0);
    });
  }

  test('the read fails: nothing is sent, Spend stays live, and a second press with a good read sends one request under the key of the open', async () => {
    const seat = openChecked();
    const file = { name: seat.fileName, credential_generation: '1:1' };
    const check = fakeRead([
      refusal(500, { error: 'fixture read failure' }),
      readOf(visibleSeat(seat.fileName, seat.body.auth_index, '1:1')),
    ]);
    await spend.confirmClaudeResetSpendChecked(seat.key, { post: seat.hub.post, read: check.read, file });
    const entry = spend.getClaudeResetSpend(seat.key);
    const html = renderView(seat.key, visibleSeat(seat.fileName, seat.body.auth_index, '1:1'), {
      ...OPENED_IDENTITY,
      fileName: seat.fileName,
    });
    const buttons = confirmButtons(html);
    console.log(
      `A9 | the read fails | posts=${seat.hub.calls.length} | reads=${check.count()} | open=${entry.confirmOpen} | spend disabled=${buttons.spend.includes('disabled=""')} | invalidations=${invalidations}`
    );
    expect(seat.hub.calls).toHaveLength(0);
    expect(entry.confirmOpen).toBe(true);
    expect(entry.inFlight).toBe(false);
    expect(text(html)).toContain(DICTATED.readFailed);
    expect(text(html)).not.toContain(DICTATED.changed);
    expect(buttons.spend).not.toContain('disabled=""');
    await spend.confirmClaudeResetSpendChecked(seat.key, { post: seat.hub.post, read: check.read, file });
    console.log(`A9 | the read fails, then a good read | posts=${seat.hub.calls.length} | key=${seat.hub.calls[0] && keyOf(seat.hub.calls[0])} | key of the open=${seat.key0}`);
    expect(check.count()).toBe(2);
    expect(seat.hub.calls).toHaveLength(1);
    expect(keyOf(seat.hub.calls[0])).toBe(seat.key0 as string);
    expect(seat.hub.calls[0].body).toEqual(seat.body);
    expect(spend.getClaudeResetSpend(seat.key).outcome?.type).toBe('answer');
  });

  test('nothing changed: exactly one read and then one POST leave, in that order, with the body and key of the open', async () => {
    const log: string[] = [];
    const seat = openChecked(log);
    const check = fakeRead([readOf(visibleSeat(seat.fileName, seat.body.auth_index, '1:1'))], log);
    await spend.confirmClaudeResetSpendChecked(seat.key, {
      post: seat.hub.post,
      read: check.read,
      file: { name: seat.fileName, credential_generation: '1:1' },
    });
    console.log(`A9 | nothing changed | order=${log.join(',')} | key=${keyOf(seat.hub.calls[0])} | key of the open=${seat.key0}`);
    expect(log).toEqual(['read', 'post']);
    expect(seat.hub.calls).toHaveLength(1);
    expect(keyOf(seat.hub.calls[0])).toBe(seat.key0 as string);
    expect(seat.hub.calls[0].body).toEqual(seat.body);
    expect(invalidations).toBe(1); // the answer's own drop, as before
  });

  test('the check in flight reads as a send in flight: the waiting line, every button off, no close', async () => {
    const seat = openChecked();
    const check = fakeRead();
    void spend.confirmClaudeResetSpendChecked(seat.key, {
      post: seat.hub.post,
      read: check.read,
      file: { name: seat.fileName, credential_generation: '1:1' },
    });
    await Promise.resolve();
    const html = renderView(seat.key, visibleSeat(seat.fileName, seat.body.auth_index, '1:1'), {
      ...OPENED_IDENTITY,
      fileName: seat.fileName,
    });
    const buttons = confirmButtons(html);
    expect(text(html)).toContain(en('waiting'));
    expect(buttons.spend).toContain('disabled=""');
    expect(buttons.cancel).toContain('disabled=""');
    expect(html).toMatch(/<button[^>]*class="modal-close-floating"[^>]*disabled=""/);
    spend.cancelClaudeResetSpend(seat.key);
    spend.markClaudeResetSpendClosing(seat.key);
    expect(spend.getClaudeResetSpend(seat.key).confirmOpen).toBe(true);
    expect(spend.getClaudeResetSpend(seat.key).closing).toBeFalsy();
    expect(seat.hub.calls).toHaveLength(0);
  });

  test('a confirm the card opened cannot reach the POST of its first delivery without the check', async () => {
    const seat = openChecked();
    await spend.confirmClaudeResetSpend(seat.key);
    await spend.confirmClaudeResetSpend(seat.key, seat.hub.post);
    expect(seat.hub.calls).toHaveLength(0);
    expect(spend.getClaudeResetSpend(seat.key).inFlight).toBe(false);

    // The source: the confirm's Spend button calls the checked press, and no file
    // under src but the store names the unchecked one or the send itself.
    const view = await readFile(join('src', 'features', 'quota', 'providers', 'claude', 'ClaudeResetSpend.tsx'), 'utf8');
    expect(view).toMatch(/onConfirm=\{\(\) =>[\s\S]{0,400}?confirmClaudeResetSpendChecked\(key,/);
    const unchecked = /\bconfirmClaudeResetSpend\b(?!Checked)/;
    const holders: string[] = [];
    const senders: string[] = [];
    async function scan(dir: string): Promise<void> {
      for (const entry of await readdir(dir, { withFileTypes: true })) {
        const path = join(dir, entry.name);
        if (entry.isDirectory()) {
          await scan(path);
          continue;
        }
        const source = await readFile(path, 'utf8');
        if (unchecked.test(source)) holders.push(path);
        if (source.includes('sendClaudeResetSpend(')) senders.push(path);
      }
    }
    await scan('src');
    console.log(`A9 | scan | unchecked confirm named in: ${holders.join(', ')} | send named in: ${senders.join(', ')}`);
    expect(holders).toEqual([join('src', 'features', 'quota', 'providers', 'claude', 'claudeResetSpendState.ts')]);
    expect(senders.sort()).toEqual(
      [
        join('src', 'features', 'quota', 'providers', 'claude', 'claudeResetSpendState.ts'),
        join('src', 'services', 'api', 'claudeResetSpend.ts'),
      ].sort()
    );
  });
});

describe('A10: one click, one key holds under the check', () => {
  test("three presses in the same moment while the check's read is in flight: one read, one POST, one key", async () => {
    const seat = freshSeat();
    const hub = fakeHub([ok({ replayed: true, result: 'reset' })]);
    spend.openClaudeResetSpend(seat.key, seat.body, {
      post: hub.post,
      makeKey: countingKeys,
      opened: { fileName: seat.fileName, generation: '1:1', identity: { ...OPENED_IDENTITY, fileName: seat.fileName } },
    });
    let answerRead: (value: unknown) => void = () => {};
    let reads = 0;
    const read = () => {
      reads += 1;
      return new Promise<unknown>((resolve) => {
        answerRead = resolve;
      });
    };
    const file = { name: seat.fileName, credential_generation: '1:1' };
    const presses = [1, 2, 3].map(() => spend.confirmClaudeResetSpendChecked(seat.key, { post: hub.post, read, file }));
    answerRead({ seats: [visibleSeat(seat.fileName, seat.body.auth_index, '1:1')], ranking_rule: 'fixture' });
    await Promise.all(presses);
    console.log(`A10 | three presses in one moment | reads=${reads} | posts=${hub.calls.length} | distinct keys=${new Set(hub.calls.map(keyOf)).size}`);
    expect(reads).toBe(1);
    expect(hub.calls).toHaveLength(1);
    expect(new Set(hub.calls.map(keyOf)).size).toBe(1);
  });

  test('a press, a refusal by the check, a close and a new confirm: a new key, and the refused key was never sent', async () => {
    const seat = freshSeat();
    const hub = fakeHub([ok({ replayed: true, result: 'reset' })]);
    const opened = { fileName: seat.fileName, generation: '1:1', identity: { ...OPENED_IDENTITY, fileName: seat.fileName } };
    spend.openClaudeResetSpend(seat.key, seat.body, { post: hub.post, makeKey: countingKeys, opened });
    const refusedKey = spend.getClaudeResetSpend(seat.key).delivery?.key;
    const check = fakeRead([ok({ seats: [visibleSeat(seat.fileName, seat.body.auth_index, '2:1')], ranking_rule: 'fixture' })]);
    await spend.confirmClaudeResetSpendChecked(seat.key, {
      post: hub.post,
      read: check.read,
      file: { name: seat.fileName, credential_generation: '1:1' },
    });
    expect(spend.getClaudeResetSpend(seat.key).check).toBe('changed');
    spend.cancelClaudeResetSpend(seat.key);
    spend.openClaudeResetSpend(seat.key, seat.body, {
      post: hub.post,
      makeKey: countingKeys,
      opened: { ...opened, generation: '2:1' },
    });
    const newKey = spend.getClaudeResetSpend(seat.key).delivery?.key;
    const good = fakeRead([ok({ seats: [visibleSeat(seat.fileName, seat.body.auth_index, '2:1')], ranking_rule: 'fixture' })]);
    await spend.confirmClaudeResetSpendChecked(seat.key, {
      post: hub.post,
      read: good.read,
      file: { name: seat.fileName, credential_generation: '2:1' },
    });
    console.log(`A10 | refused, closed, new confirm | refused key=${refusedKey} | new key=${newKey} | sent keys=${hub.calls.map(keyOf).join(',')}`);
    expect(newKey).not.toBe(refusedKey);
    expect(hub.calls.map(keyOf)).toEqual([newKey as string]);
  });
});

describe('A11: "Ask the hub again" makes no check', () => {
  test('after "not known" it delivers with no read before it, even when the read would now show another generation', async () => {
    const seat = freshSeat();
    const hub = fakeHub([noAnswer, ok({ replayed: true, result: 'reset' })]);
    spend.openClaudeResetSpend(seat.key, seat.body, {
      post: hub.post,
      makeKey: countingKeys,
      opened: { fileName: seat.fileName, generation: '1:1', identity: { ...OPENED_IDENTITY, fileName: seat.fileName } },
    });
    const check = fakeRead([
      ok({ seats: [visibleSeat(seat.fileName, seat.body.auth_index, '1:1')], ranking_rule: 'fixture' }),
      ok({ seats: [visibleSeat(seat.fileName, seat.body.auth_index, '2:1')], ranking_rule: 'fixture' }),
    ]);
    await spend.confirmClaudeResetSpendChecked(seat.key, {
      post: hub.post,
      read: check.read,
      file: { name: seat.fileName, credential_generation: '1:1' },
    });
    expect(spend.getClaudeResetSpend(seat.key).outcome?.type).toBe('no_answer');
    const pageGet = spyOn(apiClient, 'get');
    mocks.push(pageGet);
    const capture = spyOn(apiClient, 'captureConnection');
    mocks.push(capture);
    await spend.askClaudeResetSpendAgain(seat.key, hub.post);
    console.log(`A11 | ask again | posts=${hub.calls.length} | reads=${check.count()} | page gets=${pageGet.mock.calls.length}`);
    expect(hub.calls).toHaveLength(2);
    expect(check.count()).toBe(1);
    expect(pageGet).not.toHaveBeenCalled();
    expect(capture).not.toHaveBeenCalled();
    expect(spend.getClaudeResetSpend(seat.key).outcome?.type).toBe('answer');
    const view = await readFile(join('src', 'features', 'quota', 'providers', 'claude', 'ClaudeResetSpend.tsx'), 'utf8');
    expect(view).toContain('onAskAgain={() => void askClaudeResetSpendAgain(key, defaultClaudeResetSpendPost())}');
  });
});

describe('A12: the copy is in six files', () => {
  const NEW_KEYS = ['ask_not_from_record', 'ask_words', 'ask_hold', 'check_changed', 'check_read_failed'];
  test('every new key is in all six files, with the same placeholders, and no other file keeps the English', async () => {
    const locales = ['en', 'ko', 'ru', 'vi', 'zh-CN', 'zh-TW'];
    const blocks: Record<string, Record<string, string>> = {};
    for (const locale of locales)
      blocks[locale] = JSON.parse(await readFile(`src/i18n/locales/${locale}.json`, 'utf8')).claude_reset_spend;
    const placeholders = (value: string) => (value.match(/{{\s*\w+\s*}}/g) ?? []).sort().join(',');
    for (const key of NEW_KEYS) {
      for (const locale of locales) {
        const value = blocks[locale]?.[key];
        expect(typeof value).toBe('string');
        expect(value.trim().length).toBeGreaterThan(0);
        expect(placeholders(value)).toBe(placeholders(blocks.en[key]));
        if (locale !== 'en') expect(value).not.toBe(blocks.en[key]);
      }
    }
    expect(i18n.t('claude_reset_spend.ask_not_from_record', { status: 503 })).toBe(DICTATED.notFromRecord(503));
    expect(i18n.t('claude_reset_spend.ask_words')).toBe(DICTATED.wordsLead);
    expect(i18n.t('claude_reset_spend.ask_hold')).toBe(DICTATED.hold);
    expect(i18n.t('claude_reset_spend.check_changed')).toBe(DICTATED.changed);
    expect(i18n.t('claude_reset_spend.check_read_failed')).toBe(DICTATED.readFailed);
  });
});

describe("the check's read: the POST's own connection, never the page's cache", () => {
  test('one captured connection carries the read and the post; the read neither comes from nor fills the cache', async () => {
    const gets: string[] = [];
    const posts: unknown[][] = [];
    const captured = {
      get: (path: string) => {
        gets.push(path);
        return Promise.resolve({ seats: [], ranking_rule: 'fixture' });
      },
      post: (...args: unknown[]) => {
        posts.push(args);
        return Promise.resolve({});
      },
    };
    mocks.push(spyOn(apiClient, 'getConnectionRevision').mockReturnValue(500_001));
    const pageGet = spyOn(apiClient, 'get').mockImplementation(
      async () => ({ seats: [], ranking_rule: 'fixture' }) as never
    );
    mocks.push(pageGet);
    reads.invalidateClaudeLimitResets();
    await reads.readClaudeLimitResets(); // the page's cache is now full
    expect(pageGet).toHaveBeenCalledTimes(1);
    const capture = spyOn(apiClient, 'captureConnection').mockReturnValue(captured as never);
    mocks.push(capture);
    const connection = api.defaultClaudeResetSpendConnection();
    expect(capture).toHaveBeenCalledTimes(1);
    await connection.read();
    expect(gets).toEqual(['/claude/limit-resets']); // not served from the full cache
    reads.invalidateClaudeLimitResets();
    await connection.read();
    await reads.readClaudeLimitResets();
    expect(pageGet).toHaveBeenCalledTimes(2); // the check's read did not fill it
    await connection.post('/claude/limit-resets/spend', { auth_index: 'idx-x', kind: 'full' }, {
      headers: { 'Idempotency-Key': 'k-1' },
      timeout: 120_000,
    });
    expect(posts).toHaveLength(1);
    expect(capture).toHaveBeenCalledTimes(1);
  });
});

describe('round 1 class (Codex): only an HTTP 200 settles a later ask', () => {
  // The page's client hands back only the body of any 2xx. Here the real client runs over a
  // fake transport that keeps the status and settles as axios's own adapters do (validateStatus).
  type Wire = { status: number; data: unknown } | 'no answer';
  function transport(replies: Wire[]): AxiosAdapter {
    return (config) =>
      new Promise((resolve, reject) => {
        const reply = replies.shift() ?? 'no answer';
        if (reply === 'no answer') {
          reject(new AxiosError('timeout of 120000ms exceeded', 'ECONNABORTED', config));
          return;
        }
        const response = { data: reply.data, status: reply.status, statusText: '', headers: {}, config, request: {} };
        if (!config.validateStatus || config.validateStatus(reply.status)) resolve(response);
        else
          reject(
            new AxiosError(
              `Request failed with status code ${reply.status}`,
              reply.status >= 400 && reply.status < 500 ? AxiosError.ERR_BAD_REQUEST : AxiosError.ERR_BAD_RESPONSE,
              config,
              {},
              response
            )
          );
      });
  }
  const own = (body: ClaudeResetSpendBody) => ({
    auth_index: body.auth_index,
    kind: body.kind,
    grant_id: 'grant-f1',
    request_id: '00000000-0000-4000-8000-0000000000c1',
    retry: false,
    result: 'reset',
  });
  type Row = { name: string; wire: (body: ClaudeResetSpendBody) => Wire; settles: boolean; status?: number };
  const rows: Row[] = [
    { name: "202 naming this delivery's seat and kind, with a request id", wire: (body) => ({ status: 202, data: own(body) }), settles: false, status: 202 },
    { name: '202 marked replayed', wire: (body) => ({ status: 202, data: { ...own(body), replayed: true } }), settles: false, status: 202 },
    { name: '203 marked replayed', wire: (body) => ({ status: 203, data: { ...own(body), replayed: true } }), settles: false, status: 203 },
    { name: '201 marked replayed, with an error string', wire: () => ({ status: 201, data: { replayed: true, error: 'fixture words; nothing spent' } }), settles: false, status: 201 },
    { name: '204 with no body', wire: () => ({ status: 204, data: '' }), settles: false, status: 204 },
    { name: "200 naming this delivery's seat and kind, with a request id", wire: (body) => ({ status: 200, data: own(body) }), settles: true },
    { name: '200 marked replayed', wire: (body) => ({ status: 200, data: { ...own(body), replayed: true } }), settles: true },
    { name: '409 marked replayed, the claim not sent', wire: () => ({ status: 409, data: { replayed: true, error: 'the claim was not sent; nothing spent' } }), settles: true },
  ];
  let previousAdapter: typeof axios.defaults.adapter;
  beforeEach(() => {
    previousAdapter = axios.defaults.adapter;
    apiClient.setConfig({ apiBase: 'http://hub-status.example.test:8317', managementKey: 'fixture-key' });
  });
  afterEach(() => {
    axios.defaults.adapter = previousAdapter;
    apiClient.setConfig({ apiBase: '', managementKey: '' });
  });
  for (const row of rows) {
    test(`after "not known", ${row.name} ${row.settles ? 'settles' : 'stays not known'}`, async () => {
      const seat = freshSeat();
      axios.defaults.adapter = transport(['no answer', row.wire(seat.body)]);
      spend.openClaudeResetSpend(seat.key, seat.body, { post: api.defaultClaudeResetSpendPost(), makeKey: countingKeys });
      await spend.confirmClaudeResetSpend(seat.key);
      expect(spend.getClaudeResetSpend(seat.key).outcome?.type).toBe('no_answer');
      await spend.askClaudeResetSpendAgain(seat.key, api.defaultClaudeResetSpendPost());
      const entry = spend.getClaudeResetSpend(seat.key);
      const shown = outcomeText(seat.key);
      console.log(`R1 | ${row.name} | type=${entry.outcome?.type} | busy=${spend.claudeResetSpendBusy(entry)} | ${shown}`);
      expect(spend.claudeResetSpendBusy(entry)).toBe(!row.settles);
      if (!row.settles) {
        expect(shown).toContain(DICTATED.notFromRecord(row.status as number));
        expect(shown).toContain(en('ask_again'));
        expect(shown).not.toContain('The provider confirmed it.');
        expect(shown).not.toContain(en('not_spent'));
      }
    });
  }
});
