package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
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
	m.applyTestClaudeQuotaUsage(context.Background(), m.runtimeConfig.Load().(*config.Config), auth, map[string]QuotaWindow{
		"Fable":     {Utilization: percent, ResetsAt: reset},
		"five_hour": {Utilization: 100, ResetsAt: reset},
		"seven_day": {Utilization: 100, ResetsAt: reset},
	}, now)
}

// These fixtures model an immediate poll, whose headers arrive at the supplied
// clock instant. Ordering tests supply their own receipt or deliberately omit it.
func (m *Manager) applyTestClaudeQuotaUsage(ctx context.Context, cfg *config.Config, auth *Auth, windows map[string]QuotaWindow, now time.Time) {
	observed := make(map[string]QuotaWindow, len(windows))
	for name, window := range windows {
		if window.ObservedAt.IsZero() {
			window.ObservedAt = now
		}
		observed[name] = window
	}
	m.applyClaudeQuotaUsage(ctx, cfg, auth, observed, now)
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
	for _, mode := range []string{"below cap", "manual reset", "disabled feature", "removed mapping", "disabled cooling", "disabled cooling and feature"} {
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
				case "disabled cooling", "disabled cooling and feature":
					cfg.DisableCooling = true
					if mode == "disabled cooling and feature" {
						cfg.Claude.Quota.Enabled = false
					}
				}
				m.SetConfig(cfg)
			}
			auth, _ := m.GetByID(a)
			if blocked, _, _ := isAuthBlockedForModel(auth, quotaFable, now); blocked {
				t.Fatalf("%s did not release hold", mode)
			}
			reg := registry.GetGlobalRegistry()
			if reg.IsModelSuspendedForClient(a, quotaFable) || reg.IsModelQuotaExceededForClient(a, quotaFable) || reg.GetModelCount(quotaFable) != 2 {
				t.Fatalf("%s did not restore registry routing", mode)
			}
		})
	}
}

func TestClaudeQuotaPartialPollAndReloadDoNotRestoreClearedHold(t *testing.T) {
	for _, mode := range []string{"manual reset", "successful request"} {
		for _, invalid := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/invalid=%v", mode, invalid), func(t *testing.T) {
				m, a, _ := newClaudeQuotaManager(t)
				cfg := m.runtimeConfig.Load().(*config.Config).CloneForRuntime()
				cfg.Claude.Quota.ModelWindows = map[string][]string{"Fable": {quotaFable}, "Additional": {quotaFable}}
				m.SetConfig(cfg)
				now := time.Now()
				applyTestClaudeQuota(m, a, 100, now.Add(time.Hour), now)
				if mode == "manual reset" {
					if _, _, errReset := m.ResetQuota(context.Background(), a); errReset != nil {
						t.Fatal(errReset)
					}
				} else {
					m.MarkResult(context.Background(), Result{AuthID: a, Provider: "claude", Model: quotaFable, Success: true})
				}
				windows := make(map[string]QuotaWindow)
				if invalid {
					windows["Fable"] = QuotaWindow{Utilization: 100}
				}
				poll := func() *Auth {
					base, _ := m.GetByID(a)
					m.applyTestClaudeQuotaUsage(context.Background(), m.runtimeConfig.Load().(*config.Config), base, windows, time.Now())
					auth, _ := m.GetByID(a)
					return auth
				}
				if blocked, _, _ := isAuthBlockedForModel(poll(), quotaFable, now); blocked {
					t.Fatal("partial poll recreated the cleared hold")
				}
				reload := func(interval string) *Auth {
					cfg := m.runtimeConfig.Load().(*config.Config).CloneForRuntime()
					cfg.Claude.Quota.PollInterval = interval
					m.SetConfig(cfg)
					auth, _ := m.GetByID(a)
					return auth
				}
				auth := reload("2m")
				if auth.Quota.Windows["Fable"].Utilization != 100 {
					t.Fatal("test lost the retained exhausted-window observation")
				}
				if blocked, _, _ := isAuthBlockedForModel(auth, quotaFable, now); blocked {
					t.Fatal("interval-only reload recreated the cleared hold from retained usage")
				}
				if registry.GetGlobalRegistry().IsModelSuspendedForClient(a, quotaFable) {
					t.Fatal("interval-only reload suspended the cleared model in the registry")
				}
				shortReset := now.Add(5 * time.Minute)
				windows["Additional"] = QuotaWindow{Utilization: 100, ResetsAt: shortReset}
				if blocked, _, until := isAuthBlockedForModel(poll(), quotaFable, now); !blocked || !until.Equal(shortReset) {
					t.Fatal("new window revived the old longer hold")
				}
				delete(windows, "Additional")
				if blocked, _, until := isAuthBlockedForModel(poll(), quotaFable, now); !blocked || !until.Equal(shortReset) {
					t.Fatal("omitted windows did not retain exactly the newly owned hold")
				}
				if blocked, _, until := isAuthBlockedForModel(reload("3m"), quotaFable, now); !blocked || !until.Equal(shortReset) {
					t.Fatal("reload did not retain exactly the newly owned hold")
				}
				windows["Fable"] = QuotaWindow{Utilization: 100, ResetsAt: now.Add(time.Hour)}
				if blocked, _, until := isAuthBlockedForModel(poll(), quotaFable, now); !blocked || !until.Equal(now.Add(time.Hour)) {
					t.Fatal("a fresh exhausted-window observation did not re-arm the hold")
				}
			})
		}
	}
}

func TestClaudeQuotaCapReloadRechecksOnlyOwnedHolds(t *testing.T) {
	for _, capSource := range []string{"cap-percent", "window-caps", "model-windows"} {
		t.Run(capSource, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				m, a, _ := newClaudeQuotaManager(t)
				cfg := m.runtimeConfig.Load().(*config.Config).CloneForRuntime()
				capPercent := float64(80)
				cfg.Claude.Quota.CapPercent = &capPercent
				if capSource == "window-caps" {
					*cfg.Claude.Quota.CapPercent = 100
					cfg.Claude.Quota.WindowCaps = map[string]float64{"Fable": 80}
				}
				m.SetConfig(cfg)
				applyTestClaudeQuota(m, a, 90, time.Now().Add(time.Hour), time.Now())
				auth, _ := m.GetByID(a)
				if !isClaudeQuotaHold(auth.ModelStates[quotaFable]) {
					t.Fatal("test did not install the hold")
				}
				m.RegisterExecutor(&claudeQuotaTestPoller{poll: func(context.Context, *Auth) (map[string]QuotaWindow, error) {
					return nil, errors.New("usage unavailable")
				}})
				m.StartQuotaPolling(context.Background())
				t.Cleanup(m.StopQuotaPolling)
				synctest.Wait()
				cfg = cfg.CloneForRuntime()
				switch capSource {
				case "cap-percent":
					*cfg.Claude.Quota.CapPercent = 100
				case "window-caps":
					cfg.Claude.Quota.WindowCaps["Fable"] = 100
				case "model-windows":
					cfg.Claude.Quota.ModelWindows = map[string][]string{"Fable": {quotaSonnet}}
				}
				m.SetConfig(cfg)
				synctest.Wait()
				auth, _ = m.GetByID(a)
				for _, model := range []string{quotaFable, quotaSonnet} {
					if blocked, _, _ := isAuthBlockedForModel(auth, model, time.Now()); blocked {
						t.Fatalf("reload kept or created a hold for %s", model)
					}
				}
				cfg = cfg.CloneForRuntime()
				*cfg.Claude.Quota.CapPercent = 70
				cfg.Claude.Quota.WindowCaps = nil
				cfg.Claude.Quota.ModelWindows = map[string][]string{"Fable": {quotaFable}}
				m.SetConfig(cfg)
				synctest.Wait()
				auth, _ = m.GetByID(a)
				if blocked, _, _ := isAuthBlockedForModel(auth, quotaFable, time.Now()); blocked {
					t.Fatal("lower cap re-armed a hold from retained usage")
				}
			})
		})
	}
}

