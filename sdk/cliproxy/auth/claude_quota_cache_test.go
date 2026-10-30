package auth

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestClaudeQuotaCacheRefreshSharesMinuteBudgetAndRetainsFailures(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, a, b := newClaudeQuotaManager(t)
		m.Remove(context.Background(), b)
		cfg := m.runtimeConfig.Load().(*config.Config).CloneForRuntime()
		cfg.Claude.Quota.PollInterval = "5m"
		m.SetConfig(cfg)
		var calls atomic.Int64
		m.RegisterExecutor(&claudeQuotaTestPoller{poll: func(ctx context.Context, auth *Auth) (map[string]QuotaWindow, error) {
			if calls.Add(1) > 1 {
				return nil, &Error{HTTPStatus: 429}
			}
			return map[string]QuotaWindow{"Fable": {Utilization: 42, ResetsAt: time.Now().Add(time.Hour), ObservedAt: time.Now()}}, nil
		}})
		before, ok := m.ClaudeQuotaCached(a)
		if !ok || before.Status != "not_polled_yet" || before.PolledAt != nil || before.AgeSeconds != nil || before.NextPollAt == nil {
			t.Fatalf("never-polled snapshot: %+v", before)
		}
		m.StartQuotaPolling(context.Background())
		t.Cleanup(m.StopQuotaPolling)
		synctest.Wait()
		initial, _ := m.ClaudeQuotaCached(a)
		if initial.PolledAt == nil || initial.Windows["Fable"].Utilization != 42 || initial.Polls != 1 || initial.LastStatus != 200 {
			t.Fatalf("cache: %+v", initial)
		}
		for range 10 {
			if m.RequestClaudeQuotaPoll(a) {
				t.Fatal("click inside minute accepted")
			}
		}
		// Reload must not reset the budget or burst a second poll.
		cfg = cfg.CloneForRuntime()
		cfg.Claude.Quota.PriorityAtReset = !cfg.Claude.Quota.PriorityAtReset
		m.SetConfig(cfg)
		synctest.Wait()
		if calls.Load() != 1 {
			t.Fatal("reload bypassed poll budget")
		}
		<-time.After(time.Minute)
		if !m.RequestClaudeQuotaPoll(a) || m.RequestClaudeQuotaPoll(a) {
			t.Fatal("refresh did not coalesce")
		}
		synctest.Wait()
		if calls.Load() != 1 {
			t.Fatal("manual refresh bypassed next scheduled slot")
		}
		<-time.After(5 * time.Minute)
		synctest.Wait()
		after, _ := m.ClaudeQuotaCached(a)
		if calls.Load() != 2 || after.Polls != 2 || after.RateLimits != 1 || after.LastStatus != 429 || !after.PolledAt.Equal(*initial.PolledAt) || *after.AgeSeconds != 360 || after.Windows["Fable"].Utilization != 42 {
			t.Fatalf("failed poll discarded cache/age/counters: %+v calls=%d", after, calls.Load())
		}
		after.Windows["Fable"] = QuotaWindow{}
		copy, _ := m.ClaudeQuotaCached(a)
		if copy.Windows["Fable"].Utilization != 42 {
			t.Fatal("caller mutated cache")
		}
		cfg = cfg.CloneForRuntime()
		cfg.Claude.Quota.Enabled = false
		m.SetConfig(cfg)
		if _, ok := m.ClaudeQuotaCached(a); ok || m.RequestClaudeQuotaPoll(a) {
			t.Fatal("disabled cache still active")
		}
	})
}

