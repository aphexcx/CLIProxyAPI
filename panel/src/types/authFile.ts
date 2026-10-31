/**
 * Auth-file types.
 */

import type { RecentRequestBucket } from '@/utils/recentRequests';
import type { ClaudeUsagePayload, CodexUsagePayload } from './quota';

/** Native poll-loop snapshot carried by v8 credentials and credentials/quota. */
export interface QuotaCacheMetadata {
  status: 'ok' | 'not_polled_yet' | 'polling_paused';
  stale: boolean;
  polled_at: string | null;
  age_seconds: number | null;
  next_poll_at: string | null;
  refreshing: boolean;
  last_status: number;
  last_poll_at: string | null;
}

export interface ClaudeCachedUsage extends ClaudeUsagePayload, QuotaCacheMetadata {}
export interface CodexCachedUsage extends CodexUsagePayload, QuotaCacheMetadata {}

export type AuthFileType =
  | 'qwen'
  | 'kimi'
  | 'gemini'
  | 'aistudio'
  | 'claude'
  | 'codex'
  | 'devin'
  | 'meta'
  | 'antigravity'
  | 'xai'
  | 'iflow'
  | 'vertex'
  | 'empty'
  | 'unknown';

export interface AuthFileCooldown {
  scope: 'model' | 'credential';
  modelKey?: string;
  reason: string;
  retryAt: string;
  remainingSeconds: number;
  backoffLevel?: number;
  httpStatus?: number;
}

export interface AuthFileCooldownSnapshot {
  /** Server observation time, not the start of the cooldown. */
  observedAt?: string;
  /** Local receipt time anchors relative timers without relying on synchronized clocks. */
  receivedAtMs: number;
  /** null = runtime state unknown; [] = known, with no active timers. */
  records: AuthFileCooldown[] | null;
}

export interface AuthFileItem {
  name: string;
  claude_usage?: ClaudeCachedUsage;
  codex_usage?: CodexCachedUsage;
  siwcBindSupported?: boolean;
  useSiwc?: boolean;
  siwcBound?: boolean;
  siwcExpiresAt?: string;
  type?: AuthFileType | string;
  provider?: string;
  /**
   * Account email supplied by the file scanner or credential registry.
   * Never display or search account/account_type: API-key credentials may expose
   * the API key itself through those fields.
   */
  email?: string;
  /** Claude organization name supplied by v8 credential metadata. */
  organization_name?: string;
  /** Claude plan tier label ("Max 20x", "Team premium") supplied by v8 credential metadata. */
  plan_tier?: string;
  /** Codex plan supplied by v8 credential metadata. */
  plan_type?: string;
  /** GCP / Vertex project ID, used when account email is absent. */
  projectId?: string;
  size?: number;
  authIndex?: string | number | null;
  /** Generic plugin quota capability advertised by CPA's auth-files API. */
  supportsQuota?: boolean;
  quotaProvider?: string;
  /** Backend-selected quota plugin; authoritative when supplied by v8 credentials. */
  quotaPluginId?: string;
  runtimeOnly?: boolean | string;
  disabled?: boolean;
  unavailable?: boolean;
  status?: string;
  statusMessage?: string;
  lastRefresh?: string | number;
  modified?: number;
  priority?: number;
  weight?: number;
  note?: string;
  success?: unknown;
  failed?: unknown;
  /** Cumulative success/failure counts normalized by the API boundary. */
  successCount?: number;
  failureCount?: number;
  recent_requests?: RecentRequestBucket[];
  recentRequests?: RecentRequestBucket[];
  /** Absent on older servers. Never interpreted as credential health. */
  cooldownSnapshot?: AuthFileCooldownSnapshot;
  [key: string]: unknown;
}

export interface AuthFilesResponse {
  files: AuthFileItem[];
  total?: number;
  observed_at?: unknown;
  observedAt?: string;
}
