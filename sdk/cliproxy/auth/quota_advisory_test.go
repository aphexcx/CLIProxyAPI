package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func newAdvisoryQuotaManager(t *testing.T, provider string) (*Manager, *Auth, string) {
	t.Helper()
	m := NewManager(nil, &FillFirstSelector{}, nil)
	cfg := &config.Config{}
	cfg.Claude.Quota.Enabled = true
	m.SetConfig(cfg)
	model := quotaFable
	if provider == "codex" {
		model = "codex-test"
	}
	id := t.Name()
	registry.GetGlobalRegistry().RegisterClient(id, provider, []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
	auth, errRegister := m.Register(context.Background(), &Auth{ID: id, Provider: provider, Status: StatusActive, Metadata: map[string]any{"access_token": "fixture", "account_id": "fixture"}})
	if errRegister != nil {
		t.Fatal(errRegister)
	}
	return m, auth, model
}

func advisoryQuotaWindows(at time.Time, percent float64) map[string]QuotaWindow {
	return map[string]QuotaWindow{
		"five_hour": {Utilization: percent, ResetsAt: at.Add(time.Hour), ObservedAt: at},
		"seven_day": {Utilization: percent, ResetsAt: at.Add(24 * time.Hour), ObservedAt: at},
		"Fable":     {Utilization: percent, ResetsAt: at.Add(24 * time.Hour), ObservedAt: at},
	}
}

func applyAdvisoryQuota(m *Manager, auth *Auth, windows map[string]QuotaWindow, usage string, now time.Time) {
	cfg := m.runtimeConfig.Load().(*config.Config)
	if auth.Provider == "codex" {
		delete(windows, "Fable")
		m.applyCodexQuotaUsageSnapshot(context.Background(), cfg, auth, windows, json.RawMessage(usage), now)
	} else {
		m.applyClaudeQuotaUsageSnapshot(context.Background(), cfg, auth, windows, nil, now)
	}
}

func TestQuotaAdvisoryResetAndRestore(t *testing.T) {
	for _, provider := range []string{"claude", "codex"} {
		for _, scope := range []string{"credential", "model", "auth"} {
			t.Run(provider+"/"+scope, func(t *testing.T) {
				m, auth, model := newAdvisoryQuotaManager(t, provider)
				store := &recordingCooldownStateStore{}
				m.SetCooldownStateStore(store)
				delay := 2 * time.Hour
				failureModel := model
				if scope == "auth" {
					failureModel = ""
				}
				m.MarkResult(context.Background(), Result{AuthID: auth.ID, Provider: provider, Model: failureModel, CredentialScope: scope == "credential", RetryAfter: &delay, Error: &Error{HTTPStatus: 429}})
				before, _ := m.GetByID(auth.ID)
				records := m.cooldownStateRecordsSnapshot()
				if len(records) == 0 {
					t.Fatal("429 did not produce a cooldown")
				}
				at := before.UpdatedAt.Add(time.Millisecond)
				applyAdvisoryQuota(m, auth, advisoryQuotaWindows(at, 0), `{"credits":{"has_credits":false}}`, at)
				check := func() {
					t.Helper()
					current, _ := m.GetByID(auth.ID)
					if views := CooldownSnapshotForAuth(current, at); len(views) != 0 {
						t.Fatalf("contradicted cooldowns: %+v", views)
					}
					if picked, errPick := (&FillFirstSelector{}).Pick(context.Background(), provider, model, cliproxyexecutor.Options{}, []*Auth{current}); errPick != nil || picked == nil {
						t.Fatalf("next pick failed: %v", errPick)
					}
					if persisted := m.cooldownStateRecordsSnapshot(); len(persisted) != 0 {
						t.Fatalf("cleared cooldown still persisted: %+v", persisted)
					}
					if len(store.savedRecords()) != 0 {
						t.Fatal("usage clear was not saved to the cooldown store")
					}
				}
				check()
				m.SetCooldownStateStore(&mockCooldownStateStore{records: records})
				if errRestore := m.RestoreCooldownStates(context.Background()); errRestore != nil {
					t.Fatal(errRestore)
				}
				check()
			})
		}
	}
}

func TestQuotaAdvisoryRequiresFreshCompleteRoom(t *testing.T) {
	for _, provider := range []string{"claude", "codex"} {
		for _, observation := range []string{"missing", "older", "equal", "exhausted", "partial", "expired", "stale"} {
			t.Run(provider+"/"+observation, func(t *testing.T) {
				m, auth, model := newAdvisoryQuotaManager(t, provider)
				delay := 2 * time.Hour
				m.MarkResult(context.Background(), Result{AuthID: auth.ID, Provider: provider, Model: model, CredentialScope: true, RetryAfter: &delay, Error: &Error{HTTPStatus: 429}})
				before, _ := m.GetByID(auth.ID)
				at := before.UpdatedAt.Add(time.Millisecond)
				windows := advisoryQuotaWindows(at, 0)
				switch observation {
				case "missing":
					windows = nil
				case "older":
					windows = advisoryQuotaWindows(before.UpdatedAt.Add(-time.Second), 0)
				case "equal":
					windows = advisoryQuotaWindows(before.UpdatedAt, 0)
				case "exhausted":
					windows = advisoryQuotaWindows(at, 100)
				case "stale":
					at = at.Add(3 * time.Minute)
				case "partial":
					delete(windows, "seven_day")
				case "expired":
					for name, window := range windows {
						window.ResetsAt = at.Add(-time.Second)
						windows[name] = window
					}
				}
				applyAdvisoryQuota(m, auth, windows, `{"credits":{"has_credits":false}}`, at)
				current, _ := m.GetByID(auth.ID)
				if blocked, _, _ := isAuthBlockedForModel(current, model, at); !blocked {
					t.Fatal("insufficient usage released cooldown")
				}
				applyAdvisoryQuota(m, auth, advisoryQuotaWindows(at.Add(time.Second), 0), "{}", at.Add(time.Second))
				current, _ = m.GetByID(auth.ID)
				if blocked, _, _ := isAuthBlockedForModel(current, model, at.Add(time.Second)); blocked {
					t.Fatal("fresh room did not release cooldown")
				}
			})
		}
	}
}

func TestQuotaAdvisoryReportedResetBoundsRetry(t *testing.T) {
	for _, provider := range []string{"claude", "codex"} {
		t.Run(provider, func(t *testing.T) {
			m, auth, model := newAdvisoryQuotaManager(t, provider)
			for _, delay := range []time.Duration{2 * time.Hour, time.Second} {
				m.MarkResult(context.Background(), Result{AuthID: auth.ID, Provider: provider, Model: model, CredentialScope: true, RetryAfter: &delay, Error: &Error{HTTPStatus: 429}})
			}
			current, _ := m.GetByID(auth.ID)
			if got := current.Quota.NextRecoverAt.Sub(current.UpdatedAt); got > time.Second {
				t.Fatalf("local cooldown exceeds latest reported reset: %v", got)
			}
		})
	}
}

func TestQuotaAdvisoryPreservesIndependentFailures(t *testing.T) {
	for _, provider := range []string{"claude", "codex"} {
		for _, status := range []int{401, 403, 503} {
			t.Run(fmt.Sprintf("%s/%d", provider, status), func(t *testing.T) {
				m, auth, model := newAdvisoryQuotaManager(t, provider)
				m.MarkResult(context.Background(), Result{AuthID: auth.ID, Provider: provider, Model: model, Error: &Error{HTTPStatus: status}})
				before, _ := m.GetByID(auth.ID)
				deadline := before.ModelStates[model].NextRetryAfter
				delay := 2 * time.Hour
				m.MarkResult(context.Background(), Result{AuthID: auth.ID, Provider: provider, Model: model, CredentialScope: true, RetryAfter: &delay, Error: &Error{HTTPStatus: 429}})
				failed, _ := m.GetByID(auth.ID)
				at := failed.UpdatedAt.Add(time.Millisecond)
				applyAdvisoryQuota(m, auth, advisoryQuotaWindows(at, 0), "{}", at)
				current, _ := m.GetByID(auth.ID)
				if blocked, _, _ := isAuthBlockedForModel(current, model, at); !blocked {
					t.Fatal("usage released an independent failure")
				}
				if status != 401 {
					state := current.ModelStates[model]
					if state.LastError == nil || state.LastError.HTTPStatus != status || !state.NextRetryAfter.Equal(deadline) {
						t.Fatalf("independent failure changed: %+v", state)
					}
				}
			})
		}
	}
}

func TestQuotaAdvisorySnapshotAndUpdateInvariant(t *testing.T) {
	for _, provider := range []string{"claude", "codex"} {
		t.Run(provider, func(t *testing.T) {
			m, auth, model := newAdvisoryQuotaManager(t, provider)
			delay := 2 * time.Hour
			m.MarkResult(context.Background(), Result{AuthID: auth.ID, Provider: provider, Model: model, CredentialScope: true, RetryAfter: &delay, Error: &Error{HTTPStatus: 429}})
			old, _ := m.GetByID(auth.ID)
			at := old.UpdatedAt.Add(time.Millisecond)
			old.Quota.Windows = advisoryQuotaWindows(at, 0)
			if provider == "codex" {
				delete(old.Quota.Windows, "Fable")
			}
			if blocked, _, _ := isAuthBlockedForModel(old, model, at); blocked {
				t.Fatal("selector trusted a contradicted snapshot")
			}
			if len(CooldownSnapshotForAuth(old, at)) != 0 {
				t.Fatal("view retained contradicted cooldown")
			}
			if !old.Quota.Exceeded {
				t.Fatal("read-only check mutated the snapshot")
			}
			applyAdvisoryQuota(m, auth, advisoryQuotaWindows(at, 0), "{}", at)
			updated, errUpdate := m.Update(context.Background(), old)
			if errUpdate != nil {
				t.Fatal(errUpdate)
			}
			if len(CooldownSnapshotForAuth(updated, at)) != 0 || len(m.cooldownStateRecordsSnapshot()) != 0 {
				t.Fatal("Update resurrected contradicted cooldown")
			}
		})
	}
}

func TestQuotaAdvisoryCodexCredits(t *testing.T) {
	for _, credits := range []string{`{"has_credits":true}`, `{"unlimited":true}`, `{"has_credits":false,"balance":"0"}`} {
		t.Run(credits, func(t *testing.T) {
			m, auth, model := newAdvisoryQuotaManager(t, "codex")
			delay := 2 * time.Hour
			m.MarkResult(context.Background(), Result{AuthID: auth.ID, Provider: "codex", Model: model, CredentialScope: true, RetryAfter: &delay, Error: &Error{HTTPStatus: 429}})
			before, _ := m.GetByID(auth.ID)
			at := before.UpdatedAt.Add(time.Millisecond)
			applyAdvisoryQuota(m, auth, advisoryQuotaWindows(at, 100), `{"credits":`+credits+`}`, at)
			current, _ := m.GetByID(auth.ID)
			wantBlocked := credits == `{"has_credits":false,"balance":"0"}`
			if blocked, _, _ := isAuthBlockedForModel(current, model, at); blocked != wantBlocked {
				t.Fatalf("blocked=%v want=%v", blocked, wantBlocked)
			}
		})
	}
}

func TestQuotaAdvisoryWakesPollWithinBudget(t *testing.T) {
	for _, provider := range []string{"claude", "codex"} {
		t.Run(provider, func(t *testing.T) {
			m, auth, model := newAdvisoryQuotaManager(t, provider)
			now := time.Now()
			m.mu.Lock()
			seat := m.claudeQuotaPollSeatLocked(auth.ID)
			seat.lastAttempt, seat.nextPoll = now, now.Add(5*time.Minute)
			m.wakeClaudeQuotaPollLocked()
			wake := m.quotaPollWake
			m.mu.Unlock()
			m.MarkResult(context.Background(), Result{AuthID: auth.ID, Provider: provider, Model: model, CredentialScope: true, Error: &Error{HTTPStatus: 429}})
			select {
			case <-wake:
			default:
				t.Fatal("429 did not wake the usage loop")
			}
			m.mu.RLock()
			defer m.mu.RUnlock()
			if !seat.queued || !seat.nextPoll.Equal(now.Add(claudeQuotaMinimumPollInterval)) {
				t.Fatalf("unexpected poll slot: queued=%v next=%v", seat.queued, seat.nextPoll)
			}
		})
	}
}

func TestQuotaAdvisoryHonorsModelWindowsAndGuard(t *testing.T) {
	m, auth, model := newAdvisoryQuotaManager(t, "claude")
	delay := 2 * time.Hour
	m.MarkResult(context.Background(), Result{AuthID: auth.ID, Provider: "claude", Model: model, CredentialScope: true, RetryAfter: &delay, Error: &Error{HTTPStatus: 429}})
	before, _ := m.GetByID(auth.ID)
	at := before.UpdatedAt.Add(time.Millisecond)
	windows := advisoryQuotaWindows(at, 0)
	window := windows["Fable"]
	window.Utilization = 100
	windows["Fable"] = window
	applyAdvisoryQuota(m, auth, windows, "{}", at)
	current, _ := m.GetByID(auth.ID)
	if blocked, _, _ := isAuthBlockedForModel(current, model, at); !blocked {
		t.Fatal("model-specific exhausted window was ignored")
	}
	if blocked, _, _ := isAuthBlockedForModel(current, quotaSonnet, at); blocked {
		t.Fatal("unrelated model retained credential quota")
	}
	applyAdvisoryQuota(m, auth, advisoryQuotaWindows(at.Add(time.Second), 0), "{}", at.Add(time.Second))
	current, _ = m.GetByID(auth.ID)
	if blocked, _, _ := isAuthBlockedForModel(current, model, at.Add(time.Second)); blocked {
		t.Fatal("model reset did not release the remaining hold")
	}
}

func TestQuotaAdvisoryPreservesForcedCooldown(t *testing.T) {
	for _, provider := range []string{"claude", "codex"} {
		t.Run(provider, func(t *testing.T) {
			m, auth, model := newAdvisoryQuotaManager(t, provider)
			delay := time.Hour
			m.MarkResult(context.Background(), Result{AuthID: auth.ID, Provider: provider, Model: model, CredentialScope: true, RetryAfter: &delay, Error: &Error{HTTPStatus: 429, Code: ErrorCodeForceCooldown}})
			before, _ := m.GetByID(auth.ID)
			at := before.UpdatedAt.Add(time.Millisecond)
			applyAdvisoryQuota(m, auth, advisoryQuotaWindows(at, 0), "{}", at)
			current, _ := m.GetByID(auth.ID)
			if blocked, _, _ := isAuthBlockedForModel(current, model, at); !blocked {
				t.Fatal("usage overrode the forced cooldown policy")
			}
		})
	}
}

func TestQuotaAdvisoryRepeatedModelQuotaPreservesIndependentFailure(t *testing.T) {
	for _, wrapped := range []bool{false, true} {
		t.Run(fmt.Sprint(wrapped), func(t *testing.T) {
			m, auth, model := newAdvisoryQuotaManager(t, "codex")
			m.MarkResult(context.Background(), Result{AuthID: auth.ID, Provider: "codex", Model: model, Error: &Error{HTTPStatus: 403}})
			delay := 2 * time.Hour
			if wrapped {
				m.MarkResult(context.Background(), Result{AuthID: auth.ID, Provider: "codex", Model: model, CredentialScope: true, RetryAfter: &delay, Error: &Error{HTTPStatus: 429}})
			}
			for range 2 {
				m.MarkResult(context.Background(), Result{AuthID: auth.ID, Provider: "codex", Model: model, RetryAfter: &delay, Error: &Error{HTTPStatus: 429}})
			}
			before, _ := m.GetByID(auth.ID)
			at := before.UpdatedAt.Add(time.Millisecond)
			applyAdvisoryQuota(m, auth, advisoryQuotaWindows(at, 0), "{}", at)
			current, _ := m.GetByID(auth.ID)
			state := current.ModelStates[model]
			if blocked, _, _ := isAuthBlockedForModel(current, model, at); !blocked || state.LastError == nil || state.LastError.HTTPStatus != 403 {
				t.Fatal("repeated 429s lost the independent 403")
			}
		})
	}
}

func TestQuotaAdvisoryPreservesLaterCredentialFailure(t *testing.T) {
	for _, status := range []int{403, 503} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			m, auth, model := newAdvisoryQuotaManager(t, "codex")
			delay := 2 * time.Hour
			m.MarkResult(context.Background(), Result{AuthID: auth.ID, Provider: "codex", Model: model, CredentialScope: true, RetryAfter: &delay, Error: &Error{HTTPStatus: 429}})
			m.MarkResult(context.Background(), Result{AuthID: auth.ID, Provider: "codex", Error: &Error{HTTPStatus: status}})
			before, _ := m.GetByID(auth.ID)
			at := before.UpdatedAt.Add(time.Millisecond)
			applyAdvisoryQuota(m, auth, advisoryQuotaWindows(at, 0), "{}", at)
			current, _ := m.GetByID(auth.ID)
			if blocked, _, _ := isAuthBlockedForModel(current, model, at); !blocked {
				t.Fatal("usage cleared a later credential-wide failure")
			}
		})
	}
}

