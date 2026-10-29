package auth

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

const quotaFable = "claude-fable-5-1"
const quotaSonnet = "claude-sonnet-5-5"

func newClaudeQuotaManager(t *testing.T) (*Manager, string, string) {
	t.Helper()
	m := NewManager(nil, &FillFirstSelector{}, nil)
	cfg := &config.Config{RequestRetry: 3}
	cfg.Claude.Quota.Enabled = true
	cfg.Claude.Quota.PriorityAtReset = true
	m.SetConfig(cfg)
	ids := []string{t.Name() + "-a", t.Name() + "-b"}
	for _, id := range ids {
		registry.GetGlobalRegistry().RegisterClient(id, "claude", []*registry.ModelInfo{{ID: quotaFable}, {ID: quotaSonnet}})
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
		_, errRegister := m.Register(context.Background(), &Auth{ID: id, FileName: id + ".json", Provider: "claude", Status: StatusActive, Metadata: map[string]any{"access_token": "test-access"}})
		if errRegister != nil {
			t.Fatal(errRegister)
		}
	}
	return m, ids[0], ids[1]
}

func applyTestClaudeQuota(m *Manager, id string, percent float64, reset, now time.Time) {
	auth, _ := m.GetByID(id)
	m.applyClaudeQuotaUsage(context.Background(), m.runtimeConfig.Load().(*config.Config), auth, map[string]QuotaWindow{
		"Fable":     {Utilization: percent, ResetsAt: reset},
		"five_hour": {Utilization: 100, ResetsAt: reset},
		"seven_day": {Utilization: 100, ResetsAt: reset},
	}, now)
}

func TestClaudeQuotaHoldIsPerModelAndExpires(t *testing.T) {
	m, a, _ := newClaudeQuotaManager(t)
	now := time.Now()
	reset := now.Add(time.Hour)
	applyTestClaudeQuota(m, a, 100, reset, now)
	auth, _ := m.GetByID(a)
	if auth.Disabled || auth.Quota.Reason == "credential_quota" {
		t.Fatal("guard disabled or cooled the credential")
	}
	if blocked, _, until := isAuthBlockedForModel(auth, quotaFable, now); !blocked || !until.Equal(reset) {
		t.Fatalf("Fable blocked=%v until=%v", blocked, until)
	}
	if blocked, _, _ := isAuthBlockedForModel(auth, quotaSonnet, now); blocked {
		t.Fatal("Sonnet was pre-held by full account windows")
	}
	if blocked, _, _ := isAuthBlockedForModel(auth, quotaFable, reset); blocked {
		t.Fatal("hold did not expire at reset")
	}
	views := CooldownSnapshotForAuth(auth, now)
	if len(views) != 1 || views[0].Reason != "quota_cap" || views[0].Scope != "model" || !views[0].RetryAt.Equal(reset) {
		t.Fatalf("cooldowns=%+v", views)
	}
	if auth.Quota.Windows["Fable"].ObservedAt != now {
		t.Fatal("usage observation time missing")
	}
	applyTestClaudeQuota(m, a, 100, reset, reset)
	auth, _ = m.GetByID(a)
	if auth.ModelStates[quotaFable].Quota.Exceeded {
		t.Fatal("expired usage re-held the model")
	}
}

func TestClaudeQuotaReleaseResetAndReload(t *testing.T) {
	for _, mode := range []string{"below cap", "manual reset", "disabled feature", "removed mapping", "disabled cooling"} {
		t.Run(mode, func(t *testing.T) {
			m, a, _ := newClaudeQuotaManager(t)
			now := time.Now()
			applyTestClaudeQuota(m, a, 100, now.Add(time.Hour), now)
			switch mode {
			case "below cap":
				applyTestClaudeQuota(m, a, 0, now.Add(time.Hour), now.Add(time.Second))
			case "manual reset":
				if _, _, errReset := m.ResetQuota(context.Background(), a); errReset != nil {
					t.Fatal(errReset)
				}
			default:
				cfg := m.runtimeConfig.Load().(*config.Config).CloneForRuntime()
				switch mode {
				case "disabled feature":
					cfg.Claude.Quota.Enabled = false
				case "removed mapping":
					cfg.Claude.Quota.ModelWindows = map[string][]string{}
				case "disabled cooling":
					cfg.DisableCooling = true
				}
				m.SetConfig(cfg)
			}
			auth, _ := m.GetByID(a)
			if blocked, _, _ := isAuthBlockedForModel(auth, quotaFable, now); blocked {
				t.Fatalf("%s did not release hold", mode)
			}
		})
	}
}

