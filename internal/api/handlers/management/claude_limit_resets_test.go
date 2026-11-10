package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

type resetStatusPoller struct{ cachedUsagePoller }

func (p *resetStatusPoller) PollQuotaUsage(ctx context.Context, auth *coreauth.Auth) (map[string]coreauth.QuotaWindow, json.RawMessage, error) {
	windows, err := p.PollQuota(ctx, auth)
	if err != nil {
		return nil, nil, err
	}
	// The OAuth parser preserves the display-name alias used by newer responses.
	windows["Fable 5"] = windows["Fable"]
	delete(windows, "Fable")
	return windows, json.RawMessage(`{"five_hour":{"utilization":15}}`), nil
}

func (p *resetStatusPoller) PollLimitResets(context.Context, *coreauth.Auth) (json.RawMessage, error) {
	if p.fail.Load() {
		return nil, &coreauth.Error{HTTPStatus: 429}
	}
	weekly, expiry := time.Now().Add(72*time.Hour), time.Now().Add(7*24*time.Hour)
	body, _ := json.Marshal(gin.H{"cedar_ember": coreauth.ClaudeLimitResets{
		Eligible: true, AtLimit: true, WeeklyResetsAt: &weekly,
		Grants: []coreauth.ClaudeResetGrant{{ID: "synthetic-launch", Label: "Launch", ResetsLeft: 2, EndsAt: &expiry, Type: "weekly", Clears: []string{"seven_day_overage_included"}, UsableNow: true}},
	}})
	return body, nil
}

