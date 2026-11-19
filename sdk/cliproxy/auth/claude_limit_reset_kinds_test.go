package auth

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestClaudeResetGrantKindTable(t *testing.T) {
	yes, no := true, false
	cases := []struct {
		name  string
		grant ClaudeResetGrant
		want  string
	}{
		{"session", ClaudeResetGrant{Type: "session"}, ClaudeResetKindSession},
		{"weekly", ClaudeResetGrant{Type: "weekly", GivesBackWeekly: &no}, ClaudeResetKindWeekly},
		{"full", ClaudeResetGrant{Type: "full"}, ClaudeResetKindFull},
		{"fable on weekly", ClaudeResetGrant{Type: "weekly", GivesBackWeekly: &yes}, ClaudeResetKindFable},
		{"fable on full", ClaudeResetGrant{Type: "full", GivesBackWeekly: &yes}, ClaudeResetKindFable},
		{"fable untyped", ClaudeResetGrant{GivesBackWeekly: &yes}, ClaudeResetKindFable},
		// The provider's own client derives an untyped grant's kind from its
		// clears; the hub refuses to guess.
		{"untyped weekly clears", ClaudeResetGrant{Clears: []string{"seven_day"}}, ClaudeResetKindUnknown},
		{"untyped five hour clears", ClaudeResetGrant{Clears: []string{"five_hour"}, GivesBackWeekly: &no}, ClaudeResetKindUnknown},
		{"unknown type", ClaudeResetGrant{Type: "turbo", Clears: []string{"seven_day"}}, ClaudeResetKindUnknown},
		{"unknown type with fable flag", ClaudeResetGrant{Type: "turbo", GivesBackWeekly: &yes}, ClaudeResetKindUnknown},
		{"type case is exact", ClaudeResetGrant{Type: "Weekly"}, ClaudeResetKindUnknown},
		{"fable is not a provider type", ClaudeResetGrant{Type: "fable"}, ClaudeResetKindUnknown},
	}
	for _, tc := range cases {
		if got := ClaudeResetGrantKind(tc.grant); got != tc.want {
			t.Errorf("%s: kind=%q want %q", tc.name, got, tc.want)
		}
	}
	for _, kind := range []string{"fable", "full", "session", "weekly"} {
		if !ValidClaudeResetKind(kind) {
			t.Errorf("%s should be a nameable kind", kind)
		}
	}
	for _, kind := range []string{"", "unknown", "turbo", "Fable", "5-hour", " fable"} {
		if ValidClaudeResetKind(kind) {
			t.Errorf("%q must not be a nameable kind", kind)
		}
	}
}

func TestParseClaudeLimitResetsComputesKindAndIgnoresProviderKind(t *testing.T) {
	status, err := ParseClaudeLimitResets(json.RawMessage(`{"cedar_ember":{"eligible":true,"grants":[
		{"id":"g-weekly","resets_left":1,"type":"weekly","kind":"fable"},
		{"id":"g-fable","resets_left":1,"type":"full","gives_back_weekly":true},
		{"id":"g-odd","resets_left":1,"type":"turbo","kind":"weekly"}]}}`))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, grant := range status.Grants {
		got[grant.ID] = grant.Kind
	}
	if got["g-weekly"] != "weekly" || got["g-fable"] != "fable" || got["g-odd"] != "unknown" {
		t.Fatalf("kinds=%v", got)
	}
}

