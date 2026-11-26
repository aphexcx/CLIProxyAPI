import { useState, useSyncExternalStore, type ReactNode } from 'react';
import type { TFunction } from 'i18next';
import { useTranslation } from 'react-i18next';
import { Modal } from '@/components/ui/Modal';
import type { AuthFileItem } from '@/types';
import { apiClient } from '@/services/api/client';
import { claudeLimitResetAge, type ClaudeLimitResetSeat } from '@/services/api/claudeLimitResets';
import {
  defaultClaudeResetSpendPost,
  newClaudeResetSpendKey,
  type ClaudeResetSpendAnswer,
  type ClaudeResetSpendRefusal,
} from '@/services/api/claudeResetSpend';
import { deriveAuthFileIdentity } from '@/features/authFiles/identity';
import { getQuotaSeatIdentity } from '@/utils/quota/identity';
import type { SeatFreshness } from '../../seatCard';
import {
  askClaudeResetSpendAgain,
  cancelClaudeResetSpend,
  claudeResetCardState,
  claudeResetControls,
  claudeResetCountLines,
  claudeResetKindLines,
  claudeResetKindName,
  claudeResetLastSpendText,
  claudeResetResultWord,
  claudeResetSpendSeatKey,
  claudeResetUsageStale,
  confirmClaudeResetSpend,
  dismissClaudeResetSpend,
  getClaudeResetSpend,
  openClaudeResetSpend,
  subscribeClaudeResetSpend,
  type ClaudeResetControlBlock,
  type ClaudeResetSpendKind,
  type ClaudeResetSpendSeat,
} from './claudeResetSpendState';
import { ClaudeResetKindLines } from './ClaudeResetKinds';
import { useClaudeLimitResetRead, useClaudeResetClock } from './useClaudeLimitResetRead';

type SeatIdentity = { primary: string; organization: string; fileName: string };
type Usage = { freshness: SeatFreshness | null; text: string };

const kindWords = (t: TFunction, kind: string) => t(`claude_reset_spend.kind_${kind}`);

// Inline styles keep these views free of stylesheets, so tests render them directly.
const SECTION_STYLE = {
  display: 'flex',
  flexDirection: 'column',
  gap: 8,
  fontSize: 12,
  lineHeight: 1.5,
  overflowWrap: 'anywhere',
} as const;
const HEADING_STYLE = { margin: 0, fontSize: 13, fontWeight: 600 } as const;
const LIST_STYLE = {
  listStyle: 'none',
  margin: 0,
  padding: 0,
  display: 'flex',
  flexDirection: 'column',
  gap: 6,
} as const;
const OUTCOME_STYLE = {
  display: 'flex',
  flexDirection: 'column',
  alignItems: 'flex-start',
  gap: 4,
  padding: 10,
  border: '1px solid var(--border-color)',
  borderRadius: 8,
  background: 'var(--bg-secondary)',
  color: 'var(--text-primary)',
} as const;

function blockText(t: TFunction, blocked: ClaudeResetControlBlock, locale?: string) {
  switch (blocked.code) {
    case 'busy':
      return t('claude_reset_spend.disabled_busy');
    case 'cooldown':
      return t('claude_reset_spend.disabled_cooldown', {
        time: new Date(blocked.until).toLocaleString(locale),
      });
    case 'unavailable':
      return t('claude_reset_spend.disabled_unavailable', { reason: blocked.reason });
    case 'none_usable':
      return t('claude_reset_spend.disabled_none_usable');
  }
}

/** The hub's own words, shown exactly as sent. */
const HubSays = ({ t, words }: { t: TFunction; words: unknown }) =>
  typeof words === 'string' && words ? (
    <div>
      {t('claude_reset_spend.hub_says')} {words}
    </div>
  ) : null;

