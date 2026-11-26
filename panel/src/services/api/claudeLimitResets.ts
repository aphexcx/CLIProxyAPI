import { apiClient } from './client';

/** What is left of one kind of banked reset on a seat, as the hub's cached read counts it. */
export interface ClaudeResetKindSummary {
  resets_left: number;
  usable_resets: number;
  /** Expiry of the grant a spend of this kind would use. */
  soonest_expiry: string | null;
}

/** One line of the hub's spend ledger: the seat's last spend through this hub. */
export interface ClaudeResetSpendRecord {
  /** "sent" (outcome not recorded yet) or "answered". */
  phase: string;
  at: string;
  auth_index: string;
  kind?: string;
  grant_id?: string;
  request_id: string;
  retry: boolean;
  result?: string;
  /** Reason code of a claim the hub did not send, or the provider's own code. */
  reason?: string;
  resets_left_before?: Record<string, number> | null;
  resets_left_after?: Record<string, number> | null;
}

export interface ClaudeLimitResetSeat {
  name: string;
  auth_index: string;
  credential_generation: string;
  last_good_at: string | null;
  cedar_ember: {
    eligible: boolean;
    ineligible_reason: string | null;
    at_limit: boolean;
    weekly_resets_at: string | null;
    cooldown_until: string | null;
    grants: {
      id: string;
      label: string;
      resets_left: number;
      ends_at: string | null;
      type: string;
      clears: string[];
      usable_now: boolean;
      /** fable, weekly, full, session or unknown (the hub never spends unknown). */
      kind?: string;
      /** The provider's word for why the grant is not usable now. */
      unavailable_reason?: string | null;
    }[];
  } | null;
  error?: string;
  age_seconds: number | null;
  receivedAt?: number;
  stale: boolean;
  best_click_rank: number | null;
  rank_reason: string;
  /** Absent on a hub build from before the spend route; empty when no grant is listed. */
  kinds?: Record<string, ClaudeResetKindSummary> | null;
  /** Null means no reset was ever spent through this hub, unless last_spend_error is set. */
  last_spend?: ClaudeResetSpendRecord | null;
  /** Present when the hub cannot read its spend ledger: last_spend is then not known. */
  last_spend_error?: string;
}

interface LimitResetsResponse {
  seats: ClaudeLimitResetSeat[];
  ranking_rule: string;
}

// Server age plus monotonic elapsed time avoids browser/server clock skew.
export function claudeLimitResetAge(seat?: ClaudeLimitResetSeat, now = performance.now()) {
  if (seat?.age_seconds == null) return null;
  return seat.age_seconds + Math.max(0, Math.floor((now - (seat.receivedAt ?? now)) / 1_000));
}

let cached: { revision: number; until: number; value: LimitResetsResponse } | undefined;
let pending: { revision: number; promise: Promise<LimitResetsResponse> } | undefined;
let credentialRevision = 0;
let credentials = new Map<string, string>();
const listeners = new Set<() => void>();

export const getClaudeResetCredentialRevision = () => credentialRevision;
export function subscribeClaudeResetCredentials(listener: () => void) {
  listeners.add(listener);
  return () => {
    listeners.delete(listener);
  };
}

export function invalidateClaudeLimitResets() {
  credentialRevision++;
  cached = undefined;
  pending = undefined;
  for (const listener of listeners) listener();
}

export function observeClaudeResetCredentials(
  files: { name: string; credential_generation?: string }[],
  complete = true
) {
  const next = complete ? new Map<string, string>() : new Map(credentials);
  for (const file of files) next.set(file.name, file.credential_generation ?? '');
  if (
    next.size !== credentials.size ||
    [...next].some(([name, generation]) => credentials.get(name) !== generation)
  ) {
    credentials = next;
    invalidateClaudeLimitResets();
  }
}

export function matchClaudeLimitResetSeat(
  seats: ClaudeLimitResetSeat[],
  name: string,
  generation?: string
) {
  return generation
    ? seats.find((seat) => seat.name === name && seat.credential_generation === generation)
    : undefined;
}

// One cached management GET for all cards. The server owns upstream cadence.
export function readClaudeLimitResets(): Promise<LimitResetsResponse> {
  const revision = apiClient.getConnectionRevision();
  const generation = credentialRevision;
  if (cached?.revision === revision && Date.now() < cached.until)
    return Promise.resolve(cached.value);
  if (pending?.revision === revision) return pending.promise;
  const startedAt = Date.now();
  const promise = apiClient
    .get<LimitResetsResponse>('/claude/limit-resets')
    .then((value) => {
      if (apiClient.getConnectionRevision() !== revision || credentialRevision !== generation)
        throw new Error('Reset status belongs to an obsolete credential list');
      const receivedAt = performance.now();
      value = { ...value, seats: value.seats.map((seat) => ({ ...seat, receivedAt })) };
      cached = { revision, until: startedAt + 60_000, value };
      return value;
    })
    .finally(() => {
      if (pending?.promise === promise) pending = undefined;
    });
  pending = { revision, promise };
  return promise;
}
