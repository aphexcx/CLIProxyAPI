package auth

import (
	"context"
	"net/http"
	"strconv"
	"testing"
	"testing/synctest"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	internallogging "github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func claudeQuotaHeaderContext(window, utilization string, reset time.Time) context.Context {
	headers := make(http.Header)
	prefix := "Anthropic-Ratelimit-Unified-" + window
	headers.Set(prefix+"-Utilization", utilization)
	headers.Set(prefix+"-Reset", strconv.FormatInt(reset.Unix(), 10))
	ctx := internallogging.WithFreshResponseHeadersHolder(context.Background())
	internallogging.SetResponseHeaders(ctx, headers)
	return ctx
}

func TestClaudeQuotaResponseHeadersHoldAndReleaseWithoutPoll(t *testing.T) {
	for _, model := range []string{quotaFable, quotaSonnet} {
		t.Run(model, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				m, a, _ := newClaudeQuotaManager(t)
				store := &recordingCooldownStateStore{}
				m.SetCooldownStateStore(store)
				reset := time.Now().Add(time.Hour)
				observedAt := time.Now()
				ctx := claudeQuotaHeaderContext("7d_oi", "1.02", reset)
				<-time.After(time.Second)
				m.MarkResult(ctx, Result{AuthID: a, Provider: "claude", Model: model, Success: true})
				auth, _ := m.GetByID(a)
				window := auth.Quota.Windows["Fable"]
				if window.Utilization != 102 || !window.ResetsAt.Equal(reset) || !window.ObservedAt.Equal(observedAt) {
					t.Fatalf("Fable window=%+v", window)
				}
				if blocked, _, until := isAuthBlockedForModel(auth, quotaFable, time.Now()); !blocked || !until.Equal(reset) {
					t.Fatalf("header-only hold: blocked=%v until=%v", blocked, until)
				}
				if blocked, _, _ := isAuthBlockedForModel(auth, quotaSonnet, time.Now()); blocked || auth.Disabled || auth.Quota.Reason == "credential_quota" {
					t.Fatal("Fable header cooled the whole seat")
				}
				if !registry.GetGlobalRegistry().IsModelSuspendedForClient(a, quotaFable) || len(store.savedRecords()) != 1 {
					t.Fatal("header hold was not published or persisted")
				}
				if picked, errPick := m.scheduler.pickSingle(context.Background(), "claude", quotaFable, cliproxyexecutor.Options{}, nil); errPick != nil || picked == nil || picked.ID == a {
					t.Fatal("scheduler selected the held seat")
				}
				<-time.After(time.Second)
				m.MarkResult(claudeQuotaHeaderContext("7d_oi", "0.5", reset), Result{AuthID: a, Provider: "claude", Model: quotaSonnet, Success: true})
				auth, _ = m.GetByID(a)
				if blocked, _, _ := isAuthBlockedForModel(auth, quotaFable, time.Now()); blocked || len(store.savedRecords()) != 0 {
					t.Fatal("below-cap header did not release the owned hold")
				}
				if registry.GetGlobalRegistry().IsModelSuspendedForClient(a, quotaFable) || auth.QuotaRank(time.Now()) != 1 {
					t.Fatal("header release did not publish routing and rank")
				}
			})
		})
	}
}

func TestClaudeQuotaResponseHeadersNewestWindowWins(t *testing.T) {
	for _, latest := range []string{"poll", "header"} {
		for _, percent := range []float64{0, 100} {
			t.Run(latest+strconv.FormatFloat(percent, 'f', 0, 64), func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					m, a, _ := newClaudeQuotaManager(t)
					start := time.Now()
					reset := start.Add(time.Hour)
					older := claudeQuotaHeaderContext("7d_oi", strconv.FormatFloat((100-percent)/100, 'f', 2, 64), reset)
					<-time.After(time.Second)
					if latest == "poll" {
						applyTestClaudeQuota(m, a, percent, reset, time.Now())
						m.MarkResult(older, Result{AuthID: a, Provider: "claude", Model: quotaFable, Success: true})
					} else {
						m.MarkResult(claudeQuotaHeaderContext("7d_oi", strconv.FormatFloat(percent/100, 'f', 2, 64), reset), Result{AuthID: a, Provider: "claude", Model: quotaSonnet, Success: true})
						base, _ := m.GetByID(a)
						m.applyClaudeQuotaUsage(context.Background(), m.runtimeConfig.Load().(*config.Config), base, map[string]QuotaWindow{
							"Fable": {Utilization: 100 - percent, ResetsAt: reset, ObservedAt: start},
						}, time.Now())
					}
					auth, _ := m.GetByID(a)
					if auth.Quota.Windows["Fable"].Utilization != percent {
						t.Fatalf("older observation replaced %s: %+v", latest, auth.Quota.Windows["Fable"])
					}
					if blocked, _, _ := isAuthBlockedForModel(auth, quotaFable, time.Now()); blocked != (percent == 100) {
						t.Fatalf("routing ignored the newest %s observation", latest)
					}
				})
			})
		}
	}
}

