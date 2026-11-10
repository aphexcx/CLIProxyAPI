import { useEffect, useState } from 'react';
import { apiClient } from '@/services/api/client';
import { readClaudeLimitResets, type ClaudeLimitResetSeat } from '@/services/api/claudeLimitResets';

/** Observations for the holder's own click; deliberately no claim controls. */
export function ClaudeLimitResets({ name }: { name: string }) {
  const [result, setResult] = useState<{
    seat?: ClaudeLimitResetSeat;
    rule?: string;
    failed?: boolean;
    revision: number;
    name: string;
  }>();
  const revision = apiClient.getConnectionRevision();
  useEffect(() => {
    let active = true;
    const load = async () => {
      try {
        const response = await readClaudeLimitResets();
        if (active && apiClient.getConnectionRevision() === revision) {
          setResult({
            seat: response.seats.find((seat) => seat.name === name),
            rule: response.ranking_rule,
            revision,
            name,
          });
        }
      } catch {
        if (active)
          setResult((previous) => ({
            ...(previous?.revision === revision && previous.name === name ? previous : {}),
            failed: true,
            revision,
            name,
          }));
      }
    };
    void load();
    const timer = setInterval(() => void load(), 60_000);
    return () => {
      active = false;
      clearInterval(timer);
    };
  }, [name, revision]);
  const current = result?.revision === revision && result.name === name ? result : undefined;
  return (
    <ClaudeLimitResetsView seat={current?.seat} rule={current?.rule} failed={current?.failed} />
  );
}

export function ClaudeLimitResetsView({
  seat,
  rule,
  failed,
}: {
  seat?: ClaudeLimitResetSeat;
  rule?: string;
  failed?: boolean;
}) {
  const grants = seat?.cedar_ember?.grants ?? [];
  return (
    <div aria-label="Banked resets" title={rule}>
      <strong>
        Resets:{' '}
        {seat?.cedar_ember ? grants.reduce((sum, grant) => sum + grant.resets_left, 0) : '—'}
      </strong>
      {seat?.best_click_rank != null && !seat.stale && !failed && (
        <span> · Best click #{seat.best_click_rank}</span>
      )}
      <div>
        {failed
          ? 'Reset status unavailable'
          : seat?.error || seat?.rank_reason || 'Awaiting cached reset status'}
      </div>
      {seat?.age_seconds != null && (
        <small>
          Last good read: {seat.age_seconds}s ago{seat.stale || failed ? ' · stale' : ''}
        </small>
      )}
      {grants.map((grant) => (
        <div key={grant.id}>
          {grant.label || grant.id}: {grant.resets_left} left · expires{' '}
          {grant.ends_at ? new Date(grant.ends_at).toLocaleString() : 'unknown'}
          {' · '}
          {!failed && !seat?.stale && seat?.cedar_ember?.eligible && grant.usable_now
            ? 'usable now'
            : 'not usable now'}
        </div>
      ))}
    </div>
  );
}
