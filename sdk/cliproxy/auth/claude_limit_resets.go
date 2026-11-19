package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

// ClaudeLimitResets is an observation, never authorization to spend a reset.
type ClaudeLimitResets struct {
	Eligible         bool               `json:"eligible"`
	IneligibleReason *string            `json:"ineligible_reason"`
	AtLimit          bool               `json:"at_limit"`
	WeeklyResetsAt   *time.Time         `json:"weekly_resets_at"`
	CooldownUntil    *time.Time         `json:"cooldown_until"`
	Grants           []ClaudeResetGrant `json:"grants"`
}

type ClaudeResetGrant struct {
	ID               string     `json:"id"`
	Label            string     `json:"label"`
	ResetsLeft       int        `json:"resets_left"`
	ResetsTotal      *int       `json:"resets_total,omitempty"`
	StartsAt         *time.Time `json:"starts_at,omitempty"`
	EndsAt           *time.Time `json:"ends_at"`
	Type             string     `json:"type"`
	Clears           []string   `json:"clears"`
	UsableNow        bool       `json:"usable_now"`
	Paused           bool       `json:"paused,omitempty"`
	UseRequiresLimit *bool      `json:"use_requires_limit,omitempty"`
	GivesBackWeekly  *bool      `json:"gives_back_weekly,omitempty"`
	// UnavailableReason is the provider's word for why a grant is not usable now.
	UnavailableReason *string `json:"unavailable_reason,omitempty"`
	// Kind is computed by ClaudeResetGrantKind on every parse; a value the
	// provider sends under this name is never decoded, whatever its shape.
	Kind string `json:"kind"`
}

// UnmarshalJSON decodes a grant without its upstream "kind", then derives
// Kind, so an upstream field of that name cannot reject the status.
func (g *ClaudeResetGrant) UnmarshalJSON(data []byte) error {
	type upstream ClaudeResetGrant
	var decoded struct {
		upstream
		Kind json.RawMessage `json:"kind"`
	}
	if errDecode := json.Unmarshal(data, &decoded); errDecode != nil {
		return errDecode
	}
	*g = ClaudeResetGrant(decoded.upstream)
	g.Kind = ClaudeResetGrantKind(*g)
	return nil
}

// ParseClaudeLimitResets accepts the captured cedar_ember shape and ignores
// unrelated usage fields. Errors contain no upstream data or credentials.
func ParseClaudeLimitResets(usage json.RawMessage) (*ClaudeLimitResets, error) {
	var envelope struct {
		CedarEmber json.RawMessage `json:"cedar_ember"`
	}
	if json.Unmarshal(usage, &envelope) != nil || len(envelope.CedarEmber) == 0 || string(envelope.CedarEmber) == "null" {
		return nil, errors.New("cedar_ember status unavailable")
	}
	var required struct {
		Eligible *bool `json:"eligible"`
		Grants   []struct {
			ID         string `json:"id"`
			ResetsLeft *int   `json:"resets_left"`
		} `json:"grants"`
	}
	var status ClaudeLimitResets
	if json.Unmarshal(envelope.CedarEmber, &required) != nil || required.Eligible == nil || json.Unmarshal(envelope.CedarEmber, &status) != nil {
		return nil, errors.New("invalid cedar_ember status")
	}
	if status.Eligible && required.Grants == nil {
		return nil, errors.New("cedar_ember grants unavailable")
	}
	seen := make(map[string]bool)
	for i, grant := range required.Grants {
		if strings.TrimSpace(grant.ID) == "" || seen[grant.ID] || grant.ResetsLeft == nil || *grant.ResetsLeft < 0 {
			return nil, errors.New("invalid cedar_ember grant")
		}
		seen[grant.ID] = true
		g := &status.Grants[i]
		if g.ResetsTotal != nil && (*g.ResetsTotal < 0 || g.ResetsLeft > *g.ResetsTotal) {
			return nil, errors.New("invalid cedar_ember grant count")
		}
		if g.Clears == nil {
			g.Clears = []string{}
		}
		g.Kind = ClaudeResetGrantKind(*g)
	}
	if status.Grants == nil {
		status.Grants = []ClaudeResetGrant{}
	}
	return &status, nil
}

type ClaudeLimitResetsSnapshot struct {
	CredentialGeneration string             `json:"credential_generation"`
	CedarEmber           *ClaudeLimitResets `json:"cedar_ember"`
	Error                string             `json:"error,omitempty"`
	LastGoodAt           *time.Time         `json:"last_good_at"`
	AgeSeconds           *int64             `json:"age_seconds"`
	Stale                bool               `json:"stale"`
}

