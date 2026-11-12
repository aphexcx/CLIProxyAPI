package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

// Synthetic values in the CLI/web cedar_ember response shape (cp-ej6 research).
const limitResetsFixture = `{"cedar_ember":{"eligible":true,"ineligible_reason":null,"at_limit":true,"weekly_resets_at":"2027-03-01T00:00:00Z","cooldown_until":null,"grants":[{"id":"launch","label":"Launch","resets_left":2,"resets_total":3,"ends_at":"2027-03-08T00:00:00Z","type":"weekly","clears":["five_hour","seven_day_overage_included"],"usable_now":true,"use_requires_limit":true,"gives_back_weekly":true}]}}`

func TestParseClaudeLimitResets(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		valid      bool
	}{
		{"eligible", limitResetsFixture, true},
		{"ineligible", `{"cedar_ember":{"eligible":false,"ineligible_reason":"surface","grants":[]}}`, true},
		{"zero", strings.Replace(limitResetsFixture, `"resets_left":2`, `"resets_left":0`, 1), true},
		{"missing", `{}`, false}, {"null", `{"cedar_ember":null}`, false},
		{"missing grants", `{"cedar_ember":{"eligible":true}}`, false},
		{"wrong boolean", `{"cedar_ember":{"eligible":"secret"}}`, false},
		{"negative", strings.Replace(limitResetsFixture, `"resets_left":2`, `"resets_left":-1`, 1), false},
		{"count absent", strings.Replace(limitResetsFixture, `"resets_left":2,`, ``, 1), false},
		{"invalid timestamp", strings.ReplaceAll(limitResetsFixture, "2027-03-08T00:00:00Z", "secret"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, err := ParseClaudeLimitResets(json.RawMessage(tc.body))
			if (err == nil) != tc.valid {
				t.Fatalf("status=%+v err=%v", status, err)
			}
			if err != nil && strings.Contains(err.Error(), "secret") {
				t.Fatal("upstream body leaked")
			}
		})
	}
}

func TestLimitResetsRetainsLastGoodOnMissingMalformedAndFailedPoll(t *testing.T) {
	m, id, _ := newClaudeQuotaManager(t)
	cfg := m.runtimeConfig.Load().(*config.Config)
	base, _ := m.GetByID(id)
	now := time.Now().Add(-2 * time.Minute)
	m.recordClaudeLimitResets(context.Background(), cfg, base, json.RawMessage(limitResetsFixture), nil, now)
	initial := m.ClaudeLimitResetsCached(id)
	if initial.CedarEmber == nil || *initial.AgeSeconds < 120 {
		t.Fatalf("initial=%+v", initial)
	}
	for _, body := range []string{`{}`, `{"cedar_ember":{"eligible":"bad"}}`} {
		m.recordClaudeLimitResets(context.Background(), cfg, base, json.RawMessage(body), nil, now.Add(time.Minute))
		got := m.ClaudeLimitResetsCached(id)
		if got.CedarEmber == nil || !got.LastGoodAt.Equal(*initial.LastGoodAt) || got.Error == "" || !got.Stale {
			t.Fatalf("last good lost: %+v", got)
		}
		got.CedarEmber.Grants[0].Clears[0] = "mutated"
		if m.ClaudeLimitResetsCached(id).CedarEmber.Grants[0].Clears[0] == "mutated" {
			t.Fatal("caller mutated cache")
		}
	}
	m.recordClaudeLimitResets(context.Background(), cfg, base, nil, &Error{HTTPStatus: 429}, time.Now())
	failed := m.ClaudeLimitResetsCached(id)
	if failed.Error != "reset status poll failed (HTTP 429)" || !failed.LastGoodAt.Equal(*initial.LastGoodAt) {
		t.Fatalf("failed=%+v", failed)
	}
	base.Metadata["access_token"] = "replacement-fixture"
	if _, err := m.Update(context.Background(), base); err != nil {
		t.Fatal(err)
	}
	if got := m.ClaudeLimitResetsCached(id); got.CedarEmber != nil {
		t.Fatal("old credential reset data survived replacement")
	}
}

func TestLimitResetsInitialPollFailureReportsErrorWithoutInventingAge(t *testing.T) {
	m, id, _ := newClaudeQuotaManager(t)
	cfg := m.runtimeConfig.Load().(*config.Config)
	base, _ := m.GetByID(id)
	started := time.Now()
	release, ok := m.reserveClaudeQuotaPoll(context.Background(), cfg, base, time.Minute, started)
	if !ok {
		t.Fatal("initial poll not reserved")
	}
	defer release()
	m.recordClaudeLimitResets(context.Background(), cfg, base, nil, &Error{HTTPStatus: 429}, started)
	got := m.ClaudeLimitResetsCached(id)
	if got.Error != "reset status poll failed (HTTP 429)" || !got.Stale || got.CedarEmber != nil || got.AgeSeconds != nil || got.LastGoodAt != nil {
		t.Fatalf("initial failure lost or invented observation: %+v", got)
	}
}

