package auth

import (
	"context"
	"encoding/json"
	"errors"
	"math/rand/v2"
	"strings"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	log "github.com/sirupsen/logrus"
)

const claudeQuotaMinimumPollInterval = internalconfig.ClaudeQuotaMinimumPollInterval

func (m *Manager) wakeClaudeQuotaPoll() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.wakeClaudeQuotaPollLocked()
}

// Broadcast to all loop generations: a retiring loop cannot consume the only
// notification intended for its replacement. Callers hold m.mu.
func (m *Manager) wakeClaudeQuotaPollLocked() {
	if m.quotaPollWake != nil {
		close(m.quotaPollWake)
	}
	m.quotaPollWake = make(chan struct{})
}

type claudeQuotaPollSeat struct {
	lastAttempt time.Time
	nextPoll    time.Time
	polledAt    time.Time
	running     bool
	queued      bool
	epoch       uint64
	polls       uint64
	rateLimits  uint64
	lastStatus  int
	interval    time.Duration
	jitter      time.Duration
	usage       json.RawMessage
	lastFailed  bool
}

// ClaudeQuotaSnapshot contains only runtime usage, never credential material.
// Windows may include newer response headers; PolledAt is the last successful
// usage poll completion, not the time at which a management request read it.
type ClaudeQuotaSnapshot struct {
	Usage      json.RawMessage        `json:"-"`
	Status     string                 `json:"status"`
	Stale      bool                   `json:"stale"`
	Windows    map[string]QuotaWindow `json:"windows"`
	PolledAt   *time.Time             `json:"polled_at"`
	AgeSeconds *int64                 `json:"age_seconds"`
	NextPollAt *time.Time             `json:"next_poll_at"`
	Refreshing bool                   `json:"refreshing"`
	Holds      []CooldownView         `json:"holds"`
	Polls      uint64                 `json:"polls"`
	RateLimits uint64                 `json:"rate_limits"`
	LastStatus int                    `json:"last_status"`
	LastPollAt *time.Time             `json:"last_poll_at"`
}

func (m *Manager) claudeQuotaPollSeatLocked(id string) *claudeQuotaPollSeat {
	if m.quotaPollSeats == nil {
		m.quotaPollSeats = make(map[string]*claudeQuotaPollSeat)
	}
	if m.quotaPollSeats[id] == nil {
		m.quotaPollSeats[id] = &claudeQuotaPollSeat{}
	}
	return m.quotaPollSeats[id]
}

// ClaudeQuotaCached returns a copy without performing or scheduling network I/O.
func (m *Manager) ClaudeQuotaCached(id string) (ClaudeQuotaSnapshot, bool) {
	return m.cachedQuota(id, "claude")
}

// CodexQuotaCached reads the observational cache without scheduling I/O.
func (m *Manager) CodexQuotaCached(id string) (ClaudeQuotaSnapshot, bool) {
	return m.cachedQuota(id, "codex")
}

