/**
 * Spending one banked Claude reset from the quota page: the send, the per-seat
 * state (one confirm, one key, one latch), and the answer or refusal in plain
 * words. Every fixture is invented; a fake post stands in for the hub.
 */

import { afterEach, beforeEach, describe, expect, spyOn, test } from 'bun:test';
import { readdir, readFile } from 'node:fs/promises';
import { join } from 'node:path';
import { createElement } from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import i18n from '@/i18n';
import { apiClient } from '@/services/api/client';
import {
  invalidateClaudeLimitResets,
  readClaudeLimitResets,
  subscribeClaudeResetCredentials,
  type ClaudeLimitResetSeat,
} from '@/services/api/claudeLimitResets';
import {
  CLAUDE_RESET_SPEND_TIMEOUT_MS,
  defaultClaudeResetSpendPost,
  newClaudeResetSpendKey,
  type ClaudeResetSpendPost,
} from '@/services/api/claudeResetSpend';
import {
  askClaudeResetSpendAgain,
  cancelClaudeResetSpend,
  claudeResetSpendBusy,
  claudeResetSpendSeatKey,
  claudeResetSpendSeatKeyFor,
  confirmClaudeResetSpend,
  dismissClaudeResetSpend,
  getClaudeResetSpend,
  openClaudeResetSpend,
  type ClaudeResetSpendKind,
} from '@/features/quota/providers/claude/claudeResetSpendState';
import {
  ClaudeResetOutcomeView,
  ClaudeResetSpendSection,
  ClaudeResetSpendView,
} from '@/features/quota/providers/claude/ClaudeResetSpend';

/** The hub's rule for a key: 1 to 200 printable characters, no comma. */
const HUB_KEY_RULE = /^[\x21-\x2b\x2d-\x7e]{1,200}$/;
const SPEND_PATH = '/claude/limit-resets/spend';
const IN_FLIGHT =
  'a spend with this Idempotency-Key is in flight and its outcome is not recorded yet: deliver it again once it has answered';

type Call = {
  path: string;
  body: unknown;
  config: { headers: Record<string, string>; timeout: number };
};
type Reply = () => Promise<unknown>;

/** A fake hub: each call takes the next reply; with none left it never answers. */
function fakeHub(replies: Reply[] = []) {
  const calls: Call[] = [];
  const post: ClaudeResetSpendPost = (path, body, config) => {
    calls.push({ path, body, config });
    const next = replies.shift();
    return next ? next() : new Promise(() => {});
  };
  return { post, calls };
}

/** What the page's client rejects with: an ApiError with the status and body. */
const refusal = (status: number, data: unknown): Reply => () =>
  Promise.reject(Object.assign(new Error(`HTTP ${status}`), { name: 'ApiError', status, data }));
/** A timeout or a network error: the client's error carries no status. */
const noAnswer: Reply = () =>
  Promise.reject(
    Object.assign(new Error('timeout of 120000ms exceeded'), {
      name: 'ApiError',
      code: 'ECONNABORTED',
    })
  );
const ok = (data: unknown): Reply => () => Promise.resolve(data);

let seatCounter = 0;
/** A fresh seat for each case, so no state crosses tests. */
function freshSeat(kind: ClaudeResetSpendKind = 'fable') {
  seatCounter += 1;
  const authIndex = `idx-${seatCounter}`;
  return {
    key: claudeResetSpendSeatKey('http://hub.example.test:8317', authIndex),
    body: { auth_index: authIndex, kind },
  };
}
let keyCounter = 0;
const countingKeys = () => `key-${++keyCounter}`;

