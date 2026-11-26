/**
 * Spending one banked Claude reset from the quota page, without React: which
 * of four states a seat card is in, what is left of each kind, which spend
 * controls a seat offers, and the per-seat spend state, kept in memory only.
 *
 * One click is one key. A key is minted only when a confirm opens, and every
 * delivery of that confirm (the first send and each "Ask the hub again")
 * carries the same key and body, so the hub answers a second delivery from its
 * ledger and sends nothing. The confirm takes its latch before anything is
 * awaited, and the page never drops a spend request itself.
 */

import type { TFunction } from 'i18next';
import {
  invalidateClaudeLimitResets,
  type ClaudeLimitResetSeat,
  type ClaudeResetKindSummary,
} from '@/services/api/claudeLimitResets';
import {
  readClaudeResetSpendOutcome,
  sendClaudeResetSpend,
  type ClaudeResetSpendBody,
  type ClaudeResetSpendKind,
  type ClaudeResetSpendOutcome,
  type ClaudeResetSpendPost,
} from '@/services/api/claudeResetSpend';
import type { SeatFreshness } from '../../seatCard';

export type { ClaudeResetSpendKind };

/** The kinds an operator may spend, in the page's fixed order. */
export const CLAUDE_RESET_SPEND_KINDS: readonly ClaudeResetSpendKind[] = [
  'fable',
  'weekly',
  'full',
  'session',
];
/** The order of the lines on a card; unknown is a grant the hub never spends. */
const KIND_ORDER: readonly string[] = [...CLAUDE_RESET_SPEND_KINDS, 'unknown'];
/** The route's result words; any other word is read as unknown. */
const RESULTS = new Set([
  'reset',
  'already_used',
  'not_limited',
  'cooldown',
  'ineligible',
  'unavailable',
  'rate_limited',
  'auth_error',
  'unknown',
]);

const isRecord = (value: unknown): value is Record<string, unknown> =>
  typeof value === 'object' && value !== null && !Array.isArray(value);
const isSpendKind = (code: string): code is ClaudeResetSpendKind =>
  (CLAUDE_RESET_SPEND_KINDS as readonly string[]).includes(code);
const count = (value: unknown) => (typeof value === 'number' && Number.isFinite(value) ? value : 0);
const unique = (values: string[]) => [...new Set(values)];

/** A result word the page names; an unlisted word claims the least: unknown. */
export const claudeResetResultWord = (result: unknown) =>
  typeof result === 'string' && RESULTS.has(result) ? result : 'unknown';

/**
 * N: no row (still loading, the read failed, or no row matches the card).
 * O: a hub build from before the spend route (the row has no kinds).
 * H: the hub's own read does not show the seat's grants.
 * V: the grants are visible.
 */
export type ClaudeResetCardState =
  | { state: 'N'; why: 'loading' | 'failed' | 'no_row' }
  | { state: 'O' }
  | { state: 'H' }
  | { state: 'V' };

export function claudeResetCardState(
  seat: ClaudeLimitResetSeat | undefined,
  read: { failed?: boolean; loaded?: boolean }
): ClaudeResetCardState {
  if (!seat) return { state: 'N', why: read.failed ? 'failed' : read.loaded ? 'no_row' : 'loading' };
  if (!isRecord(seat.kinds)) return { state: 'O' };
  if (seat.cedar_ember?.eligible !== true) return { state: 'H' };
  return { state: 'V' };
}

export interface ClaudeResetKindLine {
  code: string;
  summary: ClaudeResetKindSummary;
  /** False for unknown and any kind the hub does not name for a spend. */
  spendable: boolean;
  /** The provider's own grant labels: the label is not the kind and differs by seat. */
  labels: string[];
  /** What the grants of this kind clear, as the read lists them. */
  clears: string[];
}

type Grants = NonNullable<ClaudeLimitResetSeat['cedar_ember']>['grants'];

/** One line per kind the hub lists, in the fixed order; unlisted codes last. */
export function claudeResetKindLines(kinds: unknown, grants: Grants = []): ClaudeResetKindLine[] {
  if (!isRecord(kinds)) return [];
  const codes = Object.keys(kinds);
  const ordered = [
    ...KIND_ORDER.filter((code) => codes.includes(code)),
    ...codes.filter((code) => !KIND_ORDER.includes(code)).sort(),
  ];
  return ordered.map((code) => {
    const raw = kinds[code];
    const summary: Record<string, unknown> = isRecord(raw) ? raw : {};
    const ofKind = grants.filter((grant) => grant.kind === code);
    return {
      code,
      summary: {
        resets_left: count(summary.resets_left),
        usable_resets: count(summary.usable_resets),
        soonest_expiry: typeof summary.soonest_expiry === 'string' ? summary.soonest_expiry : null,
      },
      spendable: isSpendKind(code),
      labels: unique(ofKind.map((grant) => (grant.label ?? '').trim()).filter(Boolean)),
      clears: unique(ofKind.flatMap((grant) => grant.clears ?? [])),
    };
  });
}

