package auth

import (
	"context"
	"fmt"
	"net/http"
	"strings"
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

func TestCodexQuotaWebsocketReceiptAfterPoll(t *testing.T) {
	for _, handshake := range []bool{false, true} {
		t.Run(map[bool]string{false: "reused-connection", true: "fresh-connection"}[handshake], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				m := NewManager(nil, nil, nil)
				cfg := &config.Config{RequestRetry: 3}
				cfg.Claude.Quota.Enabled = true
				m.SetConfig(cfg)
				base, errRegister := m.Register(context.Background(), &Auth{ID: "seat", Provider: "codex", Status: StatusActive, Metadata: map[string]any{"access_token": "fixture", "account_id": "account"}})
				if errRegister != nil {
					t.Fatal(errRegister)
				}
				ctx := internallogging.WithFreshResponseHeadersHolder(context.Background())
				if handshake {
					internallogging.SetResponseHeaders(ctx, http.Header{
						"X-Codex-Secondary-Used-Percent": {"30"}, "X-Codex-Secondary-Window-Minutes": {"10080"}, "X-Codex-Secondary-Reset-After-Seconds": {"300"},
					})
				}
				<-time.After(time.Second)
				m.applyCodexQuotaUsageSnapshot(context.Background(), m.runtimeConfig.Load().(*config.Config), base, map[string]QuotaWindow{
					"five_hour": {Utilization: 40, ResetsAt: time.Now().Add(time.Hour), ObservedAt: time.Now()},
					"seven_day": {Utilization: 50, ResetsAt: time.Now().Add(time.Hour), ObservedAt: time.Now()},
				}, nil, time.Now())
				if polled, _ := m.CodexQuotaCached(base.ID); polled.PolledAt == nil || polled.Windows["seven_day"].Utilization != 50 {
					t.Fatal("poll fixture was not applied")
				}
				<-time.After(time.Second)
				eventAt := time.Now()
				internallogging.MergeResponseHeaders(ctx, http.Header{
					"X-Codex-Primary-Used-Percent": {"80"}, "X-Codex-Primary-Window-Minutes": {"300"}, "X-Codex-Primary-Reset-After-Seconds": {"300"},
				})
				<-time.After(time.Second)
				m.MarkResult(ctx, Result{AuthID: base.ID, Provider: "codex", Model: "fixture", Success: true})
				snapshot, _ := m.CodexQuotaCached(base.ID)
				if snapshot.Windows["five_hour"].Utilization != 80 || !snapshot.Windows["five_hour"].ObservedAt.Equal(eventAt) || snapshot.Windows["seven_day"].Utilization != 50 {
					t.Fatalf("websocket ordering: %+v", snapshot.Windows)
				}
			})
		})
	}
}

func TestCodexQuotaAdditionalIdentityMatchesHTTPAndPoll(t *testing.T) {
	base := time.Unix(1800000000, 0)
	auth := &Auth{}
	for i, prefix := range []string{"X-Codex-Bengalfox-", "X-Codex-Additional-Gpt-5.3-Codex-Spark-", "X-Codex-Bengalfox-"} {
		headers := make(http.Header)
		headers.Set(prefix+"Limit-Name", "GPT-5.3-Codex-Spark")
		headers.Set(prefix+"Primary-Used-Percent", []string{"10", "20", "30"}[i])
		headers.Set(prefix+"Primary-Window-Minutes", "300")
		headers.Set(prefix+"Primary-Reset-After-Seconds", "300")
		mergeCodexQuotaWindows(auth, CodexQuotaWindows(headers, base.Add(time.Duration(i)*time.Second)))
	}
	if len(auth.Quota.Windows) != 1 || auth.Quota.Windows["additional/gpt-5.3-codex-spark/five_hour"].Utilization != 30 {
		t.Fatalf("duplicate identities: %+v", auth.Quota.Windows)
	}
}

func TestCodexQuotaRetainedGroupsStayBoundedWithoutEvictingMain(t *testing.T) {
	base := time.Unix(1800000000, 0)
	auth := &Auth{}
	mergeCodexQuotaWindows(auth, map[string]QuotaWindow{
		"five_hour":             {Utilization: 10, ResetsAt: base.Add(time.Hour), ObservedAt: base},
		"seven_day":             {Utilization: 20, ResetsAt: base.Add(time.Hour), ObservedAt: base},
		"code_review/seven_day": {Utilization: 30, ResetsAt: base.Add(time.Hour), ObservedAt: base},
	})
	for i := range 50 {
		mergeCodexQuotaWindows(auth, map[string]QuotaWindow{
			fmt.Sprintf("additional/limit-%02d/five_hour", i): {Utilization: float64(i), ResetsAt: base.Add(time.Hour), ObservedAt: base.Add(time.Duration(i+1) * time.Second)},
		})
		groups := map[string]bool{}
		for name := range auth.Quota.Windows {
			if strings.HasPrefix(name, "additional/") {
				groups[strings.TrimSuffix(name, "/five_hour")] = true
			}
		}
		if len(groups) > 8 || len(auth.Quota.Windows) > maxCodexQuotaWindows {
			t.Fatalf("unbounded retained usage: %+v", auth.Quota.Windows)
		}
	}
	if len(auth.Quota.Windows) != 11 || auth.Quota.Windows["five_hour"].Utilization != 10 || auth.Quota.Windows["seven_day"].Utilization != 20 || auth.Quota.Windows["code_review/seven_day"].Utilization != 30 || auth.Quota.Windows["additional/limit-49/five_hour"].Utilization != 49 {
		t.Fatalf("main or newest limit lost: %+v", auth.Quota.Windows)
	}
}