func TestClaudeQuotaResponseHeadersNewerUsageWithSameDeadline(t *testing.T) {
	for _, source := range []string{"header", "poll"} {
		for _, seq := range []uint64{0, 1} {
			t.Run(source+"/"+strconv.FormatUint(seq, 10), func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					m, a, _ := newClaudeQuotaManager(t)
					reset := time.Now().Add(time.Hour)
					applyTestClaudeQuota(m, a, 100, reset, time.Now())
					<-time.After(time.Second)
					stale := claudeQuotaHeaderContext("7d_oi", "0", reset)
					<-time.After(time.Second)
					latestAt := time.Now()
					if source == "poll" {
						applyTestClaudeQuota(m, a, 100, reset, latestAt)
					} else {
						m.MarkResult(claudeQuotaHeaderContext("7d_oi", "1", reset), Result{AuthID: a, Provider: "claude", Model: quotaSonnet, Success: true})
					}
					<-time.After(time.Second)
					m.MarkResult(stale, Result{AuthID: a, Provider: "claude", Model: quotaFable, Success: true, ExecutionSeq: seq})
					auth, _ := m.GetByID(a)
					if blocked, _, until := isAuthBlockedForModel(auth, quotaFable, time.Now()); !blocked || !until.Equal(reset) {
						t.Fatal("old success cleared a newer cap observation with the same deadline")
					}
					if window := auth.Quota.Windows["Fable"]; window.Utilization != 100 || !window.ObservedAt.Equal(latestAt) {
						t.Fatalf("newest usage lost: %+v", window)
					}
					<-time.After(time.Second)
					m.MarkResult(claudeQuotaHeaderContext("7d_oi", "0", reset), Result{AuthID: a, Provider: "claude", Model: quotaFable, Success: true, ExecutionSeq: seq})
					auth, _ = m.GetByID(a)
					if blocked, _, _ := isAuthBlockedForModel(auth, quotaFable, time.Now()); blocked {
						t.Fatal("new below-cap response did not release the guard")
					}
				})
			})
		}
	}
}

func TestClaudeQuotaResponseHeadersDoNotRearmClearedHold(t *testing.T) {
	for _, clear := range []string{"reset", "success"} {
		t.Run(clear, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				m, a, _ := newClaudeQuotaManager(t)
				reset := time.Now().Add(time.Hour)
				applyTestClaudeQuota(m, a, 100, reset, time.Now())
				<-time.After(time.Second)
				stale := claudeQuotaHeaderContext("7d_oi", "1", reset)
				<-time.After(time.Second)
				if clear == "reset" {
					if _, _, errReset := m.ResetQuota(context.Background(), a); errReset != nil {
						t.Fatal(errReset)
					}
				} else {
					m.MarkResult(context.Background(), Result{AuthID: a, Provider: "claude", Model: quotaFable, Success: true})
				}
				m.MarkResult(stale, Result{AuthID: a, Provider: "claude", Model: quotaSonnet, Success: true})
				auth, _ := m.GetByID(a)
				if blocked, _, _ := isAuthBlockedForModel(auth, quotaFable, time.Now()); blocked {
					t.Fatal("delayed header re-armed a cleared guard")
				}
				<-time.After(time.Second)
				m.MarkResult(claudeQuotaHeaderContext("7d_oi", "1", reset), Result{AuthID: a, Provider: "claude", Model: quotaFable, Success: true})
				auth, _ = m.GetByID(a)
				if blocked, _, _ := isAuthBlockedForModel(auth, quotaFable, time.Now()); !blocked {
					t.Fatal("fresh header did not re-arm")
				}
			})
		})
	}
}