const text = (html: string) =>
  html
    .replace(/<[^>]*>/g, ' ')
    .replace(/&#x27;/g, "'")
    .replace(/&quot;/g, '"')
    .replace(/&lt;/g, '<')
    .replace(/&gt;/g, '>')
    .replace(/&amp;/g, '&')
    .replace(/\s+/g, ' ');

const kindsAfter = {
  fable: { resets_left: 0, usable_resets: 0, soonest_expiry: null },
  full: { resets_left: 1, usable_resets: 0, soonest_expiry: null },
};
const kindsBefore = {
  fable: { resets_left: 1, usable_resets: 1, soonest_expiry: '2026-12-01T00:00:00Z' },
  full: { resets_left: 1, usable_resets: 0, soonest_expiry: null },
};
const ROUTING_NOTE =
  "a confirmed reset does not return the seat to rotation: the hub's own cooldown can hold it until the routing reset is called";
const RETRY_UNTIL = '2026-11-25T12:10:00Z';
/** A 200 answer shaped as the route builds it at BASE (handler lines 1472 to 1521). */
const answer = (result: string, extra: Record<string, unknown> = {}) => ({
  auth_index: 'idx-fixture',
  kind: 'fable',
  grant_id: 'grant-f1',
  request_id: '00000000-0000-4000-8000-000000000001',
  retry: false,
  result,
  provider: { result, http_status: 200 },
  kinds_before: kindsBefore,
  kinds_after: kindsAfter,
  ...extra,
});
const routing = (queued: boolean) => ({
  routing: {
    cooldown_changed: false,
    usage_poll_queued: queued,
    cooldown_reset_route: '/v8/management/routing/cooldown/reset',
    note: ROUTING_NOTE,
  },
});

const SEAT_NAME = 'ops-one@example.test';
const outcomeText = (key: string) =>
  text(
    renderToStaticMarkup(
      createElement(ClaudeResetOutcomeView, { spend: getClaudeResetSpend(key), seatName: SEAT_NAME })
    )
  );

/** Open one confirm on a fresh seat and press its button once; returns the seat. */
async function spendOnce(reply: Reply, kind: ClaudeResetSpendKind = 'fable') {
  const seat = freshSeat(kind);
  const hub = fakeHub([reply]);
  expect(openClaudeResetSpend(seat.key, seat.body, { post: hub.post, makeKey: countingKeys })).toBe(
    true
  );
  await confirmClaudeResetSpend(seat.key);
  return { ...seat, hub };
}

let mocks: { mockRestore(): void }[] = [];
let invalidations = 0;
let releaseListener: () => void = () => {};
// Restore the language each test found: other files read i18n's global state.
let previousLanguage = i18n.language;
beforeEach(async () => {
  previousLanguage = i18n.language;
  await i18n.changeLanguage('en');
  invalidations = 0;
  releaseListener = subscribeClaudeResetCredentials(() => {
    invalidations += 1;
  });
});
afterEach(async () => {
  releaseListener();
  for (const mock of mocks) mock.mockRestore();
  mocks = [];
  await i18n.changeLanguage(previousLanguage);
});

describe('A7: one click, one key', () => {
  type Row = { name: string; replies: Reply[]; drive: (key: string) => Promise<unknown>; calls: number };
  const rows: Row[] = [
    {
      name: 'two presses in the same moment',
      replies: [ok(answer('reset', routing(true)))],
      drive: (key) => Promise.all([confirmClaudeResetSpend(key), confirmClaudeResetSpend(key)]),
      calls: 1,
    },
    {
      name: 'a press, no answer, then Ask the hub again three times',
      replies: [noAnswer, noAnswer, noAnswer, ok(answer('reset', routing(true)))],
      drive: async (key) => {
        await confirmClaudeResetSpend(key);
        for (let i = 0; i < 3; i += 1) await askClaudeResetSpendAgain(key);
      },
      calls: 4,
    },
    {
      name: 'a press, a 409 that says the key is in flight, then again',
      replies: [refusal(409, { error: IN_FLIGHT }), ok(answer('reset', routing(true)))],
      drive: async (key) => {
        await confirmClaudeResetSpend(key);
        await askClaudeResetSpendAgain(key);
      },
      calls: 2,
    },
    {
      name: 'no answer, then two presses of Ask the hub again in the same moment',
      replies: [noAnswer, ok(answer('reset', routing(true)))],
      drive: async (key) => {
        await confirmClaudeResetSpend(key);
        await Promise.all([askClaudeResetSpendAgain(key), askClaudeResetSpendAgain(key)]);
      },
      calls: 2,
    },
    {
      name: 'an answer, then Ask the hub again and the confirm button pressed late',
      replies: [ok(answer('reset', routing(true)))],
      drive: async (key) => {
        await confirmClaudeResetSpend(key);
        await askClaudeResetSpendAgain(key);
        await confirmClaudeResetSpend(key);
      },
      calls: 1,
    },
  ];
  for (const row of rows) {
    test(row.name, async () => {
      const seat = freshSeat();
      const hub = fakeHub([...row.replies]);
      openClaudeResetSpend(seat.key, seat.body, { post: hub.post, makeKey: countingKeys });
      await row.drive(seat.key);
      const keys = new Set(hub.calls.map((call) => call.config.headers['Idempotency-Key']));
      const bodies = new Set(hub.calls.map((call) => JSON.stringify(call.body)));
      console.log(
        `A7 | ${row.name} | calls=${hub.calls.length} | distinct keys=${keys.size} | distinct bodies=${bodies.size}`
      );
      expect(hub.calls).toHaveLength(row.calls);
      expect(keys.size).toBe(1);
      expect(bodies.size).toBe(1);
      expect(getClaudeResetSpend(seat.key).outcome?.type).toBe('answer');
    });
  }

  test('two confirms opened one after another carry two different keys', async () => {
    const seat = freshSeat();
    const hub = fakeHub([ok(answer('not_limited')), ok(answer('reset', routing(true)))]);
    openClaudeResetSpend(seat.key, seat.body, { post: hub.post, makeKey: newClaudeResetSpendKey });
    await confirmClaudeResetSpend(seat.key);
    openClaudeResetSpend(seat.key, seat.body, { post: hub.post, makeKey: newClaudeResetSpendKey });
    await confirmClaudeResetSpend(seat.key);
    const keys = hub.calls.map((call) => call.config.headers['Idempotency-Key']);
    console.log(`A7 | two confirms | calls=${keys.length} | distinct keys=${new Set(keys).size}`);
    expect(keys).toHaveLength(2);
    expect(keys[0]).not.toBe(keys[1]);
  });

  test('no new confirm (and so no new key) opens while a seat waits for an answer', async () => {
    const seat = freshSeat();
    const hub = fakeHub([noAnswer]);
    openClaudeResetSpend(seat.key, seat.body, { post: hub.post, makeKey: countingKeys });
    const first = getClaudeResetSpend(seat.key).delivery?.key;
    const pending = confirmClaudeResetSpend(seat.key);
    expect(openClaudeResetSpend(seat.key, seat.body, { post: hub.post, makeKey: countingKeys })).toBe(
      false
    );
    await pending;
    expect(getClaudeResetSpend(seat.key).outcome?.type).toBe('no_answer');
    expect(
      openClaudeResetSpend(seat.key, { ...seat.body, kind: 'weekly' }, { post: hub.post, makeKey: countingKeys })
    ).toBe(false);
    expect(getClaudeResetSpend(seat.key).delivery?.key).toBe(first);
    expect(getClaudeResetSpend(seat.key).delivery?.body.kind).toBe('fable');
  });
});

describe('A8: the call', () => {
  test('one path, a body of auth_index and kind, one hub-valid key, a 120 s wait, nothing else', async () => {
    const methods = (['get', 'post', 'put', 'patch', 'delete'] as const).map((name) => {
      const spy = spyOn(apiClient, name).mockImplementation(() => Promise.resolve({}) as never);
      mocks.push(spy);
      return spy;
    });
    const seat = freshSeat('weekly');
    const hub = fakeHub([ok(answer('reset', { ...routing(false), kind: 'weekly' }))]);
    openClaudeResetSpend(seat.key, seat.body, { post: hub.post, makeKey: newClaudeResetSpendKey });
    await confirmClaudeResetSpend(seat.key);
    expect(hub.calls).toHaveLength(1);
    const [call] = hub.calls;
    expect(call.path).toBe(SPEND_PATH);
    expect(Object.keys(call.body as object).sort()).toEqual(['auth_index', 'kind']);
    expect(call.body).toEqual({ auth_index: seat.body.auth_index, kind: 'weekly' });
    expect(Object.keys(call.config.headers)).toEqual(['Idempotency-Key']);
    expect(call.config.headers['Idempotency-Key']).toMatch(HUB_KEY_RULE);
    expect(call.config.timeout).toBe(120_000);
    expect(CLAUDE_RESET_SPEND_TIMEOUT_MS).toBe(120_000);
    for (const method of methods) expect(method).not.toHaveBeenCalled();
  });

  test('the key maker always fits the hub rule', () => {
    for (let i = 0; i < 50; i += 1) expect(newClaudeResetSpendKey()).toMatch(HUB_KEY_RULE);
  });

  test('the default post pins the connection and sets the wait on this call alone', async () => {
    const calls: unknown[][] = [];
    const captured = {
      post: (...args: unknown[]) => {
        calls.push(args);
        return Promise.resolve({ ok: true });
      },
    };
    const capture = spyOn(apiClient, 'captureConnection').mockReturnValue(captured as never);
    mocks.push(capture);
    const post = defaultClaudeResetSpendPost();
    expect(capture).toHaveBeenCalledTimes(1);
    await post(SPEND_PATH, { auth_index: 'idx-x', kind: 'full' }, {
      headers: { 'Idempotency-Key': 'k-1' },
      timeout: CLAUDE_RESET_SPEND_TIMEOUT_MS,
    });
    expect(calls).toEqual([
      [SPEND_PATH, { auth_index: 'idx-x', kind: 'full' }, { headers: { 'Idempotency-Key': 'k-1' }, timeout: 120_000 }],
    ]);
  });
});

describe('A9: answers', () => {
  const notSpent = 'The hub records this as not spent.';
  type Row = { name: string; body: unknown; expects: string[]; absent?: string[]; routing?: boolean };
  const routingLine = 'This does not put the seat back in rotation by itself.';
  const rows: Row[] = [
    {
      name: 'reset',
      body: answer('reset', routing(true)),
      expects: ['The provider confirmed it.', ROUTING_NOTE, 'A usage read of this seat was queued.', 'Left now:'],
      routing: true,
    },
    {
      name: 'already_used',
      body: answer('already_used', routing(false)),
      expects: [
        'The provider says this reset was already used; this click spent nothing new.',
        ROUTING_NOTE,
        'No usage read of this seat was queued.',
      ],
      routing: true,
    },
    { name: 'not_limited', body: answer('not_limited'), expects: ['The provider says this seat is not at its limit.', notSpent] },
    { name: 'cooldown', body: answer('cooldown'), expects: ['The provider says this seat is in its reset cooldown.', notSpent] },
    { name: 'ineligible', body: answer('ineligible'), expects: ['The provider says this seat is not eligible for a reset.', notSpent] },
    { name: 'unavailable', body: answer('unavailable'), expects: ['The provider says this reset is not available now.', notSpent] },
    { name: 'rate_limited', body: answer('rate_limited'), expects: ['The provider refused the claim as too many requests.', notSpent] },
    { name: 'auth_error', body: answer('auth_error'), expects: ["The provider refused the seat's sign-in.", notSpent] },
    {
      name: 'unknown',
      body: answer('unknown', { retry_until: RETRY_UNTIL, kinds_after: null, after_read_error: 'the grants read after the claim failed' }),
      expects: [
        'No answer came back from the provider. The reset MAY have been spent.',
        `The hub holds this seat until ${new Date(RETRY_UNTIL).toLocaleString('en')}.`,
      ],
    },
    {
      name: 'a result word the table does not list (read as unknown)',
      body: answer('surprise_word', { retry_until: RETRY_UNTIL }),
      expects: ['No answer came back from the provider. The reset MAY have been spent.'],
      absent: [notSpent, 'The provider confirmed it.'],
    },
    {
      name: 'replayed true',
      body: {
        replayed: true,
        auth_index: 'idx-fixture',
        kind: 'fable',
        grant_id: 'grant-f1',
        request_id: '00000000-0000-4000-8000-000000000001',
        retry: false,
        result: 'reset',
        at: '2026-11-25T11:00:00Z',
        resets_left_before: { fable: 1, full: 1 },
        resets_left_after: { fable: 0, full: 1 },
      },
      expects: [
        'This is the recorded outcome of the same click. Nothing new was sent.',
        'The provider confirmed it.',
        "This does not put the seat back in rotation by itself: the hub's own cooldown can hold it until the routing reset is called.",
        'Left after, as recorded:',
        'Fable reset (fable): 0 left',
      ],
      absent: ['Left now:', 'usable now', 'usage read'],
    },
    {
      name: 'replayed, sent and never answered (the recorded sentence as sent)',
      body: {
        replayed: true,
        auth_index: 'idx-fixture',
        kind: 'fable',
        grant_id: 'grant-f1',
        request_id: '00000000-0000-4000-8000-000000000001',
        retry: false,
        result: 'unknown',
        at: '2026-11-25T11:00:00Z',
        resets_left_before: { fable: 1 },
        resets_left_after: null,
        recorded: 'the claim was sent and no answer was recorded: no retry of it is accepted',
      },
      expects: [
        'This is the recorded outcome of the same click. Nothing new was sent.',
        'the claim was sent and no answer was recorded: no retry of it is accepted',
        'The reset MAY have been spent.',
        'What is left after it was not recorded.',
      ],
    },
    {
      name: 'kinds_after null with after_read_error',
      body: answer('reset', { ...routing(true), kinds_after: null, after_read_error: 'the grants read after the claim failed' }),
      expects: [
        'The read after the spend failed, so what is left is not known yet.',
        'the grants read after the claim failed',
      ],
      absent: ['Left now:'],
      routing: true,
    },
    {
      name: 'ledger_error',
      body: answer('reset', {
        ...routing(true),
        ledger_error:
          'the outcome is not in the spend ledger: the ledger refuses every spend until the hub restarts, the seat stays held, and no retry of this claim is accepted',
      }),
      expects: [
        'the outcome is not in the spend ledger: the ledger refuses every spend until the hub restarts, the seat stays held, and no retry of this claim is accepted',
      ],
      routing: true,
    },
    {
      name: 'balance_error',
      body: answer('reset', {
        ...routing(true),
        balance_error:
          'what is left after the claim is not in the spend ledger, which refuses every spend until the hub restarts',
      }),
      expects: [
        'what is left after the claim is not in the spend ledger, which refuses every spend until the hub restarts',
      ],
      routing: true,
    },
  ];
  for (const row of rows) {
    test(row.name, async () => {
      const seat = await spendOnce(ok(row.body));
      const shown = outcomeText(seat.key);
      console.log(`A9 | ${row.name} | ${shown.trim()}`);
      expect(getClaudeResetSpend(seat.key).outcome?.type).toBe('answer');
      expect(shown).toContain(`Fable reset (fable) on ${SEAT_NAME}`);
      for (const line of row.expects) expect(shown).toContain(line);
      for (const line of row.absent ?? []) expect(shown).not.toContain(line);
      if (row.routing) expect(shown).toContain(routingLine);
      else if (!row.name.startsWith('replayed true')) expect(shown).not.toContain(routingLine);
      expect(shown).not.toContain('Not spent');
    });
  }

  test('the answer stays until dismissed or a new confirm opens', async () => {
    const seat = await spendOnce(ok(answer('reset', routing(true))));
    expect(getClaudeResetSpend(seat.key).outcome?.type).toBe('answer');
    expect(getClaudeResetSpend(seat.key).confirmOpen).toBe(false);
    dismissClaudeResetSpend(seat.key);
    expect(getClaudeResetSpend(seat.key).outcome).toBeUndefined();
    const again = await spendOnce(ok(answer('reset', routing(true))));
    openClaudeResetSpend(again.key, again.body, { post: fakeHub().post, makeKey: countingKeys });
    expect(getClaudeResetSpend(again.key).outcome).toBeUndefined();
  });
});

describe('A10: refusals', () => {
  const holds = {
    fable: { resets_left: 1, usable_resets: 1, soonest_expiry: '2026-12-01T00:00:00Z' },
    full: { resets_left: 1, usable_resets: 0, soonest_expiry: null },
  };
  type Row = {
    name: string;
    reply: Reply;
    heading: 'Not spent' | 'not known' | 'no route';
    error?: string;
    expects?: string[];
  };
  const NOT_KNOWN = (status: number) =>
    `The hub answered HTTP ${status}. Whether anything was spent is not known from this answer.`;
  const rows: Row[] = [
    {
      name: '400 with kinds',
      reply: refusal(400, {
        error: "the seat's grants list no reset of this kind; nothing spent",
        kind: 'weekly',
        kinds: holds,
        grants_of_kind: [],
        eligible: true,
        ineligible_reason: null,
        cooldown_until: null,
      }),
      heading: 'Not spent',
      error: "the seat's grants list no reset of this kind; nothing spent",
      expects: ['What this seat holds:', 'Fable reset (fable): 1 left · 1 usable now', 'Full reset (full): 1 left · 0 usable now'],
    },
    {
      name: '409 a spend already in flight for this seat',
      reply: refusal(409, { error: 'a spend is already in flight for this seat; nothing spent' }),
      heading: 'Not spent',
      error: 'a spend is already in flight for this seat; nothing spent',
    },
    {
      name: '409 an earlier claim with an unknown outcome',
      reply: refusal(409, {
        error:
          "an earlier claim on this seat has an unknown outcome: retry it with its retry_request_id within ten minutes of sending, or check the account's usage page; nothing spent",
      }),
      heading: 'Not spent',
      error:
        "an earlier claim on this seat has an unknown outcome: retry it with its retry_request_id within ten minutes of sending, or check the account's usage page; nothing spent",
    },
    {
      name: '409 judged again and not sent, with reason',
      reply: refusal(409, {
        error: 'the claim was judged again after its record was written and not sent; nothing spent',
        reason: 'grant_expired_at_dispatch',
        request_id: '00000000-0000-4000-8000-000000000002',
      }),
      heading: 'Not spent',
      error: 'the claim was judged again after its record was written and not sent; nothing spent',
      expects: ['Reason code: grant_expired_at_dispatch'],
    },
    {
      name: '502 with upstream_status 429',
      reply: refusal(502, { error: 'fresh grants read failed; nothing spent', upstream_status: 429 }),
      heading: 'Not spent',
      error: 'fresh grants read failed; nothing spent',
      expects: [
        "Too many reads of this seat at the provider. Wait, then press again, and do not refresh this seat's usage in between.",
      ],
    },
    {
      name: '502 organization read failed',
      reply: refusal(502, { error: 'organization read failed; nothing spent' }),
      heading: 'Not spent',
      error: 'organization read failed; nothing spent',
    },
    {
      name: '503 with cure',
      reply: refusal(503, {
        error:
          'the spend ledger cannot be read or trusted, so earlier claims cannot be ruled out (fixture problem); this request sent nothing',
        ledger_refusal: 'ledger_held_by_another_process',
        ledger: '/fixture/logs/claude-limit-reset-spends.jsonl',
        cure: 'stop the other hub process that uses this log directory, or give each hub its own log directory; then restart this hub',
      }),
      heading: 'Not spent',
      error:
        'the spend ledger cannot be read or trusted, so earlier claims cannot be ruled out (fixture problem); this request sent nothing',
      expects: [
        'What to do:',
        'stop the other hub process that uses this log directory, or give each hub its own log directory; then restart this hub',
      ],
    },
    {
      name: '404 with JSON',
      reply: refusal(404, { error: 'Claude seat not found' }),
      heading: 'Not spent',
      error: 'Claude seat not found',
    },
    {
      name: '404 with no JSON',
      reply: refusal(404, '404 page not found'),
      heading: 'no route',
      expects: ["This hub's build has no spend route. Nothing was sent."],
    },
    {
      name: 'an unlisted 500 with no JSON',
      reply: refusal(500, 'Internal Server Error'),
      heading: 'not known',
      expects: [NOT_KNOWN(500), 'The hub has not answered. Whether the reset was spent is not known yet.', 'Ask the hub again'],
    },
    {
      name: 'a 500 with a JSON body and no error string',
      reply: refusal(500, { detail: 'fixture' }),
      heading: 'not known',
      expects: [NOT_KNOWN(500), 'Ask the hub again'],
    },
    {
      name: 'an unlisted 418 with a JSON error string (R3)',
      reply: refusal(418, { error: 'fixture refusal; nothing spent' }),
      heading: 'Not spent',
      error: 'fixture refusal; nothing spent',
    },
    {
      name: 'a replayed not-sent refusal',
      reply: refusal(409, {
        replayed: true,
        error: 'the claim was not sent; nothing spent',
        reason: 'credential_changed_at_dispatch',
        result: 'not_sent',
      }),
      heading: 'Not spent',
      error: 'the claim was not sent; nothing spent',
      expects: ['This is the recorded outcome of the same click. Nothing new was sent.'],
    },
  ];
  for (const row of rows) {
    test(row.name, async () => {
      const seat = await spendOnce(row.reply);
      const spend = getClaudeResetSpend(seat.key);
      const shown = outcomeText(seat.key);
      console.log(`A10 | ${row.name} | ${shown.trim()}`);
      if (row.heading === 'Not spent') {
        expect(spend.outcome?.type).toBe('refusal');
        expect(shown).toContain('Not spent');
        expect(shown).not.toContain('Ask the hub again');
        expect(invalidations).toBe(1);
        expect(claudeResetSpendBusy(spend)).toBe(false);
      } else if (row.heading === 'no route') {
        expect(spend.outcome?.type).toBe('no_route');
        expect(shown).not.toContain('Ask the hub again');
        expect(invalidations).toBe(1);
      } else {
        expect(spend.outcome?.type).toBe('not_known');
        expect(shown).not.toContain('Not spent');
        // An HTTP answer drops the cached read, even when its outcome is not known.
        expect(invalidations).toBe(1);
        expect(claudeResetSpendBusy(spend)).toBe(true);
      }
      if (row.error) expect(shown).toContain(row.error);
      for (const line of row.expects ?? []) expect(shown).toContain(line);
      expect(shown).toContain(`Fable reset (fable) on ${SEAT_NAME}`);
    });
  }

  test('LEAVE: a 200 whose body is not JSON reads as not known, with Ask the hub again', async () => {
    const seat = await spendOnce(ok('<html>proxy page</html>'));
    const spend = getClaudeResetSpend(seat.key);
    expect(spend.outcome).toEqual({ type: 'not_known', status: 200 });
    expect(outcomeText(seat.key)).toContain('Ask the hub again');
    expect(claudeResetSpendBusy(spend)).toBe(true);
  });

  test('the hub error string is rendered as sent, character for character', async () => {
    const odd = `a <b>"quoted"</b> & 'odd' refusal; nothing spent`;
    const seat = await spendOnce(refusal(409, { error: odd }));
    expect(outcomeText(seat.key)).toContain(odd);
  });

  test('401 runs the page sign-out and shows nothing of its own', async () => {
    const seat = await spendOnce(refusal(401, { error: 'Invalid management key' }));
    const spend = getClaudeResetSpend(seat.key);
    expect(spend.outcome).toBeUndefined();
    expect(spend.confirmOpen).toBe(false);
    expect(spend.inFlight).toBe(false);
    expect(outcomeText(seat.key).trim()).toBe('');
  });

  test('a 401 on Ask the hub again keeps the earlier unknown outcome and its key', async () => {
    const seat = freshSeat();
    const hub = fakeHub([noAnswer, refusal(401, { error: 'Invalid management key' }), ok(answer('reset', routing(true)))]);
    openClaudeResetSpend(seat.key, seat.body, { post: hub.post, makeKey: countingKeys });
    await confirmClaudeResetSpend(seat.key);
    await askClaudeResetSpendAgain(seat.key);
    const spend = getClaudeResetSpend(seat.key);
    expect(spend.outcome?.type).toBe('no_answer');
    expect(spend.inFlight).toBe(false);
    expect(claudeResetSpendBusy(spend)).toBe(true);
    await askClaudeResetSpendAgain(seat.key);
    expect(new Set(hub.calls.map((call) => call.config.headers['Idempotency-Key'])).size).toBe(1);
    expect(getClaudeResetSpend(seat.key).outcome?.type).toBe('answer');
  });

  test('a 409 that says the key is in flight keeps the no-answer block and its control', async () => {
    const seat = await spendOnce(refusal(409, { error: IN_FLIGHT }));
    const spend = getClaudeResetSpend(seat.key);
    const shown = outcomeText(seat.key);
    expect(spend.outcome?.type).toBe('key_in_flight');
    expect(shown).toContain('The hub has not answered. Whether the reset was spent is not known yet.');
    expect(shown).toContain('Ask the hub again');
    expect(shown).toContain(IN_FLIGHT);
    expect(shown).not.toContain('Not spent');
    expect(claudeResetSpendBusy(spend)).toBe(true);
    expect(invalidations).toBe(1);
  });
});

const visibleSeat: ClaudeLimitResetSeat = {
  name: 'seat-one.json',
  auth_index: 'idx-view',
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
};
const viewProps = {
  seat: visibleSeat,
  loaded: true,
  identity: { primary: SEAT_NAME, organization: 'Example Org One', fileName: 'seat-one.json' },
  usage: { freshness: { kind: 'fresh' as const, ageMs: 5_000 }, text: 'Updated just now' },
};

describe('A11: no answer', () => {
  test('a post that never answers keeps the confirm open, waiting, and every control disabled', async () => {
    const seat = freshSeat();
    const hub = fakeHub();
    openClaudeResetSpend(seat.key, seat.body, { post: hub.post, makeKey: countingKeys });
    void confirmClaudeResetSpend(seat.key);
    await Promise.resolve();
    const spend = getClaudeResetSpend(seat.key);
    expect(spend.inFlight).toBe(true);
    cancelClaudeResetSpend(seat.key);
    dismissClaudeResetSpend(seat.key);
    expect(getClaudeResetSpend(seat.key).confirmOpen).toBe(true);
    const html = renderToStaticMarkup(createElement(ClaudeResetSpendView, { ...viewProps, spend: getClaudeResetSpend(seat.key) }));
    expect(text(html)).toContain('Waiting for the hub. Keep this page open.');
    expect(html).toMatch(/<button[^>]*data-reset-kind="fable"[^>]*disabled=""/);
    expect(hub.calls).toHaveLength(1);
  });

  test('a rejection with no status yields the no-answer block; Ask the hub again re-sends the same key', async () => {
    const seat = freshSeat();
    const hub = fakeHub([noAnswer, noAnswer]);
    openClaudeResetSpend(seat.key, seat.body, { post: hub.post, makeKey: countingKeys });
    await confirmClaudeResetSpend(seat.key);
    const spend = getClaudeResetSpend(seat.key);
    expect(spend.outcome?.type).toBe('no_answer');
    expect(spend.confirmOpen).toBe(false);
    expect(claudeResetSpendBusy(spend)).toBe(true);
    const shown = outcomeText(seat.key);
    expect(shown).toContain('The hub has not answered. Whether the reset was spent is not known yet.');
    expect(shown).toContain('Ask the hub again');
    const html = renderToStaticMarkup(createElement(ClaudeResetSpendView, { ...viewProps, spend }));
    expect(html).toMatch(/<button[^>]*data-reset-kind="fable"[^>]*disabled=""/);
    dismissClaudeResetSpend(seat.key);
    expect(getClaudeResetSpend(seat.key).outcome?.type).toBe('no_answer');
    await askClaudeResetSpendAgain(seat.key);
    expect(hub.calls).toHaveLength(2);
    expect(hub.calls[1].config.headers['Idempotency-Key']).toBe(hub.calls[0].config.headers['Idempotency-Key']);
    expect(hub.calls[1].body).toEqual(hub.calls[0].body);
    expect(invalidations).toBe(0);
  });
});

describe('A12: nothing spends by itself', () => {
  test('views, Details, opening and cancelling a confirm, a failed read and the minute timer call the post zero times', async () => {
    const hub = fakeHub([ok(answer('reset'))]);
    const post = spyOn(apiClient, 'post');
    mocks.push(post);
    const seat = freshSeat();
    for (const props of [
      { ...viewProps, spend: getClaudeResetSpend(seat.key) },
      { ...viewProps, seat: undefined, failed: true, spend: getClaudeResetSpend(seat.key) },
    ])
      renderToStaticMarkup(createElement(ClaudeResetSpendView, props));
    openClaudeResetSpend(seat.key, seat.body, { post: hub.post, makeKey: countingKeys });
    renderToStaticMarkup(createElement(ClaudeResetSpendView, { ...viewProps, spend: getClaudeResetSpend(seat.key) }));
    cancelClaudeResetSpend(seat.key);
    expect(getClaudeResetSpend(seat.key).confirmOpen).toBe(false);
    expect(getClaudeResetSpend(seat.key).delivery).toBeUndefined();
    await confirmClaudeResetSpend(seat.key);
    await askClaudeResetSpendAgain(seat.key);

    let now = 1_000;
    mocks.push(spyOn(Date, 'now').mockImplementation(() => now));
    mocks.push(spyOn(apiClient, 'getConnectionRevision').mockReturnValue(200_001));
    let fail = true;
    const get = spyOn(apiClient, 'get').mockImplementation(async () => {
      if (fail) throw Object.assign(new Error('read failed'), { status: 500 });
      return { seats: [visibleSeat], ranking_rule: 'fixture' };
    });
    mocks.push(get);
    invalidateClaudeLimitResets();
    await expect(readClaudeLimitResets()).rejects.toThrow('read failed');
    fail = false;
    await readClaudeLimitResets();
    now += 61_000; // the read's own minute timer
    await readClaudeLimitResets();
    expect(get).toHaveBeenCalledTimes(3);
    expect(hub.calls).toHaveLength(0);
    expect(post).not.toHaveBeenCalled();
  });
});

describe('A13: no second way to spend', () => {
  test('the spend path lives in one file, the new module; no file names reset_rate_limits', async () => {
    const holders: string[] = [];
    async function scan(dir: string): Promise<void> {
      for (const entry of await readdir(dir, { withFileTypes: true })) {
        const path = join(dir, entry.name);
        if (entry.isDirectory()) {
          await scan(path);
          continue;
        }
        const source = await readFile(path, 'utf8');
        if (source.includes(SPEND_PATH)) holders.push(path);
        expect(source).not.toContain('reset_rate_limits');
      }
    }
    await scan('src');
    expect(holders).toEqual([join('src', 'services', 'api', 'claudeResetSpend.ts')]);
  });
});

describe('A14: after an outcome', () => {
  for (const [name, reply] of [
    ['an answer', ok(answer('reset', routing(true)))],
    ['a refusal', refusal(409, { error: 'a spend is already in flight for this seat; nothing spent' })],
  ] as const) {
    test(`${name} drops the cached read once and asks for no usage refresh`, async () => {
      const post = spyOn(apiClient, 'post');
      mocks.push(post);
      await spendOnce(reply);
      expect(invalidations).toBe(1);
      expect(post.mock.calls.filter(([path]) => path === '/credentials/quota')).toHaveLength(0);
      expect(post).not.toHaveBeenCalled();
    });
  }

  test('no answer drops nothing', async () => {
    await spendOnce(noAnswer);
    expect(invalidations).toBe(0);
  });
});

describe('round 1 class: a spend outlives every transient change of the page', () => {
  // Codex round 1 (cp-4f3): the entry was keyed by the connection revision, the
  // card found it through mounted component state, and the post kept the key of
  // the connection the confirm opened on. One test walks every transient change.
  test('sign-out and sign-in, a folded Details with a failed read, and a new management key', async () => {
    const hub = 'http://hub-class.example.test:8317/v8/management';
    const file = { name: 'seat-class.json', type: 'claude', credential_generation: '4:1' };
    // The client as the page signs in: the hub and the connection revision (spies, restored after).
    mocks.push(spyOn(apiClient, 'getApiBase').mockReturnValue(hub));
    const revision = spyOn(apiClient, 'getConnectionRevision').mockReturnValue(300_001);
    mocks.push(revision);
    const first = fakeHub([noAnswer]);
    const key = claudeResetSpendSeatKeyFor(apiClient.getApiBase(), file.name, 'idx-class');
    openClaudeResetSpend(key, { auth_index: 'idx-class', kind: 'weekly' }, {
      post: first.post,
      makeKey: () => 'key-class',
      file: { hub: apiClient.getApiBase(), name: file.name },
    });
    await confirmClaudeResetSpend(key);
    expect(getClaudeResetSpend(key).outcome?.type).toBe('no_answer');

    // Sign-out, then sign-in to the same hub (with any management key): two revisions later.
    revision.mockReturnValue(300_003);
    // A card mounted fresh (Details folded and reopened) whose read has no row.
    const remounted = claudeResetSpendSeatKeyFor(apiClient.getApiBase(), file.name, undefined);
    expect(remounted).toBe(key);
    expect(claudeResetSpendBusy(getClaudeResetSpend(remounted))).toBe(true);
    expect(
      openClaudeResetSpend(remounted, { auth_index: 'idx-class', kind: 'fable' }, { post: first.post, makeKey: () => 'key-other' })
    ).toBe(false);
    const html = renderToStaticMarkup(
      createElement(ClaudeResetSpendSection, { file: file as never, usage: { freshness: null, text: '' } })
    );
    expect(text(html)).toContain('Ask the hub again');

    // Ask the hub again through the connection current at the press: same key and body.
    const current = fakeHub([ok(answer('reset', { ...routing(true), kind: 'weekly' }))]);
    await askClaudeResetSpendAgain(remounted, current.post);
    expect(current.calls).toHaveLength(1);
    expect(current.calls[0].config.headers['Idempotency-Key']).toBe('key-class');
    expect(current.calls[0].body).toEqual(first.calls[0].body);
    expect(getClaudeResetSpend(key).outcome?.type).toBe('answer');
  });
});