func TestQuotaAdvisoryRound2ConfiguredCap(t *testing.T) {
	for _, override := range []bool{false, true} {
		t.Run(fmt.Sprint(override), func(t *testing.T) {
			m, auth, model := newAdvisoryQuotaManager(t, "claude")
			cfg := &config.Config{}
			cfg.Claude.Quota.Enabled = true
			capPercent := float64(80)
			cfg.Claude.Quota.CapPercent = &capPercent
			if override {
				capPercent = 100
				cfg.Claude.Quota.WindowCaps = map[string]float64{"Fable": 80}
			}
			m.SetConfig(cfg)
			m.MarkResult(context.Background(), Result{AuthID: auth.ID, Provider: "claude", Model: model, Error: &Error{HTTPStatus: 429}})
			before, _ := m.GetByID(auth.ID)
			at := before.UpdatedAt.Add(time.Millisecond)
			applyAdvisoryQuota(m, auth, advisoryQuotaWindows(at, 90), "{}", at)
			current, _ := m.GetByID(auth.ID)
			if blocked, _, _ := isAuthBlockedForModel(current, model, at); !blocked {
				t.Fatal("usage at 90 percent released a model capped at 80 percent")
			}
			at = at.Add(time.Second)
			applyAdvisoryQuota(m, auth, advisoryQuotaWindows(at, 70), "{}", at)
			current, _ = m.GetByID(auth.ID)
			if blocked, _, _ := isAuthBlockedForModel(current, model, at); blocked {
				t.Fatal("usage below the configured cap did not release quota")
			}
		})
	}
}

