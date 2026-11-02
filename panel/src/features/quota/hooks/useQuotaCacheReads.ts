import { useEffect } from 'react';
import { useAuthStore } from '@/stores';
import { subscribeQuotaCacheReads } from '../cachedQuotaReads';

/** A visible native-cache card joins the shared cache-only list reader while connected. */
export function useQuotaCacheReads(hasServerCache: boolean): void {
  const connected = useAuthStore((state) => state.connectionStatus === 'connected');
  const readCache = hasServerCache && connected;
  useEffect(() => {
    if (readCache) return subscribeQuotaCacheReads();
  }, [readCache]);
}
