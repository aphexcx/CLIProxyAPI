package auth

import (
	"encoding/json"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

// Immutable runtime policy travels with snapshots used by selectors.
type quotaUsagePolicy struct {
	modelWindows map[string][]string
	windowCaps   map[string]float64
	maxAge       time.Duration
}

func reportedQuotaProvider(auth *Auth) bool {
	return auth != nil && (strings.EqualFold(auth.Provider, "claude") || strings.EqualFold(auth.Provider, "codex"))
}

func setQuotaUsagePolicy(auth *Auth, cfg *internalconfig.Config) {
	if !reportedQuotaProvider(auth) || cfg == nil {
		return
	}
	quota := cfg.Claude.Quota.WithDefaults()
	interval, _ := time.ParseDuration(quota.PollInterval)
	auth.Quota.usagePolicy = &quotaUsagePolicy{modelWindows: quota.ModelWindows, maxAge: 2 * max(interval, claudeQuotaMinimumPollInterval)}
	if strings.EqualFold(auth.Provider, "claude") && quota.Enabled && !quotaCooldownDisabledForAuthWithConfig(auth, cfg) {
		caps := make(map[string]float64, len(quota.ModelWindows))
		for window := range quota.ModelWindows {
			if window != "five_hour" && window != "seven_day" {
				caps[window] = quota.CapFor(window)
			}
		}
		auth.Quota.usagePolicy.windowCaps = caps
	}
}

func advisoryQuota(quota QuotaState) bool {
	return quota.Exceeded && (quota.Reason == "quota" || quota.Reason == "credential_quota")
}

func quotaFailureOnly(err *Error) bool {
	return err == nil || (err.HTTPStatus == http.StatusTooManyRequests && err.Code != ErrorCodeForceCooldown && !isTransientRateLimitResultError(err))
}

func quotaFailureAt(quota QuotaState, updatedAt time.Time) time.Time {
	if !quota.CooldownAt.IsZero() {
		return quota.CooldownAt
	}
	return updatedAt // Legacy persisted records predate the dedicated barrier.
}

// A full poll replaces windows it no longer reports. Preserve observations
// newer than that poll and keep a watermark so delayed headers cannot re-arm
// omitted windows. Empty polls use the request start as a conservative receipt
// barrier; non-empty polls carry the provider response's receipt timestamp.
func recordQuotaUsageSnapshot(auth *Auth, windows map[string]QuotaWindow, now time.Time, started ...time.Time) bool {
	observedAt := now
	if len(windows) == 0 && len(started) > 0 {
		observedAt = started[0]
	}
	for _, window := range windows {
		if window.ObservedAt.IsZero() || math.IsNaN(window.Utilization) || math.IsInf(window.Utilization, 0) || window.Utilization < 0 {
			return false
		}
		if window.ObservedAt.Before(observedAt) {
			observedAt = window.ObservedAt
		}
	}
	if !observedAt.After(auth.Quota.usageObservedAt) {
		return false
	}
	for name, previous := range auth.Quota.Windows {
		if _, present := windows[name]; !present && previous.ObservedAt.Before(observedAt) {
			delete(auth.Quota.Windows, name)
		}
	}
	auth.Quota.usageObservedAt = observedAt
	return true
}

func quotaUsageShowsRoom(auth *Auth, model string, since, now time.Time) bool {
	if !reportedQuotaProvider(auth) || since.IsZero() {
		return false
	}
	policy := auth.Quota.usagePolicy
	if policy == nil {
		policy = &quotaUsagePolicy{modelWindows: (internalconfig.ClaudeQuotaConfig{}).WithDefaults().ModelWindows, maxAge: 2 * claudeQuotaMinimumPollInterval}
	}
	fresh := func(at time.Time) bool { return at.After(since) && now.Sub(at) <= policy.maxAge }
	observed := fresh(auth.Quota.usageObservedAt)
	names := map[string]bool{"five_hour": true, "seven_day": true}
	if strings.EqualFold(auth.Provider, "claude") {
		for window, models := range policy.modelWindows {
			for _, candidate := range models {
				if canonicalModelKey(candidate) == canonicalModelKey(model) {
					names[window] = true
				}
			}
		}
	} else {
		// Check every window this plan reports, including additional limits.
		if _, monthly := auth.Quota.Windows["monthly"]; monthly {
			delete(names, "seven_day")
			names["monthly"] = true
		}
		for name := range auth.Quota.Windows {
			names[name] = true
		}
	}
	credits := strings.EqualFold(auth.Provider, "codex") && auth.Quota.creditsAvailable && fresh(auth.Quota.creditsObservedAt)
	for name := range names {
		window, ok := auth.Quota.Windows[name]
		if !ok {
			if !fresh(auth.Quota.usageObservedAt) {
				return false // Partial headers cannot prove a window is absent.
			}
			continue // Absent and null windows report no active usage limit.
		}
		if !fresh(window.ObservedAt) || math.IsNaN(window.Utilization) || math.IsInf(window.Utilization, 0) || window.Utilization < 0 {
			return false
		}
		observed = true
		if !window.ResetsAt.IsZero() && !window.ResetsAt.After(now) {
			continue
		}
		limit := float64(100)
		if capPercent, configured := policy.windowCaps[name]; configured {
			limit = capPercent
		}
		if window.Utilization >= limit && !credits {
			return false
		}
	}
	return observed
}

// Clear quota failures contradicted by newer provider usage. Independent
// failures and quota_cap shadows retain ownership of their own restrictions.
func clearContradictedQuotaCooldowns(auth *Auth, now time.Time) bool {
	if !reportedQuotaProvider(auth) || auth.Disabled || auth.Status == StatusDisabled || hasUnauthorizedAuthFailure(auth) {
		return false
	}
	if auth.LastError != nil && auth.LastError.Code == ErrorCodeForceCooldown {
		return false
	}
	if !quotaFailureOnly(auth.LastError) {
		// A credential-wide non-quota error has no model shadow to restore.
		// Only proceed when the error is retained by an independent model layer.
		retained := false
		for _, state := range auth.ModelStates {
			if state == nil {
				continue
			}
			if state.credentialQuotaShadow != nil {
				state = state.credentialQuotaShadow
			}
			if cooldownErrorEqual(state.LastError, auth.LastError) && isModelStateActiveCooldown(state, now) {
				retained = true
				break
			}
		}
		if !retained {
			return false
		}
	}
	changed := false
	var restoredAuthFailure *ModelState
	credential := auth.Quota.Reason == "credential_quota"
	if advisoryQuota(auth.Quota) && (credential || len(auth.ModelStates) == 0 || auth.quotaFailureShadow != nil) &&
		(credential || quotaFailureOnly(auth.LastError)) && quotaUsageShowsRoom(auth, "", quotaFailureAt(auth.Quota, auth.UpdatedAt), now) {
		applyCooldownFields(&auth.Quota, QuotaState{})
		auth.NextRetryAfter = time.Time{}
		auth.Unavailable = false
		restoredAuthFailure = auth.quotaFailureShadow
		auth.quotaFailureShadow = nil
		changed = true
	}
	for model, state := range auth.ModelStates {
		if state == nil || state.Status == StatusDisabled {
			continue
		}
		if shadow := state.credentialQuotaShadow; shadow != nil && advisoryQuota(shadow.Quota) && quotaFailureOnly(shadow.LastError) && quotaUsageShowsRoom(auth, model, quotaFailureAt(shadow.Quota, shadow.UpdatedAt), now) {
			if independent := shadow.credentialQuotaShadow; independent != nil && isModelStateActiveCooldown(independent, now) {
				*shadow = *independent.Clone()
			} else {
				resetModelState(shadow, now)
			}
			changed = true
		}
		wrapped := state.credentialQuotaPropagated || state.Quota.Reason == "credential_quota"
		if wrapped && advisoryQuota(auth.Quota) && auth.Quota.Reason == "credential_quota" {
			continue
		}
		if !advisoryQuota(state.Quota) || (!wrapped && !quotaFailureOnly(state.LastError)) || !quotaUsageShowsRoom(auth, model, quotaFailureAt(state.Quota, state.UpdatedAt), now) {
			continue
		}
		if shadow := state.credentialQuotaShadow; shadow != nil && isModelStateActiveCooldown(shadow, now) {
			*state = *shadow.Clone()
		} else {
			resetModelState(state, now)
		}
		changed = true
	}
	if changed {
		// Drop the old aggregate before deriving it from surviving model layers.
		if auth.Quota.Reason != "credential_quota" && len(auth.ModelStates) > 0 {
			applyCooldownFields(&auth.Quota, QuotaState{})
			updateAggregatedAvailability(auth, now)
		}
		if !auth.Unavailable && !hasModelError(auth, now) && quotaFailureOnly(auth.LastError) {
			auth.Status, auth.StatusMessage, auth.LastError = StatusActive, "", nil
		}
		if isModelStateActiveCooldown(restoredAuthFailure, now) {
			auth.Unavailable = true
			auth.Status = restoredAuthFailure.Status
			auth.StatusMessage = restoredAuthFailure.StatusMessage
			auth.LastError = cloneError(restoredAuthFailure.LastError)
			auth.NextRetryAfter = restoredAuthFailure.NextRetryAfter
			applyCooldownFields(&auth.Quota, restoredAuthFailure.Quota)
		}
	}
	return changed
}

// Selection and management views also enforce the invariant on detached
// snapshots, without mutating shared state under a read lock.
func withReconciledQuotaUsage(auth *Auth, now time.Time) *Auth {
	if !reportedQuotaProvider(auth) || (len(auth.Quota.Windows) == 0 && auth.Quota.usageObservedAt.IsZero()) {
		return auth
	}
	if !advisoryQuota(auth.Quota) {
		found := false
		for _, state := range auth.ModelStates {
			if state != nil && advisoryQuota(state.Quota) {
				found = true
				break
			}
		}
		if !found {
			return auth
		}
	}
	copyAuth := auth.Clone()
	if clearContradictedQuotaCooldowns(copyAuth, now) {
		return copyAuth
	}
	return auth
}

// A reported reset is authoritative, including a shorter replacement or zero.
// Without a reset, retry after a fixed probe interval, never an exponential ladder.
func reportedQuotaRetryAt(retryAfter *time.Duration, now time.Time) time.Time {
	delay := minQuotaCooldownFloor
	if retryAfter != nil {
		delay = max(*retryAfter, 0)
	}
	return now.Add(delay).Round(0)
}

func mergeCodexQuotaCredits(auth *Auth, available bool, observedAt time.Time) {
	if observedAt.IsZero() || observedAt.Before(auth.Quota.creditsObservedAt) {
		return
	}
	if observedAt.Equal(auth.Quota.creditsObservedAt) {
		available = available && auth.Quota.creditsAvailable
	}
	auth.Quota.creditsAvailable, auth.Quota.creditsObservedAt = available, observedAt
}

func mergeCodexUsageCredits(auth *Auth, windows map[string]QuotaWindow, usage json.RawMessage) {
	var snapshot struct {
		Credits struct {
			HasCredits bool `json:"has_credits"`
			Unlimited  bool `json:"unlimited"`
		} `json:"credits"`
	}
	if json.Unmarshal(usage, &snapshot) != nil {
		return
	}
	var observedAt time.Time
	for _, window := range windows {
		if observedAt.IsZero() || window.ObservedAt.Before(observedAt) {
			observedAt = window.ObservedAt
		}
	}
	mergeCodexQuotaCredits(auth, snapshot.Credits.HasCredits || snapshot.Credits.Unlimited, observedAt)
}

func mergeCodexHeaderCredits(auth *Auth, headers http.Header, observedAt time.Time) {
	has, errHas := strconv.ParseBool(headers.Get("X-Codex-Credits-Has-Credits"))
	unlimited, errUnlimited := strconv.ParseBool(headers.Get("X-Codex-Credits-Unlimited"))
	if errHas == nil || errUnlimited == nil {
		mergeCodexQuotaCredits(auth, has || unlimited, observedAt)
	}
}

// A quota failure advances this seat's scheduled read to the next allowed
// budget slot; it cannot start a duplicate or bypass the per-seat minute limit.
func (m *Manager) requestQuotaPollAfterFailureLocked(auth *Auth, now time.Time) {
	if !quotaPollEligible(auth) {
		return
	}
	seat := m.claudeQuotaPollSeatLocked(auth.ID)
	seat.queued = true
	next := now
	if budget := seat.lastAttempt.Add(claudeQuotaMinimumPollInterval); budget.After(next) {
		next = budget
	}
	if seat.nextPoll.IsZero() || next.Before(seat.nextPoll) {
		seat.nextPoll = next
	}
	m.wakeClaudeQuotaPollLocked()
}
