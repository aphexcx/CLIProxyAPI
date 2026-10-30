import { authFilesApi } from '@/services/api/authFiles';

export const QUOTA_CACHE_READ_INTERVAL_MS = 5_000;
const subscribers = new Set<symbol>();
let timer: ReturnType<typeof setTimeout> | undefined;
let reading = false;

function scheduleRead() {
  if (subscribers.size === 0 || timer !== undefined || reading) return;
  timer = setTimeout(async () => {
    timer = undefined;
    if (subscribers.size === 0) return;
    reading = true;
    try {
      // One cache-only list read seeds all visible seats using the existing
      // connection, file-generation and overlapping-response guards.
      await authFilesApi.list();
    } catch {
      // Retain the last snapshot; the next shared read can recover.
    } finally {
      reading = false;
      scheduleRead();
    }
  }, QUOTA_CACHE_READ_INTERVAL_MS);
}

/** A visible native-cache card joins one shared, delayed list reader. */
export function subscribeQuotaCacheReads(): () => void {
  const subscriber = Symbol();
  subscribers.add(subscriber);
  scheduleRead();
  return () => {
    subscribers.delete(subscriber);
    if (subscribers.size === 0 && timer !== undefined) {
      clearTimeout(timer);
      timer = undefined;
    }
  };
}
