import { apiClient } from './client';

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
    }[];
  } | null;
  error?: string;
  age_seconds: number | null;
  stale: boolean;
  best_click_rank: number | null;
  rank_reason: string;
}

interface LimitResetsResponse {
  seats: ClaudeLimitResetSeat[];
  ranking_rule: string;
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
      cached = { revision, until: startedAt + 60_000, value };
      return value;
    })
    .finally(() => {
      if (pending?.promise === promise) pending = undefined;
    });
  pending = { revision, promise };
  return promise;
}