func TestClaudeQuotaResponseHeadersClearedWindowStaysClearUnderNewHold(t *testing.T) {
	for _, clear := range []string{"reset", "success"} {
		for _, source := range []string{"header", "poll"} {
			t.Run(clear+"/"+source, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					m, a, _ := newClaudeQuotaManager(t)
					cfg := m.runtimeConfig.Load().(*config.Config).CloneForRuntime()
					cfg.Claude.Quota.ModelWindows = map[string][]string{"Fable": {quotaFable}, "Additional": {quotaFable}}
					m.SetConfig(cfg)
					short, long := time.Now().Add(time.Hour), time.Now().Add(2*time.Hour)
					applyTestClaudeQuota(m, a, 100, long, time.Now())
					<-time.After(time.Second)
					staleAt := time.Now()
					stale := claudeQuotaHeaderContext("7d_oi", "1", long)
					<-time.After(time.Second)
					if clear == "reset" {
						if _, _, errReset := m.ResetQuota(context.Background(), a); errReset != nil {
							t.Fatal(errReset)
						}
					} else {
						m.MarkResult(context.Background(), Result{AuthID: a, Provider: "claude", Model: quotaFable, Success: true})
					}
					<-time.After(time.Second)
					base, _ := m.GetByID(a)
					m.applyClaudeQuotaUsage(context.Background(), m.runtimeConfig.Load().(*config.Config), base, map[string]QuotaWindow{
						"Additional": {Utilization: 100, ResetsAt: short},
					}, time.Now())
					<-time.After(time.Second)
					if source == "header" {
						m.MarkResult(stale, Result{AuthID: a, Provider: "claude", Model: quotaSonnet, Success: true})
					} else {
						base, _ = m.GetByID(a)
						m.applyClaudeQuotaUsage(context.Background(), m.runtimeConfig.Load().(*config.Config), base, map[string]QuotaWindow{
							"Fable": {Utilization: 100, ResetsAt: long, ObservedAt: staleAt},
						}, time.Now())
					}
					auth, _ := m.GetByID(a)
					state := auth.ModelStates[quotaFable]
					if !state.NextRetryAfter.Equal(short) || !state.Quota.guardWindows["Fable"].IsZero() {
						t.Fatalf("delayed %s restored cleared window: %+v", source, state.Quota.guardWindows)
					}
					<-time.After(time.Second)
					m.MarkResult(claudeQuotaHeaderContext("7d_oi", "1", long), Result{AuthID: a, Provider: "claude", Model: quotaSonnet, Success: true})
					auth, _ = m.GetByID(a)
					if !auth.ModelStates[quotaFable].Quota.guardWindows["Fable"].Equal(long) {
						t.Fatal("new observation did not restore its own window")
					}
				})
			})
		}
	}
}

func TestClaudeQuotaResponseHeadersPreserveOtherWindows(t *testing.T) {
	for _, invalid := range []bool{false, true} {
		t.Run(strconv.FormatBool(invalid), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				m, a, _ := newClaudeQuotaManager(t)
				cfg := m.runtimeConfig.Load().(*config.Config).CloneForRuntime()
				cfg.Claude.Quota.ModelWindows = map[string][]string{"Fable": {quotaFable}, "Additional": {quotaFable}}
				m.SetConfig(cfg)
				short, long := time.Now().Add(time.Hour), time.Now().Add(2*time.Hour)
				base, _ := m.GetByID(a)
				m.applyClaudeQuotaUsage(context.Background(), m.runtimeConfig.Load().(*config.Config), base, map[string]QuotaWindow{
					"Fable": {Utilization: 100, ResetsAt: short}, "Additional": {Utilization: 100, ResetsAt: long},
				}, time.Now())
				<-time.After(time.Second)
				ctx := claudeQuotaHeaderContext("7d_oi", "0", short)
				if invalid {
					internallogging.SetResponseHeaders(ctx, http.Header{"Anthropic-Ratelimit-Unified-7d_oi-Utilization": {"0"}, "Anthropic-Ratelimit-Unified-7d_oi-Reset": {"invalid"}})
				}
				m.MarkResult(ctx, Result{AuthID: a, Provider: "claude", Model: quotaSonnet, Success: true})
				auth, _ := m.GetByID(a)
				state := auth.ModelStates[quotaFable]
				if !state.NextRetryAfter.Equal(long) || !state.Quota.guardWindows["Additional"].Equal(long) {
					t.Fatal("partial header discarded another window's hold")
				}
				_, fable := state.Quota.guardWindows["Fable"]
				if fable != invalid {
					t.Fatal("header did not respect validity of its own window")
				}
			})
		})
	}
}

