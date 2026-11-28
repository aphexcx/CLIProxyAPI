package helps

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
)

func moveDetail(read, written, input int64) usage.Detail {
	return usage.Detail{CacheReadTokens: read, CacheCreationTokens: written, InputTokens: input}
}

func moveScope(relation string, warm bool) ClaudeCacheScope {
	return ClaudeCacheScope{Client: "0123456789ab", Kind: ClaudeCacheKindMain,
		Move: &cliproxyexecutor.ClaudeSeatMove{Relation: relation, Warm: warm, Reason: "fixture_reason"}}
}

func newMoveMeter() *ClaudeCacheMeter {
	clock := &meterClock{now: time.Date(2026, 11, 28, 9, 0, 0, 0, time.UTC)}
	return NewClaudeCacheMeter(clock.read)
}

func captureMoveLog(t *testing.T) *logtest.Hook {
	t.Helper()
	previousHooks := log.StandardLogger().ReplaceHooks(make(log.LevelHooks))
	hook := logtest.NewGlobal()
	t.Cleanup(func() { log.StandardLogger().ReplaceHooks(previousHooks) })
	return hook
}

func moveOutcomeEntries(hook *logtest.Hook) []log.Entry {
	var entries []log.Entry
	for _, entry := range hook.AllEntries() {
		if entry.Message == "claude routing: move outcome" {
			entries = append(entries, *entry)
		}
	}
	return entries
}

// Each moved reply is filed under its relation and warm flag as KEPT or
// RE-PAID; a reply that is not a move is not filed in the moves block.
func TestClaudeCacheMeterFilesMovesByRelationAndWarm(t *testing.T) {
	meter := newMoveMeter()
	meter.Record(moveScope(cliproxyexecutor.ClaudeSeatMoveSameOrg, true), moveDetail(900, 50, 50))
	meter.Record(moveScope(cliproxyexecutor.ClaudeSeatMoveSameOrg, true), moveDetail(10, 900, 90))
	meter.Record(moveScope(cliproxyexecutor.ClaudeSeatMoveOtherOrg, false), moveDetail(0, 500, 10))
	meter.Record(moveScope(cliproxyexecutor.ClaudeSeatMoveOtherOrg, true), moveDetail(5, 700, 3))
	meter.Record(moveScope(cliproxyexecutor.ClaudeSeatMoveSameSeat, false), moveDetail(600, 0, 400))
	meter.Record(moveScope(cliproxyexecutor.ClaudeSeatMoveUnknownOrg, true), moveDetail(80, 20, 0))
	meter.Record(moveScope(cliproxyexecutor.ClaudeSeatMoveUnknownOrg, false), moveDetail(1, 20, 0))
	meter.Record(ClaudeCacheScope{Client: "0123456789ab", Kind: ClaudeCacheKindMain}, moveDetail(1000, 0, 0))

	day := meter.Snapshot().Days[0]
	want := []ClaudeCacheMoveRow{
		{Relation: "same_seat", Warm: false, Kept: 1, Read: 600},
		{Relation: "same_org", Warm: true, Kept: 1, RePaid: 1, Read: 910, Written: 950},
		{Relation: "other_org", Warm: true, RePaid: 1, Read: 5, Written: 700},
		{Relation: "other_org", Warm: false, RePaid: 1, Written: 500},
		{Relation: "unknown_org", Warm: true, Kept: 1, Read: 80, Written: 20},
		{Relation: "unknown_org", Warm: false, RePaid: 1, Read: 1, Written: 20},
	}
	if !reflect.DeepEqual(day.Moves, want) {
		t.Fatalf("moves = %+v\nwant  %+v", day.Moves, want)
	}
	if got := meterRow(t, day, "0123456789ab", ClaudeCacheKindMain).Requests; got != 8 {
		t.Fatalf("client row requests = %d, want every reply, moved or not, still counted (8)", got)
	}
	raw, errMarshal := json.Marshal(day)
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	if !strings.Contains(string(raw), `"moves":[{"relation":"same_seat","warm":false,"kept":1,"re_paid":0,"read":600,"written":0},`) {
		t.Fatalf("moves block JSON: %s", raw)
	}
}

// The KEPT rule at its edge: KEPT when the cache read is at least half of read
// + written + input, RE-PAID one token below, and a reply with none of the three
// is not filed.
func TestClaudeCacheMoveHalfThreshold(t *testing.T) {
	for _, tc := range []struct {
		read, written, input int64
		want                 string
	}{
		{read: 50, written: 30, input: 20, want: ClaudeCacheMoveKept},
		{read: 49, written: 31, input: 20, want: ClaudeCacheMoveRePaid},
		{read: 51, written: 31, input: 20, want: ClaudeCacheMoveKept},
		{read: 50, written: 31, input: 20, want: ClaudeCacheMoveRePaid},
		{read: 7, want: ClaudeCacheMoveKept},
		{input: 7, want: ClaudeCacheMoveRePaid},
		{written: 7, want: ClaudeCacheMoveRePaid},
		{want: ""},
	} {
		got, filed := claudeCacheMoveOutcome(tc.read, tc.written, tc.input)
		if got != tc.want || filed != (tc.want != "") {
			t.Fatalf("outcome(read %d, written %d, input %d) = %q, %v; want %q", tc.read, tc.written, tc.input, got, filed, tc.want)
		}
	}

	meter := newMoveMeter()
	meter.Record(moveScope(cliproxyexecutor.ClaudeSeatMoveSameOrg, true), moveDetail(50, 30, 20))
	meter.Record(moveScope(cliproxyexecutor.ClaudeSeatMoveSameOrg, true), moveDetail(49, 31, 20))
	meter.Record(moveScope(cliproxyexecutor.ClaudeSeatMoveSameOrg, true), moveDetail(0, 0, 0))
	five, hour := int64(20), int64(11)
	meter.Record(moveScope(cliproxyexecutor.ClaudeSeatMoveSameOrg, true),
		usage.Detail{CacheReadTokens: 30, CacheCreation5mTokens: &five, CacheCreation1hTokens: &hour, CacheCreationTokens: 31})
	want := []ClaudeCacheMoveRow{{Relation: "same_org", Warm: true, Kept: 1, RePaid: 2, Read: 129, Written: 92}}
	if got := meter.Snapshot().Days[0].Moves; !reflect.DeepEqual(got, want) {
		t.Fatalf("moves = %+v, want %+v", got, want)
	}
}

