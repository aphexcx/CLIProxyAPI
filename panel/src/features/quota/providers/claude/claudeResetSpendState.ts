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
 *
 * After an outcome not known yet, only the hub's own record of the key settles
 * the click. A confirm that has started to close sends nothing, and the card's
 * press checks that the seat is still the sign-in the confirm opened on.
 */

import type { TFunction } from 'i18next';
import {
  invalidateClaudeLimitResets,
  type ClaudeLimitResetSeat,
  type ClaudeResetKindSummary,
} from '@/services/api/claudeLimitResets';
import {
  readClaudeResetSpendAskAgain,
  readClaudeResetSpendOutcome,
  sendClaudeResetSpend,
  type ClaudeResetSpendBody,
  type ClaudeResetSpendCheckRead,
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

/** What a confirm opened on: the sign-in its press checks, and the lines it shows. */
export interface ClaudeResetSpendOpened {
  /** The card's file name. */
  fileName: string;
  /** The row's credential_generation; the row's auth_index is the body's. */
  generation: string;
  /** The confirm's Seat, Organization and File lines, as they read when it opened. */
  identity: { primary: string; organization: string; fileName: string };
}

/** State 6: one seat's open confirm with its key, the send in flight, the last outcome. */
export interface ClaudeResetSpendSeat {
  delivery?: { key: string; body: ClaudeResetSpendBody };
  confirmOpen: boolean;
  inFlight: boolean;
  outcome?: ClaudeResetSpendOutcome;
  /** The confirm has started to close: no press sends any more. */
  closing?: boolean;
  /** Set when the card opened the confirm: its press sends only through the check. */
  opened?: ClaudeResetSpendOpened;
  /** The press's check: the sign-in changed (never sent), or its read failed. */
  check?: 'changed' | 'read_failed';
}

const IDLE: ClaudeResetSpendSeat = Object.freeze({ confirmOpen: false, inFlight: false });
const seats = new Map<string, ClaudeResetSpendSeat>();
const posts = new Map<string, ClaudeResetSpendPost>();
/** The auth_index each card's last confirm used, by hub and file name. */
const indexByFile = new Map<string, string>();
const listeners = new Set<() => void>();

/**
 * One hub however its address is spelled: scheme and host in lower case, no
 * default port, no trailing slash. Equal identities are always one endpoint.
 */
export function claudeResetHubIdentity(apiBase: string) {
  try {
    const url = new URL(apiBase);
    return `${url.origin}${url.pathname.replace(/\/+$/, '')}`;
  } catch {
    return apiBase.trim();
  }
}

/**
 * One entry per hub and row auth_index. Nothing transient is part of it: a
 * sign-out and sign-in, another spelling of the hub's address, folding Details
 * or a failed read never orphan a spend whose outcome is not known yet.
 */
export const claudeResetSpendSeatKey = (hub: string, authIndex: string) =>
  `${claudeResetHubIdentity(hub)}\u0000${authIndex}`;

const fileKey = (hub: string, fileName: string) =>
  `${claudeResetHubIdentity(hub)}\u0000${fileName}`;

/**
 * A card's entry: by the row's auth_index, or, while the read has no row,
 * by the auth_index the card's last confirm used.
 */
export function claudeResetSpendSeatKeyFor(hub: string, fileName: string, rowIndex?: string) {
  return claudeResetSpendSeatKey(
    hub,
    rowIndex || indexByFile.get(fileKey(hub, fileName)) || ''
  );
}

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
  outcome?.type === 'no_answer' ||
  outcome?.type === 'key_in_flight' ||
  outcome?.type === 'not_known' ||
  outcome?.type === 'not_from_record';

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
  deps: {
    post: ClaudeResetSpendPost;
    makeKey: () => string;
    /** The card that opened it, so the card finds its entry while the read has no row. */
    file?: { hub: string; name: string };
    /** What the card opened it on; its press then sends only through the check. */
    opened?: ClaudeResetSpendOpened;
  }
): boolean {
  const current = getClaudeResetSpend(key);
  if (current.confirmOpen || claudeResetSpendBusy(current)) return false;
  posts.set(key, deps.post);
  if (deps.file) indexByFile.set(fileKey(deps.file.hub, deps.file.name), body.auth_index);
  put(key, {
    delivery: { key: deps.makeKey(), body: { auth_index: body.auth_index, kind: body.kind } },
    confirmOpen: true,
    inFlight: false,
    ...(deps.opened && { opened: { ...deps.opened, identity: { ...deps.opened.identity } } }),
  });
  return true;
}

/** Closes a confirm that has sent nothing; refused while its send is in flight. */
export function cancelClaudeResetSpend(key: string) {
  const current = getClaudeResetSpend(key);
  if (current.confirmOpen && !current.inFlight) put(key, IDLE);
}

/**
 * The confirm has started to close (Escape or the X, before the dialog's
 * fade): no press sends from now on. Only for an open confirm that has sent
 * nothing; the close then ends the entry and its key, as Cancel does.
 */
export function markClaudeResetSpendClosing(key: string) {
  const current = getClaudeResetSpend(key);
  if (current.confirmOpen && !current.inFlight && !current.closing) put(key, { ...current, closing: true });
}

/** An open confirm that may still make its one send. */
const sendable = (current: ClaudeResetSpendSeat) =>
  current.confirmOpen && !current.inFlight && !current.outcome && !current.closing;

/**
 * The confirm's one send, with no check. A second press, even in the same
 * moment, sends nothing, and neither does a press on a confirm that has
 * started to close or on one the card opened (that one sends only through
 * confirmClaudeResetSpendChecked). A post given here (the connection current
 * at the press) replaces the one the confirm opened with; the key and the
 * body never change.
 */