func TestQuotaAdvisoryRound2ForcedThenOrdinary(t *testing.T) {
	for _, provider := range []string{"claude", "codex"} {
		t.Run(provider, func(t *testing.T) {
			m, auth, model := newAdvisoryQuotaManager(t, provider)
			delay := time.Hour
			m.MarkResult(context.Background(), Result{AuthID: auth.ID, Provider: provider, Model: model, RetryAfter: &delay, Error: &Error{HTTPStatus: 429, Code: ErrorCodeForceCooldown}})
			before, _ := m.GetByID(auth.ID)
			deadline := before.ModelStates[model].NextRetryAfter
			m.MarkResult(context.Background(), Result{AuthID: auth.ID, Provider: provider, Model: model, Error: &Error{HTTPStatus: 429}})
			failed, _ := m.GetByID(auth.ID)
			at := failed.UpdatedAt.Add(time.Millisecond)
			applyAdvisoryQuota(m, auth, advisoryQuotaWindows(at, 0), "{}", at)
			current, _ := m.GetByID(auth.ID)
			state := current.ModelStates[model]
			if blocked, _, _ := isAuthBlockedForModel(current, model, at); !blocked || !state.NextRetryAfter.Equal(deadline) || state.LastError == nil || state.LastError.Code != ErrorCodeForceCooldown {
				t.Fatal("ordinary 429 and fresh usage replaced the forced one-hour restriction")
			}
		})
	}
}

