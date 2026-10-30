/**
 * Quota cache that survives route switches.
 */

import { create } from 'zustand';
import { getQuotaCacheFileName } from '@/utils/quota/identity';
import type {
  AntigravityQuotaState,
  ClaudeQuotaState,
  CodexQuotaState,
  DevinQuotaState,
  KimiQuotaState,
  MetaQuotaState,
  PluginQuotaState,
  XaiQuotaState,
} from '@/types';

type QuotaUpdater<T> = T | ((prev: T) => T);

interface QuotaStoreState {
  cacheGeneration: number;
  fileGenerations: Record<string, number>;
  antigravityQuota: Record<string, AntigravityQuotaState>;
  claudeQuota: Record<string, ClaudeQuotaState>;
  codexQuota: Record<string, CodexQuotaState>;
  devinQuota: Record<string, DevinQuotaState>;
  kimiQuota: Record<string, KimiQuotaState>;
  metaQuota: Record<string, MetaQuotaState>;
  pluginQuota: Record<string, PluginQuotaState>;
  xaiQuota: Record<string, XaiQuotaState>;
  setAntigravityQuota: (updater: QuotaUpdater<Record<string, AntigravityQuotaState>>) => void;
  setClaudeQuota: (updater: QuotaUpdater<Record<string, ClaudeQuotaState>>) => void;
  setCodexQuota: (updater: QuotaUpdater<Record<string, CodexQuotaState>>) => void;
  setDevinQuota: (updater: QuotaUpdater<Record<string, DevinQuotaState>>) => void;
  setKimiQuota: (updater: QuotaUpdater<Record<string, KimiQuotaState>>) => void;
  setMetaQuota: (updater: QuotaUpdater<Record<string, MetaQuotaState>>) => void;
  setPluginQuota: (updater: QuotaUpdater<Record<string, PluginQuotaState>>) => void;
  setXaiQuota: (updater: QuotaUpdater<Record<string, XaiQuotaState>>) => void;
  clearQuotaCache: (names?: string[]) => void;
}

const resolveUpdater = <T>(updater: QuotaUpdater<T>, prev: T): T => {
  if (typeof updater === 'function') {
    return (updater as (value: T) => T)(prev);
  }
  return updater;
};