/** Counts only, as a replay carries them (resets_left_after). */
export function claudeResetCountLines(counts: unknown) {
  if (!isRecord(counts)) return null;
  return claudeResetKindLines(
    Object.fromEntries(Object.entries(counts).map(([code, left]) => [code, { resets_left: left }]))
  );
}

/** Why a control is disabled. */
export type ClaudeResetControlBlock =
  | { code: 'busy' }
  | { code: 'cooldown'; until: string }
  | { code: 'unavailable'; reason: string }
  | { code: 'none_usable' };

export interface ClaudeResetControl {
  kind: ClaudeResetSpendKind;
  /** False in state H: the hub's read does not show the count. */
  countKnown: boolean;
  blocked?: ClaudeResetControlBlock;
}

/**
 * V: a control for each spendable kind the row lists, disabled when none is
 * usable. H: one control for each of the four kinds, count not known. N and O,
 * or a row with no auth index: none. Every control of a seat is disabled while
 * it waits for an answer or is in its reset cooldown.
 */
export function claudeResetControls(
  seat: ClaudeLimitResetSeat | undefined,
  card: ClaudeResetCardState,
  spend: ClaudeResetSpendSeat,
  nowMs: number
): ClaudeResetControl[] {
  if (!seat?.auth_index || (card.state !== 'V' && card.state !== 'H')) return [];
  const grants = seat.cedar_ember?.grants ?? [];
  const lines = card.state === 'V' ? claudeResetKindLines(seat.kinds, grants) : [];
  const kinds =
    card.state === 'H'
      ? CLAUDE_RESET_SPEND_KINDS
      : CLAUDE_RESET_SPEND_KINDS.filter((kind) => lines.some((line) => line.code === kind));
  const cooldown = seat.cedar_ember?.cooldown_until;
  const cooling = cooldown && Date.parse(cooldown) > nowMs ? cooldown : null;
  const busy = claudeResetSpendBusy(spend);
  return kinds.map((kind) => {
    const line = lines.find((entry) => entry.code === kind);
    let blocked: ClaudeResetControlBlock | undefined;
    if (busy) blocked = { code: 'busy' };
    else if (cooling) blocked = { code: 'cooldown', until: cooling };
    else if (line && line.summary.usable_resets <= 0) {
      const reason = grants.find((grant) => grant.kind === kind && grant.unavailable_reason)
        ?.unavailable_reason;
      blocked = reason ? { code: 'unavailable', reason } : { code: 'none_usable' };
    }
    return { kind, countKnown: card.state === 'V', blocked };
  });
}

/** Usage numbers the card cannot vouch for: anything but a fresh poll. */
export const claudeResetUsageStale = (freshness: SeatFreshness | null | undefined) =>
  freshness?.kind !== 'fresh';

/** A kind in words with its code: "Fable reset (fable)". */
export function claudeResetKindName(t: TFunction, code: string | undefined) {
  const known = code && KIND_ORDER.includes(code) ? code : 'unknown';
  return t('claude_reset_spend.kind_name', {
    name: t(`claude_reset_spend.kind_${known}`),
    code: code || 'unknown',
  });
}

/**
 * The seat's last spend in plain words, or undefined when the row says
 * nothing (an old hub). With the ledger unreadable it is "not known", never
 * "never".
 */
export function claudeResetLastSpendText(
  t: TFunction,
  seat: ClaudeLimitResetSeat,
  locale?: string
): { recorded: boolean; text: string } | undefined {
  if (seat.last_spend_error)
    return { recorded: false, text: t('claude_reset_spend.last_not_known') };
  if (seat.last_spend === null) return { recorded: false, text: t('claude_reset_spend.last_never') };
  const record = seat.last_spend;
  if (!isRecord(record)) return undefined;
  const result =
    record.phase === 'sent'
      ? t('claude_reset_spend.last_sent')
      : record.result === 'not_sent'
        ? t('claude_reset_spend.last_not_sent', { reason: record.reason || '—' })
        : t(`claude_reset_spend.result_short_${claudeResetResultWord(record.result)}`);
  const at = Date.parse(record.at);
  return {
    recorded: true,
    text: t('claude_reset_spend.last_record', {
      kind: claudeResetKindName(t, record.kind),
      time: Number.isNaN(at) ? '—' : new Date(at).toLocaleString(locale),
      result,
    }),
  };
}