// ClaudeLimitResetsCached never schedules or performs I/O. A malformed reset
// block cannot discard its last good observation or advance that observation's age.
func (m *Manager) ClaudeLimitResetsCached(id string) ClaudeLimitResetsSnapshot {
	snapshot := ClaudeLimitResetsSnapshot{Error: "quota polling unavailable", Stale: true}
	if m == nil {
		return snapshot
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	auth, seat := m.auths[id], m.limitResetSeats[id]
	if auth == nil {
		return snapshot
	}
	snapshot.CredentialGeneration = auth.CredentialGeneration()
	cfg, _ := m.runtimeConfig.Load().(*internalconfig.Config)
	if cfg == nil || !cfg.Claude.Quota.Enabled {
		return snapshot
	}
	if seat == nil || seat.generation != snapshot.CredentialGeneration {
		snapshot.Error = "not polled yet"
		return snapshot
	}
	if seat.status != nil {
		// Reparse the isolated status to copy all slices and pointers.
		_ = json.Unmarshal(seat.status, &snapshot.CedarEmber)
		at := seat.polledAt
		age := max(int64(0), int64(time.Since(at)/time.Second))
		snapshot.LastGoodAt, snapshot.AgeSeconds = &at, &age
	}
	snapshot.Error = seat.err
	freshFor := max(seat.interval, claudeQuotaMinimumPollInterval)
	if routine := m.quotaPollSeats[id]; routine != nil && routine.epoch == auth.RegistrationEpoch {
		// A due reset waits for a shared slot and cannot consume the routine
		// slot following its previous attempt. Bound that extra slot's age.
		freshFor += max(routine.interval, claudeQuotaMinimumPollInterval)
	}
	snapshot.Stale = snapshot.CedarEmber == nil || snapshot.Error != "" || time.Since(seat.polledAt) > freshFor+time.Minute
	if !quotaPollEligible(auth) {
		snapshot.Stale = true
		snapshot.Error = "polling paused"
	} else if snapshot.Stale && snapshot.Error == "" {
		snapshot.Error = "cached reset status is stale"
	}
	return snapshot
}

// CredentialGeneration is public identity, never a hash of secret material.
func (a *Auth) CredentialGeneration() string {
	return fmt.Sprintf("%d:%d", a.RegistrationEpoch, a.CredentialVersion)
}

type LimitResetsPoller interface {
	PollLimitResets(context.Context, *Auth) (json.RawMessage, error)
}

type claudeLimitResetPollSeat struct {
	generation            string
	status                json.RawMessage
	polledAt, attemptedAt time.Time
	interval              time.Duration
	err                   string
	running               bool
	routinePending        bool
	nextAttempt           time.Time
	rateLimits            int
	// observedFrom is when the read behind the cached observation began. One
	// ordering rule serves polls and spends alike: a read that began earlier
	// changes neither the observation nor its error.
	observedFrom time.Time
}

const claudeLimitResetInterval = 15 * time.Minute

// Consume an already-reserved usage slot, at most once per fifteen minutes.
// The first slot always reads routine usage. Both URL variants share the same
// minimum spacing, while a slow reset read cannot hold the next routine slot.
// Require a routine read between reset attempts even when the
// configured interval is longer than the reset cadence.
func (m *Manager) startClaudeLimitResetsPoll(ctx context.Context, cfg *internalconfig.Config, base *Auth, executor QuotaPoller, interval time.Duration) bool {
	poller, ok := executor.(LimitResetsPoller)
	if !ok || !strings.EqualFold(base.Provider, "claude") {
		return false
	}
	m.mu.Lock()
	if ctx.Err() != nil || m.runtimeConfig.Load() != cfg || !sameClaudeQuotaCredential(m.auths[base.ID], base) {
		m.mu.Unlock()
		return false
	}
	if m.limitResetSeats == nil {
		m.limitResetSeats = make(map[string]*claudeLimitResetPollSeat)
	}
	seat := m.limitResetSeats[base.ID]
	if seat == nil || seat.generation != base.CredentialGeneration() {
		seat = &claudeLimitResetPollSeat{generation: base.CredentialGeneration()}
		m.limitResetSeats[base.ID] = seat
	}
	if seat.routinePending {
		seat.routinePending = false
		m.mu.Unlock()
		return false
	}
	routine := m.quotaPollSeats[base.ID]
	if routine == nil || routine.polledAt.IsZero() || seat.running || time.Now().Before(seat.nextAttempt) {
		m.mu.Unlock()
		return false
	}
	seat.running, seat.attemptedAt, seat.interval = true, time.Now(), max(interval, claudeLimitResetInterval)
	seat.routinePending = true
	seat.nextAttempt = seat.attemptedAt.Add(seat.interval)
	m.mu.Unlock()
	go func() {
		readCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		changes, stop := m.WatchAuthChanges(base.ID)
		defer stop()
		defer func() { m.mu.Lock(); seat.running = false; m.mu.Unlock() }()
		current := func() bool {
			m.mu.RLock()
			defer m.mu.RUnlock()
			return sameClaudeQuotaCredential(m.auths[base.ID], base)
		}
		if readCtx.Err() != nil || !current() {
			return
		}
		type result struct {
			body json.RawMessage
			err  error
		}
		complete := make(chan result, 1)
		go func() { body, err := poller.PollLimitResets(readCtx, base); complete <- result{body, err} }()
		for {
			select {
			case <-readCtx.Done():
				return
			case <-changes:
				if !current() {
					return
				}
			case read := <-complete:
				m.recordClaudeLimitResets(readCtx, cfg, base, read.body, read.err, time.Now())
				return
			}
		}
	}()
	return true
}

func (m *Manager) recordClaudeLimitResets(ctx context.Context, cfg *internalconfig.Config, base *Auth, body json.RawMessage, errRead error, now time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if ctx.Err() != nil || m.runtimeConfig.Load() != cfg || !cfg.Claude.Quota.Enabled || !sameClaudeQuotaCredential(m.auths[base.ID], base) {
		return
	}
	if m.limitResetSeats == nil {
		m.limitResetSeats = make(map[string]*claudeLimitResetPollSeat)
	}
	seat := m.limitResetSeats[base.ID]
	if seat == nil || seat.generation != base.CredentialGeneration() {
		seat = &claudeLimitResetPollSeat{generation: base.CredentialGeneration()}
		m.limitResetSeats[base.ID] = seat
	}
	// A poll that began before the read the cache holds (a spend's after-read
	// published meanwhile) changes only the read backoff.
	older := seat.attemptedAt.Before(seat.observedFrom)
	if errRead != nil {
		message := "reset status poll failed"
		var statusError interface{ StatusCode() int }
		if errors.As(errRead, &statusError) {
			message = fmt.Sprintf("reset status poll failed (HTTP %d)", statusError.StatusCode())
			if statusError.StatusCode() == 429 {
				seat.rateLimits++
				// Do not retry the reset read in the next routine slot. Back off
				// 30m, 1h, then at most 2h; last-good data remains marked stale.
				backoff := claudeLimitResetInterval * time.Duration(1<<min(seat.rateLimits, 3))
				seat.nextAttempt = now.Add(backoff)
			}
		}
		if !older {
			seat.err, seat.observedFrom = message, seat.attemptedAt
		}
		return
	}
	if older {
		return
	}
	seat.observedFrom = seat.attemptedAt
	status, err := ParseClaudeLimitResets(body)
	if err != nil {
		seat.err = err.Error()
		return
	}
	seat.status, _ = json.Marshal(status)
	seat.polledAt, seat.err = now, ""
	seat.rateLimits = 0
}

// PublishClaudeResetSpendRead keeps the reset cache truthful after a claim of
// grantID on base's credential, by the ordering rule of the poll: startedAt
// is when the spend's after-read began, and a cached observation whose read
// began later is kept. The after-read replaces the seat's observation, and a
// failed after-read marks it stale, so a spent grant is never shown as fresh.
// When the claim spent or may have spent (spent), every other seat whose
// cached status lists the grant is marked stale too. A replaced credential's
// own cache is left alone.
func (m *Manager) PublishClaudeResetSpendRead(base *Auth, grantID string, spent bool, startedAt time.Time, body json.RawMessage, errRead error) {
	if m == nil || base == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, other := range m.limitResetSeats {
		if spent && id != base.ID && other != nil && !startedAt.Before(other.observedFrom) && claudeResetStatusListsGrant(other.status, grantID) {
			other.err, other.observedFrom = "a claim on a grant this seat also lists was sent from another seat; the cached reset status awaits a fresh read", startedAt
		}
	}
	if !sameClaudeQuotaCredential(m.auths[base.ID], base) {
		return
	}
	seat := m.limitResetSeats[base.ID]
	if seat == nil || seat.generation != base.CredentialGeneration() || startedAt.Before(seat.observedFrom) {
		// Nothing is cached for this credential, or a newer read is.
		return
	}
	seat.observedFrom = startedAt
	if errRead == nil {
		if status, errParse := ParseClaudeLimitResets(body); errParse == nil {
			seat.status, _ = json.Marshal(status)
			seat.polledAt, seat.err = time.Now(), ""
			return
		}
	}
	seat.err = "a claim was sent from this seat; the cached reset status awaits a fresh read"
}

// claudeResetStatusListsGrant reports whether a cached status lists grantID.
func claudeResetStatusListsGrant(status json.RawMessage, grantID string) bool {
	if len(status) == 0 || grantID == "" {
		return false
	}
	var listed struct {
		Grants []struct {
			ID string `json:"id"`
		} `json:"grants"`
	}
	if json.Unmarshal(status, &listed) != nil {
		return false
	}
	for _, grant := range listed.Grants {
		if grant.ID == grantID {
			return true
		}
	}
	return false
}
