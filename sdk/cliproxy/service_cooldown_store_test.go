package cliproxy

import (
	"context"
	"path/filepath"
	"testing"
	"testing/synctest"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	sdkAuth "github.com/router-for-me/CLIProxyAPI/v8/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

type cooldownProviderTokenStore struct {
	cooldownStore coreauth.CooldownStateStore
}

func (s *cooldownProviderTokenStore) List(context.Context) ([]*coreauth.Auth, error) {
	return nil, nil
}

func (s *cooldownProviderTokenStore) Save(context.Context, *coreauth.Auth) (string, error) {
	return "", nil
}

func (s *cooldownProviderTokenStore) Delete(context.Context, string) error {
	return nil
}

func (s *cooldownProviderTokenStore) CooldownStateStore() coreauth.CooldownStateStore {
	return s.cooldownStore
}

type serviceCooldownStateStore struct{}

type quotaReloadStore struct {
	records []coreauth.CooldownStateRecord
	loads   int
}

func (s *quotaReloadStore) Load(context.Context) ([]coreauth.CooldownStateRecord, error) {
	s.loads++
	return append([]coreauth.CooldownStateRecord(nil), s.records...), nil
}

func (s *quotaReloadStore) Save(_ context.Context, records []coreauth.CooldownStateRecord) error {
	s.records = append([]coreauth.CooldownStateRecord(nil), records...)
	return nil
}

type quotaReloadPoller struct {
	coreauth.ProviderExecutor
	windows map[string]coreauth.QuotaWindow
	calls   int
}

func (*quotaReloadPoller) Identifier() string { return "claude" }
func (p *quotaReloadPoller) PollQuota(context.Context, *coreauth.Auth) (map[string]coreauth.QuotaWindow, error) {
	p.calls++
	return p.windows, nil
}

func TestClaudeQuotaConfigApplyDoesNotRearmClearedHold(t *testing.T) {
	for _, mode := range []string{"manual reset", "successful request"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				const model = "claude-fable-5-1"
				ctx := context.Background()
				cfg := &config.Config{SaveCooldownStatus: true, RequestRetry: 3, AuthDir: t.TempDir()}
				cfg.Claude.Quota.Enabled = true
				manager := coreauth.NewManager(nil, nil, nil)
				store := &quotaReloadStore{}
				manager.SetConfig(cfg)
				manager.SetCooldownStateStore(store)
				if _, errRegister := manager.Register(ctx, &coreauth.Auth{ID: "quota-reload", Provider: "claude", Status: coreauth.StatusActive, Metadata: map[string]any{"access_token": "test-access"}}); errRegister != nil {
					t.Fatal(errRegister)
				}
				poller := &quotaReloadPoller{windows: map[string]coreauth.QuotaWindow{"Fable": {Utilization: 100, ResetsAt: time.Now().Add(time.Hour), ObservedAt: time.Now()}}}
				manager.RegisterExecutor(poller)
				manager.StartQuotaPolling(ctx)
				t.Cleanup(manager.StopQuotaPolling)
				synctest.Wait()
				auth, _ := manager.GetByID("quota-reload")
				if auth.ModelStates[model] == nil || auth.ModelStates[model].Quota.Reason != "quota_cap" {
					t.Fatal("initial poll did not install a hold")
				}
				if mode == "manual reset" {
					if _, _, errReset := manager.ResetQuota(ctx, auth.ID); errReset != nil {
						t.Fatal(errReset)
					}
				} else {
					manager.MarkResult(ctx, coreauth.Result{AuthID: auth.ID, Provider: "claude", Model: model, Success: true})
				}
				poller.windows = map[string]coreauth.QuotaWindow{}
				<-time.After(70 * time.Second)
				synctest.Wait()
				if poller.calls != 2 {
					t.Fatal("partial poll did not complete")
				}
				manager.StopQuotaPolling()
				synctest.Wait()
				// Keep the actual store non-empty with an unrelated upstream cooldown.
				manager.MarkResult(ctx, coreauth.Result{AuthID: auth.ID, Provider: "claude", Model: "claude-sonnet-5-5", Error: &coreauth.Error{HTTPStatus: 404, Message: "not found"}})
				if len(store.records) != 1 {
					t.Fatalf("store contains %d records, want one independent cooldown", len(store.records))
				}
				service := &Service{cfg: cfg, coreManager: manager, cooldownStateStore: store}
				reloaded := cfg.CloneForRuntime()
				reloaded.Claude.Quota.PollInterval = "2m"
				if !service.applyConfigUpdateWithAuthSynthesis(ctx, reloaded, false) {
					t.Fatal("config apply failed")
				}
				if store.loads == 0 {
					t.Fatal("config apply did not restore from the non-empty store")
				}
				auth, _ = manager.GetByID(auth.ID)
				if auth.Quota.Windows["Fable"].Utilization != 100 {
					t.Fatal("test lost retained exhausted usage")
				}
				for _, cooldown := range coreauth.CooldownSnapshotForAuth(auth, time.Now()) {
					if cooldown.ModelKey == model {
						t.Fatal("config apply restored the cleared hold from retained usage")
					}
				}
			})
		})
	}
}