func TestClaudeQuotaRestoreDoesNotRearmClearedHold(t *testing.T) {
	for _, mode := range []string{"manual reset", "successful request"} {
		t.Run(mode, func(t *testing.T) {
			m, a, b := newClaudeQuotaManager(t)
			now := time.Now()
			applyTestClaudeQuota(m, a, 100, now.Add(time.Hour), now)
			if mode == "manual reset" {
				if _, _, errReset := m.ResetQuota(context.Background(), a); errReset != nil {
					t.Fatal(errReset)
				}
			} else {
				m.MarkResult(context.Background(), Result{AuthID: a, Provider: "claude", Model: quotaFable, Success: true})
			}
			base, _ := m.GetByID(a)
			m.applyTestClaudeQuotaUsage(context.Background(), m.runtimeConfig.Load().(*config.Config), base, map[string]QuotaWindow{}, now)
			store := &recordingCooldownStateStore{load: []CooldownStateRecord{{
				AuthID: b, Model: quotaSonnet, NextRetryAfter: now.Add(time.Hour),
				Quota: QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: now.Add(time.Hour)},
			}}}
			m.SetCooldownStateStore(store)
			if errRestore := m.RestoreCooldownStates(context.Background()); errRestore != nil {
				t.Fatal(errRestore)
			}
			auth, _ := m.GetByID(a)
			if auth.Quota.Windows["Fable"].Utilization != 100 {
				t.Fatal("test lost retained exhausted usage")
			}
			if blocked, _, _ := isAuthBlockedForModel(auth, quotaFable, now); blocked {
				t.Fatal("restore re-armed the cleared hold from retained usage")
			}
			other, _ := m.GetByID(b)
			if blocked, _, _ := isAuthBlockedForModel(other, quotaSonnet, now); !blocked {
				t.Fatal("restore did not load the independent cooldown")
			}
		})
	}
}

