/**
 * Codex plan in words and its badge class, shared by the quota body and the
 * quota page's seat-card header.
 */

import type { TFunction } from 'i18next';
import { normalizePlanType, resolvePlanTier, PREMIUM_CODEX_PLAN_TYPES } from '@/utils/quota';
import type { QuotaClassMap } from '../../types';

export const getPlanValueClass = (planType: string | null, classes: QuotaClassMap): string => {
  // resolvePlanTier defines the elite/premium ordering, covered by quotaPlanTier tests.
  const tier = resolvePlanTier(planType);
  if (tier === 'elite') return classes.elitePlanValue;
  if (tier === 'premium') return classes.premiumPlanValue;
  return classes.codexPlanValue;
};

/** The plan in words ("Pro 200"); null when the payload names no plan. */
export const getCodexPlanLabel = (t: TFunction, planType?: string | null): string | null => {
  const normalized = normalizePlanType(planType);
  if (!normalized) return null;
  if (normalized === 'self_serve_business_prolite') {
    return t('codex_quota.plan_business_premium');
  }
  if (normalized === 'pro') return t('codex_quota.plan_pro');
  if (PREMIUM_CODEX_PLAN_TYPES.has(normalized) && normalized !== 'pro') {
    return t('codex_quota.plan_prolite');
  }
  if (normalized === 'plus') return t('codex_quota.plan_plus');
  if (normalized === 'team') return t('codex_quota.plan_team');
  if (normalized === 'free') return t('codex_quota.plan_free');
  return planType || normalized;
};