func TestClaudeQuotaResponseHeadersObservationOnly(t *testing.T) {
	for _, mode := range []string{"disabled", "api-key", "five_hour", "seven_day", "status-only", "missing-reset", "invalid-utilization", "skip"} {
		t.Run(mode, func(t *testing.T) {
			m, a, _ := newClaudeQuotaManager(t)
			if mode == "disabled" {
				cfg := m.runtimeConfig.Load().(*config.Config).CloneForRuntime()
				cfg.Claude.Quota.Enabled = false
				m.SetConfig(cfg)
			}
			if mode == "api-key" {
				auth, _ := m.GetByID(a)
				auth.Attributes = map[string]string{"api_key": "test-key"}
				auth.Metadata = nil
				if _, errUpdate := m.Update(context.Background(), auth); errUpdate != nil {
					t.Fatal(errUpdate)
				}
			}
			window := "7d_oi"
			if mode == "five_hour" {
				window = "5h"
			}
			if mode == "seven_day" {
				window = "7d"
			}
			ctx := claudeQuotaHeaderContext(window, "1", time.Now().Add(time.Hour))
			headers := internallogging.GetResponseHeaders(ctx)
			switch mode {
			case "status-only":
				headers.Del("Anthropic-Ratelimit-Unified-7d_oi-Utilization")
				headers.Set("Anthropic-Ratelimit-Unified-7d_oi-Status", "rejected")
			case "missing-reset":
				headers.Del("Anthropic-Ratelimit-Unified-7d_oi-Reset")
				headers.Set("Retry-After", "3600")
				headers.Set("Anthropic-Ratelimit-Unified-Reset", strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10))
			case "invalid-utilization":
				headers.Set("Anthropic-Ratelimit-Unified-7d_oi-Utilization", "NaN")
			}
			internallogging.SetResponseHeaders(ctx, headers)
			current, _ := m.GetByID(a)
			m.MarkResult(ctx, Result{AuthID: a, Provider: "claude", Model: quotaSonnet, Success: true, SkipQuotaObservation: mode == "skip", CredentialVersion: current.CredentialVersion})
			auth, _ := m.GetByID(a)
			if blocked, _, _ := isAuthBlockedForModel(auth, quotaFable, time.Now()); blocked || auth.Disabled || auth.QuotaRank(time.Now()) != 0 {
				t.Fatal("observational header changed guard routing")
			}
			if mode != "skip" && len(auth.Quota.Signals) == 0 {
				t.Fatal("passive snapshot missing")
			}
			if mode == "five_hour" || mode == "seven_day" {
				if auth.Quota.Windows[mode].Utilization != 100 {
					t.Fatal("account window missing from observation")
				}
			} else if len(auth.Quota.Windows) != 0 {
				t.Fatal("ineligible or malformed header created windows")
			}
		})
	}
}

func TestClaudeQuotaResponseHeadersAndPollAgree(t *testing.T) {
	for _, source := range []string{"poll", "header"} {
		t.Run(source, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				m, a, _ := newClaudeQuotaManager(t)
				cfg := m.runtimeConfig.Load().(*config.Config).CloneForRuntime()
				cfg.Claude.Quota.WindowCaps = map[string]float64{"Fable": 99}
				m.SetConfig(cfg)
				reset := time.Now().Add(time.Hour)
				for _, percent := range []float64{98, 99, 100, 0} {
					<-time.After(time.Second)
					if source == "poll" {
						applyTestClaudeQuota(m, a, percent, reset, time.Now())
					} else {
						m.MarkResult(claudeQuotaHeaderContext("7d_oi", strconv.FormatFloat(percent/100, 'f', 2, 64), reset), Result{AuthID: a, Provider: "claude", Model: quotaSonnet, Success: true})
					}
					auth, _ := m.GetByID(a)
					if blocked, _, until := isAuthBlockedForModel(auth, quotaFable, time.Now()); blocked != (percent >= 99) || blocked && !until.Equal(reset) {
						t.Fatalf("%s disagrees with cap/reset at %v%%: blocked=%v until=%v", source, percent, blocked, until)
					}
					if auth.Quota.Windows["Fable"].Utilization != percent {
						t.Fatal("window percent disagrees")
					}
				}
			})
		})
	}
}

