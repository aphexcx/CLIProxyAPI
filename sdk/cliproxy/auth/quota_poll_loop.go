package auth

import (
	"context"
	"reflect"
	"sort"
	"strings"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	log "github.com/sirupsen/logrus"
)

// QuotaPoller observes usage without refreshing or otherwise changing credentials.
type QuotaPoller interface {
	PollQuota(context.Context, *Auth) (map[string]QuotaWindow, error)
}

// StartQuotaPolling binds the optional poller to the service lifetime. Disabled
// configuration creates no goroutine, timer, or upstream request.
func (m *Manager) StartQuotaPolling(parent context.Context) {
	if m == nil {
		return
	}
	m.quotaPollMu.Lock()
	defer m.quotaPollMu.Unlock()
	m.quotaPollParent = parent
	m.restartQuotaPollingLocked()
}

// StopQuotaPolling cancels all outstanding usage reads and prevents reloads from
// restarting polling until the service starts it again.
func (m *Manager) StopQuotaPolling() {
	if m == nil {
		return
	}
	m.quotaPollMu.Lock()
	defer m.quotaPollMu.Unlock()
	m.quotaPollParent = nil
	if m.quotaPollCancel != nil {
		m.quotaPollCancel()
		m.quotaPollCancel = nil
	}
}

func (m *Manager) restartQuotaPollingLocked() {
	if m.quotaPollCancel != nil {
		m.quotaPollCancel()
		m.quotaPollCancel = nil
	}
	cfg, _ := m.runtimeConfig.Load().(*internalconfig.Config)
	if m.quotaPollParent == nil || m.quotaPollParent.Err() != nil || cfg == nil || !cfg.Claude.Quota.Enabled {
		return
	}
	quota := cfg.Claude.Quota.WithDefaults()
	if errValidate := quota.Validate(); errValidate != nil {
		log.Warn("claude quota: invalid configuration; polling disabled")
		return
	}
	if cfg.RequestRetry == 0 {
		log.Warn("claude quota: request-retry is 0; consider enabling retries for exhausted seats")
	}
	ctx, cancel := context.WithCancel(m.quotaPollParent)
	m.quotaPollCancel = cancel
	done := make(chan struct{})
	m.quotaPollDone = done
	interval, _ := time.ParseDuration(quota.PollInterval)
	go func() {
		defer close(done)
		m.quotaPollLoop(ctx, interval)
	}()
}

func (m *Manager) quotaPollLoop(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	// One outstanding read per seat. A stalled seat cannot stall other seats,
	// and no deadline is imposed on an established upstream connection.
	running := make(map[string]bool)
	completed := make(chan string)
	poll := func() {
		cfg, _ := m.runtimeConfig.Load().(*internalconfig.Config)
		if ctx.Err() != nil || cfg == nil || !cfg.Claude.Quota.Enabled {
			return
		}
		for _, auth := range m.List() {
			if !claudeQuotaEligible(auth) || running[auth.ID] {
				continue
			}
			executor, ok := m.Executor(auth.Provider)
			if !ok {
				continue
			}
			poller, ok := executor.(QuotaPoller)
			if !ok {
				continue
			}
			running[auth.ID] = true
			go func() {
				windows, errPoll := poller.PollQuota(ctx, auth)
				if errPoll != nil {
					// Transport errors may contain proxy credentials. Never log the error.
					log.WithField("auth_file", claudeQuotaName(auth)).Debug("claude quota: usage read failed")
					m.refreshClaudeQuotaRanks()
				} else {
					m.applyClaudeQuotaUsage(ctx, cfg, auth, windows, time.Now())
				}
				select {
				case completed <- auth.ID:
				case <-ctx.Done():
				}
			}()
		}
	}
	poll()
	for {
		select {
		case <-ctx.Done():
			return
		case id := <-completed:
			delete(running, id)
		case <-ticker.C:
			poll()
		}
	}
}

