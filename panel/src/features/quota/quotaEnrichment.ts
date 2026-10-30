import type { TFunction } from 'i18next';
import type { AuthFileItem, ClaudeQuotaState } from '@/types';
import {
  captureQuotaCacheGeneration,
  commitIfQuotaCacheCurrent,
  useQuotaStore,
} from '@/stores/useQuotaStore';
import { getQuotaCacheKey } from '@/utils/quota/identity';
import type { QuotaAdapter, QuotaCardState, QuotaMapUpdater } from './providers';

/** Optional metadata must neither delay quota rendering nor replace a newer result. */
export async function enrichQuotaInBackground(
  adapter: QuotaAdapter,
  file: AuthFileItem,
  data: unknown,
  expectedState: QuotaCardState,
  t: TFunction
): Promise<void> {
  if (!adapter.enrichQuota) return;
  const cacheKey = getQuotaCacheKey(file);
  const currentState = () => adapter.storeSelector(useQuotaStore.getState())[cacheKey];
  const acceptedState = currentState();
  if (acceptedState !== expectedState) {
    // Claude may retain display windows while accepting this exact server
    // snapshot. Keep identity checks against the merged state during enrichment.
    const expectedCache =
      adapter.type === 'claude' ? (expectedState as ClaudeQuotaState).serverCache : undefined;
    if (
      !expectedCache ||
      (acceptedState as ClaudeQuotaState | undefined)?.serverCache !== expectedCache
    )
      return;
  }
  const generation = captureQuotaCacheGeneration(file.name);

  try {
    const enriched = await adapter.enrichQuota(file, data, t);
    if (enriched === data) return;
    commitIfQuotaCacheCurrent(generation, () => {
      const setQuota = useQuotaStore.getState()[adapter.storeSetter] as QuotaMapUpdater;
      setQuota((prev) => {
        if (prev[cacheKey] !== acceptedState) return prev;
        return { ...prev, [cacheKey]: adapter.buildSuccessState(enriched) };
      });
    });
  } catch {
    // Keep the successful quota and its original plan fallback on optional failures.
  }
}