func TestClaudeQuotaResponseHeadersDecimalCapBoundary(t *testing.T) {
	for _, tc := range []struct {
		fraction string
		percent  float64
	}{{"0.58", 58}, {"0.29", 29}, {"0.57", 57}, {"0.0058", 0.58}, {"5.8e-1", 58}} {
		t.Run(tc.fraction, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				m, a, _ := newClaudeQuotaManager(t)
				cfg := m.runtimeConfig.Load().(*config.Config).CloneForRuntime()
				cfg.Claude.Quota.WindowCaps = map[string]float64{"Fable": tc.percent}
				m.SetConfig(cfg)
				reset := time.Now().Add(time.Hour)
				applyTestClaudeQuota(m, a, tc.percent, reset, time.Now())
				<-time.After(time.Second)
				m.MarkResult(claudeQuotaHeaderContext("7d_oi", tc.fraction, reset), Result{AuthID: a, Provider: "claude", Model: quotaSonnet, Success: true})
				auth, _ := m.GetByID(a)
				if window := auth.Quota.Windows["Fable"]; window.Utilization != tc.percent {
					t.Fatalf("header and poll disagree: utilization=%v cap=%v", window.Utilization, tc.percent)
				}
				if blocked, _, _ := isAuthBlockedForModel(auth, quotaFable, time.Now()); !blocked {
					t.Fatal("equivalent at-cap header released the poll hold")
				}
				<-time.After(time.Second)
				m.MarkResult(claudeQuotaHeaderContext("7d_oi", strconv.FormatFloat((tc.percent-0.01)/100, 'f', -1, 64), reset), Result{AuthID: a, Provider: "claude", Model: quotaSonnet, Success: true})
				auth, _ = m.GetByID(a)
				if blocked, _, _ := isAuthBlockedForModel(auth, quotaFable, time.Now()); blocked {
					t.Fatal("below-cap header retained the hold")
				}
			})
		})
	}
}

func TestClaudeQuotaResponseHeadersPublicationRejectsReplacement(t *testing.T) {
	for _, rank := range []bool{false, true} {
		t.Run(strconv.FormatBool(rank), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				m, a, _ := newClaudeQuotaManager(t)
				cfg := m.runtimeConfig.Load().(*config.Config).CloneForRuntime()
				cfg.Claude.Quota.PriorityAtReset = rank
				m.SetConfig(cfg)
				for i := 0; i < 5; i++ {
					m.MarkResult(context.Background(), Result{AuthID: a, Provider: "claude", Model: quotaSonnet, Success: true})
				}
				store := &blockingCooldownStateStore{started: make(chan struct{}), release: make(chan struct{})}
				m.SetCooldownStateStore(store)
				defer func() {
					select {
					case <-store.release:
					default:
						close(store.release)
					}
				}()
				ctx := claudeQuotaHeaderContext("7d_oi", "1", time.Now().Add(time.Hour))
				completed := make(chan struct{})
				go func() {
					m.MarkResult(ctx, Result{AuthID: a, Provider: "claude", Model: quotaSonnet, Success: true})
					close(completed)
				}()
				<-store.started
				stale, _ := m.GetByID(a)
				// Register replaces the existing credential with a new epoch and
				// generation while the old result waits in cooldown persistence.
				reg := registry.GetGlobalRegistry()
				reg.UnregisterClient(a)
				if _, errRegister := m.Register(context.Background(), &Auth{ID: a, Provider: "claude", Status: StatusActive, Metadata: map[string]any{"access_token": "replacement-access"}}); errRegister != nil {
					t.Fatal(errRegister)
				}
				reg.RegisterClient(a, "claude", []*registry.ModelInfo{{ID: quotaFable}, {ID: quotaSonnet}})
				fresh, _ := m.GetByID(a)
				if fresh.RegistrationEpoch <= stale.RegistrationEpoch || fresh.Generation >= stale.Generation {
					t.Fatal("test did not replace the registration and reset generation")
				}
				close(store.release)
				<-completed
				if reg.IsModelSuspendedForClient(a, quotaFable) || reg.IsModelQuotaExceededForClient(a, quotaFable) {
					t.Fatal("old header hold was published onto the replacement")
				}
				m.MarkResult(context.Background(), Result{AuthID: a, Provider: "claude", Model: quotaFable, CredentialVersion: fresh.CredentialVersion, RegistrationEpoch: fresh.RegistrationEpoch, Error: &Error{HTTPStatus: 404, Message: "replacement failure"}})
				if !reg.IsModelSuspendedForClient(a, quotaFable) {
					current, _ := m.GetByID(a)
					t.Fatalf("fresh registry publication missing: generation=%d state=%+v", current.Generation, current.ModelStates[quotaFable])
				}
			})
		})
	}
}