func TestQuotaAdvisoryRound2ScopeChange(t *testing.T) {
	for _, provider := range []string{"claude", "codex"} {
		for _, sibling := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/sibling=%t", provider, sibling), func(t *testing.T) {
				m, auth, model := newAdvisoryQuotaManager(t, provider)
				m.MarkResult(context.Background(), Result{AuthID: auth.ID, Provider: provider, Model: model, Error: &Error{HTTPStatus: 403}})
				before, _ := m.GetByID(auth.ID)
				deadline := before.ModelStates[model].NextRetryAfter
				m.MarkResult(context.Background(), Result{AuthID: auth.ID, Provider: provider, Model: model, Error: &Error{HTTPStatus: 429}})
				reportingModel := model
				if sibling {
					reportingModel = "other-model"
				}
				m.MarkResult(context.Background(), Result{AuthID: auth.ID, Provider: provider, Model: reportingModel, CredentialScope: true, Error: &Error{HTTPStatus: 429}})
				failed, _ := m.GetByID(auth.ID)
				at := failed.UpdatedAt.Add(time.Millisecond)
				applyAdvisoryQuota(m, auth, advisoryQuotaWindows(at, 0), "{}", at)
				current, _ := m.GetByID(auth.ID)
				state := current.ModelStates[model]
				if blocked, _, _ := isAuthBlockedForModel(current, model, at); !blocked || !state.NextRetryAfter.Equal(deadline) || state.LastError == nil || state.LastError.HTTPStatus != 403 {
					t.Fatal("model-to-credential quota transition lost the independent 403")
				}
			})
		}
	}
}

