import type { AuthFileItem } from '@/types';
import { normalizeRecentRequestAuthIndex } from '@/utils/recentRequests';
import { resolveCodexPlanType } from './resolvers';
import { isDevinFile, isPluginQuotaFile, resolveAuthProvider } from './validators';

const QUOTA_IDENTITY_SEPARATOR = '\0';

/**
 * Devin and backend quota providers can expose multiple credential identities
 * from one physical file, distinguished by auth_index.
 */
export function getQuotaCacheKey(file: AuthFileItem): string {
  if (!isDevinFile(file) && !isPluginQuotaFile(file)) return file.name;
  const authIndex = normalizeRecentRequestAuthIndex(file.authIndex);
  return `${file.name}${QUOTA_IDENTITY_SEPARATOR}${authIndex ?? ''}`;
}

/** Disambiguate same-name cards without ever falling back to account (a secret). */
export function getQuotaDisplayName(file: AuthFileItem): string {
  if (!isDevinFile(file) && !isPluginQuotaFile(file)) return file.name;
  const identity = file.email?.trim() || normalizeRecentRequestAuthIndex(file.authIndex);
  return identity ? `${file.name} · ${identity}` : file.name;
}

/** Resolve a cache identity back to the physical filename used by file mutations. */
export function getQuotaCacheFileName(key: string): string {
  const separatorIndex = key.indexOf(QUOTA_IDENTITY_SEPARATOR);
  return separatorIndex === -1 ? key : key.slice(0, separatorIndex);
}

/** Reject non-string backend fields despite AuthFileItem's index signature. */
const readSeatText = (value: unknown): string => (typeof value === 'string' ? value.trim() : '');

/**
 * Who a quota seat belongs to, from explicit account metadata only: never from
 * account/account_type (API-key credentials can carry the key there).
 */
export interface QuotaSeatIdentity {
  /** Normalized provider ("claude", "codex", ...), as the quota tabs classify it. */
  provider: string;
  /** Account email; empty when the credential carries none. */
  email: string;
  /** Claude organization name; empty for other providers or when unknown. */
  organization: string;
  /** Codex plan as stored ("plus", "team"); null for other providers or when unknown. */
  planType: string | null;
  /** Credential file name, kept so a seat can still be matched to its file. */
  fileName: string;
  /** The name a seat without an email has always shown: its file, or Devin/plugin's label. */
  fallbackName: string;
}

export function getQuotaSeatIdentity(file: AuthFileItem): QuotaSeatIdentity {
  const provider = resolveAuthProvider(file);
  return {
    provider,
    email: readSeatText(file.email),
    organization: provider === 'claude' ? readSeatText(file.organization_name) : '',
    planType: provider === 'codex' ? resolveCodexPlanType(file) : null,
    fileName: readSeatText(file.name),
    fallbackName: getQuotaDisplayName(file),
  };
}

/** Seats sort by email (seats without one last), then organization, then file name. */
function compareQuotaSeatIdentity(a: QuotaSeatIdentity, b: QuotaSeatIdentity): number {
  const keys = (seat: QuotaSeatIdentity) => [
    seat.email ? '0' : '1',
    (seat.email || seat.fallbackName).toLowerCase(),
    seat.organization.toLowerCase(),
    seat.fileName.toLowerCase(),
  ];
  const left = keys(a);
  const right = keys(b);
  for (let index = 0; index < left.length; index += 1) {
    if (left[index] !== right[index]) return left[index] < right[index] ? -1 : 1;
  }
  return 0;
}

/**
 * What tells a seat from its twin: the hash a hashed credential file carries after
 * its provider ("claude-1a2b3c4d-..."), else the file's name, plus the auth index
 * for Devin and plugin seats, whose one file can hold several identities.
 */
function getQuotaSeatShortId(file: AuthFileItem): string {
  const fileName = readSeatText(file.name);
  const match = /^[a-z]*-([0-9a-f]{6,})-/i.exec(fileName);
  const shortId = match ? match[1] : fileName.replace(/\.json$/i, '');
  if (!isDevinFile(file) && !isPluginQuotaFile(file)) return shortId;
  const authIndex = normalizeRecentRequestAuthIndex(file.authIndex);
  return authIndex ? `${shortId} #${authIndex}` : shortId;
}

/** The lines a seat is named by, first to last; a null line is not shown. */
export interface QuotaSeatLabel<T> {
  item: T;
  /** Email, or the fallback name for a seat without one. */
  primary: string;
  /** Organization (Claude) or plan (Codex), plus the short id when it alone tells two seats apart. */
  secondary: string | null;
  /** File name; null when the primary line already is the file's name. */
  fileName: string | null;
  /** Every line, for the hover. */
  title: string;
}

/**
 * Name and order seats for one view. Two seats with one email in two organizations
 * already differ by their second line; two that would read the same add their
 * short id, so no two rows ever read as one seat.
 *
 * planLabelFor names a Codex seat's plan; it gets the file's plan and may prefer a
 * fresher one (the card's live quota), so the table and the card say the same.
 */
export function describeQuotaSeats<T extends { file: AuthFileItem }>(
  items: readonly T[],
  planLabelFor: (item: T, filePlanType: string | null) => string | null
): QuotaSeatLabel<T>[] {
  const seats = items
    .map((item) => ({ item, identity: getQuotaSeatIdentity(item.file) }))
    .sort((a, b) => compareQuotaSeatIdentity(a.identity, b.identity));

  const rows = seats.map(({ item, identity }) => {
    const primary = identity.email || identity.fallbackName;
    const plan = identity.provider === 'codex' ? planLabelFor(item, identity.planType) : null;
    const detail = identity.organization || plan || '';
    const readsAs = `${primary}\n${detail}`.toLowerCase();
    return { item, identity, primary, detail, readsAs };
  });
  const readers = new Map<string, number>();
  for (const row of rows) readers.set(row.readsAs, (readers.get(row.readsAs) ?? 0) + 1);

  return rows.map(({ item, identity, primary, detail, readsAs }) => {
    const ambiguous = (readers.get(readsAs) ?? 0) > 1;
    const secondary =
      (ambiguous ? [detail, getQuotaSeatShortId(item.file)].filter(Boolean).join(' · ') : detail) ||
      null;
    const fileName = identity.email && identity.fileName ? identity.fileName : null;
    return {
      item,
      primary,
      secondary,
      fileName,
      title: [primary, secondary, fileName].filter(Boolean).join('\n'),
    };
  });
}