func TestClaudeQuotaMissingWindowAndIndependentCooldown(t *testing.T) {
	m, a, _ := newClaudeQuotaManager(t)
	now := time.Now()
	applyTestClaudeQuota(m, a, 100, now.Add(time.Hour), now)
	auth, _ := m.GetByID(a)
	m.applyTestClaudeQuotaUsage(context.Background(), m.runtimeConfig.Load().(*config.Config), auth, map[string]QuotaWindow{}, now.Add(time.Second))
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

func TestClaudeQuotaPreservesUpstreamFailureOwnership(t *testing.T) {
	for _, status := range []int{404, 429, 503} {
		for _, wrapped := range []bool{false, true} {
			t.Run(strconv.Itoa(status)+"/wrapped="+strconv.FormatBool(wrapped), func(t *testing.T) {
				m, a, _ := newClaudeQuotaManager(t)
				now := time.Now()
				applyTestClaudeQuota(m, a, 100, now.Add(time.Hour), now)
				if wrapped {
					delay := 10 * time.Minute
					m.MarkResult(context.Background(), Result{AuthID: a, Provider: "claude", Model: quotaSonnet, RetryAfter: &delay, CredentialScope: true, Error: &Error{HTTPStatus: 429}})
				}
				delay := 2 * time.Hour
				var retryAfter *time.Duration
				if status != 404 {
					retryAfter = &delay
				}
				m.MarkResult(context.Background(), Result{AuthID: a, Provider: "claude", Model: quotaFable, RetryAfter: retryAfter, Error: &Error{HTTPStatus: status, Message: "upstream failure"}})
				before, _ := m.GetByID(a)
				until := before.ModelStates[quotaFable].NextRetryAfter
				if !until.After(now.Add(time.Hour)) {
					t.Fatal("upstream failure did not establish a longer cooldown")
				}
				for _, percent := range []float64{100, 0} {
					applyTestClaudeQuota(m, a, percent, now.Add(time.Hour), now.Add(time.Second))
					after, _ := m.GetByID(a)
					if blocked, _, next := isAuthBlockedForModel(after, quotaFable, now.Add(20*time.Minute)); !blocked || !next.Equal(until) {
						t.Fatalf("usage %v changed upstream cooldown: blocked=%v next=%v want=%v", percent, blocked, next, until)
					}
					if !reflect.DeepEqual(before.ModelStates[quotaFable], after.ModelStates[quotaFable]) {
						t.Fatal("poll mutated an upstream-owned model state")
					}
				}
			})
		}
	}
}

func TestClaudeQuotaFailureWithoutCooldownPreservesHold(t *testing.T) {
	for _, mode := range []string{"disabled transient cooling", "zero transient retry hint"} {
		t.Run(mode, func(t *testing.T) {
			m, a, _ := newClaudeQuotaManager(t)
			now := time.Now()
			reset := now.Add(time.Hour)
			applyTestClaudeQuota(m, a, 100, reset, now)
			result := Result{AuthID: a, Provider: "claude", Model: quotaFable, Error: &Error{HTTPStatus: 503, Message: "upstream failure"}}
			if mode == "disabled transient cooling" {
				previous := transientErrorCooldownSeconds.Load()
				transientErrorCooldownSeconds.Store(-1)
				t.Cleanup(func() { transientErrorCooldownSeconds.Store(previous) })
			} else {
				zero := time.Duration(0)
				result.RetryAfter = &zero
				result.Error = &Error{HTTPStatus: 429, Code: ErrorCodeTransientRateLimit}
			}
			m.MarkResult(context.Background(), result)
			auth, _ := m.GetByID(a)
			if blocked, _, until := isAuthBlockedForModel(auth, quotaFable, now); !blocked || !until.Equal(reset) || !isClaudeQuotaHold(auth.ModelStates[quotaFable]) {
				t.Fatal("failure without a new cooldown erased or took ownership of the guard")
			}
			applyTestClaudeQuota(m, a, 0, reset, now.Add(time.Second))
			auth, _ = m.GetByID(a)
			if blocked, _, _ := isAuthBlockedForModel(auth, quotaFable, now); blocked {
				t.Fatal("valid usage release could not clear the retained guard")
			}
		})
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
	applyTestClaudeQuota(m, a, 0, now.Add(time.Hour), now.Add(time.Second))
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
		m.applyTestClaudeQuotaUsage(context.Background(), m.runtimeConfig.Load().(*config.Config), base, windows, time.Now())
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
	applyTestClaudeQuota(m, b, 100, now.Add(time.Hour), now.Add(time.Second))
	pick(a)
	auth, _ := m.GetByID(a)
	if auth.Attributes["quota_rank"] != "1" || authPriority(auth) != 1 {
		t.Fatal("rank not folded into priority")
	}
	applyTestClaudeQuota(m, b, 0, now.Add(time.Hour), now.Add(2*time.Second))
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
	auth.Quota.RankExpiresAt = now.Add(time.Hour)
	auth.Attributes["priority"] = strconv.Itoa(math.MaxInt)
	if authPriority(auth) != math.MaxInt {
		t.Fatal("rank overflowed priority")
	}
}

func TestClaudeQuotaOverlappingWindowsPreserveUnknownLongerHold(t *testing.T) {
	for _, invalid := range []bool{false, true} {
		t.Run("invalid="+strconv.FormatBool(invalid), func(t *testing.T) {
			m, a, _ := newClaudeQuotaManager(t)
			cfg := m.runtimeConfig.Load().(*config.Config).CloneForRuntime()
			cfg.Claude.Quota.ModelWindows = map[string][]string{"Fable": {quotaFable}, "Additional": {quotaFable}}
			m.SetConfig(cfg)
			now := time.Now()
			short, long := now.Add(time.Hour), now.Add(2*time.Hour)
			apply := func(windows map[string]QuotaWindow, at time.Time) {
				base, _ := m.GetByID(a)
				m.applyTestClaudeQuotaUsage(context.Background(), m.runtimeConfig.Load().(*config.Config), base, windows, at)
			}
			apply(map[string]QuotaWindow{"Fable": {Utilization: 100, ResetsAt: short}, "Additional": {Utilization: 100, ResetsAt: long}}, now)
			windows := map[string]QuotaWindow{"Fable": {Utilization: 100, ResetsAt: short}}
			if invalid {
				windows["Additional"] = QuotaWindow{Utilization: 0}
			}
			apply(windows, now.Add(time.Minute))
			auth, _ := m.GetByID(a)
			if !auth.ModelStates[quotaFable].NextRetryAfter.Equal(long) || !auth.Quota.Windows["Additional"].ObservedAt.Equal(now) {
				t.Fatal("unknown longer window lost its hold or original observation time")
			}
			if blocked, _, _ := isAuthBlockedForModel(auth, quotaFable, long); blocked {
				t.Fatal("unknown longer window did not expire at its own reset")
			}
			apply(map[string]QuotaWindow{"Additional": {Utilization: 0, ResetsAt: long}}, now.Add(2*time.Minute))
			auth, _ = m.GetByID(a)
			if !auth.ModelStates[quotaFable].NextRetryAfter.Equal(short) {
				t.Fatal("valid release of the longer window did not restore the shorter hold")
			}
		})
	}
}

func TestClaudeQuotaRestorePreservesLiveWindowDeadlines(t *testing.T) {
	for _, wrapped := range []bool{false, true} {
		t.Run(strconv.FormatBool(wrapped), func(t *testing.T) {
			m, a, _ := newClaudeQuotaManager(t)
			cfg := m.runtimeConfig.Load().(*config.Config).CloneForRuntime()
			cfg.Claude.Quota.ModelWindows = map[string][]string{"Fable": {quotaFable}, "Additional": {quotaFable}}
			m.SetConfig(cfg)
			store := &recordingCooldownStateStore{}
			m.SetCooldownStateStore(store)
			now := time.Now()
			long, short := now.Add(2*time.Hour), now.Add(time.Hour)
			base, _ := m.GetByID(a)
			m.applyTestClaudeQuotaUsage(context.Background(), m.runtimeConfig.Load().(*config.Config), base, map[string]QuotaWindow{
				"Fable": {Utilization: 100, ResetsAt: long}, "Additional": {Utilization: 100, ResetsAt: short},
			}, now)
			if wrapped {
				delay := 3 * time.Hour
				m.MarkResult(context.Background(), Result{AuthID: a, Provider: "claude", Model: quotaSonnet, RetryAfter: &delay, CredentialScope: true, Error: &Error{HTTPStatus: 429}})
			}
			// A disk round trip omits runtime-only per-window provenance, including shadows.
			data, errMarshal := json.Marshal(store.savedRecords())
			if errMarshal != nil {
				t.Fatal(errMarshal)
			}
			var records []CooldownStateRecord
			if errUnmarshal := json.Unmarshal(data, &records); errUnmarshal != nil {
				t.Fatal(errUnmarshal)
			}
			m.SetCooldownStateStore(&recordingCooldownStateStore{load: records})
			if errRestore := m.RestoreCooldownStates(context.Background()); errRestore != nil {
				t.Fatal(errRestore)
			}
			base, _ = m.GetByID(a)
			m.applyTestClaudeQuotaUsage(context.Background(), m.runtimeConfig.Load().(*config.Config), base, map[string]QuotaWindow{
				"Fable": {Utilization: 0, ResetsAt: long},
			}, now.Add(time.Second))
			auth, _ := m.GetByID(a)
			owned, _ := claudeQuotaHoldLayer(auth, auth.ModelStates[quotaFable], now)
			if !isClaudeQuotaHold(owned) || !owned.NextRetryAfter.Equal(short) {
				t.Fatalf("restore extended the remaining window: %+v", owned)
			}
		})
	}
}

type delayedClaudeQuotaLoadStore struct {
	recordingCooldownStateStore
	loaded  chan struct{}
	release chan struct{}
}

func (s *delayedClaudeQuotaLoadStore) Load(ctx context.Context) ([]CooldownStateRecord, error) {
	records, errLoad := s.recordingCooldownStateStore.Load(ctx)
	close(s.loaded)
	select {
	case <-s.release:
		return records, errLoad
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func TestClaudeQuotaDelayedRestorePreservesNewerLiveWindowOwnership(t *testing.T) {
	for _, wrapped := range []bool{false, true} {
		for _, newerRecord := range []bool{false, true} {
			t.Run(fmt.Sprintf("wrapped=%t/newer_record=%t", wrapped, newerRecord), func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					m, a, _ := newClaudeQuotaManager(t)
					cfg := m.runtimeConfig.Load().(*config.Config).CloneForRuntime()
					cfg.Claude.Quota.ModelWindows = map[string][]string{"Fable": {quotaFable}, "Additional": {quotaFable}}
					m.SetConfig(cfg)
					store := &recordingCooldownStateStore{}
					m.SetCooldownStateStore(store)
					now := time.Now()
					long, short := now.Add(2*time.Hour), now.Add(time.Hour)
					base, _ := m.GetByID(a)
					m.applyTestClaudeQuotaUsage(context.Background(), m.runtimeConfig.Load().(*config.Config), base, map[string]QuotaWindow{
						"Fable": {Utilization: 100, ResetsAt: long}, "Additional": {Utilization: 100, ResetsAt: short},
					}, now)
					if wrapped {
						delay := 3 * time.Hour
						m.MarkResult(context.Background(), Result{AuthID: a, Provider: "claude", Model: quotaSonnet, RetryAfter: &delay, CredentialScope: true, Error: &Error{HTTPStatus: 429}})
					}
					// Persisted records have no runtime per-window ownership.
					data, errMarshal := json.Marshal(store.savedRecords())
					if errMarshal != nil {
						t.Fatal(errMarshal)
					}
					var records []CooldownStateRecord
					if errUnmarshal := json.Unmarshal(data, &records); errUnmarshal != nil {
						t.Fatal(errUnmarshal)
					}
					if newerRecord {
						// A genuinely newer saved state must retain the conservative
						// aggregate fallback instead of using older live ownership.
						for i := range records {
							records[i].UpdatedAt = now.Add(2 * time.Second)
							if records[i].IndependentState != nil {
								records[i].IndependentState.UpdatedAt = now.Add(2 * time.Second)
							}
						}
					}
					delayed := &delayedClaudeQuotaLoadStore{
						recordingCooldownStateStore: recordingCooldownStateStore{load: records},
						loaded:                      make(chan struct{}),
						release:                     make(chan struct{}),
					}
					m.SetCooldownStateStore(delayed)
					ctx, cancel := context.WithCancel(context.Background())
					defer cancel()
					done := make(chan error, 1)
					go func() { done <- m.RestoreCooldownStates(ctx) }()
					<-delayed.loaded
					<-time.After(time.Second)
					base, _ = m.GetByID(a)
					m.applyTestClaudeQuotaUsage(context.Background(), m.runtimeConfig.Load().(*config.Config), base, map[string]QuotaWindow{
						"Fable": {Utilization: 0, ResetsAt: long},
					}, time.Now())
					auth, _ := m.GetByID(a)
					owned, _ := claudeQuotaHoldLayer(auth, auth.ModelStates[quotaFable], time.Now())
					if !isClaudeQuotaHold(owned) || !owned.NextRetryAfter.Equal(short) {
						t.Fatalf("partial usage update did not shorten the live hold: %+v", owned)
					}
					close(delayed.release)
					if errRestore := <-done; errRestore != nil {
						t.Fatal(errRestore)
					}
					want := short
					if newerRecord {
						want = long
					}
					auth, _ = m.GetByID(a)
					owned, _ = claudeQuotaHoldLayer(auth, auth.ModelStates[quotaFable], time.Now())
					if !isClaudeQuotaHold(owned) || !owned.NextRetryAfter.Equal(want) || !owned.Quota.NextRecoverAt.Equal(want) {
						t.Fatalf("restore lost the newer hold deadline: got %+v, want %v", owned, want)
					}
					if _, exists := owned.Quota.guardWindows["Fable"]; exists || len(owned.Quota.guardWindows) != 1 || !owned.Quota.guardWindows["Additional"].Equal(want) {
						t.Fatalf("restore changed window ownership: %v", owned.Quota.guardWindows)
					}
					if blocked, _, _ := isAuthBlockedForModel(auth, quotaSonnet, time.Now()); blocked != wrapped {
						t.Fatal("restore changed independent credential cooldown ownership")
					}
					found := false
					for _, record := range delayed.savedRecords() {
						if record.AuthID != a || record.Model != quotaFable {
							continue
						}
						found = true
						deadline := record.NextRetryAfter
						if wrapped && record.IndependentState != nil {
							deadline = record.IndependentState.NextRetryAfter
						}
						if !deadline.Equal(want) {
							t.Fatalf("persisted deadline = %v, want %v", deadline, want)
						}
					}
					if !found {
						t.Fatal("remaining hold was not persisted")
					}
				})
			})
		}
	}
}

func TestClaudeQuotaDelayedRestoreDoesNotRearmClearedHold(t *testing.T) {
	for _, mode := range []string{"manual reset", "successful request"} {
		for _, saved := range []string{"older guard", "tied guard", "newer guard", "older upstream"} {
			t.Run(mode+"/"+saved, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					m, a, b := newClaudeQuotaManager(t)
					m.Remove(context.Background(), b)
					cfg := m.runtimeConfig.Load().(*config.Config).CloneForRuntime()
					cfg.Claude.Quota.ModelWindows = map[string][]string{"Fable": {quotaFable}, "Additional": {quotaFable}}
					m.SetConfig(cfg)
					store := &recordingCooldownStateStore{}
					m.SetCooldownStateStore(store)
					now := time.Now()
					long, short := now.Add(2*time.Hour), now.Add(time.Hour)
					base, _ := m.GetByID(a)
					m.applyTestClaudeQuotaUsage(context.Background(), m.runtimeConfig.Load().(*config.Config), base, map[string]QuotaWindow{
						"Fable": {Utilization: 100, ResetsAt: long}, "Additional": {Utilization: 100, ResetsAt: short},
					}, now)
					data, errMarshal := json.Marshal(store.savedRecords())
					if errMarshal != nil {
						t.Fatal(errMarshal)
					}
					var records []CooldownStateRecord
					if errUnmarshal := json.Unmarshal(data, &records); errUnmarshal != nil {
						t.Fatal(errUnmarshal)
					}
					if len(records) != 1 || records[0].Model != quotaFable || !records[0].NextRetryAfter.Equal(long) {
						t.Fatalf("expected the saved two-window hold, got %+v", records)
					}
					switch saved {
					case "tied guard":
						records[0].UpdatedAt = now.Add(time.Second)
					case "newer guard":
						records[0].UpdatedAt = now.Add(2 * time.Second)
					case "older upstream":
						records[0].Quota.Reason = "quota"
						records[0].LastError = &Error{HTTPStatus: 429}
					}
					delayed := &delayedClaudeQuotaLoadStore{
						recordingCooldownStateStore: recordingCooldownStateStore{load: records},
						loaded:                      make(chan struct{}),
						release:                     make(chan struct{}),
					}
					m.SetCooldownStateStore(delayed)
					ctx, cancel := context.WithCancel(context.Background())
					defer cancel()
					done := make(chan error, 1)
					go func() { done <- m.RestoreCooldownStates(ctx) }()
					<-delayed.loaded
					<-time.After(time.Second)
					if mode == "manual reset" {
						if _, _, errReset := m.ResetQuota(context.Background(), a); errReset != nil {
							t.Fatal(errReset)
						}
					} else {
						m.MarkResult(context.Background(), Result{AuthID: a, Provider: "claude", Model: quotaFable, Success: true})
					}
					auth, _ := m.GetByID(a)
					if isModelStateActiveCooldown(auth.ModelStates[quotaFable], time.Now()) || len(delayed.savedRecords()) != 0 {
						t.Fatal("clear did not remove the hold and its persisted record")
					}
					close(delayed.release)
					if errRestore := <-done; errRestore != nil {
						t.Fatal(errRestore)
					}
					auth, _ = m.GetByID(a)
					wantBlocked := saved == "newer guard" || saved == "older upstream"
					if blocked, _, _ := isAuthBlockedForModel(auth, quotaFable, time.Now()); blocked != wantBlocked {
						t.Fatalf("restored blocked=%t, want %t: %+v", blocked, wantBlocked, auth.ModelStates[quotaFable])
					}
					if wantBlocked {
						found := false
						for _, record := range delayed.savedRecords() {
							if record.AuthID == a && record.Model == quotaFable && record.NextRetryAfter.Equal(long) {
								found = true
							}
						}
						if !auth.ModelStates[quotaFable].NextRetryAfter.Equal(long) || !found {
							t.Fatalf("restore lost a newer guard or upstream-owned cooldown: state=%+v records=%+v", auth.ModelStates[quotaFable], delayed.savedRecords())
						}
						return
					}
					if isClaudeQuotaHold(auth.ModelStates[quotaFable]) || len(delayed.savedRecords()) != 0 {
						t.Fatal("restore re-persisted the cleared quota_cap hold")
					}
					picked, errPick := m.scheduler.pickSingle(context.Background(), "claude", quotaFable, cliproxyexecutor.Options{}, nil)
					if errPick != nil || picked == nil || picked.ID != a {
						t.Fatalf("scheduler did not retain the clear: picked=%v err=%v", picked, errPick)
					}
					// Retained exhaustion cannot re-arm the hold on an omitted window.
					m.applyTestClaudeQuotaUsage(context.Background(), m.runtimeConfig.Load().(*config.Config), auth, map[string]QuotaWindow{}, time.Now())
					auth, _ = m.GetByID(a)
					if blocked, _, _ := isAuthBlockedForModel(auth, quotaFable, time.Now()); blocked || len(delayed.savedRecords()) != 0 {
						t.Fatal("partial poll re-armed a cleared hold")
					}
					// A fresh observation can install a new hold at its own reset.
					<-time.After(time.Second)
					m.applyTestClaudeQuotaUsage(context.Background(), m.runtimeConfig.Load().(*config.Config), auth, map[string]QuotaWindow{
						"Additional": {Utilization: 100, ResetsAt: short},
					}, time.Now())
					auth, _ = m.GetByID(a)
					if !isClaudeQuotaHold(auth.ModelStates[quotaFable]) || !auth.ModelStates[quotaFable].NextRetryAfter.Equal(short) {
						t.Fatal("fresh poll did not establish its own hold deadline")
					}
				})
			})
		}
	}
}