export function confirmClaudeResetSpend(key: string, post?: ClaudeResetSpendPost): Promise<void> {
  const current = getClaudeResetSpend(key);
  if (!sendable(current) || current.opened) return Promise.resolve();
  return deliver(key, current, post);
}

/** The check's verdict on one read of the hub; a body that is not the read is a failed read. */
function sameSignIn(
  read: unknown,
  opened: ClaudeResetSpendOpened,
  authIndex: string
): 'same' | 'changed' | 'read_failed' {
  if (!isRecord(read) || !Array.isArray(read.seats)) return 'read_failed';
  return read.seats.some(
    (row) =>
      isRecord(row) &&
      row.name === opened.fileName &&
      row.auth_index === authIndex &&
      row.credential_generation === opened.generation
  )
    ? 'same'
    : 'changed';
}

/**
 * The card's press on Spend: the confirm's one send, after a check that the
 * seat is still the sign-in the confirm opened on. The latch is taken before
 * anything is awaited, so a second press reads nothing and sends nothing.
 * The check: the card's file entry has the same name and credential_generation,
 * and one read of the hub, through the post's own connection, lists a row of
 * that file name with the same auth_index and credential_generation. When the
 * sign-in changed nothing is ever sent under this key and the page's cached
 * read is dropped; when the read fails nothing is sent and a new press checks
 * again, under the same key.
 */
export async function confirmClaudeResetSpendChecked(
  key: string,
  deps: {
    post: ClaudeResetSpendPost;
    read: ClaudeResetSpendCheckRead;
    onUsageRead?: () => void;
    /** The card's file entry at the press. */
    file: { name: string; credential_generation?: string };
  }
): Promise<void> {
  const current = getClaudeResetSpend(key);
  const { delivery, opened } = current;
  if (!sendable(current) || !delivery || !opened || current.check === 'changed') return;
  // The latch: set before anything is awaited. The confirm reads as a send in flight.
  put(key, { ...current, inFlight: true, check: undefined });
  let check: 'same' | 'changed' | 'read_failed' = 'changed';
  if (deps.file.name === opened.fileName && deps.file.credential_generation === opened.generation) {
    try {
      check = sameSignIn(await deps.read(), opened, delivery.body.auth_index);
    } catch {
      check = 'read_failed';
    }
  }
  const now = getClaudeResetSpend(key);
  if (now.delivery?.key !== delivery.key || !now.inFlight) return;
  if (check !== 'same') {
    put(key, { ...now, inFlight: false, check });
    // The card reads the hub again before a new confirm can open on the seat.
    if (check === 'changed') invalidateClaudeLimitResets();
    return;
  }
  return deliver(key, { ...now, inFlight: false }, deps.post, deps.onUsageRead);
}

/** Delivers the same key and body again; only while the outcome is not known. */
export function askClaudeResetSpendAgain(key: string, post?: ClaudeResetSpendPost): Promise<void> {
  const current = getClaudeResetSpend(key);
  if (current.inFlight || !unresolved(current.outcome)) return Promise.resolve();
  return deliver(key, current, post);
}

/** Drops a shown answer or refusal; an outcome not known yet stays. */
export function dismissClaudeResetSpend(key: string) {
  const current = getClaudeResetSpend(key);
  if (current.outcome && !current.confirmOpen && !claudeResetSpendBusy(current)) put(key, IDLE);
}

async function deliver(key: string, current: ClaudeResetSpendSeat, fresh?: ClaudeResetSpendPost, onUsageRead?: () => void) {
  const delivery = current.delivery;
  if (fresh) posts.set(key, fresh);
  const post = posts.get(key);
  if (!delivery || !post) return;
  // The latch: set before anything is awaited.
  put(key, { ...current, inFlight: true });
  // After an outcome not known yet, only the hub's own record of the key settles the click.
  const later = unresolved(current.outcome);
  let settled: { data: unknown } | { error: unknown };
  try {
    settled = { data: await sendClaudeResetSpend(post, delivery.key, delivery.body, later) };
  } catch (error) {
    settled = { error };
  }
  const outcome = later
    ? readClaudeResetSpendAskAgain(settled, delivery.body)
    : readClaudeResetSpendOutcome(settled);
  if (!outcome) {
    // 401: the page's own sign-out runs and this delivery reached no route. An
    // earlier delivery's unknown outcome stays, with its key, for "Ask the hub again".
    put(key, current.outcome ? { ...current, inFlight: false } : IDLE);
    return;
  }
  put(key, { delivery, confirmOpen: false, inFlight: false, outcome });
  if (!unresolved(outcome)) posts.delete(key);
  // Any HTTP answer, even one whose outcome is not known yet, drops the cached read.
  if (outcome.type !== 'no_answer') invalidateClaudeLimitResets();
  if (outcome.type === 'answer' && outcome.answer.result === 'reset' && !outcome.answer.replayed && outcome.answer.routing?.usage_read) {
    onUsageRead?.();
  }
}

/** Where focus goes when a seat's confirm closes: always an element of that seat's own section. */
export type ClaudeResetFocusTarget = 'kind_button' | 'heading' | 'outcome';

/**
 * After a send of any outcome, known or not: the seat's outcome block, which
 * is what the person reads next. With nothing sent (Cancel, Escape, the X, or
 * a confirm the check refused): the seat's button of that kind, or the
 * section's heading when that button is gone or disabled.
 */
export function claudeResetFocusAfterClose(after: {
  /** The seat's outcome once the confirm has closed. */
  outcome: ClaudeResetSpendOutcome | undefined;
  /** The seat's button of the confirm's kind, as now drawn. */
  kindButton: 'live' | 'disabled' | 'gone';
}): ClaudeResetFocusTarget {
  if (after.outcome) return 'outcome';
  return after.kindButton === 'live' ? 'kind_button' : 'heading';
}