func TestClaudeQuotaFleetPollsAreStaggeredAndClicksDoNotAddCalls(t *testing.T) {
	for _, size := range []int{11, 300, 1000} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				m := NewManager(nil, nil, nil)
				cfg := &config.Config{RequestRetry: 3}
				cfg.Claude.Quota.Enabled = true
				cfg.Claude.Quota.PollInterval = "1m"
				m.SetConfig(cfg)
				for i := range size {
					if _, errRegister := m.Register(context.Background(), &Auth{ID: fmt.Sprintf("seat-%02d", i), Provider: "claude", Status: StatusActive, Metadata: map[string]any{"access_token": "test"}}); errRegister != nil {
						t.Fatal(errRegister)
					}
				}
				var mu sync.Mutex
				var starts []time.Time
				bySeat := map[string][]time.Time{}
				m.RegisterExecutor(&claudeQuotaTestPoller{poll: func(ctx context.Context, auth *Auth) (map[string]QuotaWindow, error) {
					mu.Lock()
					defer mu.Unlock()
					starts = append(starts, time.Now())
					bySeat[auth.ID] = append(bySeat[auth.ID], time.Now())
					return nil, nil
				}})
				m.StartQuotaPolling(context.Background())
				t.Cleanup(m.StopQuotaPolling)
				for elapsed := range 130 {
					synctest.Wait()
					for _, auth := range m.List() {
						if elapsed == 120 {
							cached, ok := m.ClaudeQuotaCached(auth.ID)
							if !ok || cached.Stale || cached.PolledAt == nil {
								t.Fatalf("seat %s not fresh at the 120s threshold: %+v", auth.ID, cached)
							}
						}
						m.RequestClaudeQuotaPoll(auth.ID)
					}
					<-time.After(time.Second)
				}
				synctest.Wait()
				mu.Lock()
				defer mu.Unlock()
				if len(bySeat) != size {
					t.Fatalf("polled %d seats, want %d", len(bySeat), size)
				}
				for i := 1; i < len(starts); i++ {
					if starts[i].Sub(starts[i-1]) < time.Minute/time.Duration(size) {
						t.Fatal("fleet polls burst together")
					}
				}
				for id, times := range bySeat {
					for i := 1; i < len(times); i++ {
						if times[i].Sub(times[i-1]) < time.Minute {
							t.Fatalf("%s polled twice in a minute", id)
						}
					}
				}
			})
		})
	}
}

func TestClaudeQuotaFreshSeatPrecedesRecurringSeat(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, a, b := newClaudeQuotaManager(t)
		// The first seat is due again, while the later-sorting seat has never
		// received a slot. Simulate a fleet rotation extended by jitter.
		m.mu.Lock()
		seat := m.claudeQuotaPollSeatLocked(a)
		seat.lastAttempt = time.Now().Add(-2 * time.Minute)
		seat.nextPoll = time.Now().Add(-time.Second)
		m.mu.Unlock()
		started := make(chan string, 2)
		m.RegisterExecutor(&claudeQuotaTestPoller{poll: func(ctx context.Context, auth *Auth) (map[string]QuotaWindow, error) {
			started <- auth.ID
			return nil, nil
		}})
		m.StartQuotaPolling(context.Background())
		t.Cleanup(m.StopQuotaPolling)
		synctest.Wait()
		if len(started) != 1 || <-started != b {
			t.Fatal("recurring seat displaced a never-polled seat")
		}
	})
}

func TestClaudeQuotaWakeSurvivesRetiringLoop(t *testing.T) {
	m := NewManager(nil, nil, nil)
	// Both loop generations have computed their timers before the last old
	// read releases its slot. The retiring loop receives the notification first.
	retiring, replacement := m.quotaPollWake, m.quotaPollWake
	m.wakeClaudeQuotaPoll()
	<-retiring
	select {
	case <-replacement:
	default:
		t.Fatal("retiring loop stole replacement loop's only wakeup")
	}
}

