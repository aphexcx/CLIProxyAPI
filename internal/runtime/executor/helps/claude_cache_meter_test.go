package helps

import (
	"context"
	"net/http"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

type meterClock struct{ now time.Time }

func (c *meterClock) read() time.Time { return c.now }

func meterDetail(write5m, write1h *int64, creation, read int64) usage.Detail {
	return usage.Detail{CacheCreation5mTokens: write5m, CacheCreation1hTokens: write1h, CacheCreationTokens: creation, CacheReadTokens: read}
}

func meterRow(t *testing.T, day ClaudeCacheDay, client, kind string) ClaudeCacheClientRow {
	t.Helper()
	for _, row := range day.Clients {
		if row.Client == client && row.Kind == kind {
			return row
		}
	}
	t.Fatalf("no row for %s/%s in %+v", client, kind, day.Clients)
	return ClaudeCacheClientRow{}
}

// Test 5: sums per day, client and kind on an injected clock, no sleep.
func TestClaudeCacheMeterSumsPerDayClientAndKind(t *testing.T) {
	clock := &meterClock{now: time.Date(2026, 11, 26, 9, 0, 0, 0, time.UTC)}
	meter := NewClaudeCacheMeter(clock.read)
	meter.Record(ClaudeCacheScope{Client: "aaaaaaaaaaaa", Kind: ClaudeCacheKindMain, RuleApplied: true}, meterDetail(nil, usage.Int64Ptr(100), 100, 40))
	clock.now = clock.now.Add(time.Minute)
	meter.Record(ClaudeCacheScope{Client: "aaaaaaaaaaaa", Kind: ClaudeCacheKindMain}, meterDetail(usage.Int64Ptr(30), usage.Int64Ptr(0), 30, 10))
	meter.Record(ClaudeCacheScope{Client: "aaaaaaaaaaaa", Kind: ClaudeCacheKindSubagent}, meterDetail(usage.Int64Ptr(50), nil, 50, 5))
	meter.Record(ClaudeCacheScope{Kind: ClaudeCacheKindHelper}, meterDetail(nil, nil, 0, 0))

	snapshot := meter.Snapshot()
	if snapshot.Since != "2026-11-26T09:00:00Z" {
		t.Fatalf("since = %q", snapshot.Since)
	}
	if len(snapshot.Days) != 1 || snapshot.Days[0].Day != "2026-11-26" {
		t.Fatalf("days = %+v", snapshot.Days)
	}
	day := snapshot.Days[0]
	main := meterRow(t, day, "aaaaaaaaaaaa", ClaudeCacheKindMain)
	if main.Requests != 2 || main.Write5m != 30 || main.Write1h != 100 || main.Read != 50 || main.RuleApplied != 1 {
		t.Fatalf("main row = %+v", main)
	}
	if main.FirstAt != "2026-11-26T09:00:00Z" || main.LastAt != "2026-11-26T09:01:00Z" || main.Last5mWriteAt != "2026-11-26T09:01:00Z" {
		t.Fatalf("main row times = %+v", main)
	}
	if sub := meterRow(t, day, "aaaaaaaaaaaa", ClaudeCacheKindSubagent); sub.Write5m != 50 || sub.Requests != 1 {
		t.Fatalf("subagent row = %+v", sub)
	}
	if helper := meterRow(t, day, ClaudeCacheClientUnrouted, ClaudeCacheKindHelper); helper.Requests != 1 {
		t.Fatalf("helper row = %+v", helper)
	}
	if day.Totals.Main.Write1h != 100 || day.Totals.Main.Write5m != 30 || day.Totals.Subagent.Write5m != 50 || day.Totals.Helper.Requests != 1 {
		t.Fatalf("totals = %+v", day.Totals)
	}
	if day.MainClientsWith5mWrites != 1 {
		t.Fatalf("main_clients_with_5m_writes = %d, want 1", day.MainClientsWith5mWrites)
	}
	if day.Rule.Applied != 1 {
		t.Fatalf("rule.applied = %d, want 1", day.Rule.Applied)
	}
}

func TestClaudeCacheMeterUnsplitWrite(t *testing.T) {
	meter := NewClaudeCacheMeter((&meterClock{now: time.Date(2026, 11, 26, 9, 0, 0, 0, time.UTC)}).read)
	meter.Record(ClaudeCacheScope{Client: "bbbbbbbbbbbb", Kind: ClaudeCacheKindMain}, meterDetail(nil, nil, 70, 0))
	row := meterRow(t, meter.Snapshot().Days[0], "bbbbbbbbbbbb", ClaudeCacheKindMain)
	if row.WriteUnsplit != 70 || row.Write5m != 0 || row.Write1h != 0 || row.Last5mWriteAt != "" {
		t.Fatalf("unsplit row = %+v", row)
	}
	if meter.Snapshot().Days[0].MainClientsWith5mWrites != 0 {
		t.Fatal("an unsplit write counted as a five-minute write")
	}
}

func TestClaudeCacheMeterKeepsTodayAndYesterday(t *testing.T) {
	clock := &meterClock{now: time.Date(2026, 11, 25, 23, 59, 0, 0, time.UTC)}
	meter := NewClaudeCacheMeter(clock.read)
	meter.Record(ClaudeCacheScope{Client: "cccccccccccc", Kind: ClaudeCacheKindMain}, meterDetail(usage.Int64Ptr(1), nil, 1, 0))
	clock.now = clock.now.Add(2 * time.Minute)
	meter.Record(ClaudeCacheScope{Client: "cccccccccccc", Kind: ClaudeCacheKindMain}, meterDetail(usage.Int64Ptr(2), nil, 2, 0))
	days := meter.Snapshot().Days
	if len(days) != 2 || days[0].Day != "2026-11-26" || days[1].Day != "2026-11-25" {
		t.Fatalf("days after rollover = %+v", days)
	}
	if days[0].Totals.Main.Write5m != 2 || days[1].Totals.Main.Write5m != 1 {
		t.Fatalf("per-day sums = %+v / %+v", days[0].Totals, days[1].Totals)
	}
	clock.now = clock.now.Add(24 * time.Hour)
	days = meter.Snapshot().Days
	if len(days) != 2 || days[0].Day != "2026-11-27" || days[1].Day != "2026-11-26" || len(days[0].Clients) != 0 {
		t.Fatalf("days after a second rollover = %+v", days)
	}
	clock.now = clock.now.Add(24 * time.Hour)
	if days = meter.Snapshot().Days; len(days) != 1 || days[0].Day != "2026-11-28" {
		t.Fatalf("an empty yesterday is not reported: %+v", days)
	}
}

func TestClaudeCacheMeterFoldsOverflowIntoOther(t *testing.T) {
	meter := NewClaudeCacheMeter((&meterClock{now: time.Date(2026, 11, 26, 9, 0, 0, 0, time.UTC)}).read)
	meter.maxRows = 2
	for _, client := range []string{"c1", "c2", "c3", "c4", "c1"} {
		meter.Record(ClaudeCacheScope{Client: client, Kind: ClaudeCacheKindMain}, meterDetail(usage.Int64Ptr(10), nil, 10, 0))
	}
	day := meter.Snapshot().Days[0]
	if got := meterRow(t, day, "c1", ClaudeCacheKindMain).Requests; got != 2 {
		t.Fatalf("c1 requests = %d, want 2", got)
	}
	other := meterRow(t, day, ClaudeCacheClientOther, ClaudeCacheKindMain)
	if other.Requests != 2 || other.Write5m != 20 {
		t.Fatalf("other row = %+v, want c3 and c4 folded in", other)
	}
	if len(day.Clients) != 3 {
		t.Fatalf("rows = %+v, want c1, c2 and other", day.Clients)
	}
}

func TestClaudeCacheMeterCountsRefusals(t *testing.T) {
	meter := NewClaudeCacheMeter((&meterClock{now: time.Date(2026, 11, 26, 9, 0, 0, 0, time.UTC)}).read)
	meter.RecordRuleRefused()
	if got := meter.Snapshot().Days[0].Rule.Refused; got != 1 {
		t.Fatalf("refused = %d, want 1", got)
	}
}

func TestClaudeCacheClientAndKind(t *testing.T) {
	if got := ClaudeCacheClient(nil); got != ClaudeCacheClientUnrouted {
		t.Fatalf("no metadata: %q", got)
	}
	metadata := map[string]any{cliproxyexecutor.ClaudeSeatClientHashMetadataKey: "0123456789abcdef0123"}
	if got := ClaudeCacheClient(metadata); got != "0123456789ab" {
		t.Fatalf("client = %q, want the first 12 characters", got)
	}
	for _, tc := range []struct {
		subagent, helper bool
		want             string
	}{
		{want: ClaudeCacheKindMain},
		{subagent: true, want: ClaudeCacheKindSubagent},
		{helper: true, want: ClaudeCacheKindHelper},
	} {
		if got := ClaudeCacheKind(tc.subagent, tc.helper); got != tc.want {
			t.Fatalf("kind(%v, %v) = %q, want %q", tc.subagent, tc.helper, got, tc.want)
		}
	}
}

// The feed: only a successful record of a scoped reporter reaches the meter.
func TestUsageReporterFeedsClaudeCacheMeterOnSuccessOnly(t *testing.T) {
	client := "feed" + time.Now().UTC().Format("150405.000")
	reporter := &UsageReporter{provider: "claude"}
	reporter.SetClaudeCacheScope(client, ClaudeCacheKindMain, false)
	reporter.Publish(context.Background(), meterDetail(usage.Int64Ptr(5), nil, 5, 0))
	failed := &UsageReporter{provider: "claude"}
	failed.SetClaudeCacheScope(client, ClaudeCacheKindHelper, false)
	failed.PublishFailure(context.Background(), errTestMeterFailure)
	unscoped := &UsageReporter{provider: "claude"}
	unscoped.Publish(context.Background(), meterDetail(usage.Int64Ptr(5), nil, 5, 0))
	abandoned := &UsageReporter{provider: "claude"}
	abandoned.SetClaudeCacheScope(client, ClaudeCacheKindSubagent, false)
	abandoned.AbandonAttempt()
	abandoned.Publish(context.Background(), meterDetail(usage.Int64Ptr(5), nil, 5, 0))

	day := DefaultClaudeCacheMeter().Snapshot().Days[0]
	rows := 0
	for _, row := range day.Clients {
		if row.Client == client {
			rows++
			if row.Kind != ClaudeCacheKindMain || row.Write5m != 5 {
				t.Fatalf("unexpected row %+v", row)
			}
		}
	}
	if rows != 1 {
		t.Fatalf("rows for the client = %d, want only the successful main record", rows)
	}
}

var errTestMeterFailure = &meterTestError{}

type meterTestError struct{}

func (*meterTestError) Error() string { return "fixture failure" }

func TestIsClaudeCacheLifetimeRefusal(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
		want   bool
	}{
		{http.StatusBadRequest, `{"type":"error","error":{"type":"invalid_request_error","message":"system.1.cache_control.ttl: not available"}}`, true},
		{http.StatusBadRequest, `{"type":"error","error":{"message":"extended cache-ttl beta required"}}`, true},
		{http.StatusBadRequest, `{"type":"error","error":{"message":"invalid TTL value"}}`, true},
		{http.StatusBadRequest, `{"type":"error","error":{"message":"request throttled; throttle limit"}}`, false},
		{http.StatusBadRequest, `{"type":"error","error":{"message":"messages: text content blocks must be non-empty"}}`, false},
		{http.StatusBadRequest, `{"type":"error","error":{"message":"ok"},"echo":"cache_control"}`, false},
		{http.StatusBadRequest, `plain text naming cache_control`, true},
		{http.StatusTooManyRequests, `{"type":"error","error":{"message":"cache_control"}}`, false},
	} {
		if got := IsClaudeCacheLifetimeRefusal(tc.status, []byte(tc.body)); got != tc.want {
			t.Fatalf("IsClaudeCacheLifetimeRefusal(%d, %s) = %v, want %v", tc.status, tc.body, got, tc.want)
		}
	}
}

func TestClaudeOneHourCacheRuleOffContext(t *testing.T) {
	if ClaudeOneHourCacheRuleOff(context.Background()) {
		t.Fatal("rule off without the flag")
	}
	if !ClaudeOneHourCacheRuleOff(WithClaudeOneHourCacheRuleOff(context.Background())) {
		t.Fatal("rule on with the flag")
	}
}