func claudeQuotaEligible(auth *Auth) bool {
	if auth == nil || !strings.EqualFold(auth.Provider, "claude") || auth.Disabled || auth.Status == StatusDisabled || auth.AuthKind() != AuthKindOAuth || authAccessToken(auth) == "" {
		return false
	}
	if auth.LastError != nil && (isUnauthorizedError(auth.LastError) || isInvalidGrantResultError(auth.LastError)) {
		return false
	}
	message := strings.ToLower(auth.StatusMessage)
	if auth.LastError != nil {
		message += " " + strings.ToLower(auth.LastError.Message)
	}
	return !strings.Contains(message, "unauthorized") && !strings.Contains(message, "invalid grant") && !strings.Contains(message, "refresh token invalid") && !isInvalidGrantErrorMessage(message)
}

func claudeQuotaName(auth *Auth) string {
	if auth.FileName != "" {
		return auth.FileName
	}
	return auth.ID
}

func (m *Manager) applyClaudeQuotaUsage(ctx context.Context, cfg *internalconfig.Config, base *Auth, windows map[string]QuotaWindow, now time.Time) {
	// Fence reloads and credential replacement. Network I/O happens before these locks.
	release, errLock := m.lockAuthMutationContext(ctx, base.ID)
	if errLock != nil {
		return
	}
	defer release()
	m.configCooldownMu.Lock()
	defer m.configCooldownMu.Unlock()
	if ctx.Err() != nil || m.runtimeConfig.Load() != cfg || !cfg.Claude.Quota.Enabled {
		return
	}
	m.mu.Lock()
	auth := m.auths[base.ID]
	if !claudeQuotaEligible(auth) || auth.RegistrationEpoch != base.RegistrationEpoch || auth.CredentialVersion != base.CredentialVersion || authAccessToken(auth) != authAccessToken(base) {
		m.mu.Unlock()
		release()
		return
	}
	auth.Quota.Windows = make(map[string]QuotaWindow, len(windows))
	for name, window := range windows {
		window.ObservedAt = now
		auth.Quota.Windows[name] = window
	}
	changed := m.reconcileClaudeQuotaHoldsLocked(auth, cfg, now)
	auth.Generation++
	auth.UpdatedAt = now
	snapshots := m.recomputeClaudeQuotaRanksLocked(cfg, now)
	snapshots = append(snapshots, auth.Clone())
	m.mu.Unlock()
	release()
	m.publishClaudeQuotaSnapshots(snapshots)
	if changed {
		m.persistCooldownStatesLocked(ctx)
	}
}

// Reconcile only cooldowns owned by this guard. Missing/invalid windows never
// release an existing hold early; other upstream failures always retain ownership.
func (m *Manager) reconcileClaudeQuotaHoldsLocked(auth *Auth, cfg *internalconfig.Config, now time.Time) bool {
	quota := cfg.Claude.Quota.WithDefaults()
	enabled := quota.Enabled && claudeQuotaEligible(auth) && !quotaCooldownDisabledForAuthWithConfig(auth, cfg)
	type hold struct {
		window string
		reset  time.Time
	}
	desired := make(map[string]hold)
	governed := make(map[string]bool)
	unknown := make(map[string]bool)
	if enabled {
		for window, models := range quota.ModelWindows {
			if window == "five_hour" || window == "seven_day" {
				continue
			}
			usage, exists := auth.Quota.Windows[window]
			for _, model := range models {
				model = canonicalModelKey(model)
				if model == "" {
					continue
				}
				governed[model] = true
				unknown[model] = unknown[model] || !exists
				if exists && usage.Utilization >= quota.CapFor(window) && usage.ResetsAt.After(now) && usage.ResetsAt.After(desired[model].reset) {
					desired[model] = hold{window, usage.ResetsAt}
				}
			}
		}
	}
	changed := false
	for model, state := range auth.ModelStates {
		owned, normalized := claudeQuotaHoldLayer(auth, state, now)
		changed = changed || normalized
		if owned == nil || owned.Quota.Reason != "quota_cap" {
			continue
		}
		_, keep := desired[model]
		if !keep && !(governed[model] && unknown[model] && owned.Quota.NextRecoverAt.After(now)) {
			resetModelState(owned, now)
			if owned != state {
				composeClaudeQuotaHold(auth, state, now)
			}
			changed = true
			log.WithFields(log.Fields{"auth_file": claudeQuotaName(auth), "model": model, "window": claudeQuotaWindowForModel(quota, model)}).Info("claude quota: release")
		}
	}
	for model, hold := range desired {
		outer := ensureModelState(auth, model)
		state, normalized := claudeQuotaHoldLayer(auth, outer, now)
		changed = changed || normalized
		// A credential-wide cooldown may already wrap this model. Add the guard
		// beneath it only when there is no other live independent failure.
		if state == outer && (outer.credentialQuotaPropagated || outer.Quota.Reason == "credential_quota") && auth.Quota.NextRecoverAt.After(now) {
			if isModelStateActiveCooldown(outer.credentialQuotaShadow, now) {
				continue
			}
			outer.credentialQuotaShadow = &ModelState{}
			outer.credentialQuotaPropagated = true
			state = outer.credentialQuotaShadow
		}
		if isModelStateActiveCooldown(state, now) && state.Quota.Reason != "quota_cap" {
			continue
		}
		if state.Quota.Reason == "quota_cap" && state.NextRetryAfter.Equal(hold.reset) {
			continue
		}
		state.Unavailable = true
		state.Status = StatusError
		state.StatusMessage = "quota_cap"
		state.NextRetryAfter = hold.reset
		state.LastError = nil
		state.UpdatedAt = now
		state.cooldownRecordedSeq = m.executionSeq.Load()
		applyCooldownFields(&state.Quota, QuotaState{Exceeded: true, Reason: "quota_cap", NextRecoverAt: hold.reset})
		if state != outer {
			composeClaudeQuotaHold(auth, outer, now)
		}
		changed = true
		log.WithFields(log.Fields{"auth_file": claudeQuotaName(auth), "window": hold.window, "model": model, "resets_at": hold.reset}).Info("claude quota: hold")
	}
	return changed
}

