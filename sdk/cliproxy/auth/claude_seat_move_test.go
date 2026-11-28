package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

// Made-up organizations; no real account is named.
const (
	seatMoveOrgOne = "11111111-1111-4111-8111-111111111111"
	seatMoveOrgTwo = "22222222-2222-4222-8222-222222222222"
)

// seatMoveOrgLogName is the credential file name's organization hash, computed
// here on its own so the log is checked against the file name rule.
func seatMoveOrgLogName(organization string) string {
	if organization == "" {
		return "none"
	}
	digest := sha256.Sum256([]byte(organization))
	return hex.EncodeToString(digest[:])[:8]
}

func setSeatOrganization(t *testing.T, f *seatRoutingFixture, id, organization string) {
	t.Helper()
	auth, _ := f.m.GetByID(id)
	if organization == "" {
		delete(auth.Metadata, "organization_uuid")
	} else {
		auth.Metadata["organization_uuid"] = organization
	}
	if _, errUpdate := f.m.Update(context.Background(), auth); errUpdate != nil {
		t.Fatal(errUpdate)
	}
}

func setSeatDisabled(t *testing.T, f *seatRoutingFixture, id string) {
	t.Helper()
	auth, _ := f.m.GetByID(id)
	auth.Disabled = true
	if _, errUpdate := f.m.Update(context.Background(), auth); errUpdate != nil {
		t.Fatal(errUpdate)
	}
}

