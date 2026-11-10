import { apiClient } from './client';

export interface ClaudeLimitResetSeat {
  name: string;
  auth_index: string;
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

// One cached management GET for all cards. The server owns upstream cadence.
export function readClaudeLimitResets(): Promise<LimitResetsResponse> {
  const revision = apiClient.getConnectionRevision();
  if (cached?.revision === revision && Date.now() < cached.until)
    return Promise.resolve(cached.value);
  if (pending?.revision === revision) return pending.promise;
  const startedAt = Date.now();
  const promise = apiClient
    .get<LimitResetsResponse>('/claude/limit-resets')
    .then((value) => {
      if (apiClient.getConnectionRevision() === revision) {
        cached = { revision, until: startedAt + 60_000, value };
      }
      return value;
    })
    .finally(() => {
      if (pending?.promise === promise) pending = undefined;
    });
  pending = { revision, promise };
  return promise;
}
