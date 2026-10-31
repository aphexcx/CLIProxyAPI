/**
 * Derive account identity for cards and the SIWC target picker without React.
 *
 * Never read account/account_type: API-key credentials can carry a raw key in
 * account, while OAuth account duplicates email. Do not extract an email from
 * a filename either: hyphens in both local parts and domains make it ambiguous.
 * Only explicit organization and plan metadata may appear below the identity.
 */

import type { AuthFileItem } from '@/types';

export type AuthFileIdentityKind = 'email' | 'projectId' | 'fileName';

export type AuthFileIdentity = {
  /** Primary text; empty when there is no identity, without a made-up placeholder. */
  primary: string;
  /** Primary source; filename fallbacks use monospace rendering. */
  kind: AuthFileIdentityKind;
  /** Organization and tier (Claude) or plan (Codex); null hides the secondary row. */
  secondary: string | null;
  /** Full filename for the secondary row's tooltip. */
  fullName: string;
};

/** Reject non-string backend fields despite AuthFileItem's index signature. */
const readIdentityText = (value: unknown): string =>
  typeof value === 'string' ? value.trim() : '';

/** Strip a case-insensitive .json suffix only when some filename remains. */
export const stripJsonExtension = (name: string): string => {
  const trimmed = name.trim();
  if (trimmed.length <= 5) return trimmed;
  return trimmed.toLowerCase().endsWith('.json') ? trimmed.slice(0, -5) : trimmed;
};

/**
 * Primary fallback: email, project ID, then filename without .json.
 * Secondary text comes only from the corresponding provider's metadata.
 */
export const deriveAuthFileIdentity = (file: AuthFileItem): AuthFileIdentity => {
  const fullName = readIdentityText(file.name);
  const base = stripJsonExtension(fullName);
  const email = readIdentityText(file.email);
  const projectId = readIdentityText(file.projectId);

  const kind: AuthFileIdentityKind = email ? 'email' : projectId ? 'projectId' : 'fileName';
  const primary = email || projectId || base;

  const provider = (readIdentityText(file.type) || readIdentityText(file.provider)).toLowerCase();
  let secondary: string | null = null;
  if (provider === 'claude') {
    const parts = [readIdentityText(file.organization_name), readIdentityText(file.plan_tier)];
    secondary = parts.filter(Boolean).join(' · ') || null;
  } else if (provider === 'codex') {
    const plan = readIdentityText(file.plan_type);
    secondary = plan
      ? plan
          .replace(/[_-]+/g, ' ')
          .toLowerCase()
          .replace(/\b\w/g, (letter) => letter.toUpperCase())
      : null;
  }

  return { primary, kind, secondary, fullName };
};