func TestClaudeQuotaMissingWindowAndIndependentCooldown(t *testing.T) {
	m, a, _ := newClaudeQuotaManager(t)
	now := time.Now()
	applyTestClaudeQuota(m, a, 100, now.Add(time.Hour), now)
	auth, _ := m.GetByID(a)
	m.applyClaudeQuotaUsage(context.Background(), m.runtimeConfig.Load().(*config.Config), auth, map[string]QuotaWindow{}, now.Add(time.Second))
	auth, _ = m.GetByID(a)
	if blocked, _, _ := isAuthBlockedForModel(auth, quotaFable, now); !blocked {
		t.Fatal("missing usage released a live hold")
	}
	delay := 2 * time.Hour
	m.MarkResult(context.Background(), Result{AuthID: a, Provider: "claude", Model: quotaFable, RetryAfter: &delay, Error: &Error{HTTPStatus: 429, Message: "upstream limit"}})
	applyTestClaudeQuota(m, a, 0, now.Add(time.Hour), now.Add(2*time.Second))
	auth, _ = m.GetByID(a)
	if blocked, _, until := isAuthBlockedForModel(auth, quotaFable, now); !blocked || until.Before(now.Add(delay)) {
		t.Fatal("poll cleared or shortened independent upstream cooldown")
	}
}

func TestClaudeQuotaDoesNotEraseCredentialCooldown(t *testing.T) {
	m, a, _ := newClaudeQuotaManager(t)
	now := time.Now()
	applyTestClaudeQuota(m, a, 100, now.Add(time.Hour), now)
	delay := 2 * time.Hour
	m.MarkResult(context.Background(), Result{AuthID: a, Provider: "claude", Model: quotaSonnet, RetryAfter: &delay, CredentialScope: true, Error: &Error{HTTPStatus: 429}})
	applyTestClaudeQuota(m, a, 0, now.Add(time.Hour), now.Add(time.Second))
	auth, _ := m.GetByID(a)
	if auth.Quota.Reason != "credential_quota" {
		t.Fatal("credential cooldown lost")
	}
	for _, model := range []string{quotaFable, quotaSonnet} {
		if blocked, _, _ := isAuthBlockedForModel(auth, model, now); !blocked {
			t.Fatalf("%s lost credential cooldown", model)
		}
	}
	shadow := auth.ModelStates[quotaFable].credentialQuotaShadow
	if shadow != nil && shadow.Quota.Reason == "quota_cap" {
		t.Fatal("guard shadow did not release")
	}
}

func TestClaudeQuotaWrappedHoldReleaseAndExtension(t *testing.T) {
	for _, guardFirst := range []bool{true, false} {
		t.Run(strconv.FormatBool(guardFirst), func(t *testing.T) {
			m, a, _ := newClaudeQuotaManager(t)
			now := time.Now()
			if guardFirst {
				applyTestClaudeQuota(m, a, 100, now.Add(time.Hour), now)
			}
			delay := 10 * time.Minute
			m.MarkResult(context.Background(), Result{AuthID: a, Provider: "claude", Model: quotaSonnet, RetryAfter: &delay, CredentialScope: true, Error: &Error{HTTPStatus: 429}})
			applyTestClaudeQuota(m, a, 100, now.Add(2*time.Hour), now.Add(time.Second))
			auth, _ := m.GetByID(a)
			if blocked, _, next := isAuthBlockedForModel(auth, quotaFable, now.Add(20*time.Minute)); !blocked || !next.Equal(now.Add(2*time.Hour)) {
				t.Fatalf("guard lost beneath wrap: blocked=%v next=%v", blocked, next)
			}
			applyTestClaudeQuota(m, a, 0, now.Add(2*time.Hour), now.Add(2*time.Second))
			auth, _ = m.GetByID(a)
			if blocked, _, _ := isAuthBlockedForModel(auth, quotaFable, now.Add(20*time.Minute)); blocked {
				t.Fatal("released guard's old deadline survived in credential wrap")
			}
			if !auth.Quota.Exceeded || auth.Quota.Reason != "credential_quota" {
				t.Fatal("guard release erased upstream quota")
			}
		})
	}
}