func addSeatMoveSeat(t *testing.T, f *seatRoutingFixture, suffix, organization, priority string) string {
	t.Helper()
	id := t.Name() + "-" + suffix
	registry.GetGlobalRegistry().RegisterClient(id, "claude", []*registry.ModelInfo{{ID: quotaFable}, {ID: seatOpus}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
	if _, errRegister := f.m.Register(context.Background(), &Auth{
		ID: id, Provider: "claude", Status: StatusActive,
		Attributes: map[string]string{"priority": priority},
		Metadata:   map[string]any{"access_token": "test-only-token", "organization_uuid": organization},
	}); errRegister != nil {
		t.Fatal(errRegister)
	}
	f.ids = append(f.ids, id)
	return id
}

func otherSeat(f *seatRoutingFixture, id string) string {
	if f.ids[0] == id {
		return f.ids[1]
	}
	return f.ids[0]
}

// lastMove is the move tag the executor received on the latest attempt.
func (f *seatRoutingFixture) lastMove(t *testing.T) *cliproxyexecutor.ClaudeSeatMove {
	t.Helper()
	if len(f.moves) == 0 {
		t.Fatal("no attempt reached the executor")
	}
	value := f.moves[len(f.moves)-1]
	if value == nil {
		return nil
	}
	move, ok := value.(cliproxyexecutor.ClaudeSeatMove)
	if !ok {
		t.Fatalf("move tag has type %T", value)
	}
	return &move
}

// Each relation of a warm move: same organization, another organization, and
// unknown when either seat has no organization (a seat without one is never
// same_org, not even with another seat without one). The "client moved" line
// names both organizations and the available seats of the old one.
func TestClaudeSeatMoveTagsEachRelation(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		firstOrg, secondOrg  string
		relation             string
		wantSameOrgAvailable int
	}{
		{name: "same_org", firstOrg: seatMoveOrgOne, secondOrg: seatMoveOrgOne, relation: "same_org", wantSameOrgAvailable: 1},
		{name: "other_org", firstOrg: seatMoveOrgOne, secondOrg: seatMoveOrgTwo, relation: "other_org"},
		{name: "unknown_org/new seat has none", firstOrg: seatMoveOrgOne, relation: "unknown_org"},
		{name: "unknown_org/old seat has none", secondOrg: seatMoveOrgOne, relation: "unknown_org"},
		{name: "unknown_org/neither has one", relation: "unknown_org"},
	} {
		for _, path := range []string{"execute", "stream"} {
			t.Run(tc.name+"/"+path, func(t *testing.T) {
				f, clock := newSeatHoldFixture(t, false)
				first := f.request(t, "move-client", seatOpus, "", path)
				if move := f.lastMove(t); move != nil {
					t.Fatalf("a first placement was tagged as a move: %+v", move)
				}
				second := otherSeat(f, first)
				setSeatOrganization(t, f, first, tc.firstOrg)
				setSeatOrganization(t, f, second, tc.secondOrg)
				clock.advance(time.Minute)
				if got := f.request(t, "move-client", seatOpus, "", path); got != first {
					t.Fatalf("warm stay moved to %s", got)
				}
				if move := f.lastMove(t); move != nil {
					t.Fatalf("a warm stay was tagged as a move: %+v", move)
				}
				setSeatDisabled(t, f, first)
				clock.advance(time.Minute)
				if got := f.request(t, "move-client", seatOpus, "", path); got != second {
					t.Fatalf("move went to %s, want %s", got, second)
				}
				want := cliproxyexecutor.ClaudeSeatMove{Relation: tc.relation, Warm: true, Reason: "disabled"}
				if move := f.lastMove(t); move == nil || *move != want {
					t.Fatalf("move tag = %+v, want %+v", move, want)
				}
				moves := f.events("claude routing: client moved")
				if len(moves) != 1 {
					t.Fatalf("client moved lines = %d, want 1", len(moves))
				}
				data := moves[0].Data
				if data["previous_organization"] != seatMoveOrgLogName(tc.firstOrg) || data["organization"] != seatMoveOrgLogName(tc.secondOrg) ||
					data["same_org_available"] != tc.wantSameOrgAvailable {
					t.Fatalf("client moved line = %+v, want organizations %s -> %s and same_org_available=%d", data,
						seatMoveOrgLogName(tc.firstOrg), seatMoveOrgLogName(tc.secondOrg), tc.wantSameOrgAvailable)
				}
			})
		}
	}
}

// The avoidable case the line exists to find: the conversation leaves its
// organization while a seat of that organization was available.
func TestClaudeSeatMoveCountsAvailableSeatsOfTheOldOrganization(t *testing.T) {
	f, clock := newSeatHoldFixture(t, false)
	first := f.request(t, "avoidable-client", seatOpus, "", "execute")
	sibling := otherSeat(f, first)
	setSeatOrganization(t, f, first, seatMoveOrgOne)
	setSeatOrganization(t, f, sibling, seatMoveOrgOne)
	// The ranked seat of another organization wins the fallback's priority tier.
	ranked := addSeatMoveSeat(t, f, "ranked", seatMoveOrgTwo, "10")
	setSeatDisabled(t, f, first)
	clock.advance(time.Minute)
	if got := f.request(t, "avoidable-client", seatOpus, "", "execute"); got != ranked {
		t.Fatalf("move went to %s, want the ranked seat %s", got, ranked)
	}
	want := cliproxyexecutor.ClaudeSeatMove{Relation: "other_org", Warm: true, Reason: "disabled"}
	if move := f.lastMove(t); move == nil || *move != want {
		t.Fatalf("move tag = %+v, want %+v", move, want)
	}
	moves := f.events("claude routing: client moved")
	if len(moves) != 1 {
		t.Fatalf("client moved lines = %d, want 1", len(moves))
	}
	data := moves[0].Data
	if data["previous_organization"] != seatMoveOrgLogName(seatMoveOrgOne) || data["organization"] != seatMoveOrgLogName(seatMoveOrgTwo) ||
		data["same_org_available"] != 1 {
		t.Fatalf("client moved line = %+v, want the sibling counted (same_org_available=1)", data)
	}
}

// An expired hold: placed again on the same seat it is same_seat, on another
// seat it carries the seats' relation; warm is false either way, whether or
// not the expired hold was swept before the conversation returned.
func TestClaudeSeatMoveWarmAndExpired(t *testing.T) {
	for _, swept := range []bool{false, true} {
		for _, sameSeat := range []bool{false, true} {
			name := map[bool]string{false: "other_seat", true: "same_seat"}[sameSeat] + map[bool]string{false: "", true: "/swept"}[swept]
			t.Run(name, func(t *testing.T) {
				f, clock := newSeatHoldFixture(t, false)
				first := f.request(t, "expiring-client", seatOpus, "", "execute")
				second := otherSeat(f, first)
				setSeatOrganization(t, f, first, seatMoveOrgOne)
				setSeatOrganization(t, f, second, seatMoveOrgOne)
				clock.advance(6 * time.Minute)
				if swept {
					f.request(t, "sweeping-client", seatOpus, "", "execute")
				}
				want := cliproxyexecutor.ClaudeSeatMove{Relation: "same_org", Warm: false, Reason: "cache_expired"}
				wantSeat := second
				if sameSeat {
					setSeatDisabled(t, f, second)
					want.Relation, wantSeat = "same_seat", first
				} else {
					setSeatDisabled(t, f, first)
				}
				before := len(f.events("claude routing: client moved"))
				if got := f.request(t, "expiring-client", seatOpus, "", "execute"); got != wantSeat {
					t.Fatalf("placed on %s, want %s", got, wantSeat)
				}
				if move := f.lastMove(t); move == nil || *move != want {
					t.Fatalf("move tag = %+v, want %+v", move, want)
				}
				moved := len(f.events("claude routing: client moved")) - before
				if moved != map[bool]int{false: 1, true: 0}[sameSeat] {
					t.Fatalf("client moved lines = %d", moved)
				}
				clock.advance(time.Minute)
				f.request(t, "expiring-client", seatOpus, "", "execute")
				if move := f.lastMove(t); move != nil {
					t.Fatalf("the warm stay after the move was tagged: %+v", move)
				}
			})
		}
	}
}

// A moved request that fails and is retried on another seat is a new move with
// its own tag: the failed attempt (a warm stay) carried none.
func TestClaudeSeatMoveRetryIsItsOwnMove(t *testing.T) {
	f, clock := newSeatHoldFixture(t, false)
	first := f.request(t, "retry-client", seatOpus, "", "execute")
	second := otherSeat(f, first)
	setSeatOrganization(t, f, first, seatMoveOrgOne)
	setSeatOrganization(t, f, second, seatMoveOrgTwo)
	f.failSeat = first
	clock.advance(time.Minute)
	attempts := len(f.attempts)
	if got := f.request(t, "retry-client", seatOpus, "", "execute"); got != second {
		t.Fatalf("retry went to %s, want %s", got, second)
	}
	if got := f.attempts[attempts:]; len(got) != 2 || got[0] != first || got[1] != second {
		t.Fatalf("attempts = %v, want the failed seat then the other", got)
	}
	if f.moves[attempts] != nil {
		t.Fatalf("the failed warm attempt carried a move tag: %+v", f.moves[attempts])
	}
	want := cliproxyexecutor.ClaudeSeatMove{Relation: "other_org", Warm: true, Reason: "upstream_error"}
	if move := f.lastMove(t); move == nil || *move != want {
		t.Fatalf("retry move tag = %+v, want %+v", move, want)
	}
}

// A tag the caller's metadata already holds never reaches the executor of a
// request that is not a move.
func TestClaudeSeatMoveTagNeverRidesAlong(t *testing.T) {
	f, clock := newSeatHoldFixture(t, false)
	first := f.request(t, "stale-client", seatOpus, "", "execute")
	clock.advance(time.Minute)
	payload := []byte(`{"model":"` + seatOpus + `","messages":[{"role":"user","content":"hello"}]}`)
	stale := cliproxyexecutor.ClaudeSeatMove{Relation: "other_org", Warm: true, Reason: "disabled"}
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude, OriginalRequest: payload,
		Headers: http.Header{"X-Session-Id": []string{"stale-client"}},
		Metadata: map[string]any{cliproxyexecutor.CallerScopeMetadataKey: "shared-access-key-scope",
			cliproxyexecutor.ClaudeSeatMoveMetadataKey: stale}}
	response, errExecute := f.m.Execute(context.Background(), []string{"claude"}, cliproxyexecutor.Request{Model: seatOpus, Payload: payload}, opts)
	if errExecute != nil {
		t.Fatal(errExecute)
	}
	if string(response.Payload) != first {
		t.Fatalf("warm stay moved to %s", response.Payload)
	}
	if move := f.lastMove(t); move != nil {
		t.Fatalf("a stale tag reached the executor: %+v", move)
	}

	dst := map[string]any{cliproxyexecutor.ClaudeSeatMoveMetadataKey: stale}
	copyClaudeSeatMetadata(dst, map[string]any{})
	if _, ok := dst[cliproxyexecutor.ClaudeSeatMoveMetadataKey]; ok {
		t.Fatal("the copy kept a move tag the pick did not set")
	}
	fresh := cliproxyexecutor.ClaudeSeatMove{Relation: "same_org", Warm: true, Reason: "disabled"}
	copyClaudeSeatMetadata(dst, map[string]any{cliproxyexecutor.ClaudeSeatMoveMetadataKey: fresh})
	if dst[cliproxyexecutor.ClaudeSeatMoveMetadataKey] != fresh {
		t.Fatalf("the copy dropped the pick's move tag: %+v", dst)
	}
}
