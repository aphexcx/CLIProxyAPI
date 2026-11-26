package helps

import (
	"sort"
	"strings"
	"sync"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

// Kinds of metered Claude Messages requests.
const (
	ClaudeCacheKindMain     = "main"
	ClaudeCacheKindSubagent = "subagent"
	ClaudeCacheKindHelper   = "helper"
)

const (
	// ClaudeCacheClientUnrouted names requests the seat router gave no client hash.
	ClaudeCacheClientUnrouted = "unrouted"
	// ClaudeCacheClientOther collects clients beyond a day's row cap.
	ClaudeCacheClientOther = "other"

	claudeCacheClientHashLength = 12
	claudeCacheMaxRowsPerDay    = 2000
	claudeCacheDayLayout        = "2006-01-02"
)

var claudeCacheKinds = []string{ClaudeCacheKindMain, ClaudeCacheKindSubagent, ClaudeCacheKindHelper}

// ClaudeCacheScope names who a metered request belongs to. The Claude executor
// sets it on the usage reporter once it has classified the request.
type ClaudeCacheScope struct {
	Client      string
	Kind        string
	RuleApplied bool
}

// ClaudeCacheCounts are token sums for one row or one kind.
type ClaudeCacheCounts struct {
	Requests     int64 `json:"requests"`
	Write5m      int64 `json:"write_5m"`
	Write1h      int64 `json:"write_1h"`
	WriteUnsplit int64 `json:"write_unsplit"`
	Read         int64 `json:"read"`
}

// ClaudeCacheTotals sums a day by kind.
type ClaudeCacheTotals struct {
	Main     ClaudeCacheCounts `json:"main"`
	Subagent ClaudeCacheCounts `json:"subagent"`
	Helper   ClaudeCacheCounts `json:"helper"`
}

// ClaudeCacheRuleCounts counts the one-hour rule's outcomes for a day.
type ClaudeCacheRuleCounts struct {
	Applied int64 `json:"applied"`
	Refused int64 `json:"refused"`
}

// ClaudeCacheClientRow is one (client, kind) row of a day.
type ClaudeCacheClientRow struct {
	Client string `json:"client"`
	Kind   string `json:"kind"`
	ClaudeCacheCounts
	RuleApplied   int64  `json:"rule_applied"`
	FirstAt       string `json:"first_at"`
	LastAt        string `json:"last_at"`
	Last5mWriteAt string `json:"last_5m_write_at"`
}

// ClaudeCacheDay is one UTC day of the meter.
type ClaudeCacheDay struct {
	Day                     string                 `json:"day"`
	MainClientsWith5mWrites int                    `json:"main_clients_with_5m_writes"`
	Totals                  ClaudeCacheTotals      `json:"totals"`
	Rule                    ClaudeCacheRuleCounts  `json:"rule"`
	Clients                 []ClaudeCacheClientRow `json:"clients"`
}

// ClaudeCacheSnapshot is the meter as the management route reports it: today
// first, then yesterday when it has data. Since is when this process started
// counting; the meter is in memory and resets on restart.
type ClaudeCacheSnapshot struct {
	Since string           `json:"since"`
	Days  []ClaudeCacheDay `json:"days"`
}

type claudeCacheRowKey struct {
	client string
	kind   string
}

type claudeCacheRow struct {
	counts        ClaudeCacheCounts
	ruleApplied   int64
	firstAt       time.Time
	lastAt        time.Time
	last5mWriteAt time.Time
}

type claudeCacheDayRows struct {
	rows map[claudeCacheRowKey]*claudeCacheRow
	rule ClaudeCacheRuleCounts
}

// ClaudeCacheMeter counts Claude prompt-cache writes and reads per UTC day,
// client and kind. It is process-wide, in memory and mutex-guarded; it keeps
// today and yesterday and at most claudeCacheMaxRowsPerDay rows a day, after
// which a new client is summed into the client "other" (one row per kind, the
// only rows allowed past the cap). It stores no request or reply body.
type ClaudeCacheMeter struct {
	mu      sync.Mutex
	nowFunc func() time.Time
	maxRows int
	since   time.Time
	days    map[string]*claudeCacheDayRows
}

// NewClaudeCacheMeter returns an empty meter on the given clock (time.Now when nil).
func NewClaudeCacheMeter(nowFunc func() time.Time) *ClaudeCacheMeter {
	if nowFunc == nil {
		nowFunc = time.Now
	}
	return &ClaudeCacheMeter{
		nowFunc: nowFunc,
		maxRows: claudeCacheMaxRowsPerDay,
		since:   nowFunc().UTC(),
		days:    make(map[string]*claudeCacheDayRows),
	}
}

var defaultClaudeCacheMeter = NewClaudeCacheMeter(nil)

// DefaultClaudeCacheMeter returns the process-wide meter.
func DefaultClaudeCacheMeter() *ClaudeCacheMeter { return defaultClaudeCacheMeter }

// ClaudeCacheClient returns the meter's name for a request's client: the first
// 12 hex characters of the seat router's client hash, or "unrouted".
func ClaudeCacheClient(metadata map[string]any) string {
	hash, _ := metadata[cliproxyexecutor.ClaudeSeatClientHashMetadataKey].(string)
	hash = strings.TrimSpace(hash)
	if hash == "" {
		return ClaudeCacheClientUnrouted
	}
	if len(hash) > claudeCacheClientHashLength {
		hash = hash[:claudeCacheClientHashLength]
	}
	return hash
}

// ClaudeCacheKind classifies a request: a subagent, a helper (probe, title or
// helper-profile request), or a main conversation.
func ClaudeCacheKind(subagent, helper bool) string {
	switch {
	case subagent:
		return ClaudeCacheKindSubagent
	case helper:
		return ClaudeCacheKindHelper
	default:
		return ClaudeCacheKindMain
	}
}

func (m *ClaudeCacheMeter) dayLocked(now time.Time) *claudeCacheDayRows {
	today := now.Format(claudeCacheDayLayout)
	yesterday := now.AddDate(0, 0, -1).Format(claudeCacheDayLayout)
	for day := range m.days {
		if day != today && day != yesterday {
			delete(m.days, day)
		}
	}
	rows := m.days[today]
	if rows == nil {
		rows = &claudeCacheDayRows{rows: make(map[claudeCacheRowKey]*claudeCacheRow)}
		m.days[today] = rows
	}
	return rows
}

// Record adds one successful reply's cache usage to its scope's row.
func (m *ClaudeCacheMeter) Record(scope ClaudeCacheScope, detail usage.Detail) {
	if m == nil {
		return
	}
	client := strings.TrimSpace(scope.Client)
	if client == "" {
		client = ClaudeCacheClientUnrouted
	}
	kind := scope.Kind
	if kind != ClaudeCacheKindSubagent && kind != ClaudeCacheKindHelper {
		kind = ClaudeCacheKindMain
	}
	write5m, write1h, unsplit := claudeCacheWrites(detail)

	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.nowFunc().UTC()
	day := m.dayLocked(now)
	key := claudeCacheRowKey{client: client, kind: kind}
	row := day.rows[key]
	if row == nil {
		if len(day.rows) >= m.maxRows {
			key.client = ClaudeCacheClientOther
			row = day.rows[key]
		}
		if row == nil {
			row = &claudeCacheRow{firstAt: now}
			day.rows[key] = row
		}
	}
	row.counts.Requests++
	row.counts.Write5m += write5m
	row.counts.Write1h += write1h
	row.counts.WriteUnsplit += unsplit
	row.counts.Read += detail.CacheReadTokens
	row.lastAt = now
	if write5m > 0 {
		row.last5mWriteAt = now
	}
	if scope.RuleApplied {
		row.ruleApplied++
		day.rule.Applied++
	}
}

// RecordRuleRefused counts one upstream refusal of a request the one-hour rule changed.
func (m *ClaudeCacheMeter) RecordRuleRefused() {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.dayLocked(m.nowFunc().UTC()).rule.Refused++
}

// claudeCacheWrites splits a reply's cache writes by lifetime. Tokens the reply
// did not split are reported as unsplit.
func claudeCacheWrites(detail usage.Detail) (write5m, write1h, unsplit int64) {
	if detail.CacheCreation5mTokens != nil {
		write5m = *detail.CacheCreation5mTokens
	}
	if detail.CacheCreation1hTokens != nil {
		write1h = *detail.CacheCreation1hTokens
	}
	if rest := detail.CacheCreationTokens - write5m - write1h; rest > 0 {
		unsplit = rest
	}
	return write5m, write1h, unsplit
}

// Snapshot reports today and, when it has data, yesterday.
func (m *ClaudeCacheMeter) Snapshot() ClaudeCacheSnapshot {
	if m == nil {
		return ClaudeCacheSnapshot{Days: []ClaudeCacheDay{}}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.nowFunc().UTC()
	m.dayLocked(now)
	names := make([]string, 0, len(m.days))
	for day := range m.days {
		names = append(names, day)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names)))
	snapshot := ClaudeCacheSnapshot{Since: m.since.Format(time.RFC3339), Days: make([]ClaudeCacheDay, 0, len(names))}
	today := now.Format(claudeCacheDayLayout)
	for _, name := range names {
		rows := m.days[name]
		if name != today && len(rows.rows) == 0 && rows.rule == (ClaudeCacheRuleCounts{}) {
			continue
		}
		snapshot.Days = append(snapshot.Days, rows.snapshot(name))
	}
	return snapshot
}