func TestClaudeQuotaDelayedRestoreDoesNotRearmClearedWrappedHold(t *testing.T) {
	for _, mode := range []string{"manual reset", "successful request"} {
		for _, saved := range []string{"older guard", "tied guard", "newer guard", "older upstream"} {
			t.Run(mode+"/"+saved, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					m, a, _ := newClaudeQuotaManager(t)
					cfg := m.runtimeConfig.Load().(*config.Config).CloneForRuntime()
					cfg.Claude.Quota.ModelWindows = map[string][]string{"Fable": {quotaFable}, "Additional": {quotaFable}}
					m.SetConfig(cfg)
					store := &recordingCooldownStateStore{}
					m.SetCooldownStateStore(store)
					now := time.Now()
					long, short := now.Add(2*time.Hour), now.Add(time.Hour)
					base, _ := m.GetByID(a)
					windows := map[string]QuotaWindow{
						"Fable": {Utilization: 100, ResetsAt: long}, "Additional": {Utilization: 100, ResetsAt: short},
					}
					m.applyTestClaudeQuotaUsage(context.Background(), m.runtimeConfig.Load().(*config.Config), base, windows, now)
					delay := 90 * time.Minute
					m.MarkResult(context.Background(), Result{AuthID: a, Provider: "claude", Model: quotaSonnet, RetryAfter: &delay, CredentialScope: true, Error: &Error{HTTPStatus: 429}})
					data, errMarshal := json.Marshal(store.savedRecords())
					if errMarshal != nil {
						t.Fatal(errMarshal)
					}
					var records []CooldownStateRecord
					if errUnmarshal := json.Unmarshal(data, &records); errUnmarshal != nil {
						t.Fatal(errUnmarshal)
					}
					found := false
					for i := range records {
						if records[i].Model != quotaFable {
							continue
						}
						found = true
						shadow := records[i].IndependentState
						if records[i].Quota.Reason != "credential_quota" || !isClaudeQuotaHold(shadow) || !shadow.NextRetryAfter.Equal(long) {
							t.Fatalf("expected a wrapped two-window guard hold: %+v", records[i])
						}
						switch saved {
						case "tied guard":
							shadow.UpdatedAt = now.Add(time.Second)
						case "newer guard":
							shadow.UpdatedAt = now.Add(2 * time.Second)
						case "older upstream":
							shadow.Quota.Reason = "quota"
							shadow.LastError = &Error{HTTPStatus: 429}
						}
					}
					if !found {
						t.Fatal("wrapped guard record was not persisted")
					}
					if mode == "successful request" {
						// Success intentionally retains an active credential wrap. Lift
						// that wrap and establish a live independent hold before Load.
						if _, _, errReset := m.ResetQuota(context.Background(), a); errReset != nil {
							t.Fatal(errReset)
						}
						base, _ = m.GetByID(a)
						m.applyTestClaudeQuotaUsage(context.Background(), m.runtimeConfig.Load().(*config.Config), base, windows, now)
					}
					delayed := &delayedClaudeQuotaLoadStore{
						recordingCooldownStateStore: recordingCooldownStateStore{load: records},
						loaded:                      make(chan struct{}),
						release:                     make(chan struct{}),
					}
					m.SetCooldownStateStore(delayed)
					ctx, cancel := context.WithCancel(context.Background())
					defer cancel()
					done := make(chan error, 1)
					go func() { done <- m.RestoreCooldownStates(ctx) }()
					<-delayed.loaded
					<-time.After(time.Second)
					if mode == "manual reset" {
						if _, _, errReset := m.ResetQuota(context.Background(), a); errReset != nil {
							t.Fatal(errReset)
						}
					} else {
						m.MarkResult(context.Background(), Result{AuthID: a, Provider: "claude", Model: quotaFable, Success: true})
					}
					auth, _ := m.GetByID(a)
					if isModelStateActiveCooldown(auth.ModelStates[quotaFable], time.Now()) || len(delayed.savedRecords()) != 0 {
						t.Fatal("clear did not remove the live hold and persisted records")
					}
					close(delayed.release)
					if errRestore := <-done; errRestore != nil {
						t.Fatal(errRestore)
					}
					auth, _ = m.GetByID(a)
					state := auth.ModelStates[quotaFable]
					wantShadow := saved == "newer guard" || saved == "older upstream"
					if (state.credentialQuotaShadow != nil) != wantShadow {
						t.Fatalf("restored shadow=%+v, want present=%t", state.credentialQuotaShadow, wantShadow)
					}
					if auth.Quota.Reason != "credential_quota" || !auth.Quota.NextRecoverAt.Equal(now.Add(delay)) || state.Quota.Reason != "credential_quota" {
						t.Fatal("restore lost the upstream-owned credential cooldown")
					}
					wantDeadline := now.Add(delay)
					if wantShadow {
						wantDeadline = long
					}
					if !state.NextRetryAfter.Equal(wantDeadline) || !state.Quota.NextRecoverAt.Equal(wantDeadline) {
						t.Errorf("restored model deadlines retry=%v recover=%v, want %v", state.NextRetryAfter, state.Quota.NextRecoverAt, wantDeadline)
					}
					found = false
					for _, record := range delayed.savedRecords() {
						if record.AuthID == a && record.Model == quotaFable {
							found = true
							if (record.IndependentState != nil) != wantShadow {
								t.Fatalf("persisted independent state=%+v, want present=%t", record.IndependentState, wantShadow)
							}
							if !record.NextRetryAfter.Equal(wantDeadline) || !record.Quota.NextRecoverAt.Equal(wantDeadline) {
								t.Errorf("persisted model retained a rejected deadline: %+v", record)
							}
						}
					}
					if !found {
						t.Fatal("restore did not persist the outer upstream cooldown")
					}
					if wantShadow {
						if !state.credentialQuotaShadow.NextRetryAfter.Equal(long) {
							t.Fatal("restore changed the newer guard or upstream layer deadline")
						}
						return
					}
					// Route through the real selector on both sides of the wrap's
					// expiry, before a poll could mask a stale aggregate deadline.
					selector := &FillFirstSelector{}
					<-time.After(time.Until(now.Add(delay - time.Second)))
					if picked, errPick := selector.Pick(context.Background(), "claude", quotaFable, cliproxyexecutor.Options{}, []*Auth{auth}); errPick == nil || picked != nil {
						t.Fatal("selector ignored the surviving credential cooldown")
					}
					<-time.After(2 * time.Second)
					if picked, errPick := selector.Pick(context.Background(), "claude", quotaFable, cliproxyexecutor.Options{}, []*Auth{auth}); errPick != nil || picked == nil || picked.ID != a {
						t.Errorf("selector still blocked after the credential cooldown expired: auth=%v err=%v", picked, errPick)
					}
					// An omitted-window poll must not promote or re-persist the
					// rejected two-hour guard layer after the wrap expires.
					m.applyTestClaudeQuotaUsage(context.Background(), m.runtimeConfig.Load().(*config.Config), auth, map[string]QuotaWindow{}, time.Now())
					auth, _ = m.GetByID(a)
					if isClaudeQuotaHold(auth.ModelStates[quotaFable]) || auth.ModelStates[quotaFable].credentialQuotaShadow != nil {
						t.Fatal("expired wrap promoted the cleared guard hold")
					}
					m.persistCooldownStates(context.Background())
					if len(delayed.savedRecords()) != 0 {
						t.Fatalf("expired wrap re-persisted a cooldown: %+v", delayed.savedRecords())
					}
					if picked, errPick := selector.Pick(context.Background(), "claude", quotaFable, cliproxyexecutor.Options{}, []*Auth{auth}); errPick != nil || picked == nil || picked.ID != a {
						t.Fatalf("poll re-armed a cleared cooldown: auth=%v err=%v", picked, errPick)
					}
				})
			})
		}
	}
}

