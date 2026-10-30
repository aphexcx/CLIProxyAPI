package auth

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	internallogging "github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
)

type codexCacheTestPoller struct {
	ProviderExecutor
	poll func(context.Context, *Auth) (map[string]QuotaWindow, error)
}

func (*codexCacheTestPoller) Identifier() string { return "codex" }
func (p *codexCacheTestPoller) PollQuota(ctx context.Context, auth *Auth) (map[string]QuotaWindow, error) {
	return p.poll(ctx, auth)
}

func TestCodexQuotaCacheBudgetFailureAndObservationalState(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := NewManager(nil, nil, nil)
		cfg := &config.Config{RequestRetry: 3}
		cfg.Claude.Quota.Enabled = true
		cfg.Claude.Quota.PollInterval = "5m"
		m.SetConfig(cfg)
		auth, errRegister := m.Register(context.Background(), &Auth{ID: "codex", Provider: "codex", Status: StatusActive, Metadata: map[string]any{"access_token": "fixture", "account_id": "account"}})
		if errRegister != nil {
			t.Fatal(errRegister)
		}
		var calls atomic.Int64
		m.RegisterExecutor(&codexCacheTestPoller{poll: func(context.Context, *Auth) (map[string]QuotaWindow, error) {
			if calls.Add(1) > 1 {
				return nil, &Error{HTTPStatus: 429}
			}
			return map[string]QuotaWindow{"five_hour": {Utilization: 100, ResetsAt: time.Now().Add(time.Hour), ObservedAt: time.Now()}}, nil
		}})
		before, ok := m.CodexQuotaCached(auth.ID)
		if !ok || before.Status != "not_polled_yet" || before.PolledAt != nil {
			t.Fatalf("before: %+v", before)
		}
		m.StartQuotaPolling(context.Background())
		t.Cleanup(m.StopQuotaPolling)
		synctest.Wait()
		first, _ := m.CodexQuotaCached(auth.ID)
		if first.PolledAt == nil || first.Stale || first.LastStatus != 200 || first.Windows["five_hour"].Utilization != 100 {
			t.Fatalf("first: %+v", first)
		}
		for range 10 {
			if m.RequestCodexQuotaPoll(auth.ID) {
				t.Fatal("bypassed minimum interval")
			}
		}
		m.SetConfig(cfg.CloneForRuntime())
		synctest.Wait()
		if calls.Load() != 1 {
			t.Fatal("reload bypassed budget")
		}
		<-time.After(time.Minute)
		if !m.RequestCodexQuotaPoll(auth.ID) || m.RequestCodexQuotaPoll(auth.ID) {
			t.Fatal("refresh did not coalesce")
		}
		synctest.Wait()
		if calls.Load() != 1 {
			t.Fatal("refresh skipped scheduled slot")
		}
		<-time.After(5 * time.Minute)
		synctest.Wait()
		failed, _ := m.CodexQuotaCached(auth.ID)
		if calls.Load() != 2 || !failed.Stale || failed.LastStatus != 429 || failed.RateLimits != 1 || !failed.PolledAt.Equal(*first.PolledAt) || failed.Windows["five_hour"].Utilization != 100 {
			t.Fatalf("failed: %+v calls=%d", failed, calls.Load())
		}
		current, _ := m.GetByID(auth.ID)
		if current.Quota.Exceeded || current.Unavailable || !current.NextRetryAfter.IsZero() || len(current.ModelStates) != 0 || current.Attributes["quota_rank"] != "" {
			t.Fatal("poll changed routing state")
		}
		cfg = cfg.CloneForRuntime()
		cfg.Claude.Quota.Enabled = false
		m.SetConfig(cfg)
		if _, ok := m.CodexQuotaCached(auth.ID); ok || m.RequestCodexQuotaPoll(auth.ID) {
			t.Fatal("disabled quota still available")
		}
	})
}

func TestCodexQuotaResponseWindowsKeepReceiptOrder(t *testing.T) {
	base := time.Unix(1800000000, 0)
	auth := &Auth{}
	window := func(percent string, at time.Time) map[string]QuotaWindow {
		return CodexQuotaWindows(http.Header{
			"X-Codex-Primary-Used-Percent": {percent}, "X-Codex-Primary-Window-Minutes": {"300"}, "X-Codex-Primary-Reset-After-Seconds": {"300"},
		}, at)
	}
	mergeCodexQuotaWindows(auth, window("60", base.Add(time.Second)))
	mergeCodexQuotaWindows(auth, window("10", base))
	if got := auth.Quota.Windows["five_hour"]; got.Utilization != 60 || !got.ResetsAt.Equal(base.Add(301*time.Second)) {
		t.Fatalf("older poll replaced response: %+v", got)
	}
	mergeCodexQuotaWindows(auth, window("20", base.Add(2*time.Second)))
	if auth.Quota.Windows["five_hour"].Utilization != 20 {
		t.Fatal("newer poll rejected")
	}
}

func TestCodexQuotaDelayedPollRetainsNewerResponseInCache(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := NewManager(nil, nil, nil)
		cfg := &config.Config{RequestRetry: 3}
		cfg.Claude.Quota.Enabled = true
		m.SetConfig(cfg)
		base, errRegister := m.Register(context.Background(), &Auth{ID: "codex", Provider: "codex", Status: StatusActive, Metadata: map[string]any{"access_token": "fixture", "account_id": "account"}})
		if errRegister != nil {
			t.Fatal(errRegister)
		}
		pollReceipt := time.Now()
		bodyReady := make(chan struct{})
		m.RegisterExecutor(&codexCacheTestPoller{poll: func(context.Context, *Auth) (map[string]QuotaWindow, error) {
			<-bodyReady
			return map[string]QuotaWindow{"five_hour": {Utilization: 10, ObservedAt: pollReceipt, ResetsAt: pollReceipt.Add(time.Hour)}}, nil
		}})
		m.StartQuotaPolling(context.Background())
		t.Cleanup(m.StopQuotaPolling)
		synctest.Wait()
		<-time.After(time.Second)
		ctx := internallogging.WithFreshResponseHeadersHolder(context.Background())
		internallogging.SetResponseHeaders(ctx, http.Header{
			"X-Codex-Primary-Used-Percent": {"80"}, "X-Codex-Primary-Window-Minutes": {"300"}, "X-Codex-Primary-Reset-After-Seconds": {"300"},
		})
		m.MarkResult(ctx, Result{AuthID: base.ID, Provider: "codex", Model: "fixture-model", Success: true})
		close(bodyReady)
		synctest.Wait()
		snapshot, ok := m.CodexQuotaCached(base.ID)
		if !ok || snapshot.PolledAt == nil || snapshot.Windows["five_hour"].Utilization != 80 {
			t.Fatalf("delayed poll won: %+v", snapshot)
		}
	})
}