// Restore an independent guard layer once the upstream credential wrap expires.
func claudeQuotaHoldLayer(auth *Auth, state *ModelState, now time.Time) (*ModelState, bool) {
	if state == nil || state.credentialQuotaShadow == nil || state.credentialQuotaShadow.Quota.Reason != "quota_cap" {
		return state, false
	}
	if auth.Quota.Exceeded && auth.Quota.Reason == "credential_quota" && auth.Quota.NextRecoverAt.After(now) {
		return state.credentialQuotaShadow, false
	}
	*state = *state.credentialQuotaShadow.cloneShallow()
	return state, true
}

// Keep an existing upstream wrap's deadline consistent after changing only the
// guard-owned shadow. This never creates a credential-wide quota or changes its reason.
func composeClaudeQuotaHold(auth *Auth, state *ModelState, now time.Time) {
	next := auth.Quota.NextRecoverAt
	if shadow := state.credentialQuotaShadow; shadow != nil {
		if shadow.NextRetryAfter.After(next) {
			next = shadow.NextRetryAfter
		}
		if !isModelStateActiveCooldown(shadow, now) {
			state.credentialQuotaShadow = nil
		}
	}
	state.NextRetryAfter = next
	state.Quota.NextRecoverAt = next
	state.UpdatedAt = now
}

func claudeQuotaWindowForModel(quota internalconfig.ClaudeQuotaConfig, model string) string {
	var windows []string
	for window, models := range quota.ModelWindows {
		for _, candidate := range models {
			if canonicalModelKey(candidate) == model {
				windows = append(windows, window)
				break
			}
		}
	}
	sort.Strings(windows)
	return strings.Join(windows, ",")
}