func TestLimitResetsCannotChangeQuotaWindowsOrHolds(t *testing.T) {
	m, id, _ := newClaudeQuotaManager(t)
	cfg := m.runtimeConfig.Load().(*config.Config)
	now := time.Now()
	applyTestClaudeQuota(m, id, 100, now.Add(time.Hour), now)
	base, _ := m.GetByID(id)
	if blocked, _, _ := isAuthBlockedForModel(base, quotaFable, now); !blocked {
		t.Fatal("fixture has no Fable hold")
	}
	for _, errRead := range []error{nil, fmt.Errorf("reset status response has no valid usage windows")} {
		m.recordClaudeLimitResets(context.Background(), cfg, base, json.RawMessage(limitResetsFixture), errRead, now)
		current, _ := m.GetByID(id)
		if !reflect.DeepEqual(current.Quota, base.Quota) || !reflect.DeepEqual(current.ModelStates, base.ModelStates) {
			t.Fatal("reset observation modified routing quota state")
		}
		if blocked, _, _ := isAuthBlockedForModel(current, quotaFable, now); !blocked {
			t.Fatal("reset-only body released a Fable hold")
		}
	}
	if got := m.ClaudeLimitResetsCached(id); !got.Stale || got.CedarEmber == nil {
		t.Fatalf("reset failure did not retain stale data: %+v", got)
	}
}

type isolatedResetPoller struct {
	ProviderExecutor
	routine atomic.Int32
	resets  atomic.Int32
	finish  chan struct{}
}

func (*isolatedResetPoller) Identifier() string { return "claude" }
func (p *isolatedResetPoller) PollQuota(context.Context, *Auth) (map[string]QuotaWindow, error) {
	p.routine.Add(1)
	return map[string]QuotaWindow{"Fable": {Utilization: 100, ResetsAt: time.Now().Add(time.Hour), ObservedAt: time.Now()}}, nil
}
func (p *isolatedResetPoller) PollLimitResets(ctx context.Context, _ *Auth) (json.RawMessage, error) {
	p.resets.Add(1)
	select {
	case <-p.finish:
		return json.RawMessage(limitResetsFixture), nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func TestLimitResetsHungReadDoesNotBlockRoutinePolls(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := NewManager(nil, nil, nil)
		cfg := &config.Config{RequestRetry: 3}
		cfg.Claude.Quota.Enabled = true
		m.SetConfig(cfg)
		_, err := m.Register(context.Background(), &Auth{ID: "fixture", Provider: "claude", Status: StatusActive, Metadata: map[string]any{"access_token": "fixture"}})
		if err != nil {
			t.Fatal(err)
		}
		p := &isolatedResetPoller{finish: make(chan struct{})}
		m.RegisterExecutor(p)
		m.StartQuotaPolling(context.Background())
		defer m.StopQuotaPolling()
		synctest.Wait()
		<-time.After(150 * time.Second)
		synctest.Wait()
		if p.routine.Load() < 2 || p.resets.Load() != 1 {
			t.Fatalf("routine=%d resets=%d", p.routine.Load(), p.resets.Load())
		}
		close(p.finish)
		synctest.Wait()
		if m.ClaudeLimitResetsCached("fixture").CedarEmber == nil {
			t.Fatal("separate reset cache was not populated")
		}
	})
}

func TestLimitResetsRejectsLateObservationFromReplacedCredential(t *testing.T) {
	m, id, _ := newClaudeQuotaManager(t)
	cfg := m.runtimeConfig.Load().(*config.Config)
	old, _ := m.GetByID(id)
	replacement := old.Clone()
	replacement.Metadata["access_token"] = "replacement"
	if _, err := m.Update(context.Background(), replacement); err != nil {
		t.Fatal(err)
	}
	m.recordClaudeLimitResets(context.Background(), cfg, old, json.RawMessage(limitResetsFixture), nil, time.Now())
	got := m.ClaudeLimitResetsCached(id)
	if got.CedarEmber != nil || got.CredentialGeneration == old.CredentialGeneration() {
		t.Fatalf("old observation accepted: %+v", got)
	}
}