// A day with no move shows an empty moves block, and a relation the router
// does not name is filed as unknown_org.
func TestClaudeCacheMeterMovesBlockEmptyAndUnknownRelation(t *testing.T) {
	meter := newMoveMeter()
	raw, errMarshal := json.Marshal(meter.Snapshot())
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	if !strings.Contains(string(raw), `"moves":[]`) {
		t.Fatalf("empty day lacks an empty moves block: %s", raw)
	}
	meter.Record(moveScope("not_a_relation", true), moveDetail(1, 1, 1))
	want := []ClaudeCacheMoveRow{{Relation: "unknown_org", Warm: true, RePaid: 1, Read: 1, Written: 1}}
	if got := meter.Snapshot().Days[0].Moves; !reflect.DeepEqual(got, want) {
		t.Fatalf("moves = %+v, want %+v", got, want)
	}
}

// One "move outcome" line per filed reply, with the fields that find the case;
// no line for a reply that is not a move or reports no tokens.
func TestClaudeCacheMoveOutcomeLogLine(t *testing.T) {
	hook := captureMoveLog(t)
	meter := newMoveMeter()
	meter.Record(moveScope(cliproxyexecutor.ClaudeSeatMoveOtherOrg, false), moveDetail(10, 300, 5))
	meter.Record(ClaudeCacheScope{Client: "0123456789ab", Kind: ClaudeCacheKindMain}, moveDetail(10, 300, 5))
	meter.Record(moveScope(cliproxyexecutor.ClaudeSeatMoveOtherOrg, false), moveDetail(0, 0, 0))
	entries := moveOutcomeEntries(hook)
	if len(entries) != 1 {
		t.Fatalf("move outcome lines = %d, want 1", len(entries))
	}
	want := log.Fields{"provider": "claude", "client_hash": "0123456789ab", "kind": "main", "relation": "other_org", "warm": false,
		"reason": "fixture_reason", "outcome": "re_paid", "read": int64(10), "written": int64(300), "input": int64(5)}
	if !reflect.DeepEqual(entries[0].Data, want) {
		t.Fatalf("move outcome fields = %v, want %v", entries[0].Data, want)
	}
	if entries[0].Level != log.InfoLevel {
		t.Fatalf("move outcome level = %s, want info", entries[0].Level)
	}
}

// A failed or abandoned reply of a moved request is filed nowhere: only the
// successful record of a scoped reporter reaches the moves block.
func TestUsageReporterFilesMoveOnSuccessOnly(t *testing.T) {
	hook := captureMoveLog(t)
	movesTotal := func() int64 {
		var total int64
		for _, row := range DefaultClaudeCacheMeter().Snapshot().Days[0].Moves {
			total += row.Kept + row.RePaid
		}
		return total
	}
	move := &cliproxyexecutor.ClaudeSeatMove{Relation: cliproxyexecutor.ClaudeSeatMoveOtherOrg, Warm: true, Reason: "upstream_error"}
	client := "move" + time.Now().UTC().Format("150405.000")

	before := movesTotal()
	failed := &UsageReporter{provider: "claude"}
	failed.SetClaudeCacheScope(client, ClaudeCacheKindMain, false, move)
	failed.PublishFailure(context.Background(), errTestMeterFailure)
	abandoned := &UsageReporter{provider: "claude"}
	abandoned.SetClaudeCacheScope(client, ClaudeCacheKindMain, false, move)
	abandoned.AbandonAttempt()
	abandoned.Publish(context.Background(), moveDetail(10, 300, 5))
	if got := movesTotal() - before; got != 0 {
		t.Fatalf("failed and abandoned replies filed %d moves, want 0", got)
	}
	if got := len(moveOutcomeEntries(hook)); got != 0 {
		t.Fatalf("failed and abandoned replies logged %d move outcomes, want 0", got)
	}

	succeeded := &UsageReporter{provider: "claude"}
	succeeded.SetClaudeCacheScope(client, ClaudeCacheKindMain, false, move)
	succeeded.Publish(context.Background(), moveDetail(10, 300, 5))
	if got := movesTotal() - before; got != 1 {
		t.Fatalf("successful reply filed %d moves, want 1", got)
	}
}

func TestClaudeCacheMoveFromMetadata(t *testing.T) {
	if got := ClaudeCacheMove(nil); got != nil {
		t.Fatalf("no metadata: %+v", got)
	}
	if got := ClaudeCacheMove(map[string]any{cliproxyexecutor.ClaudeSeatMoveMetadataKey: "same_org"}); got != nil {
		t.Fatalf("a value of another type: %+v", got)
	}
	tag := cliproxyexecutor.ClaudeSeatMove{Relation: cliproxyexecutor.ClaudeSeatMoveSameOrg, Warm: true, Reason: "disabled"}
	got := ClaudeCacheMove(map[string]any{cliproxyexecutor.ClaudeSeatMoveMetadataKey: tag})
	if got == nil || *got != tag {
		t.Fatalf("move = %+v, want %+v", got, tag)
	}
}