func TestClaudeQuotaConfigApplyPreservesLiveWindowDeadlines(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const model = "claude-fable-5-1"
		ctx := context.Background()
		cfg := &config.Config{SaveCooldownStatus: true, RequestRetry: 3, AuthDir: t.TempDir()}
		cfg.Claude.Quota.Enabled = true
		cfg.Claude.Quota.ModelWindows = map[string][]string{"Fable": {model}, "Additional": {model}}
		manager := coreauth.NewManager(nil, nil, nil)
		store := &quotaReloadStore{}
		manager.SetConfig(cfg)
		manager.SetCooldownStateStore(store)
		if _, errRegister := manager.Register(ctx, &coreauth.Auth{ID: "quota-overlap", Provider: "claude", Status: coreauth.StatusActive, Metadata: map[string]any{"access_token": "test-access"}}); errRegister != nil {
			t.Fatal(errRegister)
		}
		long, short := time.Now().Add(2*time.Hour), time.Now().Add(time.Hour)
		poller := &quotaReloadPoller{windows: map[string]coreauth.QuotaWindow{
			"Fable": {Utilization: 100, ResetsAt: long, ObservedAt: time.Now()}, "Additional": {Utilization: 100, ResetsAt: short, ObservedAt: time.Now()},
		}}
		manager.RegisterExecutor(poller)
		manager.StartQuotaPolling(ctx)
		t.Cleanup(manager.StopQuotaPolling)
		synctest.Wait()
		manager.StopQuotaPolling()
		synctest.Wait()
		if len(store.records) != 1 || !store.records[0].NextRetryAfter.Equal(long) {
			t.Fatal("test did not persist the aggregate hold")
		}
		service := &Service{cfg: cfg, coreManager: manager, cooldownStateStore: store}
		reloaded := cfg.CloneForRuntime()
		reloaded.Claude.Quota.PollInterval = "2m"
		if !service.applyConfigUpdateWithAuthSynthesis(ctx, reloaded, false) || store.loads == 0 {
			t.Fatal("config apply did not restore the hold")
		}
		// The longer window releases; the omitted window must keep its own deadline.
		<-time.After(time.Second)
		poller.windows = map[string]coreauth.QuotaWindow{"Fable": {Utilization: 0, ResetsAt: long, ObservedAt: time.Now()}}
		manager.RegisterExecutor(poller)
		manager.StartQuotaPolling(ctx)
		synctest.Wait()
		// Restarting shares the previous attempt's minute budget and jitter.
		<-time.After(70 * time.Second)
		synctest.Wait()
		auth, _ := manager.GetByID("quota-overlap")
		cooldowns := coreauth.CooldownSnapshotForAuth(auth, time.Now())
		if len(cooldowns) != 1 || cooldowns[0].ModelKey != model || !cooldowns[0].RetryAt.Equal(short) {
			t.Fatalf("config restore extended the remaining window: %+v", cooldowns)
		}
	})
}

func (*serviceCooldownStateStore) Load(context.Context) ([]coreauth.CooldownStateRecord, error) {
	return nil, nil
}

func (*serviceCooldownStateStore) Save(context.Context, []coreauth.CooldownStateRecord) error {
	return nil
}

func TestResolveCooldownStateStoreUsesCapturedBackendProvider(t *testing.T) {
	originalStore := sdkAuth.GetTokenStore()
	t.Cleanup(func() {
		sdkAuth.RegisterTokenStore(originalStore)
	})

	providedStore := &serviceCooldownStateStore{}
	sdkAuth.RegisterTokenStore(&cooldownProviderTokenStore{cooldownStore: providedStore})
	cfg := &config.Config{
		AuthDir:            t.TempDir(),
		SaveCooldownStatus: true,
	}
	service, errBuild := NewBuilder().
		WithConfig(cfg).
		WithConfigPath(filepath.Join(t.TempDir(), "config.yaml")).
		Build()
	if errBuild != nil {
		t.Fatalf("Build() error = %v", errBuild)
	}

	sdkAuth.RegisterTokenStore(&cooldownProviderTokenStore{cooldownStore: &serviceCooldownStateStore{}})
	got := service.resolveCooldownStateStore(cfg)
	if got != providedStore {
		t.Fatalf("resolveCooldownStateStore() = %T, want captured backend-provided store", got)
	}
}