func (m *Manager) recomputeClaudeQuotaRanksLocked(cfg *internalconfig.Config, now time.Time) []*Auth {
	quota := cfg.Claude.Quota.WithDefaults()
	type candidate struct {
		auth            *Auth
		reset, sevenDay time.Time
		window          string
	}
	var candidates []candidate
	if quota.Enabled && quota.PriorityAtReset {
		for _, auth := range m.auths {
			if !claudeQuotaEligible(auth) {
				continue
			}
			var best candidate
			for window, models := range quota.ModelWindows {
				usage, ok := auth.Quota.Windows[window]
				if !ok || len(models) == 0 || window == "five_hour" || window == "seven_day" || usage.Utilization >= quota.CapFor(window) || !usage.ResetsAt.After(now) {
					continue
				}
				ready := false
				for _, model := range models {
					blocked, _, _ := isAuthBlockedForModel(auth, model, now)
					ready = ready || !blocked
				}
				if ready && (best.auth == nil || usage.ResetsAt.Before(best.reset) || usage.ResetsAt.Equal(best.reset) && window < best.window) {
					best = candidate{auth, usage.ResetsAt, auth.Quota.Windows["seven_day"].ResetsAt, window}
				}
			}
			if best.auth != nil {
				candidates = append(candidates, best)
			}
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		a, b := candidates[i], candidates[j]
		if !a.reset.Equal(b.reset) {
			return a.reset.Before(b.reset)
		}
		if !a.sevenDay.Equal(b.sevenDay) {
			if a.sevenDay.IsZero() {
				return false
			}
			if b.sevenDay.IsZero() {
				return true
			}
			return a.sevenDay.Before(b.sevenDay)
		}
		if claudeQuotaName(a.auth) != claudeQuotaName(b.auth) {
			return claudeQuotaName(a.auth) < claudeQuotaName(b.auth)
		}
		return a.auth.ID < b.auth.ID
	})
	var snapshots []*Auth
	for _, auth := range m.auths {
		rank := ""
		if len(candidates) > 0 && candidates[0].auth == auth {
			rank = "1"
		}
		if auth.Attributes["quota_rank"] == rank {
			continue
		}
		if auth.Attributes == nil {
			auth.Attributes = make(map[string]string)
		}
		if rank == "" {
			delete(auth.Attributes, "quota_rank")
		} else {
			auth.Attributes["quota_rank"] = rank
		}
		auth.Generation++
		auth.UpdatedAt = now
		snapshots = append(snapshots, auth.Clone())
		log.WithFields(log.Fields{"auth_file": claudeQuotaName(auth), "window": claudeQuotaWindowForRank(quota), "quota_rank": rank == "1"}).Info("claude quota: rank change")
	}
	return snapshots
}

func claudeQuotaWindowForRank(quota internalconfig.ClaudeQuotaConfig) string {
	var windows []string
	for window := range quota.ModelWindows {
		windows = append(windows, window)
	}
	sort.Strings(windows)
	return strings.Join(windows, ",")
}

func (m *Manager) publishClaudeQuotaSnapshots(snapshots []*Auth) {
	for _, auth := range snapshots {
		if m.scheduler != nil {
			m.scheduler.upsertAuth(auth)
		}
	}
}

// Called with configCooldownMu held, so an old poll cannot restore removed rules.
func (m *Manager) updateClaudeQuotaConfig(previous, cfg *internalconfig.Config) bool {
	if previous != nil && reflect.DeepEqual(previous.Claude.Quota.WithDefaults(), cfg.Claude.Quota.WithDefaults()) {
		return false
	}
	m.mu.Lock()
	changed := false
	var snapshots []*Auth
	now := time.Now()
	for _, auth := range m.auths {
		if m.reconcileClaudeQuotaHoldsLocked(auth, cfg, now) {
			changed = true
			auth.Generation++
			auth.UpdatedAt = now
			snapshots = append(snapshots, auth.Clone())
		}
		if !cfg.Claude.Quota.Enabled {
			auth.Quota.Windows = nil
		}
	}
	snapshots = append(snapshots, m.recomputeClaudeQuotaRanksLocked(cfg, now)...)
	m.mu.Unlock()
	m.publishClaudeQuotaSnapshots(snapshots)
	m.quotaPollMu.Lock()
	m.restartQuotaPollingLocked()
	m.quotaPollMu.Unlock()
	return changed
}

func (m *Manager) refreshClaudeQuotaRanks() {
	m.mu.Lock()
	cfg, _ := m.runtimeConfig.Load().(*internalconfig.Config)
	if cfg == nil || !cfg.Claude.Quota.Enabled {
		m.mu.Unlock()
		return
	}
	snapshots := m.recomputeClaudeQuotaRanksLocked(cfg, time.Now())
	m.mu.Unlock()
	m.publishClaudeQuotaSnapshots(snapshots)
}