func TestClaudeQuotaPersistRestoreAndDisable(t *testing.T) {
	m, a, _ := newClaudeQuotaManager(t)
	store := &recordingCooldownStateStore{}
	m.SetCooldownStateStore(store)
	now := time.Now()
	applyTestClaudeQuota(m, a, 100, now.Add(time.Hour), now)
	records := store.savedRecords()
	if len(records) != 1 || records[0].Quota.Reason != "quota_cap" {
		t.Fatalf("persisted records=%v", records)
	}
	for _, enabled := range []bool{true, false} {
		t.Run(strconv.FormatBool(enabled), func(t *testing.T) {
			restored := NewManager(nil, nil, nil)
			cfg := &config.Config{}
			cfg.Claude.Quota.Enabled = enabled
			restored.SetConfig(cfg)
			if _, errRegister := restored.Register(context.Background(), &Auth{ID: a, Provider: "claude", Status: StatusActive, Metadata: map[string]any{"access_token": "test-access"}}); errRegister != nil {
				t.Fatal(errRegister)
			}
			restored.SetCooldownStateStore(&recordingCooldownStateStore{load: records})
			if errRestore := restored.RestoreCooldownStates(context.Background()); errRestore != nil {
				t.Fatal(errRestore)
			}
			auth, _ := restored.GetByID(a)
			blocked, _, _ := isAuthBlockedForModel(auth, quotaFable, now)
			if blocked != enabled || len(auth.Quota.Windows) != 0 || auth.Attributes["quota_rank"] != "" {
				t.Fatal("restored hold ignored enablement or restored runtime observations")
			}
		})
	}
	applyTestClaudeQuota(m, a, 0, now.Add(time.Hour), now)
	if len(store.savedRecords()) != 0 {
		t.Fatal("released cooldown remained persisted")
	}
}

func TestClaudeQuotaOverlappingWindowsUseLatestReset(t *testing.T) {
	m, a, _ := newClaudeQuotaManager(t)
	cfg := m.runtimeConfig.Load().(*config.Config).CloneForRuntime()
	cfg.Claude.Quota.ModelWindows = map[string][]string{"Fable": {quotaFable}, "Additional": {quotaFable}}
	cfg.Claude.Quota.WindowCaps = map[string]float64{"Fable": 99}
	m.SetConfig(cfg)
	now := time.Now()
	windows := map[string]QuotaWindow{"Fable": {Utilization: 99, ResetsAt: now.Add(time.Hour)}, "Additional": {Utilization: 100, ResetsAt: now.Add(2 * time.Hour)}}
	apply := func() {
		base, _ := m.GetByID(a)
		m.applyClaudeQuotaUsage(context.Background(), m.runtimeConfig.Load().(*config.Config), base, windows, now)
	}
	apply()
	auth, _ := m.GetByID(a)
	if !auth.ModelStates[quotaFable].NextRetryAfter.Equal(now.Add(2 * time.Hour)) {
		t.Fatal("short window overwrote the longer hold")
	}
	windows["Additional"] = QuotaWindow{Utilization: 0, ResetsAt: now.Add(2 * time.Hour)}
	apply()
	auth, _ = m.GetByID(a)
	if !auth.ModelStates[quotaFable].NextRetryAfter.Equal(now.Add(time.Hour)) {
		t.Fatal("override cap of 99 did not retain remaining hold")
	}
}