func (m *Manager) cachedQuota(id, provider string) (ClaudeQuotaSnapshot, bool) {
	if m == nil {
		return ClaudeQuotaSnapshot{}, false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	cfg, _ := m.runtimeConfig.Load().(*internalconfig.Config)
	auth := m.auths[id]
	if cfg == nil || !cfg.Claude.Quota.Enabled || auth == nil || !strings.EqualFold(auth.Provider, provider) || auth.AuthKind() != AuthKindOAuth {
		return ClaudeQuotaSnapshot{}, false
	}
	now := time.Now()
	snapshot := ClaudeQuotaSnapshot{Status: "not_polled_yet", Stale: true, Windows: auth.Quota.Clone().Windows, Holds: CooldownSnapshotForAuth(auth, now)}
	if provider == "codex" {
		snapshot.Holds = nil // Codex usage polling never creates holds.
	}
	if snapshot.Windows == nil {
		snapshot.Windows = map[string]QuotaWindow{}
	}
	if seat := m.quotaPollSeats[id]; seat != nil {
		snapshot.Polls, snapshot.RateLimits, snapshot.LastStatus = seat.polls, seat.rateLimits, seat.lastStatus
		if !seat.lastAttempt.IsZero() {
			at := seat.lastAttempt
			snapshot.LastPollAt = &at
		}
		if seat.epoch == auth.RegistrationEpoch && !seat.polledAt.IsZero() {
			snapshot.Usage = append(json.RawMessage(nil), seat.usage...)
			at := seat.polledAt
			age := max(int64(0), int64(now.Sub(at)/time.Second))
			snapshot.PolledAt, snapshot.AgeSeconds, snapshot.Status = &at, &age, "ok"
			// Allow the normal staggered round to finish, but bound legacy clients
			// that cannot display cache age when a read stalls indefinitely.
			freshFor := max(seat.interval, claudeQuotaMinimumPollInterval)
			if reset := m.limitResetSeats[id]; reset != nil && reset.generation == auth.CredentialGeneration() && reset.attemptedAt.After(at) {
				// A reset consumed one slot after this observation; the next slot
				// is reserved for routine usage. Errors still become stale at once.
				freshFor += max(seat.interval, claudeQuotaMinimumPollInterval)
			}
			snapshot.Stale = seat.lastFailed || now.Sub(at) > freshFor+time.Minute
		}
		snapshot.Refreshing = seat.running || seat.queued
	}
	if quotaPollEligible(auth) {
		snapshot.NextPollAt = m.nextSeatPollLocked(id, now)
	} else {
		snapshot.Status = "polling_paused"
		snapshot.Stale = true
	}
	return snapshot, true
}

func (m *Manager) nextSeatPollLocked(id string, now time.Time) *time.Time {
	next := now
	if seat := m.quotaPollSeats[id]; seat != nil && seat.nextPoll.After(next) {
		next = seat.nextPoll
	}
	if m.quotaNextStart.After(next) {
		next = m.quotaNextStart
	}
	return &next
}

// RequestClaudeQuotaPoll only queues work on the service loop. Repeated clicks,
// automatic polls and config reloads share the same per-seat minute budget.
func (m *Manager) RequestClaudeQuotaPoll(id string) bool {
	return m.requestQuotaPoll(id, "claude")
}

// RequestCodexQuotaPoll shares the loop and its per-seat minimum interval.
func (m *Manager) RequestCodexQuotaPoll(id string) bool {
	return m.requestQuotaPoll(id, "codex")
}

func (m *Manager) requestQuotaPoll(id, provider string) bool {
	if m == nil {
		return false
	}
	m.quotaPollMu.Lock()
	defer m.quotaPollMu.Unlock()
	if m.quotaPollParent == nil || m.quotaPollParent.Err() != nil || m.quotaPollCancel == nil {
		return false
	}
	m.mu.Lock()
	cfg, _ := m.runtimeConfig.Load().(*internalconfig.Config)
	if cfg == nil || !cfg.Claude.Quota.Enabled || !quotaPollEligible(m.auths[id]) || !strings.EqualFold(m.auths[id].Provider, provider) {
		m.mu.Unlock()
		return false
	}
	seat := m.claudeQuotaPollSeatLocked(id)
	now := time.Now()
	if seat.running || seat.queued || now.Before(seat.lastAttempt.Add(claudeQuotaMinimumPollInterval)) {
		m.mu.Unlock()
		return false
	}
	// A click joins the existing scheduled slot; it cannot accelerate the budget.
	seat.queued = true
	m.mu.Unlock()
	m.wakeClaudeQuotaPoll()
	return true
}

func (m *Manager) reserveClaudeQuotaPoll(ctx context.Context, cfg *internalconfig.Config, auth *Auth, interval time.Duration, now time.Time) (func(), bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if ctx.Err() != nil || m.runtimeConfig.Load() != cfg || !sameClaudeQuotaCredential(m.auths[auth.ID], auth) {
		return nil, false
	}
	seat := m.claudeQuotaPollSeatLocked(auth.ID)
	interval = max(interval, claudeQuotaMinimumPollInterval)
	if seat.interval != interval && !seat.lastAttempt.IsZero() {
		// Apply reloads to the next slot without discarding the attempt budget.
		seat.nextPoll = seat.lastAttempt.Add(interval + seat.jitter)
	}
	seat.interval = interval
	if seat.running || now.Before(seat.nextPoll) || now.Before(seat.lastAttempt.Add(claudeQuotaMinimumPollInterval)) || now.Before(m.quotaNextStart) {
		return nil, false
	}
	// Spread a fleet's scheduled reads instead of bursting all seats on the minute.
	count := 0
	for _, candidate := range m.auths {
		if quotaPollEligible(candidate) {
			count++
		}
	}
	spacing := claudeQuotaMinimumPollInterval / time.Duration(max(count, 1))
	m.quotaNextStart = now.Add(spacing + time.Duration(rand.Int64N(int64(spacing/10+1))))
	return m.startQuotaReadLocked(seat, auth, interval, now), true
}

func (m *Manager) startQuotaReadLocked(seat *claudeQuotaPollSeat, auth *Auth, interval time.Duration, now time.Time) func() {
	if seat.epoch != auth.RegistrationEpoch {
		seat.polledAt = time.Time{}
		seat.usage = nil
		seat.epoch = auth.RegistrationEpoch
	}
	seat.lastAttempt, seat.running, seat.queued = now, true, false
	seat.interval = interval
	seat.polls++
	seat.jitter = time.Duration(rand.Int64N(int64(5 * time.Second)))
	seat.nextPoll = now.Add(interval + seat.jitter)
	return func() {
		m.mu.Lock()
		seat.running = false
		m.wakeClaudeQuotaPollLocked()
		m.mu.Unlock()
	}
}

// ReadClaudeResetUsage takes the spending seat's next usage observation immediately.
// A pre-spend read must finish first. Only a failed usage read gets a second attempt.
// It returns the reconciled credential on success and the next scheduled poll, if any.
func (m *Manager) ReadClaudeResetUsage(base *Auth) (*Auth, *time.Time) {
	if m == nil || base == nil || !strings.EqualFold(base.Provider, "claude") {
		return nil, nil
	}
	m.quotaPollMu.Lock()
	ctx := m.quotaPollCtx
	cfg, _ := m.runtimeConfig.Load().(*internalconfig.Config)
	m.quotaPollMu.Unlock()
	if ctx == nil || cfg == nil || !cfg.Claude.Quota.Enabled {
		return nil, nil
	}
	interval, _ := time.ParseDuration(cfg.Claude.Quota.WithDefaults().PollInterval)
	applied := false
	for attempts := 0; attempts < 2 && !applied; {
		m.mu.Lock()
		executor, _ := m.executorLocked(base.Provider)
		poller, ok := executor.(QuotaPoller)
		if !ok || ctx.Err() != nil || m.runtimeConfig.Load() != cfg || !sameClaudeQuotaCredential(m.auths[base.ID], base) {
			m.mu.Unlock()
			return nil, nil
		}
		seat := m.claudeQuotaPollSeatLocked(base.ID)
		if seat.running {
			wake := m.quotaPollWake
			m.mu.Unlock()
			select {
			case <-ctx.Done():
				return nil, nil
			case <-wake:
				continue
			}
		}
		finish := m.startQuotaReadLocked(seat, base, interval, time.Now())
		m.mu.Unlock()
		attempts++
		readCtx, cancel := context.WithCancel(ctx)
		applied = m.pollClaudeQuotaCredential(readCtx, cfg, base, poller)
		cancel()
		finish()
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	current := m.auths[base.ID]
	if ctx.Err() != nil || !sameClaudeQuotaCredential(current, base) {
		return nil, nil
	}
	next := m.nextSeatPollLocked(base.ID, time.Now())
	if !applied {
		return nil, next
	}
	return current.Clone(), next
}

func (m *Manager) recordClaudeQuotaPoll(base *Auth, started time.Time, errPoll error) {
	status := 200
	if errPoll != nil {
		status = 0 // No HTTP status received (transport/cancellation).
		var statusError interface{ StatusCode() int }
		if errors.As(errPoll, &statusError) {
			status = statusError.StatusCode()
		}
	}
	log.WithFields(log.Fields{"auth_file": claudeQuotaName(base), "http_status": status, "latency_ms": time.Since(started).Milliseconds(), "success": errPoll == nil}).Info(strings.ToLower(base.Provider) + " quota: usage poll")
	m.mu.Lock()
	defer m.mu.Unlock()
	if seat := m.quotaPollSeats[base.ID]; seat != nil && seat.epoch == base.RegistrationEpoch && !seat.lastAttempt.After(started) && sameClaudeQuotaCredential(m.auths[base.ID], base) {
		seat.lastStatus = status
		seat.lastFailed = errPoll != nil
		if status == 429 {
			seat.rateLimits++
		}
	}
}