func TestLimitResetsReadsCacheOnlyAndPreservesFailureAge(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg := &config.Config{}
		cfg.Claude.Quota.Enabled = true
		m := coreauth.NewManager(nil, nil, nil)
		m.SetConfig(cfg)
		_, err := m.Register(context.Background(), &coreauth.Auth{ID: "fixture", FileName: "fixture.json", Provider: "claude", Status: coreauth.StatusActive, Metadata: map[string]any{"access_token": "never-sent-fixture"}})
		if err != nil {
			t.Fatal(err)
		}
		p := &resetStatusPoller{}
		m.RegisterExecutor(p)
		h := &Handler{cfg: cfg, authManager: m}
		read := func(path string, code int) []claudeLimitResetSeat {
			t.Helper()
			rec := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(rec)
			ctx.Request = httptest.NewRequest(http.MethodGet, path, nil)
			h.GetClaudeLimitResets(ctx)
			if rec.Code != code {
				t.Fatalf("HTTP %d: %s", rec.Code, rec.Body)
			}
			var response struct {
				Seats []claudeLimitResetSeat `json:"seats"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			return response.Seats
		}
		if got := read("/?auth=fixture.json", 200); len(got) != 1 || got[0].CedarEmber != nil || !got[0].Stale {
			t.Fatalf("unpolled=%+v", got)
		}
		if p.calls.Load() != 0 {
			t.Fatal("GET initiated a poll")
		}
		m.StartQuotaPolling(context.Background())
		t.Cleanup(m.StopQuotaPolling)
		synctest.Wait()
		<-time.After(70 * time.Second)
		synctest.Wait()
		for range 10 {
			got := read("/?auth=fixture.json", 200)
			if len(got) != 1 || got[0].CedarEmber == nil || got[0].CedarEmber.Grants[0].ResetsLeft != 2 || got[0].BestClickRank == nil || *got[0].BestClickRank != 1 || got[0].Stale {
				t.Fatalf("cached=%+v", got)
			}
		}
		read("/?auth=absent", 404)
		if p.calls.Load() != 1 {
			t.Fatal("GET bypassed quota cadence")
		}
		p.fail.Store(true)
		<-time.After(16 * time.Minute)
		synctest.Wait()
		got := read("/", 200)[0]
		if got.CedarEmber == nil || got.AgeSeconds == nil || *got.AgeSeconds < 960 || !got.Stale || got.Error != "reset status poll failed (HTTP 429)" || got.BestClickRank != nil {
			t.Fatalf("failed poll=%+v", got)
		}
	})
}

func TestLimitResetGenerationMatchesCredentialListing(t *testing.T) {
	ctx := context.Background()
	cfg := &config.Config{}
	cfg.Claude.Quota.Enabled = true
	m := coreauth.NewManager(nil, nil, nil)
	m.SetConfig(cfg)
	auth, err := m.Register(ctx, &coreauth.Auth{ID: "fixture", FileName: "fixture.json", Provider: "claude", Status: coreauth.StatusActive, Attributes: map[string]string{"path": "fixture.json"}, Metadata: map[string]any{"access_token": "fixture-only"}})
	if err != nil {
		t.Fatal(err)
	}
	h := &Handler{cfg: cfg, authManager: m}
	first := m.ClaudeLimitResetsCached(auth.ID).CredentialGeneration
	for _, token := range []string{"fixture-only", "replacement"} {
		auth.Metadata["access_token"] = token
		auth, err = m.Update(ctx, auth)
		if err != nil {
			t.Fatal(err)
		}
		entry := h.buildAuthFileEntry(auth, authFileQuotaContext{v8: true})
		generation := m.ClaudeLimitResetsCached(auth.ID).CredentialGeneration
		if generation == "" || entry["credential_generation"] != generation {
			t.Fatalf("listing=%v reset=%s", entry["credential_generation"], generation)
		}
		if token == "replacement" && generation == first {
			t.Fatal("replacement kept credential generation")
		}
	}
}

func TestLimitResetRanking(t *testing.T) {
	now := time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)
	seat := func(name string, usage float64, weeklyHours, expiryHours int) claudeLimitResetSeat {
		weekly, expiry := now.Add(time.Duration(weeklyHours)*time.Hour), now.Add(time.Duration(expiryHours)*time.Hour)
		s := claudeLimitResetSeat{Name: name, FableUtilization: &usage, ClaudeLimitResetsSnapshot: coreauth.ClaudeLimitResetsSnapshot{CedarEmber: &coreauth.ClaudeLimitResets{Eligible: true, WeeklyResetsAt: &weekly, Grants: []coreauth.ClaudeResetGrant{{ResetsLeft: 1, EndsAt: &expiry, UsableNow: true, Clears: []string{"seven_day_overage_included"}}}}}}
		s.RankReason = resetRankReason(&s, now)
		return s
	}
	seats := []claudeLimitResetSeat{seat("soon-weekly", 100, 24, 200), seat("near", 95, 120, 150), seat("wall-later-expiry", 100, 72, 240), seat("wall", 100, 72, 200), seat("wall-long-weekly", 100, 96, 240), seat("low", 50, 96, 200)}
	rankClaudeResetSeats(seats)
	want := map[string]int{"wall-long-weekly": 1, "wall": 2, "wall-later-expiry": 3, "near": 4}
	for _, s := range seats {
		if expected := want[s.Name]; expected != 0 {
			if s.BestClickRank == nil || *s.BestClickRank != expected {
				t.Fatalf("%s rank=%v", s.Name, s.BestClickRank)
			}
		} else if s.BestClickRank != nil {
			t.Fatalf("ranked %s", s.Name)
		}
	}
	for _, mutate := range []func(*claudeLimitResetSeat){
		func(s *claudeLimitResetSeat) { s.Stale = true },
		func(s *claudeLimitResetSeat) { s.CedarEmber.Grants[0].ResetsLeft = 0 },
		func(s *claudeLimitResetSeat) { s.CedarEmber.Grants[0].UsableNow = false },
		func(s *claudeLimitResetSeat) { s.CedarEmber.Grants[0].EndsAt = &now },
		func(s *claudeLimitResetSeat) { s.CedarEmber.Eligible = false },
		func(s *claudeLimitResetSeat) { s.CedarEmber.Grants[0].Clears = []string{"five_hour"} },
	} {
		s := seat("skip", 100, 72, 240)
		mutate(&s)
		if resetRankReason(&s, now) == "holder action only" {
			t.Fatal("unsafe recommendation")
		}
	}
}

func TestLimitResetSurfacePreservesReasonAndCannotRank(t *testing.T) {
	body, err := os.ReadFile("../../../auth/claude/testdata/limit_resets_surface.json")
	if err != nil {
		t.Fatal(err)
	}
	status, err := coreauth.ParseClaudeLimitResets(body)
	if err != nil {
		t.Fatal(err)
	}
	seat := claudeLimitResetSeat{ClaudeLimitResetsSnapshot: coreauth.ClaudeLimitResetsSnapshot{CedarEmber: status}}
	seat.RankReason = resetRankReason(&seat, time.Now())
	seats := []claudeLimitResetSeat{seat}
	rankClaudeResetSeats(seats)
	if status.Eligible || status.IneligibleReason == nil || *status.IneligibleReason != "surface" || seats[0].BestClickRank != nil || seats[0].RankReason != "seat ineligible" {
		t.Fatal("surface answer lost its meaning or became a zero-balance recommendation")
	}
}