func TestClaudeQuotaRankRebucketsAndPreservesFilePriority(t *testing.T) {
	m, a, b := newClaudeQuotaManager(t)
	now := time.Now()
	applyTestClaudeQuota(m, a, 20, now.Add(2*time.Hour), now)
	applyTestClaudeQuota(m, b, 20, now.Add(time.Hour), now)
	pick := func(want string) {
		t.Helper()
		got, errPick := m.scheduler.pickSingle(context.Background(), "claude", quotaFable, cliproxyexecutor.Options{}, nil)
		if errPick != nil || got == nil || got.ID != want {
			t.Fatalf("pick=%v err=%v want=%s", got, errPick, want)
		}
	}
	pick(b)
	applyTestClaudeQuota(m, b, 100, now.Add(time.Hour), now)
	pick(a)
	auth, _ := m.GetByID(a)
	if auth.Attributes["quota_rank"] != "1" || authPriority(auth) != 1 {
		t.Fatal("rank not folded into priority")
	}
	applyTestClaudeQuota(m, b, 0, now.Add(time.Hour), now)
	pick(b)
	auth, _ = m.GetByID(a)
	auth.Attributes["priority"] = "10"
	if _, errUpdate := m.Update(context.Background(), auth); errUpdate != nil {
		t.Fatal(errUpdate)
	}
	pick(a)
	auth, _ = m.GetByID(a)
	if auth.Attributes["priority"] != "10" {
		t.Fatal("file priority changed")
	}
	auth.Attributes["quota_rank"] = "1"
	auth.Attributes["priority"] = strconv.Itoa(math.MaxInt)
	if authPriority(auth) != math.MaxInt {
		t.Fatal("rank overflowed priority")
	}
}

func TestClaudeQuotaObservationIsRuntimeOnlyAndCloned(t *testing.T) {
	m, a, _ := newClaudeQuotaManager(t)
	now := time.Now()
	applyTestClaudeQuota(m, a, 100, now.Add(time.Hour), now)
	auth, _ := m.GetByID(a)
	auth.Quota.Windows["Fable"] = QuotaWindow{}
	current, _ := m.GetByID(a)
	if current.Quota.Windows["Fable"].Utilization != 100 {
		t.Fatal("usage snapshot aliases manager memory")
	}
	if _, ok := current.Metadata["quota_rank"]; ok {
		t.Fatal("rank stored in metadata")
	}
	m.mu.RLock()
	records := m.cooldownStateRecordsForAuthLocked(m.auths[a], now)
	m.mu.RUnlock()
	data, errMarshal := json.Marshal(records)
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	if strings.Contains(string(data), "windows") || strings.Contains(string(data), "quota_rank") || !strings.Contains(string(data), "quota_cap") {
		t.Fatalf("unexpected persisted cooldown: %s", data)
	}
}

func TestClaudeQuotaStalePollCannotMutateReplacementOrReload(t *testing.T) {
	for _, change := range []string{"token", "registration", "disabled", "configuration"} {
		t.Run(change, func(t *testing.T) {
			m, a, _ := newClaudeQuotaManager(t)
			base, _ := m.GetByID(a)
			cfg := m.runtimeConfig.Load().(*config.Config)
			next := base.Clone()
			switch change {
			case "token":
				next.Metadata["access_token"] = "new-access"
			case "disabled":
				next.Disabled = true
			case "registration":
				if _, errRegister := m.Register(context.Background(), next); errRegister != nil {
					t.Fatal(errRegister)
				}
			case "configuration":
				nextCfg := cfg.CloneForRuntime()
				nextCfg.Claude.Quota.Enabled = false
				m.SetConfig(nextCfg)
			}
			if change == "token" || change == "disabled" {
				if _, errUpdate := m.Update(context.Background(), next); errUpdate != nil {
					t.Fatal(errUpdate)
				}
			}
			now := time.Now()
			m.applyClaudeQuotaUsage(context.Background(), cfg, base, map[string]QuotaWindow{"Fable": {Utilization: 100, ResetsAt: now.Add(time.Hour)}}, now)
			current, _ := m.GetByID(a)
			if len(current.Quota.Windows) != 0 || len(CooldownSnapshotForAuth(current, now)) != 0 {
				t.Fatal("stale poll published")
			}
		})
	}
}

