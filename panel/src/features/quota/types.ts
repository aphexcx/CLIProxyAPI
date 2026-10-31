/**
 * Typed style contract for the quota rendering layer.
 *
 * Quota bodies render in two hosts with different skins: the quota page
 * (QuotaBody.module.scss) and the auth-file card (AuthFileQuota.module.scss).
 * A host binds its own CSS module into a QuotaClassMap with bindQuotaClasses;
 * a missing class name throws at module initialization with the list of
 * missing keys, replacing the old string styleMap's silent class="undefined".
 */

import type { ReactNode } from 'react';

export interface QuotaClassMap {
  // Quota rows (shared by every provider)
  quotaRow: string;
  quotaRowHeader: string;
  quotaModel: string;
  quotaMeta: string;
  quotaPercent: string;
  quotaReset: string;
  quotaResetRelative: string;
  quotaResetRelativeSoon: string;
  quotaAmount: string;
  quotaMessage: string;
  // Plan chip row (Codex names, reused by claude/antigravity/kimi/xai;
  // premium = gold card, elite = Pro 20x liquid platinum: finalized assets, do not restyle)
  codexPlan: string;
  codexPlanItem: string;
  codexPlanLabel: string;
  codexPlanValue: string;
  premiumPlanValue: string;
  elitePlanValue: string;
  // Codex manual reset credits
  codexResetCredits: string;
  codexResetCreditsTitle: string;
  codexResetCreditRow: string;
  codexResetCreditRowSoon: string;
  codexResetCreditLabel: string;
  codexResetCreditTime: string;
  codexResetCreditsError: string;
  // Antigravity groups
  antigravityQuotaGroup: string;
  antigravityQuotaGroupHeader: string;
  antigravityQuotaGroupTitle: string;
  antigravityQuotaGroupDescription: string;
  // Level bars (QuotaMeter)
  quotaBar: string;
  quotaBarFill: string;
  quotaBarFillHigh: string;
  quotaBarFillMedium: string;
  quotaBarFillLow: string;
}

export const QUOTA_CLASS_KEYS: readonly (keyof QuotaClassMap)[] = [
  'quotaRow',
  'quotaRowHeader',
  'quotaModel',
  'quotaMeta',
  'quotaPercent',
  'quotaReset',
  'quotaResetRelative',
  'quotaResetRelativeSoon',
  'quotaAmount',
  'quotaMessage',
  'codexPlan',
  'codexPlanItem',
  'codexPlanLabel',
  'codexPlanValue',
  'premiumPlanValue',
  'elitePlanValue',
  'codexResetCredits',
  'codexResetCreditsTitle',
  'codexResetCreditRow',
  'codexResetCreditRowSoon',
  'codexResetCreditLabel',
  'codexResetCreditTime',
  'codexResetCreditsError',
  'antigravityQuotaGroup',
  'antigravityQuotaGroupHeader',
  'antigravityQuotaGroupTitle',
  'antigravityQuotaGroupDescription',
  'quotaBar',
  'quotaBarFill',
  'quotaBarFillHigh',
  'quotaBarFillMedium',
  'quotaBarFillLow',
];

/** Host CSS module to typed contract. Throws on a missing key (fail loud); `source` locates the error. */
export function bindQuotaClasses(module: Record<string, string>, source: string): QuotaClassMap {
  const missing = QUOTA_CLASS_KEYS.filter((key) => !module[key]);
  if (missing.length > 0) {
    throw new Error(`[quota] ${source} 缺少额度契约类名: ${missing.join(', ')}`);
  }
  const bound = {} as Record<keyof QuotaClassMap, string>;
  for (const key of QUOTA_CLASS_KEYS) {
    bound[key] = module[key];
  }
  return bound;
}

export interface QuotaBodyProps<TState> {
  quota: TState;
  classes: QuotaClassMap;
  /** Host-owned chips appended to the provider's plan row (`codexPlanItem` spans). */
  planItems?: ReactNode;
  /** Host-owned content placed right after the plan row, before usage rows. */
  afterPlan?: ReactNode;
  /**
   * The host already shows the plan, the usage windows and the cache state;
   * render only the remaining details. Hosts that omit it are unchanged.
   */
  extrasOnly?: boolean;
}
