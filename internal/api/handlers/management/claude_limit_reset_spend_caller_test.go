package management

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
)

func (f *resetSpendFixture) spendWithCaller(caller context.Context, key, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequestWithContext(caller, http.MethodPost, "/v8/management/claude/limit-resets/spend", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Request.Header.Set("Idempotency-Key", key)
	f.h.SpendClaudeLimitReset(c)
	return rec
}

func TestClaudeResetSpendSettlesAfterCallerLeaves(t *testing.T) {
	for _, stage := range []string{"after sent", "during claim", "during balance read"} {
		t.Run(stage, func(t *testing.T) {
			f := newResetSpendFixture(t, resetFixtureGrants()...)
			f.h.cfg.Claude.Quota.Enabled = true
			f.h.authManager.SetConfig(f.h.cfg)
			caller, cancel := context.WithCancel(t.Context())
			defer cancel()
			switch stage {
			case "after sent":
				path := filepath.Join(f.logDir, claudeResetSpendLedgerName)
				f.h.claudeResetSpends.now = func() time.Time {
					raw, _ := os.ReadFile(path)
					if bytes.Contains(raw, []byte(`"phase":"sent"`)) {
						cancel()
					}
					return f.clock.now()
				}
			case "during claim":
				f.provider.onClaim = func(fakeResetClaim) { cancel() }
			case "during balance read":
				var reads atomic.Int32
				f.provider.onRead = func() {
					if reads.Add(1) == 2 {
						cancel()
					}
				}
			}
			const key = "fixture-disconnected-spend"
			body := spendBody(map[string]any{"auth_index": f.index, "kind": "fable"})
			rec := f.spendWithCaller(caller, key, body)
			var answer map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &answer); err != nil {
				t.Fatal(err)
			}
			if caller.Err() == nil || rec.Code != http.StatusOK || answer["result"] != "reset" || answer["after_read_error"] != nil {
				t.Errorf("caller canceled=%t, status=%d, result=%v, balance read failed=%t", caller.Err() != nil, rec.Code, answer["result"], answer["after_read_error"] != nil)
			}
			records := f.ledgerRecords(t)
			if len(records) != 3 || records[0]["phase"] != "sent" || records[1]["phase"] != "answered" || records[1]["result"] != "reset" || records[2]["phase"] != "balance" {
				t.Errorf("expected sent, answered with reset, and balance on disk, got %d records", len(records))
			}
			for _, record := range records {
				if record["operation"] != claudeResetOperation(key) || record["request_id"] != answer["request_id"] {
					t.Error("ledger record belongs to a different spend")
				}
			}
			if cache := f.h.authManager.ClaudeLimitResetsCached("fixture"); cache.Error != "" || cache.CedarEmber == nil {
				t.Error("the cache lacks a successful grants read")
			}
			code, replay := f.spendKeyed(t, key, body)
			after, _ := replay["resets_left_after"].(map[string]any)
			if code != http.StatusOK || replay["replayed"] != true || replay["result"] != "reset" || after["fable"] != float64(1) || replay["retry_until"] != nil {
				t.Errorf("replay status=%d, result=%v, fable balance=%v", code, replay["result"], after["fable"])
			}
			if _, _, claims := f.provider.counts(); len(claims) != 1 {
				t.Errorf("provider received %d claims, want 1", len(claims))
			}
		})
	}
}

func TestClaudeResetSpendCallerLeavesDuringPreflight(t *testing.T) {
	for _, stage := range []string{"organization read", "fresh grants read"} {
		t.Run(stage, func(t *testing.T) {
			f := newResetSpendFixture(t, resetFixtureGrants()...)
			caller, cancel := context.WithCancel(t.Context())
			defer cancel()
			if stage == "organization read" {
				f.provider.onProfile = cancel
			} else {
				f.provider.onRead = cancel
			}
			const key = "fixture-preflight"
			body := spendBody(map[string]any{"auth_index": f.index, "kind": "fable"})
			if rec := f.spendWithCaller(caller, key, body); rec.Code != http.StatusBadGateway {
				t.Fatalf("canceled read returned %d", rec.Code)
			}
			if _, _, claims := f.provider.counts(); len(claims) != 0 {
				t.Fatal("a canceled read sent a claim")
			}
			raw, err := os.ReadFile(filepath.Join(f.logDir, claudeResetSpendLedgerName))
			if (err != nil && !os.IsNotExist(err)) || len(raw) != 0 {
				t.Fatal("expected an empty or absent ledger after a canceled read")
			}
			code, answer := f.spendKeyed(t, key, body)
			if code != http.StatusOK || answer["replayed"] == true || answer["result"] != "reset" {
				t.Fatalf("new delivery status=%d, result=%v", code, answer["result"])
			}
			if _, _, claims := f.provider.counts(); len(claims) != 1 {
				t.Errorf("new delivery sent %d claims, want 1", len(claims))
			}
		})
	}
}

