package auth

import (
	"encoding/json"
	"testing"
	"time"
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