func TestClaudeQuotaExpiredSevenDayWatermarkDoesNotRank(t *testing.T) {
	m, a, b := newClaudeQuotaManager(t)
	now := time.Now()
	for _, id := range []string{a, b} {
		sevenDay := now.Add(time.Minute)
		if id == b {
			sevenDay = now.Add(2 * time.Hour)
		}
		base, _ := m.GetByID(id)
		m.applyClaudeQuotaUsage(context.Background(), m.runtimeConfig.Load().(*config.Config), base, map[string]QuotaWindow{
			"Fable": {Utilization: 0, ResetsAt: now.Add(time.Hour)}, "seven_day": {Utilization: 0, ResetsAt: sevenDay},
		}, now)
	}
	first, _ := m.GetByID(a)
	if first.QuotaRank(now) != 1 {
		t.Fatal("earlier active seven-day window did not win the tie")
	}
	later := now.Add(2 * time.Minute)
	m.applyClaudeQuotaUsage(context.Background(), m.runtimeConfig.Load().(*config.Config), first, nil, later)
	first, _ = m.GetByID(a)
	second, _ := m.GetByID(b)
	if first.QuotaRank(later) != 0 || second.QuotaRank(later) != 1 {
		t.Fatal("expired seven-day watermark still wins the ranking tie")
	}
	if !first.Quota.Windows["seven_day"].ResetsAt.Equal(now.Add(time.Minute)) {
		t.Fatal("expiry discarded the observation ordering watermark")
	}
}

func TestClaudeQuotaResponseHeadersRepeatedAndExpiredWatermarks(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, a, _ := newClaudeQuotaManager(t)
		start := time.Now()
		old := claudeQuotaHeaderContext("7d_oi", "1", start.Add(time.Hour))
		<-time.After(time.Second)
		current := claudeQuotaHeaderContext("7d_oi", "1", start.Add(time.Minute))
		m.MarkResult(current, Result{AuthID: a, Provider: "claude", Model: quotaSonnet, Success: true})
		<-time.After(time.Second)
		if _, _, errReset := m.ResetQuota(context.Background(), a); errReset != nil {
			t.Fatal(errReset)
		}
		// Replaying the exact accepted snapshot is not a fresh observation.
		m.MarkResult(current, Result{AuthID: a, Provider: "claude", Model: quotaSonnet, Success: true})
		auth, _ := m.GetByID(a)
		if blocked, _, _ := isAuthBlockedForModel(auth, quotaFable, time.Now()); blocked {
			t.Fatal("replayed snapshot re-armed a reset")
		}
		<-time.After(time.Minute)
		// An empty poll after reset must not discard the ordering watermark.
		m.applyClaudeQuotaUsage(context.Background(), m.runtimeConfig.Load().(*config.Config), auth, nil, time.Now())
		m.MarkResult(old, Result{AuthID: a, Provider: "claude", Model: quotaSonnet, Success: true})
		auth, _ = m.GetByID(a)
		if blocked, _, _ := isAuthBlockedForModel(auth, quotaFable, time.Now()); blocked || !auth.Quota.Windows["Fable"].ResetsAt.Equal(start.Add(time.Minute)) {
			t.Fatal("expired observation lost its ordering watermark")
		}
	})
}