func TestClaudeQuotaDelayedRestoreRejectsClearedGuardUnderLiveUpstreamCooldown(t *testing.T) {
	for _, mode := range []string{"manual reset", "successful request"} {
		for _, wrappedRecord := range []bool{false, true} {
			for _, credentialScope := range []bool{false, true} {
				for _, tied := range []bool{false, true} {
					t.Run(fmt.Sprintf("%s/wrapped=%t/credential_scope=%t/tied=%t", mode, wrappedRecord, credentialScope, tied), func(t *testing.T) {
						synctest.Test(t, func(t *testing.T) {
							m, a, _ := newClaudeQuotaManager(t)
							cfg := m.runtimeConfig.Load().(*config.Config).CloneForRuntime()
							cfg.Claude.Quota.ModelWindows = map[string][]string{"Fable": {quotaFable}, "Additional": {quotaFable}}
							m.SetConfig(cfg)
							cfg = m.runtimeConfig.Load().(*config.Config)
							store := &recordingCooldownStateStore{}
							m.SetCooldownStateStore(store)
							now := time.Now()
							windows := map[string]QuotaWindow{
								"Fable":      {Utilization: 100, ResetsAt: now.Add(2 * time.Hour)},
								"Additional": {Utilization: 100, ResetsAt: now.Add(time.Hour)},
							}
							base, _ := m.GetByID(a)
							m.applyTestClaudeQuotaUsage(context.Background(), cfg, base, windows, now)
							auth, _ := m.GetByID(a)
							if !isClaudeQuotaHold(auth.ModelStates[quotaFable]) || len(store.savedRecords()) == 0 {
								t.Fatal("fixture did not install and persist the guard hold")
							}
							savedWrapDeadline := now.Add(90 * time.Minute)
							if wrappedRecord {
								delay := 90 * time.Minute
								m.MarkResult(context.Background(), Result{AuthID: a, Provider: "claude", Model: quotaSonnet, RetryAfter: &delay, CredentialScope: true, Error: &Error{HTTPStatus: 429}})
							}
							data, errMarshal := json.Marshal(store.savedRecords())
							if errMarshal != nil {
								t.Fatal(errMarshal)
							}
							var records []CooldownStateRecord
							if errUnmarshal := json.Unmarshal(data, &records); errUnmarshal != nil {
								t.Fatal(errUnmarshal)
							}
							tiedAt := now.Add(2 * time.Second)
							if credentialScope {
								// Sibling propagation preserves the clear's state timestamp.
								tiedAt = now.Add(time.Second)
							}
							for i := range records {
								if records[i].Model == quotaFable && tied {
									if wrappedRecord {
										records[i].IndependentState.UpdatedAt = tiedAt
									} else {
										records[i].UpdatedAt = tiedAt
									}
								}
							}
							if wrappedRecord && mode == "successful request" {
								// Success retains a credential wrap, so lift it before Load
								// and let the in-flight success clear an independent hold.
								if _, _, errReset := m.ResetQuota(context.Background(), a); errReset != nil {
									t.Fatal(errReset)
								}
								base, _ = m.GetByID(a)
								m.applyTestClaudeQuotaUsage(context.Background(), cfg, base, windows, now)
							}
							delayed := &delayedClaudeQuotaLoadStore{
								recordingCooldownStateStore: recordingCooldownStateStore{load: records},
								loaded:                      make(chan struct{}),
								release:                     make(chan struct{}),
							}
							m.SetCooldownStateStore(delayed)
							ctx, cancel := context.WithCancel(context.Background())
							defer cancel()
							done := make(chan error, 1)
							go func() { done <- m.RestoreCooldownStates(ctx) }()
							<-delayed.loaded
							<-time.After(time.Second)
							if mode == "manual reset" {
								if _, _, errReset := m.ResetQuota(context.Background(), a); errReset != nil {
									t.Fatal(errReset)
								}
							} else {
								m.MarkResult(context.Background(), Result{AuthID: a, Provider: "claude", Model: quotaFable, Success: true})
							}
							auth, _ = m.GetByID(a)
							if isModelStateActiveCooldown(auth.ModelStates[quotaFable], time.Now()) || len(delayed.savedRecords()) != 0 {
								t.Fatal("clear did not remove the live hold and persisted records")
							}
							<-time.After(time.Second)
							delay := 30 * time.Minute
							liveDeadline := time.Now().Add(delay)
							model := quotaFable
							if credentialScope {
								model = quotaSonnet
							}
							m.MarkResult(context.Background(), Result{AuthID: a, Provider: "claude", Model: model, RetryAfter: &delay, CredentialScope: credentialScope, Error: &Error{HTTPStatus: 429}})
							auth, _ = m.GetByID(a)
							if state := auth.ModelStates[quotaFable]; state == nil || !state.UpdatedAt.Equal(tiedAt) || !state.NextRetryAfter.Equal(liveDeadline) {
								t.Fatalf("fixture did not establish the live upstream state: %+v", state)
							}
							close(delayed.release)
							if errRestore := <-done; errRestore != nil {
								t.Fatal(errRestore)
							}
							auth, _ = m.GetByID(a)
							state := auth.ModelStates[quotaFable]
							if isClaudeQuotaHold(state) || isClaudeQuotaHold(state.credentialQuotaShadow) {
								t.Error("restore re-installed the cleared guard beneath an upstream cooldown")
							}
							if credentialScope && state.credentialQuotaShadow != nil {
								t.Error("propagated live cooldown acquired an independent shadow")
							}
							wantDeadline := liveDeadline
							if wrappedRecord && !credentialScope {
								wantDeadline = savedWrapDeadline
							}
							if !state.NextRetryAfter.Equal(wantDeadline) || !state.Quota.NextRecoverAt.Equal(wantDeadline) {
								t.Errorf("model deadlines retry=%v recover=%v, want upstream deadline %v", state.NextRetryAfter, state.Quota.NextRecoverAt, wantDeadline)
							}
							for _, record := range delayed.savedRecords() {
								if record.AuthID != a || record.Model != quotaFable {
									continue
								}
								if record.Quota.Reason == "quota_cap" || isClaudeQuotaHold(record.IndependentState) || (credentialScope && record.IndependentState != nil) {
									t.Error("restore re-persisted the rejected guard layer")
								}
								if !record.NextRetryAfter.Equal(wantDeadline) || !record.Quota.NextRecoverAt.Equal(wantDeadline) {
									t.Errorf("persisted model retained the guard's aggregate deadline: %+v", record)
								}
							}
							selectableAt := liveDeadline
							if wrappedRecord {
								// Auth-level restore still replaces the live 30-minute wrap
								// with the saved 90-minute wrap; that upstream rule is unchanged.
								if !auth.Quota.NextRecoverAt.Equal(savedWrapDeadline) {
									t.Fatal("auth-level restore behavior changed")
								}
								selectableAt = savedWrapDeadline
							}
							selector := &FillFirstSelector{}
							<-time.After(time.Until(selectableAt.Add(-time.Second)))
							if picked, errPick := selector.Pick(context.Background(), "claude", quotaFable, cliproxyexecutor.Options{}, []*Auth{auth}); errPick == nil || picked != nil {
								t.Error("selector ignored the surviving upstream cooldown")
							}
							<-time.After(2 * time.Second)
							if picked, errPick := selector.Pick(context.Background(), "claude", quotaFable, cliproxyexecutor.Options{}, []*Auth{auth}); errPick != nil || picked == nil || picked.ID != a {
								t.Errorf("selector still blocked after upstream expiry: auth=%v err=%v", picked, errPick)
							}
							m.applyTestClaudeQuotaUsage(context.Background(), cfg, auth, map[string]QuotaWindow{}, time.Now())
							auth, _ = m.GetByID(a)
							if isClaudeQuotaHold(auth.ModelStates[quotaFable]) || isClaudeQuotaHold(auth.ModelStates[quotaFable].credentialQuotaShadow) {
								t.Error("expired upstream cooldown promoted the rejected guard")
							}
							m.persistCooldownStates(context.Background())
							if len(delayed.savedRecords()) != 0 {
								t.Errorf("expired upstream cooldown re-persisted: %+v", delayed.savedRecords())
							}
							if picked, errPick := selector.Pick(context.Background(), "claude", quotaFable, cliproxyexecutor.Options{}, []*Auth{auth}); errPick != nil || picked == nil || picked.ID != a {
								t.Errorf("empty poll re-armed the cleared guard: auth=%v err=%v", picked, errPick)
							}
						})
					})
				}
			}
		}
	}
}

