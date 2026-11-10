package auth

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
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
	m.applyClaudeQuotaUsageSnapshot(context.Background(), cfg, base, nil, json.RawMessage(limitResetsFixture), now)
	initial := m.ClaudeLimitResetsCached(id)
	if initial.CedarEmber == nil || *initial.AgeSeconds < 120 {
		t.Fatalf("initial=%+v", initial)
	}
	for _, body := range []string{`{}`, `{"cedar_ember":{"eligible":"bad"}}`} {
		m.applyClaudeQuotaUsageSnapshot(context.Background(), cfg, base, nil, json.RawMessage(body), now.Add(time.Minute))
		got := m.ClaudeLimitResetsCached(id)
		if got.CedarEmber == nil || !got.LastGoodAt.Equal(*initial.LastGoodAt) || got.Error == "" || !got.Stale {
			t.Fatalf("last good lost: %+v", got)
		}
		got.CedarEmber.Grants[0].Clears[0] = "mutated"
		if m.ClaudeLimitResetsCached(id).CedarEmber.Grants[0].Clears[0] == "mutated" {
			t.Fatal("caller mutated cache")
		}
	}
	m.recordClaudeQuotaPoll(base, time.Now(), &Error{HTTPStatus: 429})
	failed := m.ClaudeLimitResetsCached(id)
	if failed.Error != "usage poll failed (HTTP 429)" || !failed.LastGoodAt.Equal(*initial.LastGoodAt) {
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
	m.recordClaudeQuotaPoll(base, started, &Error{HTTPStatus: 429})
	got := m.ClaudeLimitResetsCached(id)
	if got.Error != "usage poll failed (HTTP 429)" || !got.Stale || got.CedarEmber != nil || got.AgeSeconds != nil || got.LastGoodAt != nil {
		t.Fatalf("initial failure lost or invented observation: %+v", got)
	}
}