export const useQuotaStore = create<QuotaStoreState>((set) => ({
  cacheGeneration: 0,
  fileGenerations: {},
  antigravityQuota: {},
  claudeQuota: {},
  codexQuota: {},
  devinQuota: {},
  kimiQuota: {},
  metaQuota: {},
  pluginQuota: {},
  xaiQuota: {},
  setAntigravityQuota: (updater) =>
    set((state) => ({
      antigravityQuota: resolveUpdater(updater, state.antigravityQuota),
    })),
  setClaudeQuota: (updater) =>
    set((state) => {
      const next = resolveUpdater(updater, state.claudeQuota);
      let retained = next;
      for (const [key, quota] of Object.entries(next)) {
        const previous = state.claudeQuota[key];
        if (previous?.serverCache && quota.serverCache) {
          const previousPoll = Date.parse(
            previous.serverCache.lastPollAt ?? previous.serverCache.polledAt ?? ''
          );
          const nextPoll = Date.parse(
            quota.serverCache.lastPollAt ?? quota.serverCache.polledAt ?? ''
          );
          const previousObservation = Date.parse(previous.serverCache.polledAt ?? '');
          const nextObservation = Date.parse(quota.serverCache.polledAt ?? '');
          // Null timestamps can be a newer server cache after a restart. Order
          // those diagnostics by request sequence, not the retained display time.
          if (
            quota.serverCache.requestSequence < previous.serverCache.requestSequence ||
            (Number.isFinite(previousPoll) &&
              Number.isFinite(nextPoll) &&
              nextPoll < previousPoll) ||
            (Number.isFinite(previousObservation) &&
              Number.isFinite(nextObservation) &&
              nextObservation < previousObservation)
          ) {
            if (retained === next) retained = { ...next };
            // A coalesced request can finish after a newer list snapshot was
            // retained by the loading state. Complete the local operation
            // without replacing that newer server observation.
            retained[key] =
              previous.status === 'loading' && quota.status === 'success'
                ? { ...previous, status: 'success', error: undefined, errorStatus: undefined }
                : previous;
            continue;
          }
        }
        if (
          previous &&
          quota !== previous &&
          (quota.status === 'loading' ||
            quota.status === 'error' ||
            quota.serverCache?.polledAt === null) &&
          !quota.observedAt &&
          (previous.serverCache ||
            previous.observedAt ||
            previous.windows.length ||
            previous.extraUsage ||
            previous.dollarWindows?.length)
        ) {
          if (retained === next) retained = { ...next };
          retained[key] = {
            ...quota,
            windows: quota.windows.length
              ? [
                  ...quota.windows,
                  ...previous.windows.filter(
                    (retainedWindow) =>
                      !quota.windows.some((window) => window.id === retainedWindow.id)
                  ),
                ]
              : previous.windows,
            extraUsage: quota.extraUsage ?? previous.extraUsage,
            dollarWindows: quota.dollarWindows?.length
              ? quota.dollarWindows
              : previous.dollarWindows,
            observedAt: previous.observedAt,
            serverCache: quota.serverCache ?? previous.serverCache,
            planType: quota.planType ?? previous.planType,
            stale:
              quota.serverCache?.polledAt === null || quota.status === 'error' || previous.stale,
          };
        }
      }
      return { claudeQuota: retained };
    }),
  setCodexQuota: (updater) =>
    set((state) => {
      const next = resolveUpdater(updater, state.codexQuota);
      let retained = next;
      for (const [key, quota] of Object.entries(next)) {
        const previous = state.codexQuota[key];
        if (previous?.serverCache && quota.serverCache) {
          const prior = previous.serverCache;
          const incoming = quota.serverCache;
          if (
            incoming.requestSequence < prior.requestSequence ||
            Date.parse(incoming.lastPollAt ?? incoming.polledAt ?? '') <
              Date.parse(prior.lastPollAt ?? prior.polledAt ?? '') ||
            Date.parse(incoming.polledAt ?? '') < Date.parse(prior.polledAt ?? '')
          ) {
            if (retained === next) retained = { ...next };
            retained[key] =
              previous.status === 'loading' && quota.status === 'success'
                ? { ...previous, status: 'success', error: undefined, errorStatus: undefined }
                : previous;
            continue;
          }
        }
        if (
          previous &&
          quota !== previous &&
          !quota.observedAt &&
          (quota.status === 'loading' ||
            quota.status === 'error' ||
            quota.serverCache?.polledAt === null) &&
          (previous.serverCache || previous.observedAt || previous.windows.length)
        ) {
          if (retained === next) retained = { ...next };
          retained[key] = {
            ...previous,
            ...quota,
            windows: quota.windows.length ? quota.windows : previous.windows,
            observedAt: previous.observedAt,
            serverCache: quota.serverCache ?? previous.serverCache,
            planType: quota.planType ?? previous.planType,
            subscriptionActiveUntil:
              quota.subscriptionActiveUntil ?? previous.subscriptionActiveUntil,
            creditBalance: quota.creditBalance ?? previous.creditBalance,
            stale:
              quota.serverCache?.polledAt === null || quota.status === 'error' || previous.stale,
          };
        }
      }
      return { codexQuota: retained };
    }),
  setDevinQuota: (updater) =>
    set((state) => ({
      devinQuota: resolveUpdater(updater, state.devinQuota),
    })),
  setKimiQuota: (updater) =>
    set((state) => ({
      kimiQuota: resolveUpdater(updater, state.kimiQuota),
    })),
  setMetaQuota: (updater) =>
    set((state) => ({ metaQuota: resolveUpdater(updater, state.metaQuota) })),
  setPluginQuota: (updater) =>
    set((state) => ({
      pluginQuota: resolveUpdater(updater, state.pluginQuota),
    })),
  setXaiQuota: (updater) =>
    set((state) => ({
      xaiQuota: resolveUpdater(updater, state.xaiQuota),
    })),
  clearQuotaCache: (names) =>
    set((state) => {
      if (names) {
        if (names.length === 0) return state;
        const fileGenerations = { ...state.fileGenerations };
        names.forEach((name) => {
          fileGenerations[name] = (fileGenerations[name] ?? 0) + 1;
        });
        const invalidatedNames = new Set(names);
        const omitNames = <T>(cache: Record<string, T>): Record<string, T> => {
          const keysToDelete = Object.keys(cache).filter((key) =>
            invalidatedNames.has(getQuotaCacheFileName(key))
          );
          if (keysToDelete.length === 0) return cache;
          const next = { ...cache };
          keysToDelete.forEach((key) => delete next[key]);
          return next;
        };
        return {
          fileGenerations,
          antigravityQuota: omitNames(state.antigravityQuota),
          claudeQuota: omitNames(state.claudeQuota),
          codexQuota: omitNames(state.codexQuota),
          devinQuota: omitNames(state.devinQuota),
          kimiQuota: omitNames(state.kimiQuota),
          metaQuota: omitNames(state.metaQuota),
          pluginQuota: omitNames(state.pluginQuota),
          xaiQuota: omitNames(state.xaiQuota),
        };
      }
      return {
        cacheGeneration: state.cacheGeneration + 1,
        fileGenerations: {},
        antigravityQuota: {},
        claudeQuota: {},
        codexQuota: {},
        devinQuota: {},
        kimiQuota: {},
        metaQuota: {},
        pluginQuota: {},
        xaiQuota: {},
      };
    }),
}));

export const captureQuotaCacheGeneration = (name?: string) => {
  const { cacheGeneration, fileGenerations } = useQuotaStore.getState();
  return { cacheGeneration, fileGenerations, name };
};

export const commitIfQuotaCacheCurrent = (
  generation: ReturnType<typeof captureQuotaCacheGeneration>,
  commit: () => void,
  name: string | undefined = generation.name
): boolean => {
  const current = useQuotaStore.getState();
  if (current.cacheGeneration !== generation.cacheGeneration) return false;
  // File-scoped requests survive mutations to unrelated credentials.
  if (name !== undefined) {
    if ((current.fileGenerations[name] ?? 0) !== (generation.fileGenerations[name] ?? 0)) {
      return false;
    }
  } else if (current.fileGenerations !== generation.fileGenerations) {
    return false;
  }
  commit();
  return true;
};
