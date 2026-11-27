package auth

import (
	"context"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

// A reported 429 layer above a 402 block, both beneath a credential wrap.
func nestedQuotaShadowChain(now time.Time) (*ModelState, *ModelState) {
	nested := &ModelState{Unavailable: true, Status: StatusError, StatusMessage: "payment_required",
		NextRetryAfter: now.Add(3 * time.Hour), LastError: &Error{HTTPStatus: 402, Message: "payment required"}, UpdatedAt: now.Add(-10 * time.Minute)}
	layer := &ModelState{Unavailable: true, Status: StatusError, StatusMessage: "quota",
		NextRetryAfter: now.Add(3 * time.Hour), Quota: QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: now.Add(30 * time.Minute), CooldownAt: now.Add(-5 * time.Minute)},
		LastError: &Error{HTTPStatus: 429, Message: "rate limited"}, UpdatedAt: now.Add(-5 * time.Minute), credentialQuotaShadow: nested}
	return layer, nested
}

func TestRestoreUnderLiveCredentialWrapKeepsNestedLayer(t *testing.T) {
	now := time.Now()
	layer, _ := nestedQuotaShadowChain(now)
	outer := &ModelState{Unavailable: true, Status: StatusError, NextRetryAfter: now.Add(3 * time.Hour),
		Quota:     QuotaState{Exceeded: true, Reason: "credential_quota", NextRecoverAt: now.Add(time.Hour), CooldownAt: now.Add(-time.Minute)},
		LastError: &Error{HTTPStatus: 429, Message: "credential quota"},
		UpdatedAt: now.Add(-time.Minute), credentialQuotaPropagated: true, credentialQuotaShadow: layer}
	a := &Auth{ID: "nested-restore", Provider: "claude", Status: StatusError, Unavailable: true, NextRetryAfter: now.Add(time.Hour),
		Quota:       QuotaState{Exceeded: true, Reason: "credential_quota", NextRecoverAt: now.Add(time.Hour), CooldownAt: now.Add(-time.Minute)},
		ModelStates: map[string]*ModelState{"claude-sonnet-5-5": outer}, UpdatedAt: now.Add(-time.Minute)}
	m := NewManager(nil, nil, nil)
	m.mu.Lock()
	m.auths[a.ID] = a
	record := CooldownStateRecord{AuthID: a.ID, Provider: "claude", Model: "claude-sonnet-5-5", NextRetryAfter: now.Add(20 * time.Minute),
		Reason: "transient upstream error", LastError: &Error{HTTPStatus: 500, Message: "boom"}, UpdatedAt: now.Add(-20 * time.Minute)}
	m.restoreCooldownRecordWithCredentialDeadlineLocked(record, now, time.Time{})
	m.mu.Unlock()
	state := a.ModelStates["claude-sonnet-5-5"]
	if state.credentialQuotaShadow == nil || state.credentialQuotaShadow.credentialQuotaShadow == nil {
		t.Fatal("restore beneath a live credential wrap dropped the nested independent layer")
	}
	if rec, _ := modelCooldownStateRecord(a, "claude-sonnet-5-5", state, now); len(rec.IndependentLayers) != 1 {
		t.Fatalf("persisted layers = %d, want 1", len(rec.IndependentLayers))
	}
	// Fresh usage with room releases the advisory layers, not the 402 beneath them.
	a.Quota.usageObservedAt = now
	clearContradictedQuotaCooldowns(a, now)
	state = a.ModelStates["claude-sonnet-5-5"]
	if !state.Unavailable || !state.NextRetryAfter.After(now) {
		t.Fatalf("usage room released the independent 402 block: %+v", state)
	}
}

func TestModelLessSuccessPromotionKeepsNestedLayer(t *testing.T) {
	now := time.Now()
	layer, nested := nestedQuotaShadowChain(now)
	outer := &ModelState{Unavailable: true, Status: StatusError, NextRetryAfter: now.Add(-time.Second),
		Quota:     QuotaState{Exceeded: true, Reason: "credential_quota", NextRecoverAt: now.Add(-time.Second)},
		UpdatedAt: now.Add(-time.Hour), credentialQuotaPropagated: true, credentialQuotaShadow: layer}
	a := &Auth{ID: "nested-success", Provider: "claude", Status: StatusError,
		Quota:       QuotaState{Exceeded: true, Reason: "credential_quota", NextRecoverAt: now.Add(-time.Second)},
		ModelStates: map[string]*ModelState{"claude-sonnet-5-5": outer}}
	clearAuthStateOnSuccess(a, now)
	state := a.ModelStates["claude-sonnet-5-5"]
	if state.credentialQuotaShadow == nil || state.credentialQuotaShadow.LastError == nil ||
		state.credentialQuotaShadow.LastError.HTTPStatus != nested.LastError.HTTPStatus {
		t.Fatalf("promotion after an expired wrap dropped the nested 402 layer: %+v", state.credentialQuotaShadow)
	}
}

func TestDisablingCoolingClearsAuthQuotaShadow(t *testing.T) {
	ctx := context.Background()
	m := NewManager(nil, nil, nil)
	m.SetConfig(&config.Config{})
	if _, errRegister := m.Register(ctx, &Auth{ID: "shadow-toggle", Provider: "claude", Status: StatusActive}); errRegister != nil {
		t.Fatal(errRegister)
	}
	m.MarkResult(ctx, Result{AuthID: "shadow-toggle", Provider: "claude", Model: "claude-sonnet-5-5", Success: true})
	m.MarkResult(ctx, Result{AuthID: "shadow-toggle", Provider: "claude", Error: &Error{HTTPStatus: 402, Message: "payment required"}})
	minute := time.Minute
	m.MarkResult(ctx, Result{AuthID: "shadow-toggle", Provider: "claude", Error: &Error{HTTPStatus: 429, Message: "rate limited"}, RetryAfter: &minute})
	m.SetConfig(&config.Config{DisableCooling: true})
	m.mu.RLock()
	shadowKept := m.auths["shadow-toggle"].quotaFailureShadow != nil
	m.mu.RUnlock()
	if shadowKept {
		t.Fatal("disabling cooling kept the independent auth shadow")
	}
	m.SetConfig(&config.Config{})
	m.MarkResult(ctx, Result{AuthID: "shadow-toggle", Provider: "claude", Error: &Error{HTTPStatus: 429, Message: "rate limited"}, RetryAfter: &minute})
	a, _ := m.GetByID("shadow-toggle")
	if wait := time.Until(a.NextRetryAfter); wait > 2*time.Minute {
		t.Fatalf("reported 1m reset held the seat for %s: a cleared block came back", wait.Round(time.Minute))
	}
}