/** The card's "Banked resets" section in Details: one control per kind, the confirm, the outcome. */
export function ClaudeResetSpendSection({ file, usage }: { file: AuthFileItem; usage: Usage }) {
  const read = useClaudeLimitResetRead(file.name, file.credential_generation);
  // Keep the seat's entry visible while the read reloads after an outcome.
  const id = `${file.name}\u0000${file.credential_generation ?? ''}`;
  const [remembered, setRemembered] = useState<{ id: string; authIndex: string }>();
  const rowIndex = read.seat?.auth_index ?? '';
  if (rowIndex && (remembered?.id !== id || remembered.authIndex !== rowIndex))
    setRemembered({ id, authIndex: rowIndex });
  const authIndex = rowIndex || (remembered?.id === id ? remembered.authIndex : '');
  const key = claudeResetSpendSeatKey(apiClient.getConnectionRevision(), authIndex);
  const spend = useSyncExternalStore(
    subscribeClaudeResetSpend,
    () => getClaudeResetSpend(key),
    () => getClaudeResetSpend(key)
  );
  const identity = deriveAuthFileIdentity(file);
  return (
    <ClaudeResetSpendView
      seat={read.seat}
      failed={read.failed}
      loaded={read.loaded}
      identity={{
        primary: identity.primary,
        organization: getQuotaSeatIdentity(file).organization,
        fileName: identity.fullName,
      }}
      usage={usage}
      spend={spend}
      onPress={(kind) => {
        if (!rowIndex) return;
        openClaudeResetSpend(
          key,
          { auth_index: rowIndex, kind },
          { post: defaultClaudeResetSpendPost(), makeKey: newClaudeResetSpendKey }
        );
      }}
      onCancel={() => cancelClaudeResetSpend(key)}
      onConfirm={() => void confirmClaudeResetSpend(key)}
      onAskAgain={() => void askClaudeResetSpendAgain(key)}
      onDismiss={() => dismissClaudeResetSpend(key)}
    />
  );
}

export function ClaudeResetSpendView({
  seat,
  failed,
  loaded,
  identity,
  usage,
  spend,
  onPress,
  onCancel,
  onConfirm,
  onAskAgain,
  onDismiss,
}: {
  seat?: ClaudeLimitResetSeat;
  failed?: boolean;
  loaded?: boolean;
  identity: SeatIdentity;
  usage: Usage;
  spend: ClaudeResetSpendSeat;
  onPress?: (kind: ClaudeResetSpendKind) => void;
  onCancel?: () => void;
  onConfirm?: () => void;
  onAskAgain?: () => void;
  onDismiss?: () => void;
}) {
  const { t, i18n } = useTranslation();
  const clock = useClaudeResetClock();
  const card = claudeResetCardState(seat, { failed, loaded });
  const controls = claudeResetControls(seat, card, spend, clock.wall);
  const kind = spend.delivery?.body.kind;
  let plain: ReactNode = null;
  if (card.state === 'N') plain = t(`claude_reset_spend.state_${card.why}`);
  else if (card.state === 'O') plain = t('claude_reset_spend.state_old_hub');
  else if (!seat?.auth_index) plain = t('claude_reset_spend.state_no_index');
  return (
    <section aria-label={t('claude_reset_spend.section')} style={SECTION_STYLE}>
      <h3 style={HEADING_STYLE}>{t('claude_reset_spend.section')}</h3>
      {plain && <div>{plain}</div>}
      {controls.length > 0 && (
        <ul style={LIST_STYLE}>
          {controls.map((control) => (
            <li key={control.kind}>
              <button
                type="button"
                className="btn btn-secondary btn-sm"
                data-reset-kind={control.kind}
                disabled={Boolean(control.blocked)}
                onClick={() => onPress?.(control.kind)}
              >
                {t('claude_reset_spend.control', { kind: kindWords(t, control.kind) })}
              </button>
              {!control.countKnown && <span> · {t('claude_reset_spend.count_not_known')}</span>}
              {control.blocked && (
                <span> · {blockText(t, control.blocked, i18n.resolvedLanguage)}</span>
              )}
            </li>
          ))}
        </ul>
      )}
      <ClaudeResetOutcomeView
        spend={spend}
        seatName={identity.primary}
        onAskAgain={onAskAgain}
        onDismiss={onDismiss}
      />
      {kind && (
        <Modal
          open={spend.confirmOpen}
          title={t('claude_reset_spend.confirm_title', { kind: kindWords(t, kind) })}
          onClose={() => onCancel?.()}
          closeDisabled={spend.inFlight}
          footer={
            <>
              <button
                type="button"
                className="btn btn-secondary"
                onClick={onCancel}
                disabled={spend.inFlight}
              >
                {t('claude_reset_spend.cancel')}
              </button>
              <button
                type="button"
                className="btn btn-danger"
                onClick={onConfirm}
                disabled={spend.inFlight}
              >
                {t('claude_reset_spend.spend', { kind: kindWords(t, kind) })}
              </button>
            </>
          }
        >
          <ClaudeResetConfirmBody
            seat={seat}
            failed={failed}
            kind={kind}
            identity={identity}
            usage={usage}
            inFlight={spend.inFlight}
          />
        </Modal>
      )}
    </section>
  );
}