func TestClaudeQuotaResponseHeadersReleaseOnlyGuardOwnership(t *testing.T) {
	for _, wrapped := range []bool{false, true} {
		t.Run(strconv.FormatBool(wrapped), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				m, a, _ := newClaudeQuotaManager(t)
				reset := time.Now().Add(time.Hour)
				applyTestClaudeQuota(m, a, 100, reset, time.Now())
				<-time.After(time.Second)
				retry := 20 * time.Minute
				m.MarkResult(context.Background(), Result{AuthID: a, Provider: "claude", Model: quotaFable, Error: &Error{HTTPStatus: 429}, RetryAfter: &retry, CredentialScope: wrapped})
				before, _ := m.GetByID(a)
				<-time.After(time.Second)
				m.MarkResult(claudeQuotaHeaderContext("7d_oi", "0", reset), Result{AuthID: a, Provider: "claude", Model: quotaSonnet, Success: true})
				auth, _ := m.GetByID(a)
				state := auth.ModelStates[quotaFable]
				if wrapped {
					if state.credentialQuotaShadow != nil || !state.NextRetryAfter.Equal(auth.Quota.NextRecoverAt) || auth.Quota.Reason != "credential_quota" {
						t.Fatal("header did not release just the wrapped guard")
					}
				} else if isClaudeQuotaHold(state) || !state.NextRetryAfter.Equal(before.ModelStates[quotaFable].NextRetryAfter) {
					t.Fatal("header changed an upstream-owned cooldown")
				}
				if blocked, _, _ := isAuthBlockedForModel(auth, quotaFable, time.Now()); !blocked {
					t.Fatal("header cleared upstream ownership")
				}
			})
		})
	}
}

func TestClaudeQuotaResponseHeadersWakeExpiryDuringStalledPoll(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, a, _ := newClaudeQuotaManager(t)
		m.RegisterExecutor(&claudeQuotaTestPoller{poll: func(ctx context.Context, _ *Auth) (map[string]QuotaWindow, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		}})
		m.StartQuotaPolling(context.Background())
		t.Cleanup(m.StopQuotaPolling)
		synctest.Wait()
		reset := time.Now().Add(10 * time.Second)
		m.MarkResult(claudeQuotaHeaderContext("7d_oi", "1", reset), Result{AuthID: a, Provider: "claude", Model: quotaSonnet, Success: true})
		synctest.Wait()
		if !registry.GetGlobalRegistry().IsModelSuspendedForClient(a, quotaFable) {
			t.Fatal("header hold not projected")
		}
		<-time.After(10 * time.Second)
		synctest.Wait()
		if registry.GetGlobalRegistry().IsModelSuspendedForClient(a, quotaFable) {
			t.Fatal("header expiry waited for the poll tick")
		}
	})
}

func TestClaudeQuotaHeaderWindowParsing(t *testing.T) {
	now := time.Unix(1800000000, 0)
	reset := now.Add(time.Hour)
	for _, tc := range []struct {
		name, utilization, reset string
		valid                    bool
	}{
		{"fraction", "0.25", strconv.FormatInt(reset.Unix(), 10), true},
		{"timestamp", "0.25", reset.Format(time.RFC3339Nano), true},
		{"missing usage", "", strconv.FormatInt(reset.Unix(), 10), false},
		{"negative usage", "-1", strconv.FormatInt(reset.Unix(), 10), false},
		{"NaN usage", "NaN", strconv.FormatInt(reset.Unix(), 10), false},
		{"infinite usage", "+Inf", strconv.FormatInt(reset.Unix(), 10), false},
		{"overflow percent", "1e308", strconv.FormatInt(reset.Unix(), 10), false},
		{"missing reset", "0.25", "", false},
		{"NaN reset", "0.25", "NaN", false},
		{"infinite reset", "0.25", "+Inf", false},
		{"negative reset", "0.25", "-1", false},
		{"overflow reset", "0.25", "1e30", false},
		{"unserializable reset", "0.25", "1e18", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			headers := http.Header{
				"anthropic-ratelimit-unified-7d_oi-utilization": {tc.utilization},
				"anthropic-ratelimit-unified-7d_oi-reset":       {tc.reset},
			}
			windows := claudeQuotaHeaderWindows(headers, now)
			window, ok := windows["Fable"]
			if ok != tc.valid {
				t.Fatalf("valid=%v want=%v", ok, tc.valid)
			}
			if ok && (window.Utilization != 25 || !window.ResetsAt.Equal(reset) || !window.ObservedAt.Equal(now)) {
				t.Fatalf("window=%+v", window)
			}
		})
	}
}