func TestClaudeQuotaRejectedWrappedHoldRequiresSurvivingCredentialCooldown(t *testing.T) {
	for _, saved := range []string{"missing credential", "expired credential"} {
		t.Run(saved, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				m, a, _ := newClaudeQuotaManager(t)
				store := &recordingCooldownStateStore{}
				m.SetCooldownStateStore(store)
				now := time.Now()
				applyTestClaudeQuota(m, a, 100, now.Add(2*time.Hour), now)
				delay := 90 * time.Minute
				m.MarkResult(context.Background(), Result{AuthID: a, Provider: "claude", Model: quotaSonnet, RetryAfter: &delay, CredentialScope: true, Error: &Error{HTTPStatus: 429}})
				var records []CooldownStateRecord
				for _, record := range store.savedRecords() {
					if record.Model == quotaFable {
						records = append(records, record)
					} else if record.Model == "" && saved == "expired credential" {
						record.NextRetryAfter = now
						record.Quota.NextRecoverAt = now
						records = append(records, record)
					}
				}
				<-time.After(time.Second)
				if _, _, errReset := m.ResetQuota(context.Background(), a); errReset != nil {
					t.Fatal(errReset)
				}
				store.load = records
				if errRestore := m.RestoreCooldownStates(context.Background()); errRestore != nil {
					t.Fatal(errRestore)
				}
				auth, _ := m.GetByID(a)
				selector := &FillFirstSelector{}
				if picked, errPick := selector.Pick(context.Background(), "claude", quotaFable, cliproxyexecutor.Options{}, []*Auth{auth}); errPick != nil || picked == nil || picked.ID != a {
					t.Fatalf("rejected guard aggregate restored without a surviving credential deadline: auth=%v err=%v", picked, errPick)
				}
				if len(store.savedRecords()) != 0 || auth.ModelStates[quotaFable].credentialQuotaShadow != nil {
					t.Fatal("restore persisted the rejected wrapped guard")
				}
			})
		})
	}
}

func TestClaudeQuotaRestoredHoldPreservesUnknownLongerWindow(t *testing.T) {
	m, a, _ := newClaudeQuotaManager(t)
	cfg := m.runtimeConfig.Load().(*config.Config).CloneForRuntime()
	cfg.Claude.Quota.ModelWindows = map[string][]string{"Fable": {quotaFable}, "Additional": {quotaFable}}
	m.SetConfig(cfg)
	store := &recordingCooldownStateStore{}
	m.SetCooldownStateStore(store)
	now := time.Now()
	long := now.Add(2 * time.Hour)
	applyTestClaudeQuota(m, a, 100, long, now)
	restored := NewManager(nil, nil, nil)
	restored.SetConfig(cfg)
	if _, errRegister := restored.Register(context.Background(), &Auth{ID: a, Provider: "claude", Status: StatusActive, Metadata: map[string]any{"access_token": "test-access"}}); errRegister != nil {
		t.Fatal(errRegister)
	}
	restored.SetCooldownStateStore(&recordingCooldownStateStore{load: store.savedRecords()})
	if errRestore := restored.RestoreCooldownStates(context.Background()); errRestore != nil {
		t.Fatal(errRestore)
	}
	base, _ := restored.GetByID(a)
	if len(base.Quota.Windows) != 0 {
		t.Fatal("restored runtime observations")
	}
	reloaded := restored.runtimeConfig.Load().(*config.Config).CloneForRuntime()
	reloaded.Claude.Quota.PollInterval = "2m"
	restored.SetConfig(reloaded)
	base, _ = restored.GetByID(a)
	if !base.ModelStates[quotaFable].NextRetryAfter.Equal(long) {
		t.Fatal("interval-only reload lost the restored hold")
	}
	restored.applyTestClaudeQuotaUsage(context.Background(), restored.runtimeConfig.Load().(*config.Config), base, map[string]QuotaWindow{
		"Additional": {Utilization: 100, ResetsAt: now.Add(time.Hour)},
	}, now.Add(time.Second))
	after, _ := restored.GetByID(a)
	if !after.ModelStates[quotaFable].NextRetryAfter.Equal(long) {
		t.Fatal("partial observation shortened a restored hold")
	}
}