func (d *claudeCacheDayRows) snapshot(name string) ClaudeCacheDay {
	day := ClaudeCacheDay{Day: name, Rule: d.rule, Clients: make([]ClaudeCacheClientRow, 0, len(d.rows))}
	for key, row := range d.rows {
		total := &day.Totals.Main
		switch key.kind {
		case ClaudeCacheKindSubagent:
			total = &day.Totals.Subagent
		case ClaudeCacheKindHelper:
			total = &day.Totals.Helper
		}
		total.Requests += row.counts.Requests
		total.Write5m += row.counts.Write5m
		total.Write1h += row.counts.Write1h
		total.WriteUnsplit += row.counts.WriteUnsplit
		total.Read += row.counts.Read
		if key.kind == ClaudeCacheKindMain && row.counts.Write5m > 0 {
			day.MainClientsWith5mWrites++
		}
		day.Clients = append(day.Clients, ClaudeCacheClientRow{
			Client:            key.client,
			Kind:              key.kind,
			ClaudeCacheCounts: row.counts,
			RuleApplied:       row.ruleApplied,
			FirstAt:           claudeCacheTime(row.firstAt),
			LastAt:            claudeCacheTime(row.lastAt),
			Last5mWriteAt:     claudeCacheTime(row.last5mWriteAt),
		})
	}
	sort.Slice(day.Clients, func(i, j int) bool {
		a, b := day.Clients[i], day.Clients[j]
		if a.Kind != b.Kind {
			return claudeCacheKindOrder(a.Kind) < claudeCacheKindOrder(b.Kind)
		}
		if a.Write5m != b.Write5m {
			return a.Write5m > b.Write5m
		}
		return a.Client < b.Client
	})
	return day
}

func claudeCacheKindOrder(kind string) int {
	for i, known := range claudeCacheKinds {
		if kind == known {
			return i
		}
	}
	return len(claudeCacheKinds)
}

func claudeCacheTime(at time.Time) string {
	if at.IsZero() {
		return ""
	}
	return at.Format(time.RFC3339)
}