func TestQuotaAdvisoryRound2AuthDeadline(t *testing.T) {
	for _, provider := range []string{"claude", "codex"} {
		t.Run(provider, func(t *testing.T) {
			m, auth, model := newAdvisoryQuotaManager(t, provider)
			m.MarkResult(context.Background(), Result{AuthID: auth.ID, Provider: provider, Error: &Error{HTTPStatus: 403}})
			before, _ := m.GetByID(auth.ID)
			deadline := before.NextRetryAfter
			m.MarkResult(context.Background(), Result{AuthID: auth.ID, Provider: provider, Error: &Error{HTTPStatus: 429}})
			failed, _ := m.GetByID(auth.ID)
			if !failed.NextRetryAfter.Equal(deadline) {
				t.Error("ordinary auth-level 429 shortened the independent 403 deadline")
			}
			at := failed.UpdatedAt.Add(time.Millisecond)
			applyAdvisoryQuota(m, auth, advisoryQuotaWindows(at, 0), "{}", at)
			current, _ := m.GetByID(auth.ID)
			if blocked, _, _ := isAuthBlockedForModel(current, model, at); !blocked || !current.NextRetryAfter.Equal(deadline) || current.LastError == nil || current.LastError.HTTPStatus != 403 {
				t.Fatal("fresh usage released the independent auth-level 403")
			}
		})
	}
}