func TestClaudeQuotaExpiryPublishesWhilePollsFail(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, a, b := newClaudeQuotaManager(t)
		m.Remove(context.Background(), b)
		reg := registry.GetGlobalRegistry()
		reg.UnregisterClient(b)
		reg.RegisterClient(a, "claude", []*registry.ModelInfo{{ID: quotaFable}})
		cfg := m.runtimeConfig.Load().(*config.Config).CloneForRuntime()
		cfg.Claude.Quota.PollInterval = "15s"
		m.SetConfig(cfg)
		store := &recordingCooldownStateStore{}
		m.SetCooldownStateStore(store)
		now := time.Now()
		reset := now.Add(45 * time.Second)
		applyTestClaudeQuota(m, a, 100, reset, now)
		if reg.GetModelCount(quotaFable) != 0 {
			t.Fatal("live hold did not suspend routing")
		}
		polls := 0
		m.RegisterExecutor(&claudeQuotaTestPoller{poll: func(context.Context, *Auth) (map[string]QuotaWindow, error) {
			polls++
			return nil, errors.New("usage unavailable")
		}})
		m.StartQuotaPolling(context.Background())
		t.Cleanup(m.StopQuotaPolling)
		synctest.Wait()
		<-time.After(45 * time.Second)
		synctest.Wait()
		if polls != 1 {
			t.Fatal("failed usage read bypassed the shared minute budget")
		}
		auth, _ := m.GetByID(a)
		if blocked, _, _ := isAuthBlockedForModel(auth, quotaFable, time.Now()); blocked {
			t.Fatal("hold remained active at reset")
		}
		if reg.IsModelSuspendedForClient(a, quotaFable) || reg.IsModelQuotaExceededForClient(a, quotaFable) || reg.GetModelCount(quotaFable) != 1 {
			t.Fatal("expiry did not clear the registry projection independently of polling")
		}
		if got := util.ResolveAutoModel("auto"); got != quotaFable {
			t.Fatalf("automatic model routing after reset resolved %q", got)
		}
		if len(store.savedRecords()) != 0 {
			t.Fatal("expired guard remained in cooldown persistence")
		}
	})
}

func TestClaudeQuotaExpiryDoesNotRestoreClearedSibling(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, a, _ := newClaudeQuotaManager(t)
		cfg := m.runtimeConfig.Load().(*config.Config).CloneForRuntime()
		cfg.Claude.Quota.ModelWindows = map[string][]string{"Fable": {quotaFable}, "Additional": {quotaSonnet}}
		m.SetConfig(cfg)
		now := time.Now()
		base, _ := m.GetByID(a)
		m.applyTestClaudeQuotaUsage(context.Background(), m.runtimeConfig.Load().(*config.Config), base, map[string]QuotaWindow{
			"Fable": {Utilization: 100, ResetsAt: now.Add(time.Minute)}, "Additional": {Utilization: 100, ResetsAt: now.Add(time.Hour)},
		}, now)
		m.MarkResult(context.Background(), Result{AuthID: a, Provider: "claude", Model: quotaSonnet, Success: true})
		m.RegisterExecutor(&claudeQuotaTestPoller{poll: func(context.Context, *Auth) (map[string]QuotaWindow, error) {
			return nil, errors.New("usage unavailable")
		}})
		m.StartQuotaPolling(context.Background())
		t.Cleanup(m.StopQuotaPolling)
		synctest.Wait()
		<-time.After(time.Minute)
		synctest.Wait()
		auth, _ := m.GetByID(a)
		for _, model := range []string{quotaFable, quotaSonnet} {
			if blocked, _, _ := isAuthBlockedForModel(auth, model, time.Now()); blocked {
				t.Fatalf("expiry retained or restored %s", model)
			}
		}
	})
}

func TestClaudeQuotaPublicationRejectsReplacedRegistration(t *testing.T) {
	m, a, _ := newClaudeQuotaManager(t)
	now := time.Now()
	for i := 0; i < 5; i++ {
		applyTestClaudeQuota(m, a, 100, now.Add(time.Hour), now)
	}
	stale, _ := m.GetByID(a)
	m.Remove(context.Background(), a)
	reg := registry.GetGlobalRegistry()
	reg.UnregisterClient(a)
	if _, errRegister := m.Register(context.Background(), &Auth{ID: a, Provider: "claude", Status: StatusActive, Metadata: map[string]any{"access_token": "replacement-access"}}); errRegister != nil {
		t.Fatal(errRegister)
	}
	reg.RegisterClient(a, "claude", []*registry.ModelInfo{{ID: quotaFable}, {ID: quotaSonnet}})
	fresh, _ := m.GetByID(a)
	if fresh.RegistrationEpoch <= stale.RegistrationEpoch || fresh.Generation >= stale.Generation {
		t.Fatal("test did not replace the registration and reset its generation")
	}
	// Deterministically deliver the delayed old publication after replacement.
	m.publishClaudeQuotaSnapshots([]*Auth{stale})
	if reg.IsModelSuspendedForClient(a, quotaFable) || reg.IsModelQuotaExceededForClient(a, quotaFable) {
		t.Fatal("old hold was published onto the replacement credential")
	}
	m.MarkResult(context.Background(), Result{AuthID: a, Provider: "claude", Model: quotaFable, Error: &Error{HTTPStatus: 404, Message: "replacement failure"}})
	if !reg.IsModelSuspendedForClient(a, quotaFable) {
		t.Fatal("old generation poisoned subsequent fresh registry publications")
	}
}

func TestClaudeQuotaPublicationUsesLatestSameRegistration(t *testing.T) {
	m, a, _ := newClaudeQuotaManager(t)
	now := time.Now()
	applyTestClaudeQuota(m, a, 100, now.Add(time.Hour), now)
	// Pause an expiry between its manager mutation and registry publication.
	m.mu.Lock()
	current := m.auths[a]
	resetModelState(current.ModelStates[quotaFable], now)
	current.Generation++
	delayed := current.Clone()
	m.mu.Unlock()
	updated := delayed.Clone()
	updated.Attributes = map[string]string{"priority": "2"}
	if _, errUpdate := m.Update(context.Background(), updated); errUpdate != nil {
		t.Fatal(errUpdate)
	}
	latest, _ := m.GetByID(a)
	if latest.RegistrationEpoch != delayed.RegistrationEpoch || latest.Generation <= delayed.Generation {
		t.Fatal("test did not advance generation within the same registration")
	}
	m.publishClaudeQuotaSnapshots([]*Auth{delayed})
	reg := registry.GetGlobalRegistry()
	if reg.IsModelSuspendedForClient(a, quotaFable) || reg.IsModelQuotaExceededForClient(a, quotaFable) {
		t.Fatal("ordinary concurrent update discarded the pending hold release")
	}
}

func TestClaudeQuotaRankExpiresWithoutPollCompletion(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, a, b := newClaudeQuotaManager(t)
		now := time.Now()
		applyTestClaudeQuota(m, b, 20, now.Add(time.Minute), now)
		pick := func(model, want string) {
			t.Helper()
			got, errPick := m.scheduler.pickSingle(context.Background(), "claude", model, cliproxyexecutor.Options{}, nil)
			if errPick != nil || got == nil || got.ID != want {
				t.Fatalf("%s pick=%v err=%v want=%s", model, got, errPick, want)
			}
		}
		// Prime both model shards before expiry; they share provider metadata.
		pick(quotaFable, b)
		pick(quotaSonnet, b)
		m.RegisterExecutor(&claudeQuotaTestPoller{poll: func(ctx context.Context, _ *Auth) (map[string]QuotaWindow, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		}})
		m.StartQuotaPolling(context.Background())
		t.Cleanup(m.StopQuotaPolling)
		synctest.Wait()
		<-time.After(time.Minute)
		synctest.Wait()
		auth, _ := m.GetByID(b)
		if auth.QuotaRank(time.Now()) != 0 || authPriority(auth) != 0 {
			t.Fatal("rank stayed live after its window reset without a successful poll")
		}
		pick(quotaFable, a)
		pick(quotaSonnet, a)
		// A newly built shard must not reuse the provider's stale priority.
		m.scheduler.mu.Lock()
		delete(m.scheduler.providers["claude"].modelShards, quotaFable)
		m.scheduler.mu.Unlock()
		pick(quotaFable, a)
	})
}

func TestClaudeQuotaPriorityFilterUsesOneInstant(t *testing.T) {
	reset := time.Now().Add(-time.Hour)
	a := &Auth{ID: "a"}
	b := &Auth{ID: "b", Attributes: map[string]string{"quota_rank": "1"}, Quota: QuotaState{RankExpiresAt: reset}}
	auths := []*Auth{a, b}
	if got := highestPriorityAuthsAt(auths, reset.Add(-time.Nanosecond)); len(got) != 1 || got[0] != b {
		t.Fatal("priority passes did not share the supplied instant before reset")
	}
	if got := highestPriorityAuthsAt(auths, reset); len(got) != 2 {
		t.Fatal("rank expiry left the priority filter empty")
	}
}