/** What the confirm says before one reset is spent; it sends nothing and asks the hub nothing. */
export function ClaudeResetConfirmBody({
  seat,
  failed,
  kind,
  identity,
  usage,
  inFlight,
}: {
  seat?: ClaudeLimitResetSeat;
  failed?: boolean;
  kind: ClaudeResetSpendKind;
  identity: SeatIdentity;
  usage: Usage;
  inFlight: boolean;
}) {
  const { t, i18n } = useTranslation();
  const clock = useClaudeResetClock();
  const locale = i18n.resolvedLanguage;
  const card = claudeResetCardState(seat, { failed });
  const line =
    card.state === 'V'
      ? claudeResetKindLines(seat?.kinds, seat?.cedar_ember?.grants).find((entry) => entry.code === kind)
      : undefined;
  const age = claudeLimitResetAge(seat, clock.mono);
  const left =
    card.state !== 'V'
      ? t('claude_reset_spend.confirm_left_not_known')
      : t(age == null ? 'claude_reset_spend.confirm_left_value_no_age' : 'claude_reset_spend.confirm_left_value', {
          left: line?.summary.resets_left ?? 0,
          usable: line?.summary.usable_resets ?? 0,
          seconds: age,
        });
  const last = seat ? claudeResetLastSpendText(t, seat, locale) : undefined;
  const rows: [string, ReactNode][] = [[t('claude_reset_spend.confirm_seat'), identity.primary]];
  if (identity.organization)
    rows.push([t('claude_reset_spend.confirm_organization'), identity.organization]);
  rows.push(
    [t('claude_reset_spend.confirm_file'), identity.fileName],
    [t('claude_reset_spend.confirm_kind'), claudeResetKindName(t, kind)],
    [t('claude_reset_spend.confirm_left'), left]
  );
  if (line?.summary.soonest_expiry)
    rows.push([
      t('claude_reset_spend.confirm_expiry'),
      new Date(line.summary.soonest_expiry).toLocaleString(locale),
    ]);
  if (line?.clears.length) rows.push([t('claude_reset_spend.confirm_clears'), line.clears.join(', ')]);
  if (last?.recorded) rows.push([t('claude_reset_spend.confirm_last'), last.text]);
  return (
    <div>
      <dl>
        {rows.map(([term, value]) => (
          <div key={term}>
            <dt>{term}</dt>
            <dd>{value}</dd>
          </div>
        ))}
      </dl>
      {card.state === 'V' && (seat?.stale || failed) && <p>{t('claude_reset_spend.confirm_stale_counts')}</p>}
      {claudeResetUsageStale(usage.freshness) && (
        <p>
          {t('claude_reset_spend.confirm_stale_usage', {
            age: usage.text || t('claude_reset_spend.confirm_usage_not_read'),
          })}
        </p>
      )}
      <p>
        <strong>{t('claude_reset_spend.confirm_irreversible')}</strong>
      </p>
      {inFlight && <p role="status">{t('claude_reset_spend.waiting')}</p>}
    </div>
  );
}

function AnswerBody({ t, answer, locale }: { t: TFunction; answer: ClaudeResetSpendAnswer; locale?: string }) {
  const result = claudeResetResultWord(answer.result);
  const replayed = answer.replayed === true;
  const sentence =
    result !== 'unknown'
      ? t(`claude_reset_spend.result_${result}`)
      : answer.retry_until
        ? t('claude_reset_spend.result_unknown', {
            time: new Date(answer.retry_until).toLocaleString(locale),
          })
        : t('claude_reset_spend.result_unknown_no_hold');
  let routing: ReactNode = null;
  if (result === 'reset' || result === 'already_used') {
    const note = !replayed && typeof answer.routing?.note === 'string' ? answer.routing.note : '';
    const queued = replayed ? undefined : answer.routing?.usage_poll_queued;
    routing = (
      <>
        <div>
          {note ? (
            <>
              {t('claude_reset_spend.routing_lead')} {note}
            </>
          ) : (
            t('claude_reset_spend.routing_own')
          )}
        </div>
        {typeof queued === 'boolean' && (
          <div>{t(queued ? 'claude_reset_spend.usage_queued' : 'claude_reset_spend.usage_not_queued')}</div>
        )}
      </>
    );
  }
  let left: ReactNode;
  if (replayed) {
    const counts = claudeResetCountLines(answer.resets_left_after);
    left = counts ? (
      <>
        <div>{t('claude_reset_spend.left_recorded')}</div>
        {counts.map((line) => (
          <div key={line.code}>
            {`${claudeResetKindName(t, line.code)}: ${t('claude_reset_spend.left_count', { left: line.summary.resets_left })}`}
          </div>
        ))}
      </>
    ) : (
      <div>{t('claude_reset_spend.left_not_recorded')}</div>
    );
  } else if (answer.kinds_after && typeof answer.kinds_after === 'object') {
    left = (
      <>
        <div>{t('claude_reset_spend.left_now')}</div>
        <ClaudeResetKindLines lines={claudeResetKindLines(answer.kinds_after)} />
      </>
    );
  } else left = <div>{t('claude_reset_spend.left_not_known')}</div>;
  return (
    <>
      {replayed && <div>{t('claude_reset_spend.replayed')}</div>}
      <HubSays t={t} words={answer.recorded} />
      <div>{sentence}</div>
      {routing}
      {left}
      <HubSays t={t} words={answer.after_read_error} />
      <HubSays t={t} words={answer.ledger_error} />
      <HubSays t={t} words={answer.balance_error} />
    </>
  );
}