func TestClaudeQuotaRejectedCredentialCancelsStalledRead(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, a, b := newClaudeQuotaManager(t)
		m.Remove(context.Background(), b)
		cancelled := make(chan struct{})
		m.RegisterExecutor(&claudeQuotaTestPoller{poll: func(ctx context.Context, _ *Auth) (map[string]QuotaWindow, error) {
			<-ctx.Done()
			close(cancelled)
			return nil, ctx.Err()
		}})
		m.StartQuotaPolling(context.Background())
		t.Cleanup(m.StopQuotaPolling)
		synctest.Wait()
		m.MarkResult(context.Background(), Result{AuthID: a, Provider: "claude", Model: quotaFable, Error: &Error{HTTPStatus: 401, Message: "unauthorized"}})
		synctest.Wait()
		select {
		case <-cancelled:
		default:
			t.Fatal("rejected credential retained its established quota read")
		}
	})
}

func TestClaudeQuotaResetRearmsSleepingLoop(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, a, b := newClaudeQuotaManager(t)
		m.Remove(context.Background(), b)
		m.mu.Lock()
		m.auths[a].LastError = &Error{Code: "invalid_grant", Message: "invalid grant"}
		m.mu.Unlock()
		var calls atomic.Int64
		m.RegisterExecutor(&claudeQuotaTestPoller{poll: func(context.Context, *Auth) (map[string]QuotaWindow, error) {
			calls.Add(1)
			return nil, nil
		}})
		m.StartQuotaPolling(context.Background())
		t.Cleanup(m.StopQuotaPolling)
		synctest.Wait()
		if calls.Load() != 0 || !m.nextClaudeQuotaPoll().IsZero() || !m.nextClaudeQuotaExpiry().IsZero() {
			t.Fatal("fixture must have no eligible seat or guard expiry")
		}
		if _, _, errReset := m.ResetQuota(context.Background(), a); errReset != nil {
			t.Fatal(errReset)
		}
		synctest.Wait()
		if calls.Load() != 1 {
			t.Fatal("cooldown reset did not wake the sole seat without another event")
		}
	})
}

func TestClaudeQuotaRefreshFailureCancelsStalledRead(t *testing.T) {
	for _, failure := range []struct {
		name string
		err  error
	}{
		{"invalid_grant", &Error{Code: "invalid_grant", Message: "invalid grant"}},
		{"unauthorized", &Error{HTTPStatus: 401, Message: "unauthorized"}},
	} {
		t.Run(failure.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				m, a, b := newClaudeQuotaManager(t)
				m.Remove(context.Background(), b)
				started, cancelled := make(chan struct{}), make(chan struct{})
				m.RegisterExecutor(&claudeQuotaTestPoller{
					ProviderExecutor: &mockOAuthErrorExecutor{id: "claude", errToReturn: failure.err},
					poll: func(ctx context.Context, _ *Auth) (map[string]QuotaWindow, error) {
						close(started)
						<-ctx.Done()
						close(cancelled)
						return nil, ctx.Err()
					},
				})
				m.StartQuotaPolling(context.Background())
				t.Cleanup(m.StopQuotaPolling)
				synctest.Wait()
				select {
				case <-started:
				default:
					t.Fatal("fixture did not start the quota read")
				}
				if _, errRefresh := m.refreshAuthForRequest(context.Background(), a, ""); errRefresh == nil {
					t.Fatal("fixture refresh must fail")
				}
				synctest.Wait()
				select {
				case <-cancelled:
				default:
					t.Fatal("refresh failure retained the ineligible seat's stalled quota read")
				}
			})
		})
	}
}