/** State 6: one seat's open confirm with its key, the send in flight, the last outcome. */
export interface ClaudeResetSpendSeat {
  delivery?: { key: string; body: ClaudeResetSpendBody };
  confirmOpen: boolean;
  inFlight: boolean;
  outcome?: ClaudeResetSpendOutcome;
}

const IDLE: ClaudeResetSpendSeat = Object.freeze({ confirmOpen: false, inFlight: false });
const seats = new Map<string, ClaudeResetSpendSeat>();
const posts = new Map<string, ClaudeResetSpendPost>();
const listeners = new Set<() => void>();

/** One entry per hub connection and row auth_index; Details folding keeps it. */
export const claudeResetSpendSeatKey = (connectionRevision: number, authIndex: string) =>
  `${connectionRevision}\u0000${authIndex}`;

export const getClaudeResetSpend = (key: string): ClaudeResetSpendSeat => seats.get(key) ?? IDLE;

export function subscribeClaudeResetSpend(listener: () => void) {
  listeners.add(listener);
  return () => {
    listeners.delete(listener);
  };
}

function put(key: string, next: ClaudeResetSpendSeat) {
  if (next === IDLE) {
    seats.delete(key);
    posts.delete(key);
  } else seats.set(key, next);
  for (const listener of listeners) listener();
}

/** The outcome is not known yet: only "Ask the hub again" may deliver, with the same key. */
const unresolved = (outcome: ClaudeResetSpendOutcome | undefined) =>
  outcome?.type === 'no_answer' || outcome?.type === 'key_in_flight' || outcome?.type === 'not_known';

/** While true every kind control of the seat is disabled and no confirm opens. */
export const claudeResetSpendBusy = (spend: ClaudeResetSpendSeat) =>
  spend.inFlight || unresolved(spend.outcome);

/**
 * Opens a confirm and mints its one key. Sends nothing and asks the hub
 * nothing. Refused while a confirm is open or the seat waits for an answer.
 */
export function openClaudeResetSpend(
  key: string,
  body: ClaudeResetSpendBody,
  deps: { post: ClaudeResetSpendPost; makeKey: () => string }
): boolean {
  const current = getClaudeResetSpend(key);
  if (current.confirmOpen || claudeResetSpendBusy(current)) return false;
  posts.set(key, deps.post);
  put(key, {
    delivery: { key: deps.makeKey(), body: { auth_index: body.auth_index, kind: body.kind } },
    confirmOpen: true,
    inFlight: false,
  });
  return true;
}

/** Closes a confirm that has sent nothing; refused while its send is in flight. */
export function cancelClaudeResetSpend(key: string) {
  const current = getClaudeResetSpend(key);
  if (current.confirmOpen && !current.inFlight) put(key, IDLE);
}

/** The confirm's one send. A second press, even in the same moment, sends nothing. */
export function confirmClaudeResetSpend(key: string): Promise<void> {
  const current = getClaudeResetSpend(key);
  if (!current.confirmOpen || current.inFlight || current.outcome) return Promise.resolve();
  return deliver(key, current);
}

/** Delivers the same key and body again; only while the outcome is not known. */
export function askClaudeResetSpendAgain(key: string): Promise<void> {
  const current = getClaudeResetSpend(key);
  if (current.inFlight || !unresolved(current.outcome)) return Promise.resolve();
  return deliver(key, current);
}

/** Drops a shown answer or refusal; an outcome not known yet stays. */
export function dismissClaudeResetSpend(key: string) {
  const current = getClaudeResetSpend(key);
  if (current.outcome && !current.confirmOpen && !claudeResetSpendBusy(current)) put(key, IDLE);
}

async function deliver(key: string, current: ClaudeResetSpendSeat) {
  const delivery = current.delivery;
  const post = posts.get(key);
  if (!delivery || !post) return;
  // The latch: set before anything is awaited.
  put(key, { ...current, inFlight: true });
  let settled: { data: unknown } | { error: unknown };
  try {
    settled = { data: await sendClaudeResetSpend(post, delivery.key, delivery.body) };
  } catch (error) {
    settled = { error };
  }
  const outcome = readClaudeResetSpendOutcome(settled);
  if (!outcome) {
    // 401: the page's own sign-out runs and this delivery reached no route. An
    // earlier delivery's unknown outcome stays, with its key, for "Ask the hub again".
    put(key, current.outcome ? { ...current, inFlight: false } : IDLE);
    return;
  }
  put(key, { delivery, confirmOpen: false, inFlight: false, outcome });
  if (!unresolved(outcome)) {
    posts.delete(key);
    invalidateClaudeLimitResets();
  }
}