func TestQuotaAdvisoryScopeChangeRetainsLongerQuota(t *testing.T) {
	for _, sibling := range []bool{false, true} {
		t.Run(fmt.Sprint(sibling), func(t *testing.T) {
			m, auth, model := newAdvisoryQuotaManager(t, "codex")
			m.MarkResult(context.Background(), Result{AuthID: auth.ID, Provider: "codex", Model: model, Error: &Error{HTTPStatus: 403}})
			before, _ := m.GetByID(auth.ID)
			deadline := before.ModelStates[model].NextRetryAfter
			delay := 2 * time.Hour
			m.MarkResult(context.Background(), Result{AuthID: auth.ID, Provider: "codex", Model: model, RetryAfter: &delay, Error: &Error{HTTPStatus: 429}})
			reportingModel := model
			if sibling {
				reportingModel = "other-model"
			}
			m.MarkResult(context.Background(), Result{AuthID: auth.ID, Provider: "codex", Model: reportingModel, CredentialScope: true, Error: &Error{HTTPStatus: 429}})
			failed, _ := m.GetByID(auth.ID)
			if blocked, _, _ := isAuthBlockedForModel(failed, model, deadline.Add(time.Minute)); !blocked {
				t.Fatal("scope change dropped the longer model quota without a usage observation")
			}
			at := failed.UpdatedAt.Add(time.Millisecond)
			applyAdvisoryQuota(m, auth, advisoryQuotaWindows(at, 0), "{}", at)
			current, _ := m.GetByID(auth.ID)
			state := current.ModelStates[model]
			if state.LastError == nil || state.LastError.HTTPStatus != 403 || !state.NextRetryAfter.Equal(deadline) {
				t.Fatal("usage did not restore the exact independent 403 restriction")
			}
		})
	}
}