func TestClaudeQuotaBlockedCredentialDoesNotBlockConfigReload(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, a, _ := newClaudeQuotaManager(t)
		base, _ := m.GetByID(a)
		cfg := m.runtimeConfig.Load().(*config.Config)
		release := m.lockAuthMutation(a)
		done := make(chan struct{})
		go func() {
			defer close(done)
			m.applyClaudeQuotaUsage(context.Background(), cfg, base, map[string]QuotaWindow{}, time.Now())
		}()
		synctest.Wait()
		next := cfg.CloneForRuntime()
		next.Claude.Quota.Enabled = false
		m.SetConfig(next)
		release()
		<-done
	})
}

func TestClaudeQuotaEligibility(t *testing.T) {
	base := &Auth{Provider: "claude", Metadata: map[string]any{"access_token": "test-access"}}
	if !claudeQuotaEligible(base) {
		t.Fatal("valid OAuth seat excluded")
	}
	for name, edit := range map[string]func(*Auth){
		"disabled":        func(a *Auth) { a.Disabled = true },
		"disabled status": func(a *Auth) { a.Status = StatusDisabled },
		"unauthorized":    func(a *Auth) { a.LastError = &Error{HTTPStatus: 401} },
		"invalid grant":   func(a *Auth) { a.StatusMessage = "refresh token invalid" },
		"API key":         func(a *Auth) { a.Attributes = map[string]string{AttributeAPIKey: "test-key"} },
		"missing token":   func(a *Auth) { a.Metadata = nil },
		"other provider":  func(a *Auth) { a.Provider = "codex" },
	} {
		t.Run(name, func(t *testing.T) {
			auth := base.Clone()
			edit(auth)
			if claudeQuotaEligible(auth) {
				t.Fatal("ineligible seat selected")
			}
		})
	}
}

type claudeQuotaTestPoller struct {
	ProviderExecutor
	poll func(context.Context, *Auth) (map[string]QuotaWindow, error)
}

func (*claudeQuotaTestPoller) Identifier() string { return "claude" }
func (p *claudeQuotaTestPoller) PollQuota(ctx context.Context, auth *Auth) (map[string]QuotaWindow, error) {
	return p.poll(ctx, auth)
}

func TestClaudeQuotaDisabledStartsNoLoopAndReloadCancelsReads(t *testing.T) {
	m, a, b := newClaudeQuotaManager(t)
	cfg := m.runtimeConfig.Load().(*config.Config).CloneForRuntime()
	cfg.Claude.Quota.Enabled = false
	m.SetConfig(cfg)
	started := make(chan string, 2)
	cancelled := make(chan string, 2)
	m.RegisterExecutor(&claudeQuotaTestPoller{poll: func(ctx context.Context, auth *Auth) (map[string]QuotaWindow, error) {
		started <- auth.ID
		<-ctx.Done()
		cancelled <- auth.ID
		return nil, ctx.Err()
	}})
	m.StartQuotaPolling(context.Background())
	t.Cleanup(m.StopQuotaPolling)
	if m.quotaPollCancel != nil || m.quotaPollDone != nil {
		t.Fatal("disabled feature started a loop")
	}
	cfg.Claude.Quota.Enabled = true
	m.SetConfig(cfg)
	receive := func(ch <-chan string) string {
		t.Helper()
		select {
		case id := <-ch:
			return id
		case <-time.After(5 * time.Second):
			t.Fatal("poll synchronization timed out")
			return ""
		}
	}
	seen := map[string]bool{receive(started): true, receive(started): true}
	if !seen[a] || !seen[b] {
		t.Fatal("stalled seat prevented another seat from polling")
	}
	cfg.Claude.Quota.Enabled = false
	m.SetConfig(cfg)
	_ = receive(cancelled)
	_ = receive(cancelled)
	if m.quotaPollCancel != nil {
		t.Fatal("reload left poller running")
	}
}