func TestClaudeQuotaObservationIsRuntimeOnlyAndCloned(t *testing.T) {
	m, a, _ := newClaudeQuotaManager(t)
	now := time.Now()
	applyTestClaudeQuota(m, a, 100, now.Add(time.Hour), now)
	auth, _ := m.GetByID(a)
	auth.Quota.Windows["Fable"] = QuotaWindow{}
	delete(auth.ModelStates[quotaFable].Quota.guardWindows, "Fable")
	current, _ := m.GetByID(a)
	if current.Quota.Windows["Fable"].Utilization != 100 {
		t.Fatal("usage snapshot aliases manager memory")
	}
	if !current.ModelStates[quotaFable].Quota.guardWindows["Fable"].Equal(now.Add(time.Hour)) {
		t.Fatal("hold provenance aliases manager memory")
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
			m.applyTestClaudeQuotaUsage(context.Background(), cfg, base, map[string]QuotaWindow{"Fable": {Utilization: 100, ResetsAt: now.Add(time.Hour)}}, now)
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
			m.applyTestClaudeQuotaUsage(context.Background(), cfg, base, map[string]QuotaWindow{}, time.Now())
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
	synctest.Test(t, func(t *testing.T) {
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
		<-time.After(40 * time.Second)
		synctest.Wait()
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
	})
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

func TestClaudeQuotaStalledReadReleasesSlotOnCredentialChange(t *testing.T) {
	for _, mode := range []string{"refresh", "update", "register", "remove and register"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				m, a, b := newClaudeQuotaManager(t)
				m.Remove(context.Background(), b)
				old, _ := m.GetByID(a)
				called := make(chan *Auth, 8)
				contexts := make(chan context.Context, 8)
				oldComplete := make(chan struct{})
				defer close(oldComplete)
				newComplete := make(chan struct{})
				defer close(newComplete)
				m.RegisterExecutor(&claudeQuotaTestPoller{poll: func(ctx context.Context, auth *Auth) (map[string]QuotaWindow, error) {
					called <- auth
					contexts <- ctx
					if _, ok := ctx.Deadline(); ok {
						t.Error("usage read has a deadline")
					}
					if auth.RegistrationEpoch == old.RegistrationEpoch && auth.CredentialVersion == old.CredentialVersion {
						// Deliberately ignore cancellation to exercise a late completion.
						<-oldComplete
						return map[string]QuotaWindow{"Fable": {Utilization: 100, ResetsAt: time.Now().Add(time.Hour)}}, nil
					}
					select {
					case <-newComplete:
					case <-ctx.Done():
					}
					return nil, ctx.Err()
				}})
				m.StartQuotaPolling(context.Background())
				t.Cleanup(m.StopQuotaPolling)
				synctest.Wait()
				if len(called) != 1 {
					t.Fatal("initial read did not start")
				}
				<-called
				oldContext := <-contexts
				// An ordinary update must keep the same in-flight credential read.
				ordinary := old.Clone()
				ordinary.Attributes = map[string]string{"priority": "2"}
				if _, errUpdate := m.Update(context.Background(), ordinary); errUpdate != nil {
					t.Fatal(errUpdate)
				}
				synctest.Wait()
				if oldContext.Err() != nil {
					t.Fatal("ordinary update cancelled a current read")
				}
				replacement, _ := m.GetByID(a)
				var errChange error
				switch mode {
				case "refresh", "update":
					replacement.Metadata["access_token"] = "replacement-access"
					if mode == "refresh" {
						_, errChange = m.UpdateRefreshedAuth(context.Background(), old, replacement)
					} else {
						_, errChange = m.Update(context.Background(), replacement)
					}
				case "register":
					_, errChange = m.Register(context.Background(), replacement)
				case "remove and register":
					m.Remove(context.Background(), a)
					synctest.Wait()
					if oldContext.Err() == nil {
						t.Error("removal did not cancel the read")
					}
					_, errChange = m.Register(context.Background(), replacement)
				}
				if errChange != nil {
					t.Fatal(errChange)
				}
				synctest.Wait()
				if oldContext.Err() == nil {
					t.Error("credential change did not cancel the read")
				}
				<-time.After(5 * time.Minute)
				synctest.Wait()
				if len(called) != 1 {
					t.Fatal("next tick skipped the replacement behind a stalled read")
				}
				fresh := <-called
				if fresh.RegistrationEpoch == old.RegistrationEpoch && fresh.CredentialVersion == old.CredentialVersion {
					t.Fatal("next tick read the obsolete credential")
				}
				// Let the old result arrive while the replacement is still reading.
				oldComplete <- struct{}{}
				synctest.Wait()
				current, _ := m.GetByID(a)
				if len(current.Quota.Windows) != 0 || isClaudeQuotaHold(current.ModelStates[quotaFable]) {
					t.Fatal("late obsolete completion applied usage")
				}
				<-time.After(5 * time.Minute)
				synctest.Wait()
				if len(called) != 0 {
					t.Fatal("late obsolete completion cleared the replacement running slot")
				}
			})
		})
	}
}

func TestClaudeQuotaPollCadenceDoesNotOverlapStalledSeat(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, a, b := newClaudeQuotaManager(t)
		cfg := m.runtimeConfig.Load().(*config.Config).CloneForRuntime()
		cfg.Claude.Quota.PollInterval = "5m"
		m.SetConfig(cfg)
		reset := time.Now().Add(time.Minute)
		applyTestClaudeQuota(m, a, 100, reset, time.Now())
		applyTestClaudeQuota(m, b, 20, reset, time.Now())
		seat, _ := m.GetByID(b)
		if seat.QuotaRank(time.Now()) != 1 {
			t.Fatal("test did not install the rank")
		}
		called := make(chan string, 8)
		cancelled := make(chan error, 2)
		complete := make(chan struct{})
		m.RegisterExecutor(&claudeQuotaTestPoller{poll: func(ctx context.Context, auth *Auth) (map[string]QuotaWindow, error) {
			if _, ok := ctx.Deadline(); ok {
				t.Error("usage read has a total-request deadline")
			}
			called <- auth.ID
			if auth.ID == a {
				select {
				case <-complete:
				case <-ctx.Done():
					cancelled <- ctx.Err()
					return nil, ctx.Err()
				}
			}
			return map[string]QuotaWindow{}, nil
		}})
		m.StartQuotaPolling(context.Background())
		t.Cleanup(m.StopQuotaPolling)
		synctest.Wait()
		// Both seats get their own launch slot within the first minute.
		<-time.After(40 * time.Second)
		synctest.Wait()
		if len(called) != 2 {
			t.Fatal("initial poll did not visit both seats")
		}
		<-called
		<-called
		// Local expiry must recover both routing and rank while a read is hung.
		<-time.After(time.Minute)
		synctest.Wait()
		if len(cancelled) != 0 {
			t.Fatal("established usage read was cancelled by a timeout")
		}
		seat, _ = m.GetByID(a)
		if blocked, _, _ := isAuthBlockedForModel(seat, quotaFable, time.Now()); blocked {
			t.Fatal("hung read prevented local hold expiry")
		}
		if registry.GetGlobalRegistry().IsModelSuspendedForClient(a, quotaFable) {
			t.Fatal("hung read prevented registry recovery")
		}
		seat, _ = m.GetByID(b)
		if seat.QuotaRank(time.Now()) != 0 || !seat.Quota.RankExpiresAt.IsZero() {
			t.Fatal("hung read prevented local rank expiry")
		}
		if len(called) != 0 {
			t.Fatal("poll overlapped an outstanding read")
		}
		// The next tick still polls available seats without overlapping a hung read.
		<-time.After(4 * time.Minute)
		synctest.Wait()
		if len(cancelled) != 0 || len(called) != 1 || <-called != b {
			t.Fatal("next tick cancelled or overlapped the hung read, or skipped the other seat")
		}
		close(complete)
		synctest.Wait()
		<-time.After(5*time.Minute + 10*time.Second)
		synctest.Wait()
		if len(called) != 2 {
			t.Fatal("completed seat was not retried on the next tick")
		}
		seen := map[string]bool{<-called: true, <-called: true}
		if !seen[a] || !seen[b] {
			t.Fatal("next tick did not poll both seats")
		}
	})
}