// The two grant shapes a read of live seats found on 2026-11-19 (ids
// replaced). The Fable grant's type is "full": only gives_back_weekly
// tells it from a full reset. The grant labelled a full limit reset has type
// "weekly". The kind comes from the fields, never from the label.
func TestClaudeResetKindOfTheLiveGrantShapes(t *testing.T) {
	status, err := ParseClaudeLimitResets(json.RawMessage(`{"cedar_ember":{"eligible":true,"ineligible_reason":null,"at_limit":false,"cooldown_until":null,"grants":[
		{"id":"fixture-a","type":"weekly","gives_back_weekly":false,"label":"One full limit reset (November 2026)","clears":["seven_day","seven_day_overage_included"],"ends_at":"2026-12-10T22:00:00+00:00","usable_now":true,"paused":false,"resets_left":1,"resets_total":1},
		{"id":"fixture-b","type":"full","gives_back_weekly":true,"label":"New model launch: one Fable reset for Max, Team Premium and Enterprise Premium","clears":["seven_day_overage_included"],"ends_at":"2027-03-09T07:59:00+00:00","usable_now":false,"paused":false,"resets_left":0,"resets_total":1,"unavailable_reason":"no_grant"}]}}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := status.Grants[0].Kind; got != ClaudeResetKindWeekly {
		t.Fatalf("shape (a) kind=%q", got)
	}
	if got := status.Grants[1].Kind; got != ClaudeResetKindFable {
		t.Fatalf("shape (b) kind=%q", got)
	}
	if reason := status.Grants[1].UnavailableReason; reason == nil || *reason != "no_grant" {
		t.Fatalf("unavailable_reason=%v", reason)
	}
	now := time.Date(2026, 11, 19, 9, 0, 0, 0, time.UTC)
	if _, ok := SelectClaudeResetGrant(status.Grants, ClaudeResetKindFull, now); ok {
		t.Fatal("a Fable grant of type full was offered as a full reset")
	}
	if _, ok := SelectClaudeResetGrant(status.Grants, ClaudeResetKindFable, now); ok {
		t.Fatal("a Fable grant that is not usable now was offered")
	}
	if grant, ok := SelectClaudeResetGrant(status.Grants, ClaudeResetKindWeekly, now); !ok || grant.ID != "fixture-a" {
		t.Fatalf("weekly selection=%q ok=%v", grant.ID, ok)
	}
}

func TestSelectClaudeResetGrantPicksSoonestUsableOfKind(t *testing.T) {
	now := time.Date(2026, 11, 19, 8, 0, 0, 0, time.UTC)
	at := func(days int) *time.Time { value := now.Add(time.Duration(days) * 24 * time.Hour); return &value }
	yes := true
	grants := []ClaudeResetGrant{
		{ID: "fable-late", Type: "weekly", GivesBackWeekly: &yes, ResetsLeft: 1, UsableNow: true, EndsAt: at(60)},
		{ID: "fable-soon", Type: "full", GivesBackWeekly: &yes, ResetsLeft: 1, UsableNow: true, EndsAt: at(30)},
		{ID: "fable-paused", Type: "full", GivesBackWeekly: &yes, ResetsLeft: 1, UsableNow: true, Paused: true, EndsAt: at(1)},
		{ID: "fable-empty", Type: "full", GivesBackWeekly: &yes, ResetsLeft: 0, UsableNow: true, EndsAt: at(1)},
		{ID: "fable-not-usable", Type: "full", GivesBackWeekly: &yes, ResetsLeft: 1, EndsAt: at(1)},
		{ID: "fable-expired", Type: "full", GivesBackWeekly: &yes, ResetsLeft: 1, UsableNow: true, EndsAt: at(-1)},
		{ID: "fable-future", Type: "full", GivesBackWeekly: &yes, ResetsLeft: 1, UsableNow: true, StartsAt: at(1), EndsAt: at(2)},
		{ID: "full-open", Type: "full", ResetsLeft: 1, UsableNow: true},
		{ID: "full-b", Type: "full", ResetsLeft: 1, UsableNow: true, EndsAt: at(20)},
		{ID: "full-a", Type: "full", ResetsLeft: 1, UsableNow: true, EndsAt: at(20)},
		{ID: "weekly-turbo", Type: "turbo", Clears: []string{"seven_day"}, ResetsLeft: 3, UsableNow: true, EndsAt: at(1)},
	}
	for kind, want := range map[string]string{"fable": "fable-soon", "full": "full-a", "weekly": ""} {
		grant, ok := SelectClaudeResetGrant(grants, kind, now)
		if (want == "") == ok || (ok && grant.ID != want) {
			t.Errorf("%s: got %q ok=%v want %q", kind, grant.ID, ok, want)
		}
	}
	if _, ok := SelectClaudeResetGrant(grants, "unknown", now); ok {
		t.Fatal("an unknown grant must never be selected")
	}
	// Expired grants no longer remain; the expiry shown is the grant a spend
	// of that kind would use.
	summary := SummarizeClaudeResetKinds(grants, now)
	fable := summary["fable"]
	if fable.ResetsLeft != 5 || fable.UsableResets != 2 || fable.SoonestExpiry == nil || !fable.SoonestExpiry.Equal(*at(30)) {
		t.Fatalf("fable summary=%+v", fable)
	}
	full := summary["full"]
	if full.ResetsLeft != 3 || full.UsableResets != 3 || full.SoonestExpiry == nil || !full.SoonestExpiry.Equal(*at(20)) {
		t.Fatalf("full summary=%+v", full)
	}
	if unknown := summary["unknown"]; unknown.ResetsLeft != 3 || unknown.UsableResets != 0 || unknown.SoonestExpiry != nil {
		t.Fatalf("unknown summary=%+v", unknown)
	}
	if _, listed := summary["weekly"]; listed {
		t.Fatal("a kind with no grant must not be listed")
	}
}

// Gate r1 MINOR 14: a grant whose id a claim cannot carry is never selected or
// counted usable, so it cannot shadow a later grant a claim can name.
func TestClaudeResetGrantWithAnUnclaimableIDIsNeverSpendable(t *testing.T) {
	now := time.Date(2026, 11, 19, 8, 0, 0, 0, time.UTC)
	at := func(days int) *time.Time { value := now.Add(time.Duration(days) * 24 * time.Hour); return &value }
	grants := []ClaudeResetGrant{
		{ID: "Weekly-Upper", Type: "weekly", ResetsLeft: 1, UsableNow: true, EndsAt: at(1)},
		{ID: "weekly/slash", Type: "weekly", ResetsLeft: 1, UsableNow: true, EndsAt: at(2)},
		{ID: "weekly-ok", Type: "weekly", ResetsLeft: 1, UsableNow: true, EndsAt: at(5)},
	}
	if grant, ok := SelectClaudeResetGrant(grants, "weekly", now); !ok || grant.ID != "weekly-ok" {
		t.Fatalf("selected %q ok=%v", grant.ID, ok)
	}
	if weekly := SummarizeClaudeResetKinds(grants, now)["weekly"]; weekly.ResetsLeft != 3 || weekly.UsableResets != 1 || !weekly.SoonestExpiry.Equal(*at(5)) {
		t.Fatalf("weekly summary=%+v", weekly)
	}
}

// Gate r1 MINOR 15: an upstream "kind" of any shape is never decoded, so it
// cannot reject a status the read would otherwise accept.
func TestParseClaudeLimitResetsIgnoresAnUpstreamKindOfAnyShape(t *testing.T) {
	for _, kind := range []string{`{}`, `[]`, `7`, `null`, `"fable"`} {
		status, err := ParseClaudeLimitResets(json.RawMessage(`{"cedar_ember":{"eligible":true,"grants":[{"id":"g-weekly","resets_left":1,"type":"weekly","kind":` + kind + `}]}}`))
		if err != nil || len(status.Grants) != 1 || status.Grants[0].Kind != "weekly" {
			t.Fatalf("kind %s: status=%+v err=%v", kind, status, err)
		}
	}
}

// Gate r1 MINOR 12: after a spend the reset cache never shows the spent grant
// as fresh. The spend's after-read replaces the observation, another seat
// whose cache lists the grant turns stale, a poll attempt that began before
// the spend cannot bring the old count back, a failed after-read marks the
// observation stale, and a replaced credential's cache is left alone.
func TestPublishClaudeResetSpendReadKeepsTheCacheTruthful(t *testing.T) {
	m, id, twin := newClaudeQuotaManager(t)
	cfg := m.runtimeConfig.Load().(*config.Config)
	base, _ := m.GetByID(id)
	twinBase, _ := m.GetByID(twin)
	m.recordClaudeLimitResets(context.Background(), cfg, twinBase, json.RawMessage(limitResetsFixture), nil, time.Now())
	if got := m.ClaudeLimitResetsCached(twin); got.Stale {
		t.Fatalf("precondition: twin %+v", got)
	}
	launchLeft := func() int { return cachedLaunchLeft(t, m, id) }
	m.recordClaudeLimitResets(context.Background(), cfg, base, json.RawMessage(limitResetsFixture), nil, time.Now())
	if got := m.ClaudeLimitResetsCached(id); got.Stale || launchLeft() != 2 {
		t.Fatalf("precondition: %+v", got)
	}
	spent := strings.Replace(limitResetsFixture, `"resets_left":2`, `"resets_left":1`, 1)
	m.PublishClaudeResetSpendRead(base, "launch", true, time.Now(), json.RawMessage(spent), nil)
	if got := m.ClaudeLimitResetsCached(id); got.Stale || launchLeft() != 1 {
		t.Fatalf("after the spend: %+v", got)
	}
	if got := m.ClaudeLimitResetsCached(twin); !got.Stale || got.Error == "" {
		t.Fatalf("another seat listing the spent grant still reads fresh: %+v", got)
	}
	setAttempt := func(at time.Time) {
		m.mu.Lock()
		m.limitResetSeats[id].attemptedAt = at
		m.mu.Unlock()
	}
	setAttempt(time.Now().Add(-time.Minute))
	m.recordClaudeLimitResets(context.Background(), cfg, base, json.RawMessage(limitResetsFixture), nil, time.Now())
	if launchLeft() != 1 {
		t.Fatal("a poll that began before the spend brought the old count back")
	}
	setAttempt(time.Now().Add(time.Second))
	m.recordClaudeLimitResets(context.Background(), cfg, base, json.RawMessage(spent), nil, time.Now())
	m.PublishClaudeResetSpendRead(base, "launch", true, time.Now().Add(2*time.Second), nil, errors.New("fixture after-read failed"))
	if got := m.ClaudeLimitResetsCached(id); !got.Stale || got.Error == "" {
		t.Fatalf("a failed after-read left the observation fresh: %+v", got)
	}
	replaced := base.Clone()
	replaced.Metadata = map[string]any{"access_token": "fixture-replacement-token"}
	if _, err := m.Update(context.Background(), replaced); err != nil {
		t.Fatal(err)
	}
	before := m.ClaudeLimitResetsCached(id)
	m.PublishClaudeResetSpendRead(base, "other-grant", true, time.Now().Add(3*time.Second), json.RawMessage(limitResetsFixture), nil)
	if after := m.ClaudeLimitResetsCached(id); after.CredentialGeneration != before.CredentialGeneration || (after.CedarEmber == nil) != (before.CedarEmber == nil) || after.Error != before.Error {
		t.Fatalf("a replaced credential's cache changed: before=%+v after=%+v", before, after)
	}
}

// cachedLaunchLeft is what the reset cache shows left on the fixture's
// "launch" grant.
func cachedLaunchLeft(t *testing.T, m *Manager, id string) int {
	t.Helper()
	got := m.ClaudeLimitResetsCached(id)
	if got.CedarEmber == nil {
		t.Fatalf("nothing cached: %+v", got)
	}
	for _, grant := range got.CedarEmber.Grants {
		if grant.ID == "launch" {
			return grant.ResetsLeft
		}
	}
	t.Fatalf("no launch grant: %+v", got.CedarEmber)
	return -1
}

// setClaudeLimitResetAttempt marks when the seat's current poll attempt began.
func setClaudeLimitResetAttempt(m *Manager, id string, at time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.limitResetSeats[id].attemptedAt = at
}

// Gate r1 Codex MINOR 3: one ordering rule for polls and spends, in both
// completion orders. A poll that began before a spend's after-read changes
// nothing when it then fails or answers malformed, and a delayed after-read
// does not replace a newer completed poll.
func TestClaudeResetCacheOrdersPollsAndSpendsByWhenTheirReadBegan(t *testing.T) {
	m, id, _ := newClaudeQuotaManager(t)
	cfg := m.runtimeConfig.Load().(*config.Config)
	base, _ := m.GetByID(id)
	spent := strings.Replace(limitResetsFixture, `"resets_left":2`, `"resets_left":1`, 1)
	m.recordClaudeLimitResets(context.Background(), cfg, base, json.RawMessage(limitResetsFixture), nil, time.Now())
	pollStart := time.Now()
	setClaudeLimitResetAttempt(m, id, pollStart)
	m.PublishClaudeResetSpendRead(base, "launch", true, pollStart.Add(time.Second), json.RawMessage(spent), nil)
	for name, late := range map[string]struct {
		body json.RawMessage
		err  error
	}{"failed": {nil, errors.New("fixture poll failure")}, "malformed": {json.RawMessage(`{}`), nil}} {
		m.recordClaudeLimitResets(context.Background(), cfg, base, late.body, late.err, time.Now())
		if got := m.ClaudeLimitResetsCached(id); got.Stale || got.Error != "" || cachedLaunchLeft(t, m, id) != 1 {
			t.Fatalf("an older poll that %s changed the spend's observation: %+v", name, got)
		}
	}
	setClaudeLimitResetAttempt(m, id, pollStart.Add(3*time.Second))
	m.recordClaudeLimitResets(context.Background(), cfg, base, json.RawMessage(limitResetsFixture), nil, time.Now())
	m.PublishClaudeResetSpendRead(base, "launch", true, pollStart.Add(2*time.Second), json.RawMessage(spent), nil)
	if got := m.ClaudeLimitResetsCached(id); got.Stale || cachedLaunchLeft(t, m, id) != 2 {
		t.Fatalf("a delayed after-read replaced a newer poll: %+v", got)
	}
}

// Gate r1 Codex MINOR 4: a claim the provider refused spent nothing, so
// another seat listing the grant keeps its fresh observation.
func TestPublishClaudeResetSpendReadLeavesSiblingsOfARefusal(t *testing.T) {
	m, id, twin := newClaudeQuotaManager(t)
	cfg := m.runtimeConfig.Load().(*config.Config)
	for _, seat := range []string{id, twin} {
		auth, _ := m.GetByID(seat)
		m.recordClaudeLimitResets(context.Background(), cfg, auth, json.RawMessage(limitResetsFixture), nil, time.Now())
	}
	base, _ := m.GetByID(id)
	m.PublishClaudeResetSpendRead(base, "launch", false, time.Now(), json.RawMessage(limitResetsFixture), nil)
	if got := m.ClaudeLimitResetsCached(twin); got.Stale || got.Error != "" {
		t.Fatalf("a refused claim marked another seat stale: %+v", got)
	}
}
