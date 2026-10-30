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