func TestClaudeQuotaRankSurvivesThreeWayMerge(t *testing.T) {
	base := &Auth{Attributes: map[string]string{"quota_rank": "1", "priority": "5"}, Quota: QuotaState{Windows: map[string]QuotaWindow{"Fable": {Utilization: 12}}}}
	updated := base.Clone()
	delete(updated.Attributes, "quota_rank")
	updated.Attributes["priority"] = "6"
	updated.Quota.Windows = nil
	merged := MergeRefreshedAuth(base, base.Clone(), updated)
	if merged.Attributes["quota_rank"] != "1" || merged.Attributes["priority"] != "6" || !reflect.DeepEqual(merged.Quota.Windows, base.Quota.Windows) {
		t.Fatal("three-way merge lost runtime quota state")
	}
}

func TestClaudeQuotaUpdatePreservesRuntimeState(t *testing.T) {
	m, a, _ := newClaudeQuotaManager(t)
	now := time.Now()
	applyTestClaudeQuota(m, a, 20, now.Add(time.Hour), now)
	before, _ := m.GetByID(a)
	updated := before.Clone()
	updated.Attributes = map[string]string{"priority": "4"}
	updated.Quota = QuotaState{}
	if _, errUpdate := m.Update(context.Background(), updated); errUpdate != nil {
		t.Fatal(errUpdate)
	}
	after, _ := m.GetByID(a)
	if after.Attributes["quota_rank"] != "1" || !reflect.DeepEqual(after.Quota.Windows, before.Quota.Windows) || authPriority(after) != 5 {
		t.Fatal("replacement lost usage or rank")
	}
}

func TestClaudeQuotaPollFailureDoesNotDisable(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, a, b := newClaudeQuotaManager(t)
		seat, _ := m.GetByID(b)
		seat.Disabled = true
		if _, errUpdate := m.Update(context.Background(), seat); errUpdate != nil {
			t.Fatal(errUpdate)
		}
		called := make(chan string, 2)
		m.RegisterExecutor(&claudeQuotaTestPoller{poll: func(_ context.Context, auth *Auth) (map[string]QuotaWindow, error) {
			called <- auth.ID
			return nil, errors.New("failed")
		}})
		m.StartQuotaPolling(context.Background())
		t.Cleanup(m.StopQuotaPolling)
		synctest.Wait()
		if len(called) != 1 || <-called != a {
			t.Fatal("poller did not skip disabled seat")
		}
		current, _ := m.GetByID(a)
		if current.Disabled || current.Status == StatusDisabled || len(current.Quota.Windows) != 0 {
			t.Fatal("poll failure mutated seat")
		}
	})
}

func TestClaudeQuotaPollCadenceDoesNotOverlapStalledSeat(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, a, b := newClaudeQuotaManager(t)
		called := make(chan string, 8)
		m.RegisterExecutor(&claudeQuotaTestPoller{poll: func(ctx context.Context, auth *Auth) (map[string]QuotaWindow, error) {
			called <- auth.ID
			if auth.ID == a {
				<-ctx.Done()
				return nil, ctx.Err()
			}
			return map[string]QuotaWindow{}, nil
		}})
		m.StartQuotaPolling(context.Background())
		t.Cleanup(m.StopQuotaPolling)
		synctest.Wait()
		if len(called) != 2 {
			t.Fatal("initial poll did not visit both seats")
		}
		<-called
		<-called
		// synctest advances virtual time; no wall-clock sleep or network deadline.
		<-time.After(5 * time.Minute)
		synctest.Wait()
		if len(called) != 1 || <-called != b {
			t.Fatal("poll cadence overlapped a stalled seat or stalled a healthy one")
		}
	})
}
