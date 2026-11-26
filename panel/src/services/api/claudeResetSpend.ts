/**
 * The page's one way to spend a banked Claude reset: the hub's spend route,
 * which reads the seat's grants fresh from the provider and claims one grant
 * of the named kind. The page sends only auth_index and kind; it never names a
 * grant and never retries a claim whose outcome is unknown.
 *
 * Every delivery of one confirm carries the same Idempotency-Key, so the hub
 * answers a second delivery from its ledger and sends nothing.
 */

import { apiClient } from './client';
import type { ClaudeResetKindSummary } from './claudeLimitResets';

export const CLAUDE_RESET_SPEND_PATH = '/claude/limit-resets/spend';
/**
 * Longer than the page's default: the route works under the request's own
 * context, so a request the browser drops can cut a claim in flight.
 */
export const CLAUDE_RESET_SPEND_TIMEOUT_MS = 120_000;
const KEY_HEADER = 'Idempotency-Key';
/** The route's 409 for a key whose first delivery has not answered yet. */
const KEY_IN_FLIGHT = 'a spend with this Idempotency-Key is in flight';

export type ClaudeResetSpendKind = 'fable' | 'weekly' | 'full' | 'session';

export interface ClaudeResetSpendBody {
  auth_index: string;
  kind: ClaudeResetSpendKind;
}

export type ClaudeResetSpendPost = (
  path: string,
  body: ClaudeResetSpendBody,
  config: { headers: Record<string, string>; timeout: number }
) => Promise<unknown>;

/** The route's HTTP 200, as it builds a first answer or a replay from its ledger. */
export interface ClaudeResetSpendAnswer {
  auth_index?: string;
  kind?: string;
  grant_id?: string;
  request_id?: string;
  retry?: boolean;
  result?: string;
  replayed?: boolean;
  /** A replay of a claim that was sent and never answered. */
  recorded?: string;
  retry_until?: string;
  kinds_after?: Record<string, ClaudeResetKindSummary> | null;
  /** On a replay: counts only. */
  resets_left_after?: Record<string, number> | null;
  routing?: { cooldown_changed?: boolean; usage_poll_queued?: boolean; note?: string };
  after_read_error?: string;
  ledger_error?: string;
  balance_error?: string;
}

/** A refusal's JSON body; error is the hub's own words. */
export interface ClaudeResetSpendRefusal {
  error: string;
  kinds?: Record<string, ClaudeResetKindSummary>;
  upstream_status?: number;
  cure?: string;
  reason?: string;
  replayed?: boolean;
  ledger_error?: string;
}

export type ClaudeResetSpendOutcome =
  | { type: 'answer'; answer: ClaudeResetSpendAnswer }
  | { type: 'refusal'; status: number; refusal: ClaudeResetSpendRefusal }
  /** A 404 with no JSON body: a hub build without the route. Nothing was sent. */
  | { type: 'no_route' }
  /** A status or body the page cannot read: whether anything was spent is not known. */
  | { type: 'not_known'; status: number }
  /** The hub says this key's first delivery has not answered yet. */
  | { type: 'key_in_flight'; error: string }
  /** A timeout or a network error. */
  | { type: 'no_answer' };

const isRecord = (value: unknown): value is Record<string, unknown> =>
  typeof value === 'object' && value !== null && !Array.isArray(value);

/** One delivery: the path, a body of auth_index and kind only, the key, and the long wait. */
export function sendClaudeResetSpend(
  post: ClaudeResetSpendPost,
  key: string,
  body: ClaudeResetSpendBody
): Promise<unknown> {
  return post(
    CLAUDE_RESET_SPEND_PATH,
    { auth_index: body.auth_index, kind: body.kind },
    { headers: { [KEY_HEADER]: key }, timeout: CLAUDE_RESET_SPEND_TIMEOUT_MS }
  );
}

/** A post pinned to the hub connection current when the confirm opened. */
export function defaultClaudeResetSpendPost(): ClaudeResetSpendPost {
  const client = apiClient.captureConnection();
  return (path, body, config) => client.post(path, body, config);
}

/** A random UUID: printable, no comma, 36 characters, as the route requires. */
export function newClaudeResetSpendKey(): string {
  if (typeof globalThis.crypto?.randomUUID === 'function') return globalThis.crypto.randomUUID();
  return 'xxxxxxxx-xxxx-4xxx-yxxx-xxxxxxxxxxxx'.replace(/[xy]/g, (char) => {
    const value = Math.floor(Math.random() * 16);
    return (char === 'x' ? value : (value & 0x3) | 0x8).toString(16);
  });
}

/**
 * Reads what one delivery settled with. Undefined for a 401: the page's own
 * sign-out handles it and the spend shows nothing of its own.
 */
export function readClaudeResetSpendOutcome(
  settled: { data: unknown } | { error: unknown }
): ClaudeResetSpendOutcome | undefined {
  if ('data' in settled) {
    return isRecord(settled.data)
      ? { type: 'answer', answer: settled.data as ClaudeResetSpendAnswer }
      : { type: 'not_known', status: 200 };
  }
  const error = isRecord(settled.error) ? settled.error : {};
  const status = typeof error.status === 'number' ? error.status : undefined;
  if (status === undefined) return { type: 'no_answer' };
  if (status === 401) return undefined;
  const data = error.data;
  const message = isRecord(data) && typeof data.error === 'string' && data.error ? data.error : '';
  if (status === 409 && message.startsWith(KEY_IN_FLIGHT))
    return { type: 'key_in_flight', error: message };
  if (status === 404 && !isRecord(data)) return { type: 'no_route' };
  if (message)
    return { type: 'refusal', status, refusal: { ...(data as object), error: message } };
  return { type: 'not_known', status };
}