func TestClaudeQuotaFloorTimerRecoversMissedWake(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, a, b := newClaudeQuotaManager(t)
		m.Remove(context.Background(), b)
		cfg := m.runtimeConfig.Load().(*config.Config).CloneForRuntime()
		cfg.Claude.Quota.PollInterval = "5m"
		m.SetConfig(cfg)
		m.mu.Lock()
		m.auths[a].LastError = &Error{Code: "invalid_grant", Message: "invalid grant"}
		m.mu.Unlock()
		var calls atomic.Int64
		m.RegisterExecutor(&claudeQuotaTestPoller{poll: func(context.Context, *Auth) (map[string]QuotaWindow, error) {
			calls.Add(1)
			return nil, nil
		}})
		m.StartQuotaPolling(context.Background())
		t.Cleanup(m.StopQuotaPolling)
		synctest.Wait()
		if calls.Load() != 0 || !m.nextClaudeQuotaPoll().IsZero() || !m.nextClaudeQuotaExpiry().IsZero() {
			t.Fatal("fixture must have no eligible seat or guard expiry")
		}
		<-time.After(10 * time.Second)
		// Deliberately omit notification to model a future missed mutation path.
		m.mu.Lock()
		m.auths[a].LastError = nil
		m.mu.Unlock()
		synctest.Wait()
		if calls.Load() != 0 {
			t.Fatal("fixture unexpectedly woke the loop")
		}
		<-time.After(50 * time.Second)
		synctest.Wait()
		if calls.Load() != 1 {
			t.Fatal("fallback timer did not recover polling within one minute")
		}
	})
}

func TestClaudeQuotaRestoredHoldRearmsSleepingLoop(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, a, b := newClaudeQuotaManager(t)
		m.Remove(context.Background(), b)
		m.RegisterExecutor(&claudeQuotaTestPoller{poll: func(ctx context.Context, _ *Auth) (map[string]QuotaWindow, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		}})
		m.StartQuotaPolling(context.Background())
		t.Cleanup(m.StopQuotaPolling)
		synctest.Wait() // No pending expiry; the sole poll is already stalled.
		now := time.Now()
		reset := now.Add(30 * time.Second)
		store := &recordingCooldownStateStore{load: []CooldownStateRecord{{AuthID: a, Model: quotaFable, NextRetryAfter: reset, Reason: "quota_cap", Quota: QuotaState{Exceeded: true, Reason: "quota_cap", NextRecoverAt: reset}, UpdatedAt: now}}}
		m.SetCooldownStateStore(store)
		if errRestore := m.RestoreCooldownStates(context.Background()); errRestore != nil {
			t.Fatal(errRestore)
		}
		auth, _ := m.GetByID(a)
		if !isClaudeQuotaHold(auth.ModelStates[quotaFable]) {
			t.Fatal("fixture did not restore a guard hold")
		}
		synctest.Wait()
		<-time.After(30 * time.Second)
		synctest.Wait()
		auth, _ = m.GetByID(a)
		if isClaudeQuotaHold(auth.ModelStates[quotaFable]) || len(store.savedRecords()) != 0 {
			t.Fatal("restored hold expired without cleanup or persistence")
		}
	})
}

func TestClaudeQuotaIntervalReloadReschedulesExistingSlot(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, _, b := newClaudeQuotaManager(t)
		m.Remove(context.Background(), b)
		cfg := m.runtimeConfig.Load().(*config.Config).CloneForRuntime()
		cfg.Claude.Quota.PollInterval = "5m"
		m.SetConfig(cfg)
		var calls atomic.Int64
		m.RegisterExecutor(&claudeQuotaTestPoller{poll: func(context.Context, *Auth) (map[string]QuotaWindow, error) { calls.Add(1); return nil, nil }})
		m.StartQuotaPolling(context.Background())
		t.Cleanup(m.StopQuotaPolling)
		synctest.Wait()
		<-time.After(70 * time.Second)
		cfg = cfg.CloneForRuntime()
		cfg.Claude.Quota.PollInterval = "1m"
		m.SetConfig(cfg)
		synctest.Wait()
		if calls.Load() != 2 {
			t.Fatal("shorter interval retained the old five-minute slot")
		}
		cfg = cfg.CloneForRuntime()
		cfg.Claude.Quota.PollInterval = "10m"
		m.SetConfig(cfg)
		<-time.After(2 * time.Minute)
		synctest.Wait()
		if calls.Load() != 2 {
			t.Fatal("longer interval retained the old one-minute slot")
		}
	})
}
