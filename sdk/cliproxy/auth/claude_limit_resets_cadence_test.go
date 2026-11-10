package auth

import (
	"context"
	"encoding/json"
	"testing"
	"testing/synctest"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

type resetReadAttempt struct {
	reset bool
	at    time.Time
}
type scheduledResetPoller struct {
	isolatedResetPoller
	attempts chan resetReadAttempt
}

func (p *scheduledResetPoller) PollQuota(ctx context.Context, auth *Auth) (map[string]QuotaWindow, error) {
	p.attempts <- resetReadAttempt{at: time.Now()}
	return p.isolatedResetPoller.PollQuota(ctx, auth)
}
func (p *scheduledResetPoller) PollLimitResets(context.Context, *Auth) (json.RawMessage, error) {
	p.attempts <- resetReadAttempt{reset: true, at: time.Now()}
	return nil, &Error{HTTPStatus: 429}
}

type successfulScheduledResetPoller struct{ scheduledResetPoller }

func (p *successfulScheduledResetPoller) PollLimitResets(context.Context, *Auth) (json.RawMessage, error) {
	p.attempts <- resetReadAttempt{reset: true, at: time.Now()}
	return json.RawMessage(limitResetsFixture), nil
}

func TestLimitResetsPreserveRoutineReadsAtLongIntervals(t *testing.T) {
	for _, interval := range []time.Duration{time.Minute, 15 * time.Minute, 30 * time.Minute} {
		t.Run(interval.String(), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				m := NewManager(nil, nil, nil)
				cfg := &config.Config{}
				cfg.Claude.Quota.Enabled = true
				cfg.Claude.Quota.PollInterval = interval.String()
				m.SetConfig(cfg)
				if _, err := m.Register(context.Background(), &Auth{ID: "fixture", Provider: "claude", Status: StatusActive, Metadata: map[string]any{"access_token": "fixture-only"}}); err != nil {
					t.Fatal(err)
				}
				p := &successfulScheduledResetPoller{scheduledResetPoller{attempts: make(chan resetReadAttempt, 16)}}
				m.RegisterExecutor(p)
				m.StartQuotaPolling(context.Background())
				defer m.StopQuotaPolling()
				var previous resetReadAttempt
				resets := 0
				for i := 0; i < 8; i++ {
					attempt := <-p.attempts
					if attempt.reset && (i == 0 || previous.reset) {
						t.Fatal("reset reads displaced the next routine usage read")
					}
					if i > 0 && attempt.at.Sub(previous.at) < interval {
						t.Fatalf("usage requests only %s apart", attempt.at.Sub(previous.at))
					}
					if attempt.reset {
						resets++
					}
					previous = attempt
					synctest.Wait()
				}
				if resets == 0 {
					t.Fatal("no reset status was read")
				}
			})
		})
	}
}

func TestLimitResetsShareSpacedSlotsAndBackOff429(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := NewManager(nil, nil, nil)
		cfg := &config.Config{}
		cfg.Claude.Quota.Enabled = true
		m.SetConfig(cfg)
		if _, err := m.Register(context.Background(), &Auth{ID: "fixture", Provider: "claude", Status: StatusActive, Metadata: map[string]any{"access_token": "fixture-only"}}); err != nil {
			t.Fatal(err)
		}
		p := &scheduledResetPoller{attempts: make(chan resetReadAttempt, 1024)}
		m.RegisterExecutor(p)
		m.StartQuotaPolling(context.Background())
		defer m.StopQuotaPolling()
		synctest.Wait()
		<-time.After(65 * time.Minute)
		synctest.Wait()
		var previous, lastReset time.Time
		resets, routines := 0, 0
		for len(p.attempts) > 0 {
			attempt := <-p.attempts
			if !previous.IsZero() && attempt.at.Sub(previous) < time.Minute {
				t.Fatalf("usage requests only %s apart", attempt.at.Sub(previous))
			}
			if attempt.reset {
				if previous.IsZero() {
					t.Fatal("reset read took the first routine slot")
				}
				if !lastReset.IsZero() && attempt.at.Sub(lastReset) < 30*time.Minute {
					t.Fatal("429 reset read retried before backoff")
				}
				lastReset = attempt.at
				resets++
			} else {
				routines++
			}
			previous = attempt.at
		}
		if resets != 2 || routines < 45 {
			t.Fatalf("resets=%d routine=%d", resets, routines)
		}
		status := m.ClaudeLimitResetsCached("fixture")
		if !status.Stale || status.Error != "reset status poll failed (HTTP 429)" {
			t.Fatalf("status=%+v", status)
		}
	})
}