func TestQuotaAdvisoryAuthRetainsLongerQuota(t *testing.T) {
	for _, provider := range []string{"claude", "codex"} {
		t.Run(provider, func(t *testing.T) {
			m, auth, model := newAdvisoryQuotaManager(t, provider)
			m.MarkResult(context.Background(), Result{AuthID: auth.ID, Provider: provider, Error: &Error{HTTPStatus: 403}})
			before, _ := m.GetByID(auth.ID)
			deadline := before.NextRetryAfter
			delay := 2 * time.Hour
			m.MarkResult(context.Background(), Result{AuthID: auth.ID, Provider: provider, RetryAfter: &delay, Error: &Error{HTTPStatus: 429}})
			failed, _ := m.GetByID(auth.ID)
			if blocked, _, _ := isAuthBlockedForModel(failed, model, deadline.Add(time.Minute)); !blocked {
				t.Fatal("independent auth failure discarded the longer incoming quota")
			}
			at := failed.UpdatedAt.Add(time.Millisecond)
			applyAdvisoryQuota(m, auth, advisoryQuotaWindows(at, 0), "{}", at)
			current, _ := m.GetByID(auth.ID)
			if current.LastError == nil || current.LastError.HTTPStatus != 403 || !current.NextRetryAfter.Equal(deadline) {
				t.Fatal("usage did not restore the exact independent auth restriction")
			}
		})
	}
}

func TestQuotaAdvisoryRestrictionsSurviveRestore(t *testing.T) {
	for _, authLevel := range []bool{false, true} {
		t.Run(fmt.Sprint(authLevel), func(t *testing.T) {
			m, auth, model := newAdvisoryQuotaManager(t, "codex")
			failureModel := model
			if authLevel {
				failureModel = ""
			}
			m.MarkResult(context.Background(), Result{AuthID: auth.ID, Provider: "codex", Model: failureModel, Error: &Error{HTTPStatus: 403}})
			before, _ := m.GetByID(auth.ID)
			deadline := before.NextRetryAfter
			if !authLevel {
				deadline = before.ModelStates[model].NextRetryAfter
			}
			delay := 2 * time.Hour
			m.MarkResult(context.Background(), Result{AuthID: auth.ID, Provider: "codex", Model: failureModel, RetryAfter: &delay, Error: &Error{HTTPStatus: 429}})
			if !authLevel {
				m.MarkResult(context.Background(), Result{AuthID: auth.ID, Provider: "codex", Model: model, CredentialScope: true, Error: &Error{HTTPStatus: 429}})
			}
			encoded, errMarshal := json.Marshal(m.cooldownStateRecordsSnapshot())
			if errMarshal != nil {
				t.Fatal(errMarshal)
			}
			var records []CooldownStateRecord
			if errDecode := json.Unmarshal(encoded, &records); errDecode != nil {
				t.Fatal(errDecode)
			}
			restored := NewManager(nil, &FillFirstSelector{}, nil)
			restored.SetConfig(m.runtimeConfig.Load().(*config.Config))
			restoredAuth, errRegister := restored.Register(context.Background(), auth.Clone())
			if errRegister != nil {
				t.Fatal(errRegister)
			}
			restored.SetCooldownStateStore(&recordingCooldownStateStore{load: records})
			if errRestore := restored.RestoreCooldownStates(context.Background()); errRestore != nil {
				t.Fatal(errRestore)
			}
			current, _ := restored.GetByID(auth.ID)
			if blocked, _, _ := isAuthBlockedForModel(current, model, deadline.Add(time.Minute)); !blocked {
				t.Fatal("restoration lost the longer quota deadline")
			}
			// File/refresh snapshots do not carry private runtime provenance.
			// An update retaining the same quota must keep its independent layer.
			current.quotaFailureShadow = nil
			updated, errUpdate := restored.Update(context.Background(), current)
			if errUpdate != nil {
				t.Fatal(errUpdate)
			}
			current = updated
			at := current.UpdatedAt.Add(time.Millisecond)
			applyAdvisoryQuota(restored, restoredAuth, advisoryQuotaWindows(at, 0), "{}", at)
			current, _ = restored.GetByID(auth.ID)
			lastError, next := current.LastError, current.NextRetryAfter
			if !authLevel {
				lastError, next = current.ModelStates[model].LastError, current.ModelStates[model].NextRetryAfter
			}
			if lastError == nil || lastError.HTTPStatus != 403 || !next.Equal(deadline) {
				t.Fatal("restoration lost the independent 403 beneath advisory quota")
			}
		})
	}
}
