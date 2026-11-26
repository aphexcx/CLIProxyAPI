import { useEffect, useState, useSyncExternalStore } from 'react';
import { apiClient } from '@/services/api/client';
import {
  readClaudeLimitResets,
  matchClaudeLimitResetSeat,
  getClaudeResetCredentialRevision,
  subscribeClaudeResetCredentials,
  type ClaudeLimitResetSeat,
} from '@/services/api/claudeLimitResets';

/**
 * A card's row of the hub's cached reset read, through the one cached GET for
 * all cards, refreshed each minute. A failed refresh keeps the row it had, so
 * its numbers stay on screen marked stale.
 */
export function useClaudeLimitResetRead(name: string, generation?: string) {
  const [result, setResult] = useState<{
    seat?: ClaudeLimitResetSeat;
    rule?: string;
    failed?: boolean;
    revision: number;
    name: string;
    generation?: string;
    credentialRevision: number;
  }>();
  const revision = apiClient.getConnectionRevision();
  const credentialRevision = useSyncExternalStore(
    subscribeClaudeResetCredentials,
    getClaudeResetCredentialRevision,
    getClaudeResetCredentialRevision
  );
  useEffect(() => {
    let active = true;
    const load = async () => {
      try {
        const response = await readClaudeLimitResets();
        if (active && apiClient.getConnectionRevision() === revision) {
          setResult({
            seat: matchClaudeLimitResetSeat(response.seats, name, generation),
            rule: response.ranking_rule,
            revision,
            name,
            generation,
            credentialRevision,
          });
        }
      } catch {
        if (active)
          setResult((previous) => ({
            ...(previous?.revision === revision &&
            previous.name === name &&
            previous.generation === generation &&
            previous.credentialRevision === credentialRevision
              ? previous
              : {}),
            failed: true,
            revision,
            name,
            generation,
            credentialRevision,
          }));
      }
    };
    void load();
    const timer = setInterval(() => void load(), 60_000);
    return () => {
      active = false;
      clearInterval(timer);
    };
  }, [name, revision, generation, credentialRevision]);
  const current =
    result?.revision === revision &&
    result.name === name &&
    result.generation === generation &&
    result.credentialRevision === credentialRevision
      ? result
      : undefined;
  return {
    seat: current?.seat,
    rule: current?.rule,
    failed: current?.failed,
    loaded: current !== undefined,
  };
}

/** A second-resolution clock: monotonic for the read's age, wall time for cooldowns. */
export function useClaudeResetClock() {
  const [clock, setClock] = useState(() => ({ mono: performance.now(), wall: Date.now() }));
  useEffect(() => {
    const timer = setInterval(() => setClock({ mono: performance.now(), wall: Date.now() }), 1_000);
    return () => clearInterval(timer);
  }, []);
  return clock;
}