function RefusalBody({ t, refusal }: { t: TFunction; refusal: ClaudeResetSpendRefusal }) {
  const holds = refusal.kinds && typeof refusal.kinds === 'object' ? claudeResetKindLines(refusal.kinds) : null;
  return (
    <>
      <div>
        <strong>{t('claude_reset_spend.not_spent')}</strong>
      </div>
      <div>{refusal.error}</div>
      {refusal.replayed === true && <div>{t('claude_reset_spend.replayed')}</div>}
      {typeof refusal.reason === 'string' && refusal.reason && (
        <div>{t('claude_reset_spend.reason', { reason: refusal.reason })}</div>
      )}
      {holds &&
        (holds.length ? (
          <>
            <div>{t('claude_reset_spend.holds')}</div>
            <ClaudeResetKindLines lines={holds} />
          </>
        ) : (
          <div>{t('claude_reset_spend.holds_none')}</div>
        ))}
      {refusal.upstream_status === 429 && <div>{t('claude_reset_spend.upstream_429')}</div>}
      {typeof refusal.cure === 'string' && refusal.cure && (
        <div>
          {t('claude_reset_spend.cure')} {refusal.cure}
        </div>
      )}
      <HubSays t={t} words={refusal.ledger_error} />
    </>
  );
}

/** The last outcome of a seat's spend; it stays until dismissed or a new confirm opens. */
export function ClaudeResetOutcomeView({
  spend,
  seatName,
  onAskAgain,
  onDismiss,
}: {
  spend: ClaudeResetSpendSeat;
  seatName: string;
  onAskAgain?: () => void;
  onDismiss?: () => void;
}) {
  const { t, i18n } = useTranslation();
  const outcome = spend.outcome;
  if (!outcome) return null;
  const kind = outcome.type === 'answer' && outcome.answer.kind ? outcome.answer.kind : spend.delivery?.body.kind;
  const heading = t('claude_reset_spend.outcome_heading', {
    kind: claudeResetKindName(t, kind),
    seat: seatName,
  });
  let body: ReactNode;
  let settled = true;
  switch (outcome.type) {
    case 'answer':
      body = <AnswerBody t={t} answer={outcome.answer} locale={i18n.resolvedLanguage} />;
      break;
    case 'refusal':
      body = <RefusalBody t={t} refusal={outcome.refusal} />;
      break;
    case 'no_route':
      body = <div>{t('claude_reset_spend.no_route')}</div>;
      break;
    default:
      settled = false;
      body = (
        <>
          {outcome.type === 'not_known' && (
            <div>{t('claude_reset_spend.http_not_known', { status: outcome.status })}</div>
          )}
          <div>{t('claude_reset_spend.no_answer')}</div>
          {outcome.type === 'key_in_flight' && <HubSays t={t} words={outcome.error} />}
          {spend.inFlight && <div role="status">{t('claude_reset_spend.waiting')}</div>}
          <button
            type="button"
            className="btn btn-secondary btn-sm"
            onClick={onAskAgain}
            disabled={spend.inFlight}
          >
            {t('claude_reset_spend.ask_again')}
          </button>
        </>
      );
  }
  return (
    <div role={outcome.type === 'answer' ? 'status' : 'alert'} style={OUTCOME_STYLE}>
      <div>
        <strong>{heading}</strong>
      </div>
      {body}
      {settled && (
        <button type="button" className="btn btn-ghost btn-sm" onClick={onDismiss}>
          {t('claude_reset_spend.dismiss')}
        </button>
      )}
    </div>
  );
}