func TestClaudeResetSpendHoldsKeyAndSeatAfterCallerLeaves(t *testing.T) {
	f := newResetSpendFixture(t, resetFixtureGrants()...)
	caller, cancel := context.WithCancel(t.Context())
	defer cancel()
	arrived, release := make(chan struct{}), make(chan struct{})
	var received atomic.Int32
	f.provider.onClaim = func(fakeResetClaim) {
		if received.Add(1) == 1 {
			cancel()
			close(arrived)
			<-release
		}
	}
	const key = "fixture-held-spend"
	body := spendBody(map[string]any{"auth_index": f.index, "kind": "fable"})
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.spendWithCaller(caller, key, body)
	}()
	finish := sync.OnceFunc(func() {
		close(release)
		<-done
	})
	defer finish()
	select {
	case <-done:
		t.Fatal("handler returned before the held claim was observed")
	case <-arrived:
	}
	for _, tc := range []struct{ key, message string }{
		{key, "a spend with this Idempotency-Key is in flight and its outcome is not recorded yet: deliver it again once it has answered"},
		{"fixture-other-spend", "a spend is already in flight for this seat; nothing spent"},
	} {
		code, answer := f.spendKeyed(t, tc.key, body)
		if code != http.StatusConflict || answer["error"] != tc.message {
			t.Errorf("held claim: status=%d, error=%v", code, answer["error"])
		}
	}
	if got := received.Load(); got != 1 {
		t.Errorf("provider received %d claims while held", got)
	}
	finish()
	code, replay := f.spendKeyed(t, key, body)
	if code != http.StatusOK || replay["replayed"] != true || replay["result"] != "reset" {
		t.Errorf("settled replay status=%d, result=%v", code, replay["result"])
	}
	if _, _, claims := f.provider.counts(); len(claims) != 1 {
		t.Errorf("provider recorded %d claims after replay", len(claims))
	}
}

func TestClaudeResetSpendLogsCompletionForDepartedCaller(t *testing.T) {
	hook := new(logtest.Hook)
	previous := log.StandardLogger().ReplaceHooks(log.LevelHooks{})
	log.AddHook(hook)
	t.Cleanup(func() { log.StandardLogger().ReplaceHooks(previous) })
	f := newResetSpendFixture(t, resetFixtureGrants()...)
	body := spendBody(map[string]any{"auth_index": f.index, "kind": "fable"})
	caller, cancel := context.WithCancel(t.Context())
	defer cancel()
	f.provider.onClaim = func(fakeResetClaim) { cancel() }
	f.spendWithCaller(caller, "fixture-log-completion", body)
	_, _, claims := f.provider.counts()
	if len(claims) != 1 {
		t.Fatalf("provider recorded %d claims", len(claims))
	}
	count := 0
	for _, entry := range hook.AllEntries() {
		if entry.Message != "Claude reset spend finished after caller left" {
			continue
		}
		count++
		if entry.Level != log.InfoLevel || len(entry.Data) != 3 || entry.Data["auth_index"] != f.index || entry.Data["kind"] != "fable" || entry.Data["request_id"] != claims[0].RequestID {
			t.Error("completion log must contain only the seat index, kind and claim request ID at Info")
		}
		line, err := entry.String()
		if err != nil || strings.Contains(line, resetFixtureToken) || strings.Contains(line, resetFixtureOrganization) || strings.Contains(line, "fixture@example.invalid") {
			t.Error("completion log contains private data or could not be formatted")
		}
	}
	if count != 1 {
		t.Errorf("got %d completion log lines, want 1", count)
	}
}
