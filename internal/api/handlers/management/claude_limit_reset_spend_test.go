package management

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	claudeauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/claude"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
)

// Every value below is a fixture. No test in this file reaches the real
// provider, holds a real token or spends a real reset: the HTTP client dials
// only the local TLS server that stands in for api.anthropic.com.
const (
	resetFixtureOrganization      = "00000000-0000-4000-8000-0000000000aa"
	resetFixtureToken             = "fixture-access-token"
	resetFixtureOtherOrganization = "00000000-0000-4000-8000-0000000000bb"
	resetFixtureOtherToken        = "fixture-access-token-other"
)

// resetFixtureAccounts maps each fixture token to its account's organization.
// The fixture credentials of the non-OAuth seats are listed too, so a spend
// that wrongly sends one fails the test's own assertion, not the fake's.
var resetFixtureAccounts = map[string]string{
	resetFixtureToken: resetFixtureOrganization, resetFixtureOtherToken: resetFixtureOtherOrganization,
	"fixture-stale-access-token": resetFixtureOrganization, "fixture-api-key": resetFixtureOrganization,
	"fixture-session-token": resetFixtureOrganization, "fixture-generic-token": resetFixtureOrganization,
	"fixture-id-token": resetFixtureOrganization,
}

// resetFixtureEpoch is the fixture clock's start; every fixture time derives
// from it, never from the machine's own clock.
var resetFixtureEpoch = time.Date(2026, 11, 19, 8, 0, 0, 0, time.UTC)

type fakeResetGrant struct {
	ID              string   `json:"id"`
	Label           string   `json:"label"`
	ResetsLeft      int      `json:"resets_left"`
	EndsAt          string   `json:"ends_at,omitempty"`
	Type            string   `json:"type,omitempty"`
	GivesBackWeekly bool     `json:"gives_back_weekly"`
	Clears          []string `json:"clears"`
	UsableNow       bool     `json:"usable_now"`
	Paused          bool     `json:"paused"`
}

type fakeResetClaim struct {
	Program   string `json:"program"`
	GrantID   string `json:"grant_id"`
	RequestID string `json:"request_id"`
}

type fakeResetProvider struct {
	t      *testing.T
	server *httptest.Server

	mu            sync.Mutex
	grants        []fakeResetGrant
	eligible      bool
	reason        string
	cooldownUntil string
	profileStatus int
	readStatus    int
	claimStatus   int
	claimResult   string
	requests      int
	reads         int
	claims        []fakeResetClaim
	bearers       []string
	cacheControls []string
	noWindows     bool
	claimReason   string
	onClaim       func(fakeResetClaim)
	onRead        func()
	onProfile     func()
}

func newFakeResetProvider(t *testing.T, grants ...fakeResetGrant) *fakeResetProvider {
	t.Helper()
	f := &fakeResetProvider{t: t, grants: grants, eligible: true, claimResult: "reset"}
	f.server = httptest.NewTLSServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeResetProvider) serve(w http.ResponseWriter, r *http.Request) {
	// Hooks run outside the lock, so a hook that blocks one claim does not
	// block the other seats' reads.
	var claim fakeResetClaim
	isClaim := r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/reset_rate_limits")
	if isClaim {
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&claim); err != nil || claim.Program != "cedar_ember" || r.Header.Get("Content-Type") != "application/json" {
			f.t.Errorf("malformed claim: %v %+v", err, claim)
		}
	}
	f.mu.Lock()
	onRead, onProfile, onClaim := f.onRead, f.onProfile, f.onClaim
	f.mu.Unlock()
	switch {
	case isClaim && onClaim != nil:
		onClaim(claim)
	case r.URL.Path == "/api/oauth/usage" && onRead != nil:
		onRead()
	case r.URL.Path == "/api/oauth/profile" && onProfile != nil:
		onProfile()
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests++
	f.bearers = append(f.bearers, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	if r.Host != "api.anthropic.com" {
		f.t.Errorf("request for host %q", r.Host)
	}
	// Every spend request closes its connection: no socket outlives a spend.
	if !r.Close {
		f.t.Errorf("request %s %s keeps its connection alive", r.Method, r.URL.Path)
	}
	organization := resetFixtureAccounts[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")]
	if organization == "" || r.Header.Get("Anthropic-Beta") != "oauth-2025-04-20" || !strings.HasPrefix(r.Header.Get("User-Agent"), "claude-cli/") {
		f.t.Errorf("unexpected headers for %s: %v", r.URL.Path, r.Header)
	}
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/api/oauth/profile":
		if f.profileStatus != 0 {
			w.WriteHeader(f.profileStatus)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"account":      map[string]any{"uuid": "00000000-0000-4000-8000-000000000001", "email": "fixture@example.invalid"},
			"organization": map[string]any{"uuid": organization},
		})
	case r.Method == http.MethodGet && r.URL.Path == "/api/oauth/usage" && r.URL.RawQuery == "cedar_ember=1&skip_spend=1":
		f.reads++
		f.cacheControls = append(f.cacheControls, r.Header.Get("Cache-Control"))
		if f.readStatus != 0 {
			w.WriteHeader(f.readStatus)
			return
		}
		status := map[string]any{"eligible": f.eligible, "ineligible_reason": nil, "at_limit": true, "grants": f.grants}
		if f.reason != "" {
			status["ineligible_reason"] = f.reason
		}
		if f.cooldownUntil != "" {
			status["cooldown_until"] = f.cooldownUntil
		}
		if f.noWindows {
			_ = json.NewEncoder(w).Encode(map[string]any{"cedar_ember": status})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"five_hour": map[string]any{"utilization": 100}, "cedar_ember": status})
	case isClaim && r.URL.Path == "/api/organizations/"+organization+"/reset_rate_limits" && r.URL.RawQuery == "":
		f.claims = append(f.claims, claim)
		if f.claimStatus != 0 {
			w.WriteHeader(f.claimStatus)
			return
		}
		left := 0
		for i := range f.grants {
			if f.grants[i].ID == claim.GrantID && f.claimResult == "reset" {
				f.grants[i].ResetsLeft--
				left = f.grants[i].ResetsLeft
			}
		}
		answer := map[string]any{"result": f.claimResult, "resets_left": left, "cleared": []string{"seven_day"}}
		if f.claimReason != "" {
			answer["reason"] = f.claimReason
		}
		_ = json.NewEncoder(w).Encode(answer)
	default:
		f.t.Errorf("unexpected provider request %s %s", r.Method, r.URL)
		w.WriteHeader(http.StatusNotFound)
	}
}

// client dials only the fake, whatever host a request names.
func (f *fakeResetProvider) client() *http.Client {
	transport := f.server.Client().Transport.(*http.Transport).Clone()
	address := f.server.Listener.Addr().String()
	transport.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, address)
	}
	transport.TLSClientConfig.ServerName = "example.com"
	return &http.Client{Transport: transport}
}

func (f *fakeResetProvider) counts() (requests, reads int, claims []fakeResetClaim) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests, f.reads, append([]fakeResetClaim(nil), f.claims...)
}

func (f *fakeResetProvider) set(update func(*fakeResetProvider)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	update(f)
}

func resetFixtureGrants() []fakeResetGrant {
	base := resetFixtureEpoch
	day := func(days int) string { return base.Add(time.Duration(days) * 24 * time.Hour).Format(time.RFC3339) }
	return []fakeResetGrant{
		{ID: "fable-late", Label: "Fixture Fable", ResetsLeft: 1, EndsAt: day(60), Type: "weekly", GivesBackWeekly: true, Clears: []string{"seven_day_overage_included"}, UsableNow: true},
		{ID: "fable-soon", Label: "Fixture Fable", ResetsLeft: 1, EndsAt: day(30), Type: "full", GivesBackWeekly: true, Clears: []string{"seven_day_overage_included"}, UsableNow: true},
		{ID: "full-b", Label: "Fixture full", ResetsLeft: 1, EndsAt: day(20), Type: "full", Clears: []string{"five_hour", "seven_day"}, UsableNow: true},
		{ID: "full-a", Label: "Fixture full", ResetsLeft: 1, EndsAt: day(20), Type: "full", Clears: []string{"five_hour", "seven_day"}, UsableNow: true},
		{ID: "weekly-1", Label: "Fixture weekly", ResetsLeft: 1, EndsAt: day(10), Type: "weekly", Clears: []string{"seven_day"}, UsableNow: true},
		{ID: "session-1", Label: "Fixture session", ResetsLeft: 1, EndsAt: day(5), Type: "session", Clears: []string{"five_hour"}, UsableNow: true},
		{ID: "turbo-1", Label: "Fixture unknown", ResetsLeft: 3, EndsAt: day(1), Type: "turbo", Clears: []string{"seven_day"}, UsableNow: true},
		{ID: "untyped-1", Label: "Fixture untyped", ResetsLeft: 3, EndsAt: day(1), Clears: []string{"seven_day"}, UsableNow: true},
	}
}

// fixtureClock is the handler's controllable clock; tests advance it, never sleep.
type fixtureClock struct {
	mu sync.Mutex
	at time.Time
}

func (c *fixtureClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *fixtureClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(d)
}

type resetSpendFixture struct {
	h        *Handler
	provider *fakeResetProvider
	index    string
	logDir   string
	clock    *fixtureClock
}

func newResetSpendFixture(t *testing.T, grants ...fakeResetGrant) *resetSpendFixture {
	t.Helper()
	gin.SetMode(gin.TestMode)
	f := &resetSpendFixture{provider: newFakeResetProvider(t, grants...), logDir: t.TempDir(), clock: &fixtureClock{at: resetFixtureEpoch}}
	cfg := &config.Config{}
	m := coreauth.NewManager(nil, nil, nil)
	m.SetConfig(cfg)
	for _, seat := range []*coreauth.Auth{
		{ID: "fixture", FileName: "fixture.json", Provider: "claude", Status: coreauth.StatusActive, Attributes: map[string]string{"runtime_only": "true"}, Metadata: map[string]any{"access_token": resetFixtureToken}},
		{ID: "twin", FileName: "twin.json", Provider: "claude", Status: coreauth.StatusActive, Attributes: map[string]string{"runtime_only": "true"}, Metadata: map[string]any{"access_token": resetFixtureToken}},
		{ID: "other", FileName: "other.json", Provider: "claude", Status: coreauth.StatusActive, Attributes: map[string]string{"runtime_only": "true"}, Metadata: map[string]any{"access_token": resetFixtureOtherToken}},
		{ID: "codex", FileName: "codex.json", Provider: "codex", Status: coreauth.StatusActive, Attributes: map[string]string{"runtime_only": "true"}, Metadata: map[string]any{"access_token": resetFixtureToken}},
	} {
		if _, err := m.Register(context.Background(), seat); err != nil {
			t.Fatal(err)
		}
	}
	f.h = &Handler{cfg: cfg, authManager: m, logDir: f.logDir}
	fake := f.provider.client()
	f.h.claudeResetClient = func(cfg *config.Config, proxyURL string) *claudeauth.ClaudeAuth {
		return claudeauth.NewClaudeAuthWithProxyURL(cfg, proxyURL, claudeauth.WithHTTPClient(fake))
	}
	f.h.claudeResetSpends.now = f.clock.now
	f.index = f.seatIndex(t, "fixture")
	// The test's process exits at its end: its journal and lock go with it.
	t.Cleanup(func() { forgetClaudeResetJournal(f.logDir) })
	return f
}

func (f *resetSpendFixture) seatIndex(t *testing.T, id string) string {
	t.Helper()
	for _, auth := range f.h.authManager.List() {
		if auth.ID == id {
			return auth.EnsureIndex()
		}
	}
	t.Fatalf("seat %s not registered", id)
	return ""
}

// resetSpendKeys numbers the Idempotency-Key of each fixture spend.
var resetSpendKeys atomic.Int64

// spend sends one spend under a fresh Idempotency-Key.
func (f *resetSpendFixture) spend(t *testing.T, body string) (int, map[string]any) {
	t.Helper()
	return f.spendKeyed(t, fmt.Sprintf("fixture-operation-%d", resetSpendKeys.Add(1)), body)
}

// spendKeyed sends one spend under key; an empty key sends none.
func (f *resetSpendFixture) spendKeyed(t *testing.T, key, body string) (int, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v8/management/claude/limit-resets/spend", strings.NewReader(body))
	ctx.Request.Header.Set("Content-Type", "application/json")
	if key != "" {
		ctx.Request.Header.Set("Idempotency-Key", key)
	}
	f.h.SpendClaudeLimitReset(ctx)
	if strings.Contains(rec.Body.String(), resetFixtureToken) || strings.Contains(rec.Body.String(), resetFixtureOrganization) {
		t.Fatal("token leaked into the answer")
	}
	var answer map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &answer); err != nil {
		t.Fatalf("answer is not JSON: %s", rec.Body)
	}
	return rec.Code, answer
}

func spendBody(fields map[string]any) string {
	encoded, _ := json.Marshal(fields)
	return string(encoded)
}

func TestClaudeResetSpendRefusesARequestThatNamesNoKind(t *testing.T) {
	f := newResetSpendFixture(t, resetFixtureGrants()...)
	for _, body := range []string{
		spendBody(map[string]any{"auth_index": f.index}),
		spendBody(map[string]any{"auth_index": f.index, "kind": ""}),
		spendBody(map[string]any{"auth_index": f.index, "kind": "   "}),
		spendBody(map[string]any{"auth_index": f.index, "kind": nil}),
		spendBody(map[string]any{"auth_index": f.index, "grant_id": "full-a"}),
		spendBody(map[string]any{"auth_index": f.index, "grant_id": "fable-soon", "retry_request_id": "x"}),
		`{"auth_index":"` + f.index + `","kind":["fable"]}`,
		``,
	} {
		code, answer := f.spend(t, body)
		if code != http.StatusBadRequest {
			t.Fatalf("%s: status=%d answer=%v", body, code, answer)
		}
	}
	if requests, _, _ := f.provider.counts(); requests != 0 {
		t.Fatalf("a request naming no kind reached the provider %d times", requests)
	}
}

func TestClaudeResetSpendRefusesKindsOutsideTheTable(t *testing.T) {
	f := newResetSpendFixture(t, resetFixtureGrants()...)
	for _, kind := range []string{"turbo", "unknown", "5-hour", "fable reset", "weekly,fable", "any"} {
		code, answer := f.spend(t, spendBody(map[string]any{"auth_index": f.index, "kind": kind}))
		if code != http.StatusBadRequest {
			t.Fatalf("%q: status=%d answer=%v", kind, code, answer)
		}
	}
	// A misspelled field is refused rather than ignored: "grantid" would
	// otherwise spend the soonest grant instead of the one the caller meant.
	code, _ := f.spend(t, `{"auth_index":"`+f.index+`","kind":"fable","grantid":"fable-late"}`)
	if code != http.StatusBadRequest {
		t.Fatalf("unknown field status=%d", code)
	}
	if requests, _, _ := f.provider.counts(); requests != 0 {
		t.Fatalf("an unknown kind reached the provider %d times", requests)
	}
}

func TestClaudeResetSpendSpendsOnlyTheNamedKindSoonestFirst(t *testing.T) {
	f := newResetSpendFixture(t, resetFixtureGrants()...)
	want := []struct{ kind, grant string }{{"fable", "fable-soon"}, {"full", "full-a"}, {"weekly", "weekly-1"}, {"session", "session-1"}, {"FULL", "full-b"}}
	for i, step := range want {
		code, answer := f.spend(t, spendBody(map[string]any{"auth_index": f.index, "kind": step.kind}))
		if code != http.StatusOK || answer["result"] != "reset" || answer["grant_id"] != step.grant || answer["kind"] != strings.ToLower(step.kind) || answer["auth_index"] != f.index {
			t.Fatalf("%s: status=%d answer=%v", step.kind, code, answer)
		}
		if requestID, _ := answer["request_id"].(string); requestID == "" {
			t.Fatalf("%s: answer has no request id: %v", step.kind, answer)
		}
		_, _, claims := f.provider.counts()
		if len(claims) != i+1 || claims[i].GrantID != step.grant || claims[i].RequestID != answer["request_id"] {
			t.Fatalf("%s: claims=%+v", step.kind, claims)
		}
	}
	// A fresh read decides every spend: both full grants are now spent, so a
	// third full spend finds none, whatever an earlier read listed.
	code, answer := f.spend(t, spendBody(map[string]any{"auth_index": f.index, "kind": "full"}))
	if code != http.StatusConflict {
		t.Fatalf("spent kind: status=%d answer=%v", code, answer)
	}
	kinds, _ := answer["kinds"].(map[string]any)
	full, _ := kinds["full"].(map[string]any)
	if full == nil || full["usable_resets"] != float64(0) {
		t.Fatalf("409 must say what is left of each kind: %v", answer)
	}
	if _, _, claims := f.provider.counts(); len(claims) != len(want) {
		t.Fatalf("claims=%d", len(claims))
	}
}

func TestClaudeResetSpendHonoursOnlyAGrantOfTheNamedKind(t *testing.T) {
	f := newResetSpendFixture(t, resetFixtureGrants()...)
	for _, refused := range []struct {
		kind, grant string
		code        int
	}{
		{"weekly", "fable-late", http.StatusBadRequest},
		{"full", "fable-soon", http.StatusBadRequest},
		{"weekly", "turbo-1", http.StatusBadRequest},
		{"weekly", "untyped-1", http.StatusBadRequest},
		{"fable", "full-a", http.StatusBadRequest},
		{"fable", "no-such-grant", http.StatusConflict},
		{"fable", "Bad/Grant", http.StatusBadRequest},
	} {
		code, answer := f.spend(t, spendBody(map[string]any{"auth_index": f.index, "kind": refused.kind, "grant_id": refused.grant}))
		if code != refused.code {
			t.Fatalf("%s/%s: status=%d answer=%v", refused.kind, refused.grant, code, answer)
		}
	}
	if _, _, claims := f.provider.counts(); len(claims) != 0 {
		t.Fatalf("a mismatched grant was claimed: %+v", claims)
	}
	code, answer := f.spend(t, spendBody(map[string]any{"auth_index": f.index, "kind": "fable", "grant_id": "fable-late"}))
	if code != http.StatusOK || answer["grant_id"] != "fable-late" {
		t.Fatalf("named grant: status=%d answer=%v", code, answer)
	}
	if _, _, claims := f.provider.counts(); len(claims) != 1 || claims[0].GrantID != "fable-late" {
		t.Fatalf("claims=%+v", claims)
	}
}

func TestClaudeResetSpendNeverMapsAnUnknownGrantType(t *testing.T) {
	grants := resetFixtureGrants()
	f := newResetSpendFixture(t, grants[6], grants[7])
	for _, kind := range []string{"fable", "full", "weekly", "session"} {
		code, answer := f.spend(t, spendBody(map[string]any{"auth_index": f.index, "kind": kind}))
		if code != http.StatusBadRequest {
			t.Fatalf("%s: status=%d answer=%v", kind, code, answer)
		}
		kinds, _ := answer["kinds"].(map[string]any)
		unknown, _ := kinds["unknown"].(map[string]any)
		if len(kinds) != 1 || unknown == nil || unknown["resets_left"] != float64(6) || unknown["usable_resets"] != float64(0) {
			t.Fatalf("%s: kinds=%v", kind, kinds)
		}
	}
	if _, reads, claims := f.provider.counts(); len(claims) != 0 || reads != 4 {
		t.Fatalf("reads=%d claims=%+v", reads, claims)
	}
}

func TestClaudeResetSpendSpendsNothingWhenTheSeatCannotSpend(t *testing.T) {
	cases := []struct {
		name   string
		update func(*fakeResetProvider)
		code   int
	}{
		{"profile read fails", func(p *fakeResetProvider) { p.profileStatus = http.StatusUnauthorized }, http.StatusBadGateway},
		{"grants read rate limited", func(p *fakeResetProvider) { p.readStatus = http.StatusTooManyRequests }, http.StatusBadGateway},
		{"seat ineligible", func(p *fakeResetProvider) { p.eligible, p.reason = false, "surface" }, http.StatusConflict},
		{"cooldown", func(p *fakeResetProvider) {
			p.cooldownUntil = resetFixtureEpoch.Add(time.Hour).Format(time.RFC3339)
		}, http.StatusConflict},
		{"paused", func(p *fakeResetProvider) {
			for i := range p.grants {
				p.grants[i].Paused = true
			}
		}, http.StatusConflict},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newResetSpendFixture(t, resetFixtureGrants()...)
			f.provider.set(tc.update)
			code, answer := f.spend(t, spendBody(map[string]any{"auth_index": f.index, "kind": "fable"}))
			if code != tc.code {
				t.Fatalf("status=%d answer=%v", code, answer)
			}
			if _, _, claims := f.provider.counts(); len(claims) != 0 {
				t.Fatalf("claims=%+v", claims)
			}
		})
	}
	f := newResetSpendFixture(t, resetFixtureGrants()...)
	for index, want := range map[string]int{"": http.StatusBadRequest, "no-such-seat": http.StatusNotFound, f.seatIndex(t, "codex"): http.StatusNotFound} {
		if code, answer := f.spend(t, spendBody(map[string]any{"auth_index": index, "kind": "fable"})); code != want {
			t.Fatalf("seat %q: status=%d answer=%v", index, code, answer)
		}
	}
	if requests, _, _ := f.provider.counts(); requests != 0 {
		t.Fatalf("an unknown seat reached the provider %d times", requests)
	}
}

func TestClaudeResetSpendUnknownOutcomeRetriesOnlyWithTheSameRequestID(t *testing.T) {
	f := newResetSpendFixture(t, resetFixtureGrants()...)
	f.provider.set(func(p *fakeResetProvider) { p.claimStatus = http.StatusBadGateway })
	code, first := f.spend(t, spendBody(map[string]any{"auth_index": f.index, "kind": "fable"}))
	requestID, _ := first["request_id"].(string)
	if code != http.StatusOK || first["result"] != "unknown" || requestID == "" || first["grant_id"] != "fable-soon" || first["retry_until"] == nil {
		t.Fatalf("status=%d answer=%v", code, first)
	}
	claimCount := func() int { _, _, claims := f.provider.counts(); return len(claims) }
	// The hub never retries on its own, and a new spend on the seat waits.
	for _, body := range []string{
		spendBody(map[string]any{"auth_index": f.index, "kind": "fable"}),
		spendBody(map[string]any{"auth_index": f.index, "kind": "full"}),
		spendBody(map[string]any{"auth_index": f.index, "kind": "fable", "retry_request_id": "another-request"}),
	} {
		if code, answer := f.spend(t, body); code != http.StatusConflict {
			t.Fatalf("%s: status=%d answer=%v", body, code, answer)
		}
	}
	for _, body := range []string{
		spendBody(map[string]any{"auth_index": f.index, "kind": "full", "retry_request_id": requestID}),
		spendBody(map[string]any{"auth_index": f.index, "kind": "fable", "grant_id": "fable-late", "retry_request_id": requestID}),
	} {
		if code, answer := f.spend(t, body); code != http.StatusBadRequest {
			t.Fatalf("%s: status=%d answer=%v", body, code, answer)
		}
	}
	if claimCount() != 1 {
		t.Fatalf("claims=%d", claimCount())
	}
	// A second credential file of the same account is held too; a seat of
	// another account is not.
	if code, answer := f.spend(t, spendBody(map[string]any{"auth_index": f.seatIndex(t, "twin"), "kind": "weekly"})); code != http.StatusConflict {
		t.Fatalf("twin seat: status=%d answer=%v", code, answer)
	}
	if code, answer := f.spend(t, spendBody(map[string]any{"auth_index": f.seatIndex(t, "other"), "kind": "weekly"})); code != http.StatusOK || answer["grant_id"] != "weekly-1" {
		t.Fatalf("other seat: status=%d answer=%v", code, answer)
	}
	if claimCount() != 2 {
		t.Fatalf("claims=%d", claimCount())
	}
	// A refusal on a retry cannot prove the first claim did not spend.
	f.clock.advance(4 * time.Minute)
	f.provider.set(func(p *fakeResetProvider) { p.claimStatus, p.claimResult = 0, "cooldown" })
	code, answer := f.spend(t, spendBody(map[string]any{"auth_index": f.index, "kind": "fable", "retry_request_id": requestID}))
	if code != http.StatusOK || answer["result"] != "cooldown" || answer["request_id"] != requestID || answer["retry"] != true {
		t.Fatalf("retry: status=%d answer=%v", code, answer)
	}
	if code, answer := f.spend(t, spendBody(map[string]any{"auth_index": f.index, "kind": "fable"})); code != http.StatusConflict {
		t.Fatalf("still unsettled: status=%d answer=%v", code, answer)
	}
	f.provider.set(func(p *fakeResetProvider) { p.claimResult = "reset" })
	code, answer = f.spend(t, spendBody(map[string]any{"auth_index": f.index, "kind": "fable", "retry_request_id": requestID}))
	if code != http.StatusOK || answer["result"] != "reset" || answer["request_id"] != requestID {
		t.Fatalf("settling retry: status=%d answer=%v", code, answer)
	}
	_, _, claims := f.provider.counts()
	if len(claims) != 4 || claims[0].RequestID != requestID {
		t.Fatalf("claims=%+v", claims)
	}
	for _, claim := range claims[2:] {
		if claim.RequestID != requestID || claim.GrantID != "fable-soon" {
			t.Fatalf("a retry changed the claim: %+v", claims)
		}
	}
	// Settled: the request id cannot be sent again.
	if code, answer := f.spend(t, spendBody(map[string]any{"auth_index": f.index, "kind": "fable", "retry_request_id": requestID})); code != http.StatusConflict {
		t.Fatalf("settled retry: status=%d answer=%v", code, answer)
	}

	// Ten minutes after an unknown outcome the retry is refused, and the seat
	// takes a new, deliberate spend again.
	f.provider.set(func(p *fakeResetProvider) { p.claimStatus = http.StatusInternalServerError })
	_, lost := f.spend(t, spendBody(map[string]any{"auth_index": f.index, "kind": "full"}))
	lostID, _ := lost["request_id"].(string)
	if lost["result"] != "unknown" || lostID == "" {
		t.Fatalf("answer=%v", lost)
	}
	f.clock.advance(10 * time.Minute)
	f.provider.set(func(p *fakeResetProvider) { p.claimStatus = 0 })
	if code, answer := f.spend(t, spendBody(map[string]any{"auth_index": f.index, "kind": "full", "retry_request_id": lostID})); code != http.StatusConflict {
		t.Fatalf("expired retry: status=%d answer=%v", code, answer)
	}
	code, answer = f.spend(t, spendBody(map[string]any{"auth_index": f.index, "kind": "full"}))
	if code != http.StatusOK || answer["result"] != "reset" || answer["request_id"] == lostID {
		t.Fatalf("after expiry: status=%d answer=%v", code, answer)
	}
}

func TestClaudeResetSpendWritesOneRecordPerSpend(t *testing.T) {
	f := newResetSpendFixture(t, resetFixtureGrants()...)
	code, answer := f.spend(t, spendBody(map[string]any{"auth_index": f.index, "kind": "fable"}))
	if code != http.StatusOK || answer["result"] != "reset" {
		t.Fatalf("status=%d answer=%v", code, answer)
	}
	before, _ := answer["kinds_before"].(map[string]any)
	after, _ := answer["kinds_after"].(map[string]any)
	fableBefore, _ := before["fable"].(map[string]any)
	fableAfter, _ := after["fable"].(map[string]any)
	if fableBefore["resets_left"] != float64(2) || fableAfter["resets_left"] != float64(1) {
		t.Fatalf("before=%v after=%v", before, after)
	}

	file, err := os.Open(filepath.Join(f.logDir, claudeResetSpendLedgerName))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	var records []map[string]any
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		var record map[string]any
		if errDecode := json.Unmarshal(scanner.Bytes(), &record); errDecode != nil {
			t.Fatalf("ledger line is not JSON: %s", scanner.Text())
		}
		records = append(records, record)
	}
	// Three lines per spend: "sent", synced before the claim left;
	// "answered", the outcome, written as soon as it was known; and
	// "balance", what was left after the claim.
	if len(records) != 3 || records[0]["phase"] != "sent" || records[0]["result"] != "unknown" || records[0]["request_id"] != answer["request_id"] ||
		records[1]["phase"] != "answered" || records[2]["phase"] != "balance" || records[2]["request_id"] != answer["request_id"] {
		t.Fatalf("records=%v", records)
	}
	record := records[1]
	for _, field := range []string{"phase", "operation", "account", "at", "auth_index", "kind", "grant_id", "request_id", "result", "resets_left_before"} {
		if _, ok := record[field]; !ok {
			t.Fatalf("record lacks %s: %v", field, record)
		}
	}
	leftBefore, _ := record["resets_left_before"].(map[string]any)
	leftAfter, _ := records[2]["resets_left_after"].(map[string]any)
	if record["auth_index"] != f.index || record["kind"] != "fable" || record["grant_id"] != "fable-soon" || record["result"] != "reset" || leftBefore["fable"] != float64(2) || leftAfter["fable"] != float64(1) {
		t.Fatalf("record=%v", record)
	}
	raw, _ := os.ReadFile(filepath.Join(f.logDir, claudeResetSpendLedgerName))
	if strings.Contains(string(raw), resetFixtureToken) || strings.Contains(string(raw), resetFixtureOrganization) || strings.Contains(string(raw), "example.invalid") {
		t.Fatalf("ledger holds a secret or an identity: %s", raw)
	}

	// The credentials read shows the last spend per seat, and a hub restarted
	// on the same log directory still shows it.
	running := f.h
	f.restart()
	for _, h := range []*Handler{running, f.h} {
		rec := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(rec)
		ctx.Request = httptest.NewRequest(http.MethodGet, "/v8/management/credentials", nil)
		ctx.Set(ConfigV8ContextKey, true)
		h.ListAuthFiles(ctx)
		var list struct {
			Files []map[string]any `json:"files"`
		}
		if errDecode := json.Unmarshal(rec.Body.Bytes(), &list); errDecode != nil {
			t.Fatal(errDecode)
		}
		seen := 0
		for _, entry := range list.Files {
			last, present := entry["claude_last_reset_spend"]
			switch entry["id"] {
			case "fixture":
				spent, _ := last.(map[string]any)
				if spent["kind"] != "fable" || spent["result"] != "reset" || spent["at"] == nil {
					t.Fatalf("fixture entry=%v", entry)
				}
				seen++
			case "other":
				if !present || last != nil {
					t.Fatalf("a seat never spent through this hub reads null: %v", entry)
				}
				seen++
			case "codex":
				if present {
					t.Fatalf("a Codex seat carries a Claude spend field: %v", entry)
				}
				seen++
			}
		}
		if seen != 3 {
			t.Fatalf("list=%s", rec.Body)
		}
	}
}

// The hold is on disk before the claim leaves, so a hub that restarts (or
// dies) while an answer is outstanding still refuses a new claim on the seat.
// It accepts the retry with the same request id only when the ledger records
// the answer as unknown: a claim with no recorded answer may have been
// answered, so it is never retried (round 2, R2-2).
func TestClaudeResetSpendHoldSurvivesARestart(t *testing.T) {
	f := newResetSpendFixture(t, resetFixtureGrants()...)
	ledgerPath := filepath.Join(f.logDir, claudeResetSpendLedgerName)
	f.provider.set(func(p *fakeResetProvider) {
		p.claimStatus = http.StatusBadGateway
		p.onClaim = func(claim fakeResetClaim) {
			raw, err := os.ReadFile(ledgerPath)
			if err != nil || !strings.Contains(string(raw), `"phase":"sent"`) || !strings.Contains(string(raw), claim.RequestID) {
				t.Errorf("the claim left before its sent line was on disk: %v %s", err, raw)
			}
		}
	})
	_, first := f.spend(t, spendBody(map[string]any{"auth_index": f.index, "kind": "fable"}))
	requestID, _ := first["request_id"].(string)
	if first["result"] != "unknown" || requestID == "" {
		t.Fatalf("answer=%v", first)
	}
	f.restart()
	if code, answer := f.spend(t, spendBody(map[string]any{"auth_index": f.index, "kind": "full"})); code != http.StatusConflict {
		t.Fatalf("after restart: status=%d answer=%v", code, answer)
	}
	f.provider.set(func(p *fakeResetProvider) { p.claimStatus, p.onClaim = 0, nil })
	code, answer := f.spend(t, spendBody(map[string]any{"auth_index": f.index, "kind": "fable", "retry_request_id": requestID}))
	if code != http.StatusOK || answer["result"] != "reset" || answer["request_id"] != requestID {
		t.Fatalf("retry after restart: status=%d answer=%v", code, answer)
	}
	// Settled on disk too: a second restart lets the seat spend again.
	f.restart()
	if code, answer := f.spend(t, spendBody(map[string]any{"auth_index": f.index, "kind": "full"})); code != http.StatusOK || answer["result"] != "reset" {
		t.Fatalf("after settling: status=%d answer=%v", code, answer)
	}

	// A hub that died after the sent line, before any answer.
	g := newResetSpendFixture(t, resetFixtureGrants()...)
	sent, _ := json.Marshal(claudeResetSpendRecord{
		Phase: claudeResetPhaseSent, Operation: claudeResetOperation("restored-operation"), Account: claudeResetAccount(resetFixtureOrganization),
		At: g.clock.now().Add(-time.Minute), AuthIndex: g.index, Kind: "fable", GrantID: "fable-late", RequestID: "restored-request", Result: "unknown",
		ResetsLeftBefore: map[string]int{"fable": 2},
	})
	if err := os.WriteFile(filepath.Join(g.logDir, claudeResetSpendLedgerName), append(sent, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, answer := g.spend(t, spendBody(map[string]any{"auth_index": g.index, "kind": "fable"})); code != http.StatusConflict {
		t.Fatalf("restored hold: status=%d answer=%v", code, answer)
	}
	code, answer = g.spend(t, spendBody(map[string]any{"auth_index": g.index, "kind": "fable", "retry_request_id": "restored-request"}))
	_, _, claims := g.provider.counts()
	if code != http.StatusConflict || len(claims) != 0 || !strings.Contains(fmt.Sprint(answer["error"]), "no retry is accepted") {
		t.Fatalf("a retry of a claim with no recorded answer: status=%d answer=%v claims=%+v", code, answer, claims)
	}
}

// Gate r1 MINOR 20: the ledger loads, then stops taking writes before the
// sent line: the spend reaches its pre-send append and sends nothing.
func TestClaudeResetSpendRefusesWhenTheLedgerCannotBeWritten(t *testing.T) {
	f := newResetSpendFixture(t, resetFixtureGrants()...)
	ledgerPath := filepath.Join(f.logDir, claudeResetSpendLedgerName)
	f.provider.set(func(p *fakeResetProvider) {
		p.onProfile = func() {
			if err := os.Mkdir(ledgerPath, 0o700); err != nil {
				t.Error(err)
			}
		}
	})
	// A ledger path taken by something this process did not write is never
	// written over: the journal refuses.
	code, answer := f.spend(t, spendBody(map[string]any{"auth_index": f.index, "kind": "fable"}))
	if code != http.StatusServiceUnavailable || answer["ledger_refusal"] != claudeResetRefusalChanged {
		t.Fatalf("status=%d answer=%v", code, answer)
	}
	// The account reservation checks the ledger again, so the spend stops
	// before its grants read.
	if _, reads, claims := f.provider.counts(); len(claims) != 0 || reads != 0 {
		t.Fatalf("reads=%d claims=%+v", reads, claims)
	}
}

// restart replaces the handler and drops the process's journal state, as a
// hub restart does: only the ledger on disk carries over.
func (f *resetSpendFixture) restart() {
	forgetClaudeResetJournal(f.logDir)
	h := &Handler{cfg: f.h.cfg, authManager: f.h.authManager, logDir: f.logDir, claudeResetClient: f.h.claudeResetClient}
	h.claudeResetSpends.now = f.clock.now
	f.h = h
}

// forgetClaudeResetJournal drops the process's state of the ledger in dir and
// closes what the process held open, its lock included, as an exiting
// process does.
func forgetClaudeResetJournal(dir string) {
	resolved, err := claudeResetJournalDir(dir)
	if err != nil {
		return
	}
	claudeResetJournals.mu.Lock()
	journal := claudeResetJournals.byPath[resolved]
	delete(claudeResetJournals.byPath, resolved)
	claudeResetJournals.mu.Unlock()
	closeClaudeResetJournal(journal)
}

// closeClaudeResetJournal closes a journal's lock, ledger and sentinel.
func closeClaudeResetJournal(journal *claudeResetJournal) {
	if journal == nil {
		return
	}
	journal.mu.Lock()
	defer journal.mu.Unlock()
	for _, file := range []*os.File{journal.file, journal.sentinel, journal.lock} {
		if file != nil {
			_ = file.Close()
		}
	}
	journal.file, journal.sentinel, journal.lock = nil, nil, nil
}

// Codex round 1, class "claim identity not persisted": a hold restored from
// the ledger still binds its account, for a new claim on another credential
// file of that account and for the retry itself.
func TestClaudeResetSpendRestartKeepsTheAccountBinding(t *testing.T) {
	f := newResetSpendFixture(t, resetFixtureGrants()...)
	f.provider.set(func(p *fakeResetProvider) { p.claimStatus = http.StatusBadGateway })
	_, first := f.spend(t, spendBody(map[string]any{"auth_index": f.index, "kind": "fable"}))
	requestID, _ := first["request_id"].(string)
	if first["result"] != "unknown" || requestID == "" {
		t.Fatalf("answer=%v", first)
	}
	raw, _ := os.ReadFile(filepath.Join(f.logDir, claudeResetSpendLedgerName))
	if strings.Contains(string(raw), resetFixtureOrganization) {
		t.Fatalf("the ledger holds the organization: %s", raw)
	}
	f.restart()
	f.provider.set(func(p *fakeResetProvider) { p.claimStatus = 0 })
	if code, answer := f.spend(t, spendBody(map[string]any{"auth_index": f.seatIndex(t, "twin"), "kind": "weekly"})); code != http.StatusConflict {
		t.Fatalf("twin after restart: status=%d answer=%v", code, answer)
	}
	// The seat's credential now belongs to another account: the retry must
	// not claim the original grant id against the replacement organization.
	for _, auth := range f.h.authManager.List() {
		if auth.ID == "fixture" {
			auth.Metadata = map[string]any{"access_token": resetFixtureOtherToken}
			if _, err := f.h.authManager.Update(context.Background(), auth); err != nil {
				t.Fatal(err)
			}
		}
	}
	if code, answer := f.spend(t, spendBody(map[string]any{"auth_index": f.index, "kind": "fable", "retry_request_id": requestID})); code != http.StatusConflict {
		t.Fatalf("retry on a replaced credential: status=%d answer=%v", code, answer)
	}
	if _, _, claims := f.provider.counts(); len(claims) != 1 {
		t.Fatalf("claims=%+v", claims)
	}
}

// Codex round 1, class "account serialization incomplete": a retry holds the
// account while its claim is in flight, even past the retry window.
func TestClaudeResetSpendRetryHoldsTheAccountWhileInFlight(t *testing.T) {
	f := newResetSpendFixture(t, resetFixtureGrants()...)
	f.provider.set(func(p *fakeResetProvider) { p.claimStatus = http.StatusBadGateway })
	_, first := f.spend(t, spendBody(map[string]any{"auth_index": f.index, "kind": "fable"}))
	requestID, _ := first["request_id"].(string)
	f.clock.advance(9 * time.Minute)
	arrived, release := make(chan struct{}), make(chan struct{})
	var held atomic.Bool
	f.provider.set(func(p *fakeResetProvider) {
		p.claimStatus = 0
		// Only the retry's claim is held; any later claim passes straight on.
		p.onClaim = func(fakeResetClaim) {
			if held.CompareAndSwap(false, true) {
				close(arrived)
				<-release
			}
		}
	})
	done := make(chan int, 1)
	go func() {
		code, _ := f.spend(t, spendBody(map[string]any{"auth_index": f.index, "kind": "fable", "retry_request_id": requestID}))
		done <- code
	}()
	select {
	case <-arrived:
	case code := <-done:
		t.Fatalf("the retry answered %d before its claim arrived", code)
	}
	f.clock.advance(2 * time.Minute)
	code, answer := f.spend(t, spendBody(map[string]any{"auth_index": f.seatIndex(t, "twin"), "kind": "weekly"}))
	close(release)
	if retried := <-done; retried != http.StatusOK {
		t.Fatalf("retry status=%d", retried)
	}
	if code != http.StatusConflict {
		t.Fatalf("twin during an in-flight retry: status=%d answer=%v", code, answer)
	}
	if _, _, claims := f.provider.counts(); len(claims) != 2 {
		t.Fatalf("claims=%+v", claims)
	}
}

// Codex round 1, class "ledger recovery fails open": a torn tail and a ledger
// the hub cannot read or trust stop every spend.
func TestClaudeResetSpendLedgerRecoveryNeverFailsOpen(t *testing.T) {
	earlier := claudeResetSpendRecord{
		Phase: claudeResetPhaseSent, Operation: claudeResetOperation("earlier-operation"), Account: claudeResetAccount(resetFixtureOtherOrganization),
		At: resetFixtureEpoch.Add(-time.Hour), AuthIndex: "elsewhere", Kind: "weekly", GrantID: "weekly-1", RequestID: "earlier", Result: "unknown",
		ResetsLeftBefore: map[string]int{"weekly": 1},
	}
	sentLine, _ := json.Marshal(earlier)
	earlier.Phase, earlier.Result = claudeResetPhaseAnswered, "reset"
	answeredLine, _ := json.Marshal(earlier)
	complete := append(append(sentLine, '\n'), answeredLine...)
	// Round 2, R2-4: a last line with no newline is a write cut short, refused
	// whatever it parses as, the same way on every start, naming its line.
	for name, tail := range map[string][]byte{"torn tail": []byte(`{"phase":`), "cut after a complete record": complete} {
		t.Run(name, func(t *testing.T) {
			f := newResetSpendFixture(t, resetFixtureGrants()...)
			if err := os.WriteFile(filepath.Join(f.logDir, claudeResetSpendLedgerName), tail, 0o600); err != nil {
				t.Fatal(err)
			}
			wantLine := float64(bytes.Count(tail, []byte("\n")) + 1)
			for restart := 0; restart < 2; restart++ {
				f.restart()
				code, answer := f.spend(t, spendBody(map[string]any{"auth_index": f.index, "kind": "full"}))
				if code != http.StatusServiceUnavailable || answer["ledger_refusal"] != claudeResetRefusalInvalidLine || answer["ledger_line"] != wantLine {
					t.Fatalf("start %d: status=%d answer=%v", restart, code, answer)
				}
			}
			if requests, _, _ := f.provider.counts(); requests != 0 {
				t.Fatalf("a spend reached the provider over a torn ledger: %d", requests)
			}
			if raw, _ := os.ReadFile(filepath.Join(f.logDir, claudeResetSpendLedgerName)); !bytes.Equal(raw, tail) {
				t.Fatalf("the hub wrote to a ledger it refused: %q", raw)
			}
		})
	}
	for name, prepare := range map[string]func(path string) error{
		"corrupt line": func(path string) error {
			return os.WriteFile(path, []byte("not a record\n"), 0o600)
		},
		"record without a phase": func(path string) error {
			return os.WriteFile(path, []byte(`{"auth_index":"x","request_id":"y"}`+"\n"), 0o600)
		},
		"unreadable": func(path string) error { return os.Mkdir(path, 0o700) },
	} {
		t.Run(name, func(t *testing.T) {
			f := newResetSpendFixture(t, resetFixtureGrants()...)
			if err := prepare(filepath.Join(f.logDir, claudeResetSpendLedgerName)); err != nil {
				t.Fatal(err)
			}
			for attempt := 0; attempt < 2; attempt++ {
				if code, answer := f.spend(t, spendBody(map[string]any{"auth_index": f.index, "kind": "fable"})); code != http.StatusServiceUnavailable {
					t.Fatalf("attempt %d: status=%d answer=%v", attempt, code, answer)
				}
			}
			if requests, _, _ := f.provider.counts(); requests != 0 {
				t.Fatalf("a spend reached the provider over a ledger it cannot trust: %d", requests)
			}
		})
	}
}

// Codex round 1, class "stale clock": a grant is judged at the time of the
// claim, not at the time the spend began.
func TestClaudeResetSpendJudgesGrantsAtTheTimeOfTheClaim(t *testing.T) {
	at := func(f *resetSpendFixture, d time.Duration) string { return f.clock.now().Add(d).Format(time.RFC3339) }
	t.Run("selection after the read", func(t *testing.T) {
		f := newResetSpendFixture(t)
		f.provider.set(func(p *fakeResetProvider) {
			p.grants = []fakeResetGrant{
				{ID: "fable-a", ResetsLeft: 1, EndsAt: at(f, time.Second), Type: "full", GivesBackWeekly: true, Clears: []string{}, UsableNow: true},
				{ID: "fable-b", ResetsLeft: 1, EndsAt: at(f, time.Hour), Type: "full", GivesBackWeekly: true, Clears: []string{}, UsableNow: true},
			}
			p.onRead = func() { f.clock.advance(2 * time.Second) }
		})
		code, answer := f.spend(t, spendBody(map[string]any{"auth_index": f.index, "kind": "fable"}))
		if code != http.StatusOK || answer["grant_id"] != "fable-b" {
			t.Fatalf("status=%d answer=%v", code, answer)
		}
	})
	t.Run("expiry before the claim", func(t *testing.T) {
		g := newResetSpendFixture(t)
		g.provider.set(func(p *fakeResetProvider) {
			p.grants = []fakeResetGrant{{ID: "fable-c", ResetsLeft: 1, EndsAt: at(g, 5*time.Second), Type: "full", GivesBackWeekly: true, Clears: []string{}, UsableNow: true}}
			p.onProfile = func() { g.clock.advance(10 * time.Second) }
		})
		if code, answer := g.spend(t, spendBody(map[string]any{"auth_index": g.index, "kind": "fable"})); code != http.StatusConflict {
			t.Fatalf("expired before the claim: status=%d answer=%v", code, answer)
		}
		if _, _, claims := g.provider.counts(); len(claims) != 0 {
			t.Fatalf("an expired grant was claimed: %+v", claims)
		}
	})
}

// Codex round 1, class "unsynchronized configuration": a spend reads the
// configuration once, under the handler's lock, while reloads replace it.
// Run with -race to see the race it guards against.
func TestClaudeResetSpendReadsConfigurationUnderTheLock(t *testing.T) {
	f := newResetSpendFixture(t, resetFixtureGrants()...)
	// The spend works from its own copy: the live object is edited in place
	// by management writes, so holding it after the lock is released races.
	build := f.h.claudeResetClient
	f.h.claudeResetClient = func(cfg *config.Config, proxyURL string) *claudeauth.ClaudeAuth {
		f.h.mu.Lock()
		live := cfg == f.h.cfg
		f.h.mu.Unlock()
		if live {
			t.Error("the spend holds the live configuration object")
		}
		return build(cfg, proxyURL)
	}
	stop := make(chan struct{})
	reloaded := make(chan struct{})
	go func() {
		defer close(reloaded)
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			if i%10 == 0 {
				f.h.SetConfig(&config.Config{})
				continue
			}
			// Management writes edit the live configuration in place, under
			// the handler's lock (updateStringField, PutProxyURL).
			f.h.mu.Lock()
			f.h.cfg.ProxyURL = ""
			f.h.mu.Unlock()
		}
	}()
	// "already_used" leaves the grants in place, so every spend reaches the claim.
	f.provider.set(func(p *fakeResetProvider) { p.claimResult = "already_used" })
	for i := 0; i < 30; i++ {
		kind := []string{"fable", "full", "weekly"}[i%3]
		if code, answer := f.spend(t, spendBody(map[string]any{"auth_index": f.index, "kind": kind})); code != http.StatusOK {
			t.Fatalf("%s: status=%d answer=%v", kind, code, answer)
		}
	}
	close(stop)
	<-reloaded
}

// Codex rounds 3 and 4: the spend never edits the hub's routing state. A
// confirmed reset leaves the seat's cooldowns as they are (clearing them
// blindly lifted limits the spent kind did not refill, independent model
// failures and a replacement credential's cooldown), says so, and reports
// truthfully whether a usage poll was queued. The existing routing reset is
// the operator's step that lifts the cooldown.
func TestClaudeResetSpendLeavesTheHubRoutingStateToTheOperator(t *testing.T) {
	block := func(t *testing.T, f *resetSpendFixture) {
		t.Helper()
		next := time.Now().Add(7 * 24 * time.Hour)
		auth, ok := f.h.authManager.GetByID("fixture")
		if !ok {
			t.Fatal("fixture seat missing")
		}
		auth.Status, auth.StatusMessage, auth.Unavailable, auth.NextRetryAfter = coreauth.StatusError, "quota exhausted", true, next
		auth.Quota = coreauth.QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: next, BackoffLevel: 2}
		auth.ModelStates = map[string]*coreauth.ModelState{"claude-fixture-model": {
			Status: coreauth.StatusError, StatusMessage: "quota exhausted", Unavailable: true, NextRetryAfter: next,
			Quota: coreauth.QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: next, BackoffLevel: 2},
		}}
		if _, err := f.h.authManager.Update(context.Background(), auth); err != nil {
			t.Fatal(err)
		}
	}
	blocked := func(f *resetSpendFixture) bool {
		auth, _ := f.h.authManager.GetByID("fixture")
		state := auth.ModelStates["claude-fixture-model"]
		return auth.Unavailable && auth.Quota.Exceeded && !auth.NextRetryAfter.IsZero() && state != nil && state.Unavailable && state.Quota.Exceeded
	}
	for name, update := range map[string]func(*fakeResetProvider){
		"reset":        func(*fakeResetProvider) {},
		"already used": func(p *fakeResetProvider) { p.claimResult = "already_used" },
		"unknown":      func(p *fakeResetProvider) { p.claimStatus = http.StatusBadGateway },
		"refused":      func(p *fakeResetProvider) { p.claimResult = "cooldown" },
	} {
		t.Run(name, func(t *testing.T) {
			f := newResetSpendFixture(t, resetFixtureGrants()...)
			block(t, f)
			if !blocked(f) {
				t.Fatal("precondition: the seat is not blocked")
			}
			f.provider.set(update)
			code, answer := f.spend(t, spendBody(map[string]any{"auth_index": f.index, "kind": "weekly"}))
			if code != http.StatusOK {
				t.Fatalf("status=%d answer=%v", code, answer)
			}
			if !blocked(f) {
				t.Fatal("the spend edited the hub's routing state")
			}
			if answer["result"] != "reset" && answer["result"] != "already_used" {
				return
			}
			routing, _ := answer["routing"].(map[string]any)
			// Quota polling is off in this fixture, so no poll can be queued.
			if routing["cooldown_changed"] != false || routing["usage_poll_queued"] != false || routing["cooldown_reset_route"] != "/v8/management/routing/cooldown/reset" {
				t.Fatalf("routing=%v", answer["routing"])
			}
			rec := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(rec)
			ctx.Request = httptest.NewRequest(http.MethodPost, "/v8/management/routing/cooldown/reset", strings.NewReader(`{"auth_index":"`+f.index+`"}`))
			ctx.Request.Header.Set("Content-Type", "application/json")
			f.h.ResetQuota(ctx)
			if auth, _ := f.h.authManager.GetByID("fixture"); rec.Code != http.StatusOK || auth.Unavailable || auth.Quota.Exceeded {
				t.Fatalf("the routing reset did not lift the cooldown: %d %s", rec.Code, rec.Body)
			}
		})
	}
}

// Codex round 2: a ledger the hub cannot read is reported as such by both
// reads, never as "never spent through this hub".
func TestClaudeResetSpendHistoryReportsAnUnreadableLedger(t *testing.T) {
	f := newResetSpendFixture(t, resetFixtureGrants()...)
	if code, answer := f.spend(t, spendBody(map[string]any{"auth_index": f.index, "kind": "fable"})); code != http.StatusOK {
		t.Fatalf("status=%d answer=%v", code, answer)
	}
	ledger, err := os.OpenFile(filepath.Join(f.logDir, claudeResetSpendLedgerName), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = ledger.WriteString("not a record\n"); err != nil {
		t.Fatal(err)
	}
	if err = ledger.Close(); err != nil {
		t.Fatal(err)
	}
	f.restart()

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v8/management/credentials", nil)
	ctx.Set(ConfigV8ContextKey, true)
	f.h.ListAuthFiles(ctx)
	var list struct {
		Files []map[string]any `json:"files"`
	}
	if errDecode := json.Unmarshal(rec.Body.Bytes(), &list); errDecode != nil {
		t.Fatal(errDecode)
	}
	claudeSeats := 0
	for _, entry := range list.Files {
		if entry["provider"] != "claude" {
			continue
		}
		claudeSeats++
		if entry["claude_last_reset_spend"] != nil || entry["claude_last_reset_spend_error"] == nil {
			t.Fatalf("credentials entry hides the unreadable ledger: %v", entry)
		}
	}
	if claudeSeats != 3 {
		t.Fatalf("list=%s", rec.Body)
	}

	rec = httptest.NewRecorder()
	ctx, _ = gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v8/management/claude/limit-resets", nil)
	f.h.GetClaudeLimitResets(ctx)
	var resets struct {
		Seats []map[string]any `json:"seats"`
	}
	if errDecode := json.Unmarshal(rec.Body.Bytes(), &resets); errDecode != nil {
		t.Fatal(errDecode)
	}
	if len(resets.Seats) != 3 {
		t.Fatalf("limit-resets=%s", rec.Body)
	}
	for _, seat := range resets.Seats {
		if seat["last_spend"] != nil || seat["last_spend_error"] == nil {
			t.Fatalf("limit-resets seat hides the unreadable ledger: %v", seat)
		}
	}
}

// jumpWhenSentIsDurable makes a spend's durable write take d: the first clock
// read after the ledger gains a sent line finds the clock d later.
func (f *resetSpendFixture) jumpWhenSentIsDurable(d time.Duration) {
	path := filepath.Join(f.logDir, claudeResetSpendLedgerName)
	sentLines := func() int {
		raw, _ := os.ReadFile(path)
		return strings.Count(string(raw), `"phase":"sent"`)
	}
	baseline := sentLines()
	var jumped atomic.Bool
	f.h.claudeResetSpends.now = func() time.Time {
		if !jumped.Load() && sentLines() > baseline && jumped.CompareAndSwap(false, true) {
			f.clock.advance(d)
		}
		return f.clock.now()
	}
}

type resetSpendResult struct {
	code   int
	answer map[string]any
}

// spendPausedAt runs one spend in the background and holds it inside its
// first profile read ("profile") or grants read ("grants"); during runs while
// it is held. A spend that answers before reaching the read is returned as is.
func (f *resetSpendFixture) spendPausedAt(t *testing.T, read, body string, during func()) resetSpendResult {
	t.Helper()
	arrived, release := make(chan struct{}), make(chan struct{})
	// Released before any assertion in during can end the test (gate r2
	// MINOR 20).
	var releaseOnce sync.Once
	releaseHeld := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseHeld)
	var paused atomic.Bool
	hold := func() {
		if paused.CompareAndSwap(false, true) {
			close(arrived)
			<-release
		}
	}
	f.provider.set(func(p *fakeResetProvider) {
		if read == "profile" {
			p.onProfile = hold
		} else {
			p.onRead = hold
		}
	})
	defer f.provider.set(func(p *fakeResetProvider) { p.onProfile, p.onRead = nil, nil })
	done := make(chan resetSpendResult, 1)
	go func() {
		code, answer := r2SpendQuietly(f.h, fmt.Sprintf("fixture-operation-%d", resetSpendKeys.Add(1)), body)
		done <- resetSpendResult{code, answer}
	}()
	select {
	case <-arrived:
	case early := <-done:
		return early
	}
	during()
	releaseHeld()
	return <-done
}

// Gate r1 class A (MAJORs 1, 2): only an enabled OAuth seat spends, with the
// Claude quota poll's access-token selection, and only while the registry
// still holds that same credential when the claim is dispatched.
func TestClaudeResetSpendClassAOnlyTheSameOAuthCredentialSpends(t *testing.T) {
	t.Run("credential kinds", func(t *testing.T) {
		f := newResetSpendFixture(t, resetFixtureGrants()...)
		for _, seat := range []struct {
			id       string
			attrs    map[string]string
			meta     map[string]any
			disabled bool
			spends   bool
		}{
			{"api-key", map[string]string{"api_key": "fixture-api-key", "base_url": "https://claude.example.invalid"}, nil, false, false},
			{"oauth", nil, map[string]any{"access_token": resetFixtureToken, "refresh_token": "fixture-refresh"}, false, true},
			// A refresh writes access_token; a stale accessToken must not win.
			{"oauth-refreshed", nil, map[string]any{"accessToken": "fixture-stale-access-token", "access_token": resetFixtureToken}, false, true},
			{"api-key-kind", map[string]string{"auth_kind": "apikey"}, map[string]any{"access_token": resetFixtureToken}, false, false},
			{"session-token", map[string]string{"session_token": "fixture-session-token"}, nil, false, false},
			{"generic-token", nil, map[string]any{"token": "fixture-generic-token"}, false, false},
			{"id-token", nil, map[string]any{"id_token": "fixture-id-token"}, false, false},
			{"disabled-oauth", nil, map[string]any{"access_token": resetFixtureToken}, true, false},
		} {
			attrs := map[string]string{"runtime_only": "true"}
			for key, value := range seat.attrs {
				attrs[key] = value
			}
			auth := &coreauth.Auth{ID: seat.id, FileName: seat.id + ".json", Provider: "claude", Status: coreauth.StatusActive, Attributes: attrs, Metadata: seat.meta, Disabled: seat.disabled}
			if seat.disabled {
				auth.Status = coreauth.StatusDisabled
			}
			if _, err := f.h.authManager.Register(context.Background(), auth); err != nil {
				t.Fatal(err)
			}
			f.provider.set(func(p *fakeResetProvider) { p.bearers = nil })
			code, answer := f.spend(t, spendBody(map[string]any{"auth_index": f.seatIndex(t, seat.id), "kind": "fable"}))
			var bearers []string
			f.provider.set(func(p *fakeResetProvider) { bearers = append(bearers, p.bearers...) })
			if !seat.spends {
				if code == http.StatusOK || len(bearers) != 0 {
					t.Fatalf("%s: a seat that may not spend reached the provider %d times: status=%d answer=%v", seat.id, len(bearers), code, answer)
				}
				continue
			}
			if code != http.StatusOK || answer["result"] != "reset" || len(bearers) == 0 {
				t.Fatalf("%s: status=%d answer=%v", seat.id, code, answer)
			}
			for _, bearer := range bearers {
				if bearer != resetFixtureToken {
					t.Fatalf("%s: a spend request did not carry the seat's access_token", seat.id)
				}
			}
		}
	})
	for _, change := range []struct {
		name  string
		apply func(t *testing.T, m *coreauth.Manager)
	}{
		{"replaced", func(t *testing.T, m *coreauth.Manager) {
			auth, _ := m.GetByID("fixture")
			auth.Metadata = map[string]any{"access_token": resetFixtureOtherToken}
			if _, err := m.Update(context.Background(), auth); err != nil {
				t.Error(err)
			}
		}},
		{"disabled", func(t *testing.T, m *coreauth.Manager) {
			auth, _ := m.GetByID("fixture")
			auth.Disabled, auth.Status = true, coreauth.StatusDisabled
			if _, err := m.Update(context.Background(), auth); err != nil {
				t.Error(err)
			}
		}},
		{"removed", func(t *testing.T, m *coreauth.Manager) { m.Remove(context.Background(), "fixture") }},
	} {
		t.Run("changed during the read/"+change.name, func(t *testing.T) {
			f := newResetSpendFixture(t, resetFixtureGrants()...)
			got := f.spendPausedAt(t, "grants", spendBody(map[string]any{"auth_index": f.index, "kind": "fable"}), func() { change.apply(t, f.h.authManager) })
			if _, _, claims := f.provider.counts(); len(claims) != 0 || got.code == http.StatusOK {
				t.Fatalf("a credential %s during the read still spent: claims=%d status=%d answer=%v", change.name, len(claims), got.code, got.answer)
			}
		})
	}
}

// Gate r1 class B (MAJORs 3, 4, 5): every send, first or retry, is authorized
// by a fresh read taken under the account reservation and judged again on the
// clock after the durable write. In each column exactly one cell sends: the
// one whose grant is present and spendable.
func TestClaudeResetSpendClassBOnlyAFreshReadUnderTheReservationAuthorizesASend(t *testing.T) {
	grant := fakeResetGrant{ID: "fable-x", Label: "Fixture Fable", ResetsLeft: 1, EndsAt: resetFixtureEpoch.Add(30 * 24 * time.Hour).Format(time.RFC3339), Type: "full", GivesBackWeekly: true, Clears: []string{}, UsableNow: true}
	plain := func(t *testing.T, f *resetSpendFixture, body string) (resetSpendResult, int) {
		code, answer := f.spend(t, body)
		return resetSpendResult{code, answer}, 0
	}
	cells := []struct {
		name  string
		sends bool
		// run makes the cell's change and sends the attempt; it returns the
		// answer and the claims others made meanwhile.
		run func(t *testing.T, f *resetSpendFixture, body string, retry bool) (resetSpendResult, int)
	}{
		{"grant present", true, func(t *testing.T, f *resetSpendFixture, body string, _ bool) (resetSpendResult, int) {
			return plain(t, f, body)
		}},
		{"grant absent", false, func(t *testing.T, f *resetSpendFixture, body string, _ bool) (resetSpendResult, int) {
			f.provider.set(func(p *fakeResetProvider) { p.grants = []fakeResetGrant{} })
			return plain(t, f, body)
		}},
		{"seat ineligible", false, func(t *testing.T, f *resetSpendFixture, body string, _ bool) (resetSpendResult, int) {
			f.provider.set(func(p *fakeResetProvider) { p.eligible, p.reason = false, "fixture" })
			return plain(t, f, body)
		}},
		{"kind changed", false, func(t *testing.T, f *resetSpendFixture, body string, _ bool) (resetSpendResult, int) {
			f.provider.set(func(p *fakeResetProvider) { p.grants[0].GivesBackWeekly = false })
			return plain(t, f, body)
		}},
		{"expired during the write", false, func(t *testing.T, f *resetSpendFixture, body string, retry bool) (resetSpendResult, int) {
			if retry {
				// The retry starts one second inside its ten-minute window.
				f.clock.advance(claudeResetRetryWindow - time.Second)
			} else {
				f.provider.set(func(p *fakeResetProvider) { p.grants[0].EndsAt = f.clock.now().Add(time.Second).Format(time.RFC3339) })
			}
			f.jumpWhenSentIsDurable(2 * time.Second)
			return plain(t, f, body)
		}},
		{"spent by another seat meanwhile", false, func(t *testing.T, f *resetSpendFixture, body string, retry bool) (resetSpendResult, int) {
			outside := 0
			got := f.spendPausedAt(t, "profile", body, func() {
				if retry {
					// The unknown claim holds the account in the hub, so the
					// grant is spent by one of the account's other clients.
					f.provider.set(func(p *fakeResetProvider) { p.grants[0].ResetsLeft = 0 })
					return
				}
				code, answer := f.spend(t, spendBody(map[string]any{"auth_index": f.seatIndex(t, "twin"), "kind": "fable", "grant_id": "fable-x"}))
				if code != http.StatusOK || answer["result"] != "reset" {
					t.Errorf("the twin seat's spend: status=%d answer=%v", code, answer)
				}
				outside = 1
			})
			return got, outside
		}},
	}
	for _, retry := range []bool{false, true} {
		column, sending := "first", 0
		if retry {
			column = "retry"
		}
		for _, cell := range cells {
			t.Run(column+"/"+cell.name, func(t *testing.T) {
				f := newResetSpendFixture(t, grant)
				body := spendBody(map[string]any{"auth_index": f.index, "kind": "fable", "grant_id": "fable-x"})
				if retry {
					f.provider.set(func(p *fakeResetProvider) { p.claimStatus = http.StatusBadGateway })
					_, first := f.spend(t, body)
					requestID, _ := first["request_id"].(string)
					if first["result"] != "unknown" || requestID == "" {
						t.Fatalf("setup: answer=%v", first)
					}
					f.provider.set(func(p *fakeResetProvider) { p.claimStatus = 0 })
					body = spendBody(map[string]any{"auth_index": f.index, "kind": "fable", "retry_request_id": requestID})
				}
				_, _, before := f.provider.counts()
				got, outside := cell.run(t, f, body, retry)
				_, _, after := f.provider.counts()
				attempt := len(after) - len(before) - outside
				if attempt > 1 || (attempt == 1) != cell.sends || (got.code == http.StatusOK) != cell.sends {
					t.Fatalf("claims sent by this attempt=%d, want a send=%v: status=%d answer=%v", attempt, cell.sends, got.code, got.answer)
				}
				if cell.sends {
					sending++
				}
				if retry && !cell.sends && cell.name != "expired during the write" {
					if code, answer := f.spend(t, spendBody(map[string]any{"auth_index": f.index, "kind": "fable"})); code != http.StatusConflict {
						t.Fatalf("a refused retry released its hold: status=%d answer=%v", code, answer)
					}
				}
			})
		}
		if sending != 1 {
			t.Fatalf("%s column: %d cells sent, want exactly one", column, sending)
		}
	}
}

// Gate r1 class C (MAJORs 6, 8, 9, 10): the ledger is the truth. A damaged
// ledger, read by a hub restarted inside the retry window, never accepts a
// fresh spend on the seat or on another seat of its account.
func TestClaudeResetSpendClassCADamagedLedgerNeverReleasesAHold(t *testing.T) {
	account := claudeResetAccount(resetFixtureOrganization)
	line := func(f *resetSpendFixture, fields map[string]any, drop ...string) string {
		record := map[string]any{
			"phase": "sent", "operation": "0123456789abcdef0123456789abcdef", "account": account,
			"at": f.clock.now().Add(-time.Minute).Format(time.RFC3339Nano), "auth_index": f.index, "kind": "fable",
			"grant_id": "fable-late", "named_grant": "", "request_id": "fixture-request-1", "retry": false, "result": "unknown",
			"resets_left_before": map[string]int{"fable": 2},
		}
		for key, value := range fields {
			record[key] = value
		}
		for _, key := range drop {
			delete(record, key)
		}
		encoded, _ := json.Marshal(record)
		return string(encoded) + "\n"
	}
	answeredAt := func(f *resetSpendFixture) string {
		return f.clock.now().Add(-30 * time.Second).Format(time.RFC3339Nano)
	}
	attempts := func(t *testing.T, f *resetSpendFixture) (int, []int) {
		var codes []int
		for _, seat := range []string{"fixture", "twin"} {
			code, _ := f.spend(t, spendBody(map[string]any{"auth_index": f.seatIndex(t, seat), "kind": "fable"}))
			codes = append(codes, code)
		}
		_, _, claims := f.provider.counts()
		return len(claims), codes
	}
	for _, tc := range []struct {
		name    string
		ledger  func(f *resetSpendFixture) string
		accepts bool
	}{
		{"intact settled history", func(f *resetSpendFixture) string {
			return line(f, nil) + line(f, map[string]any{"phase": "answered", "result": "reset", "at": answeredAt(f)})
		}, true},
		{"missing timestamp", func(f *resetSpendFixture) string { return line(f, nil, "at") }, false},
		{"missing account", func(f *resetSpendFixture) string { return line(f, nil, "account") }, false},
		{"answered with no result", func(f *resetSpendFixture) string {
			return line(f, nil) + line(f, map[string]any{"phase": "answered", "at": answeredAt(f)}, "result")
		}, false},
		{"orphan retry", func(f *resetSpendFixture) string { return line(f, map[string]any{"retry": true}) }, false},
		{"unknown result", func(f *resetSpendFixture) string {
			return line(f, nil) + line(f, map[string]any{"phase": "answered", "result": "refunded", "at": answeredAt(f)})
		}, false},
		{"torn last line", func(f *resetSpendFixture) string { return line(f, nil) + `{"phase":"answered","operation":"01` }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newResetSpendFixture(t, resetFixtureGrants()...)
			if err := os.WriteFile(filepath.Join(f.logDir, claudeResetSpendLedgerName), []byte(tc.ledger(f)), 0o600); err != nil {
				t.Fatal(err)
			}
			f.restart()
			claims, codes := attempts(t, f)
			if tc.accepts && claims != 2 {
				t.Fatalf("an intact ledger refused a spend: claims=%d statuses=%v", claims, codes)
			}
			if !tc.accepts && (claims != 0 || codes[0] == http.StatusOK || codes[1] == http.StatusOK) {
				t.Fatalf("a damaged ledger accepted a fresh spend: claims=%d statuses=%v", claims, codes)
			}
		})
	}
	t.Run("missing file after a recorded send", func(t *testing.T) {
		f := newResetSpendFixture(t, resetFixtureGrants()...)
		f.provider.set(func(p *fakeResetProvider) { p.claimStatus = http.StatusBadGateway })
		if _, first := f.spend(t, spendBody(map[string]any{"auth_index": f.index, "kind": "fable"})); first["result"] != "unknown" {
			t.Fatalf("setup: answer=%v", first)
		}
		if err := os.Remove(filepath.Join(f.logDir, claudeResetSpendLedgerName)); err != nil {
			t.Fatal(err)
		}
		f.provider.set(func(p *fakeResetProvider) { p.claimStatus = 0 })
		f.restart()
		if claims, codes := attempts(t, f); claims != 1 || codes[0] == http.StatusOK || codes[1] == http.StatusOK {
			t.Fatalf("a lost ledger accepted a fresh spend: claims=%d statuses=%v", claims, codes)
		}
	})
}

// Gate r1 class D (MAJOR 7): the caller can replay a spend safely. The same
// Idempotency-Key delivered again returns the recorded outcome and sends
// nothing, before and after a restart; the key naming another operation is a
// conflict; a spend without a key is refused before any provider call.
func TestClaudeResetSpendClassDARedeliveredSpendSendsNothing(t *testing.T) {
	f := newResetSpendFixture(t, fakeResetGrant{ID: "fable-two", Label: "Fixture Fable", ResetsLeft: 2, EndsAt: resetFixtureEpoch.Add(30 * 24 * time.Hour).Format(time.RFC3339), Type: "full", GivesBackWeekly: true, Clears: []string{}, UsableNow: true})
	const key = "fixture-operation-replayed"
	body := spendBody(map[string]any{"auth_index": f.index, "kind": "fable", "grant_id": "fable-two"})
	code, first := f.spendKeyed(t, key, body)
	if code != http.StatusOK || first["result"] != "reset" {
		t.Fatalf("status=%d answer=%v", code, first)
	}
	// The response is lost; the caller delivers the identical request again.
	for attempt := 0; attempt < 2; attempt++ {
		if attempt == 1 {
			f.restart()
		}
		code, again := f.spendKeyed(t, key, body)
		if code != http.StatusOK || again["result"] != "reset" || again["request_id"] != first["request_id"] || again["replayed"] != true {
			t.Fatalf("redelivery %d: status=%d answer=%v", attempt, code, again)
		}
	}
	requests, _, claims := f.provider.counts()
	left := -1
	f.provider.set(func(p *fakeResetProvider) { left = p.grants[0].ResetsLeft })
	if len(claims) != 1 || left != 1 {
		t.Fatalf("one operation sent %d claims and left %d credits", len(claims), left)
	}
	for _, other := range []string{
		spendBody(map[string]any{"auth_index": f.index, "kind": "fable"}),
		spendBody(map[string]any{"auth_index": f.index, "kind": "full"}),
		spendBody(map[string]any{"auth_index": f.seatIndex(t, "other"), "kind": "fable", "grant_id": "fable-two"}),
	} {
		if code, answer := f.spendKeyed(t, key, other); code != http.StatusConflict {
			t.Fatalf("%s under a used key: status=%d answer=%v", other, code, answer)
		}
	}
	if code, answer := f.spendKeyed(t, "", body); code != http.StatusBadRequest {
		t.Fatalf("no Idempotency-Key: status=%d answer=%v", code, answer)
	}
	if after, _, _ := f.provider.counts(); after != requests {
		t.Fatalf("a conflicting or unkeyed spend reached the provider %d times", after-requests)
	}
	// The ledger answers the key whatever has become of the seat since: a
	// seat disabled, then removed, still gets its recorded outcome back.
	for _, step := range []struct {
		name   string
		change func()
	}{
		{"disabled", func() {
			auth, _ := f.h.authManager.GetByID("fixture")
			auth.Disabled, auth.Status = true, coreauth.StatusDisabled
			if _, err := f.h.authManager.Update(context.Background(), auth); err != nil {
				t.Fatal(err)
			}
		}},
		{"removed", func() { f.h.authManager.Remove(context.Background(), "fixture") }},
	} {
		step.change()
		code, again := f.spendKeyed(t, key, body)
		if code != http.StatusOK || again["result"] != "reset" || again["replayed"] != true {
			t.Fatalf("redelivery after the seat was %s: status=%d answer=%v", step.name, code, again)
		}
	}
	if after, _, _ := f.provider.counts(); after != requests {
		t.Fatalf("a redelivery reached the provider %d times", after-requests)
	}
}

// ledgerRecords reads every line of the fixture's ledger.
func (f *resetSpendFixture) ledgerRecords(t *testing.T) []map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(f.logDir, claudeResetSpendLedgerName))
	if err != nil {
		t.Fatal(err)
	}
	var records []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var record map[string]any
		if errDecode := json.Unmarshal([]byte(line), &record); errDecode != nil {
			t.Fatalf("ledger line is not JSON: %s", line)
		}
		records = append(records, record)
	}
	return records
}

// Gate r1 MAJOR 8: a known outcome is on disk before the balance read, so a
// hub that dies while that read hangs accepts no retry of a confirmed claim;
// and an outcome that cannot be written keeps the hold and says so.
func TestClaudeResetSpendRecordsAKnownOutcomeAtOnce(t *testing.T) {
	t.Run("restart while the balance read hangs", func(t *testing.T) {
		f := newResetSpendFixture(t, resetFixtureGrants()...)
		var reads atomic.Int32
		arrived, release := make(chan struct{}), make(chan struct{})
		// Released before any assertion can end the test (gate r2 MINOR 20).
		var releaseOnce sync.Once
		releaseRead := func() { releaseOnce.Do(func() { close(release) }) }
		t.Cleanup(releaseRead)
		f.provider.set(func(p *fakeResetProvider) {
			p.onRead = func() {
				if reads.Add(1) == 2 {
					close(arrived)
					<-release
				}
			}
		})
		done := make(chan int, 1)
		go func() {
			code, _ := r2SpendQuietly(f.h, fmt.Sprintf("fixture-operation-%d", resetSpendKeys.Add(1)), spendBody(map[string]any{"auth_index": f.index, "kind": "fable"}))
			done <- code
		}()
		select {
		case <-arrived:
		case code := <-done:
			t.Fatalf("the spend answered %d before its balance read", code)
		}
		records := f.ledgerRecords(t)
		if len(records) != 2 || records[1]["phase"] != "answered" || records[1]["result"] != "reset" {
			t.Fatalf("the confirmed outcome was not on disk before the balance read: %v", records)
		}
		requestID, _ := records[0]["request_id"].(string)
		f.restart()
		code, answer := f.spend(t, spendBody(map[string]any{"auth_index": f.index, "kind": "fable", "retry_request_id": requestID}))
		releaseRead()
		if spent := <-done; spent != http.StatusOK {
			t.Fatalf("the spend answered %d", spent)
		}
		if code != http.StatusConflict {
			t.Fatalf("a restarted hub accepted a retry of a confirmed claim: status=%d answer=%v", code, answer)
		}
		if _, _, claims := f.provider.counts(); len(claims) != 1 {
			t.Fatalf("claims=%+v", claims)
		}
	})
	t.Run("the outcome cannot be written", func(t *testing.T) {
		f := newResetSpendFixture(t, resetFixtureGrants()...)
		ledgerPath := filepath.Join(f.logDir, claudeResetSpendLedgerName)
		f.provider.set(func(p *fakeResetProvider) {
			p.onClaim = func(fakeResetClaim) {
				if err := os.Rename(ledgerPath, ledgerPath+".aside"); err != nil {
					t.Error(err)
				}
				if err := os.Mkdir(ledgerPath, 0o700); err != nil {
					t.Error(err)
				}
			}
		})
		code, answer := f.spend(t, spendBody(map[string]any{"auth_index": f.index, "kind": "fable"}))
		if code != http.StatusOK || answer["result"] != "reset" || answer["ledger_error"] == nil {
			t.Fatalf("status=%d answer=%v", code, answer)
		}
		requestID, _ := answer["request_id"].(string)
		if err := os.Remove(ledgerPath); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(ledgerPath+".aside", ledgerPath); err != nil {
			t.Fatal(err)
		}
		f.provider.set(func(p *fakeResetProvider) { p.onClaim = nil })
		bodies := []string{
			spendBody(map[string]any{"auth_index": f.index, "kind": "full"}),
			spendBody(map[string]any{"auth_index": f.index, "kind": "fable", "retry_request_id": requestID}),
		}
		// The running hub refuses every spend once a write failed.
		for _, body := range bodies {
			if code, answer := f.spend(t, body); code != http.StatusServiceUnavailable || answer["ledger_refusal"] == nil {
				t.Fatalf("%s after an unwritten outcome: status=%d answer=%v", body, code, answer)
			}
		}
		// Round 2, R2-2: after a restart the ledger, which holds the sent line
		// and no answer, still holds the seat and refuses the retry.
		f.restart()
		for _, body := range bodies {
			code, answer := f.spend(t, body)
			if code != http.StatusConflict {
				t.Fatalf("%s after a restart: status=%d answer=%v", body, code, answer)
			}
			if strings.Contains(body, "retry_request_id") && !strings.Contains(fmt.Sprint(answer["error"]), "no retry is accepted") {
				t.Fatalf("the retry after a restart: answer=%v", answer)
			}
		}
		if _, _, claims := f.provider.counts(); len(claims) != 1 {
			t.Fatalf("a claim the hub knew had succeeded was sent again: claims=%+v", claims)
		}
	})
}

// r2SpendQuietly is spendKeyed without any t.Fatal, so it can run on any
// goroutine of a test.
func r2SpendQuietly(h *Handler, key, body string) (int, map[string]any) {
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v8/management/claude/limit-resets/spend", strings.NewReader(body))
	ctx.Request.Header.Set("Content-Type", "application/json")
	if key != "" {
		ctx.Request.Header.Set("Idempotency-Key", key)
	}
	h.SpendClaudeLimitReset(ctx)
	var answer map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &answer)
	return rec.Code, answer
}

// Gate r1 MAJOR 10: holds belong to the ledger, not to a handler. A second
// handler on the same ledger, and a handler moved to another log directory,
// both see a claim whose outcome is unknown.
func TestClaudeResetSpendHoldsBelongToTheLedger(t *testing.T) {
	f := newResetSpendFixture(t, resetFixtureGrants()...)
	elsewhere := t.TempDir()
	second := &Handler{cfg: f.h.cfg, authManager: f.h.authManager, logDir: f.logDir, claudeResetClient: f.h.claudeResetClient}
	moved := &Handler{cfg: f.h.cfg, authManager: f.h.authManager, logDir: elsewhere, claudeResetClient: f.h.claudeResetClient}
	for _, h := range []*Handler{second, moved} {
		h.claudeResetSpends.now = f.clock.now
		// Each has read its ledger, empty so far.
		if history := h.claudeResetHistory(); history.err != nil || len(history.last) != 0 {
			t.Fatalf("history=%+v", history)
		}
	}
	f.provider.set(func(p *fakeResetProvider) { p.claimStatus = http.StatusBadGateway })
	if _, first := f.spend(t, spendBody(map[string]any{"auth_index": f.index, "kind": "fable"})); first["result"] != "unknown" {
		t.Fatalf("answer=%v", first)
	}
	f.provider.set(func(p *fakeResetProvider) { p.claimStatus = 0 })
	moved.logDir = f.logDir
	for name, h := range map[string]*Handler{"second handler": second, "moved handler": moved} {
		f.h = h
		if code, answer := f.spend(t, spendBody(map[string]any{"auth_index": f.index, "kind": "full"})); code != http.StatusConflict {
			t.Fatalf("%s spent past the hold: status=%d answer=%v", name, code, answer)
		}
	}
	if _, _, claims := f.provider.counts(); len(claims) != 1 {
		t.Fatalf("claims=%+v", claims)
	}
}

// Gate r1 MINOR 11: a body that could be read two ways is refused before any
// provider call: a duplicate key, or a selector sent empty or null.
func TestClaudeResetSpendRefusesAmbiguousBodies(t *testing.T) {
	f := newResetSpendFixture(t, resetFixtureGrants()...)
	index := `"auth_index":"` + f.index + `"`
	for _, body := range []string{
		`{` + index + `,"kind":"fable","kind":"full"}`,
		`{` + index + `,` + index + `,"kind":"fable"}`,
		`{` + index + `,"kind":"fable","grant_id":"fable-late","grant_id":"fable-soon"}`,
		`{` + index + `,"kind":"fable","grant_id":""}`,
		`{` + index + `,"kind":"fable","grant_id":"  "}`,
		`{` + index + `,"kind":"fable","grant_id":null}`,
		`{` + index + `,"kind":"fable","retry_request_id":""}`,
		`{` + index + `,"kind":"fable","retry_request_id":null}`,
		`{` + index + `,"kind":"fable"} {}`,
		// Trailing input past the size limit is never hidden by the limit.
		`{` + index + `,"kind":"fable"}` + strings.Repeat(" ", 1<<16-len(`{`+index+`,"kind":"fable"}`)) + `{}`,
		`{` + index + `,"kind":"fable"}` + strings.Repeat(" ", 1<<16),
	} {
		if code, answer := f.spend(t, body); code != http.StatusBadRequest {
			t.Fatalf("%s: status=%d answer=%v", body, code, answer)
		}
	}
	if requests, _, _ := f.provider.counts(); requests != 0 {
		t.Fatalf("an ambiguous body reached the provider %d times", requests)
	}
}

// Gate r1 MINOR 13: the spend's grants read keeps the established reader's
// checks: it asks for no cached copy, and a response with no valid usage
// window authorizes nothing.
func TestClaudeResetSpendReadsKeepTheEstablishedChecks(t *testing.T) {
	f := newResetSpendFixture(t, resetFixtureGrants()...)
	if code, answer := f.spend(t, spendBody(map[string]any{"auth_index": f.index, "kind": "fable"})); code != http.StatusOK {
		t.Fatalf("status=%d answer=%v", code, answer)
	}
	var controls []string
	f.provider.set(func(p *fakeResetProvider) { controls = append(controls, p.cacheControls...) })
	if len(controls) != 2 || controls[0] != "no-cache" || controls[1] != "no-cache" {
		t.Fatalf("Cache-Control of the reads=%q", controls)
	}
	g := newResetSpendFixture(t, resetFixtureGrants()...)
	g.provider.set(func(p *fakeResetProvider) { p.noWindows = true })
	if code, answer := g.spend(t, spendBody(map[string]any{"auth_index": g.index, "kind": "fable"})); code != http.StatusBadGateway {
		t.Fatalf("a read with no usage window: status=%d answer=%v", code, answer)
	}
	if _, _, claims := g.provider.counts(); len(claims) != 0 {
		t.Fatalf("claims=%+v", claims)
	}
}

// Gate r1 MINOR 17: the provider's reason is kept only as a code; free text,
// which could carry an identity, never reaches the answer or the ledger.
func TestClaudeResetSpendKeepsOnlyAReasonCode(t *testing.T) {
	for reason, want := range map[string]string{
		"not_rate_limited": "not_rate_limited",
		"Seat fixture@example.invalid of org 00000000-0000-4000-8000-0000000000aa is not limited": "unrecognized",
	} {
		f := newResetSpendFixture(t, resetFixtureGrants()...)
		f.provider.set(func(p *fakeResetProvider) { p.claimResult, p.claimReason = "not_limited", reason })
		code, answer := f.spend(t, spendBody(map[string]any{"auth_index": f.index, "kind": "fable"}))
		provider, _ := answer["provider"].(map[string]any)
		if code != http.StatusOK || provider["reason"] != want {
			t.Fatalf("%q: status=%d answer=%v", reason, code, answer)
		}
		records := f.ledgerRecords(t)
		if records[1]["reason"] != want {
			t.Fatalf("%q: ledger=%v", reason, records)
		}
		raw, _ := os.ReadFile(filepath.Join(f.logDir, claudeResetSpendLedgerName))
		if strings.Contains(string(raw), "example.invalid") {
			t.Fatalf("the ledger holds the provider's free text: %s", raw)
		}
	}
}

// Self-review r1 (MAJOR): a directory sync that fails is retried by every
// later append until one succeeds, so no claim leaves before the ledger's
// directory entries are durable; and a failed append is cut back off the
// ledger, so a restart reads exactly what memory holds.
func TestClaudeResetSpendLedgerIsDurableBeforeAnyClaim(t *testing.T) {
	f := newResetSpendFixture(t, resetFixtureGrants()...)
	var syncs atomic.Int32
	var calls [][]string
	claudeResetSyncDirectories = func(dirs []string) error {
		if syncs.Add(1) == 1 {
			return errors.New("fixture directory sync failure")
		}
		calls = append(calls, append([]string(nil), dirs...))
		return syncClaudeResetDirectories(dirs)
	}
	t.Cleanup(func() { claudeResetSyncDirectories = syncClaudeResetDirectories })
	ledgerPath := filepath.Join(f.logDir, claudeResetSpendLedgerName)
	// A failed directory sync refuses that spend and every one after it until
	// a restart, and leaves no record behind.
	for attempt := 1; attempt <= 2; attempt++ {
		code, answer := f.spend(t, spendBody(map[string]any{"auth_index": f.index, "kind": "fable"}))
		if code != http.StatusServiceUnavailable || answer["ledger_refusal"] != claudeResetRefusalWriteFailed {
			t.Fatalf("attempt %d after a failed directory sync: status=%d answer=%v", attempt, code, answer)
		}
		if info, err := os.Stat(ledgerPath); err != nil || info.Size() != 0 {
			t.Fatalf("attempt %d left a record behind: %v %v", attempt, info, err)
		}
	}
	if got := syncs.Load(); got != 1 {
		t.Fatalf("directory syncs=%d, want none after the journal refused", got)
	}
	f.restart()
	if code, answer := f.spend(t, spendBody(map[string]any{"auth_index": f.index, "kind": "fable"})); code != http.StatusOK || answer["result"] != "reset" {
		t.Fatalf("after a restart: status=%d answer=%v", code, answer)
	}
	// The first durable append synced the ledger's directory and every
	// ancestor before the claim left (the ledger's entry), then the
	// ledger's directory again (the sentinel's); Codex round 6 MAJOR 3.
	dir := f.h.claudeResetSpends.mustDir(t, f.logDir)
	if len(calls) != 2 || len(calls[0]) < 2 || calls[0][0] != dir || calls[0][1] != filepath.Dir(dir) || len(calls[1]) != 1 || calls[1][0] != dir {
		t.Fatalf("directories synced=%v", calls)
	}
	if _, _, claims := f.provider.counts(); len(claims) != 1 {
		t.Fatalf("claims=%+v", claims)
	}
	if records := f.ledgerRecords(t); len(records) != 3 {
		t.Fatalf("records=%v", records)
	}
	f.restart()
	if code, answer := f.spend(t, spendBody(map[string]any{"auth_index": f.index, "kind": "full"})); code != http.StatusOK {
		t.Fatalf("after a second restart: status=%d answer=%v", code, answer)
	}
}

// Self-review r1 (MINOR): spending the last grant leaves an empty balance,
// which the ledger keeps as an empty map, so a restarted hub still reads it.
func TestClaudeResetSpendOfTheLastGrantKeepsTheLedgerReadable(t *testing.T) {
	f := newResetSpendFixture(t, fakeResetGrant{ID: "fable-only", Label: "Fixture Fable", ResetsLeft: 1, EndsAt: resetFixtureEpoch.Add(30 * 24 * time.Hour).Format(time.RFC3339), Type: "full", GivesBackWeekly: true, Clears: []string{}, UsableNow: true})
	f.provider.set(func(p *fakeResetProvider) {
		p.onClaim = func(fakeResetClaim) { f.provider.set(func(p *fakeResetProvider) { p.grants = []fakeResetGrant{} }) }
	})
	if code, answer := f.spend(t, spendBody(map[string]any{"auth_index": f.index, "kind": "fable"})); code != http.StatusOK || answer["result"] != "reset" {
		t.Fatalf("status=%d answer=%v", code, answer)
	}
	f.restart()
	history := f.h.claudeResetHistory()
	last, err := history.seat(f.index)
	if err != nil || last == nil || last.Result != "reset" || last.ResetsLeftAfter == nil || len(last.ResetsLeftAfter) != 0 {
		t.Fatalf("after a restart: last=%+v err=%v", last, err)
	}
}

// Self-review r1 (MINOR): a first claim's key delivered again after a retry
// settled the claim reports the retry's outcome, not a stale unknown.
func TestClaudeResetSpendReplayShowsTheSettlingRetry(t *testing.T) {
	f := newResetSpendFixture(t, resetFixtureGrants()...)
	f.provider.set(func(p *fakeResetProvider) { p.claimStatus = http.StatusBadGateway })
	body := spendBody(map[string]any{"auth_index": f.index, "kind": "fable"})
	_, first := f.spendKeyed(t, "fixture-first-claim", body)
	requestID, _ := first["request_id"].(string)
	if first["result"] != "unknown" || requestID == "" {
		t.Fatalf("answer=%v", first)
	}
	f.provider.set(func(p *fakeResetProvider) { p.claimStatus = 0 })
	if code, answer := f.spend(t, spendBody(map[string]any{"auth_index": f.index, "kind": "fable", "retry_request_id": requestID})); code != http.StatusOK || answer["result"] != "reset" {
		t.Fatalf("retry: status=%d answer=%v", code, answer)
	}
	code, again := f.spendKeyed(t, "fixture-first-claim", body)
	latest, _ := again["latest"].(map[string]any)
	if code != http.StatusOK || again["result"] != "unknown" || latest["result"] != "reset" || latest["retry"] != true || again["retry_until"] != nil {
		t.Fatalf("replay of the first claim: status=%d answer=%v", code, again)
	}
}

// Gate r1 Codex MINOR 6: the hub log carries what was left after a spend,
// beside the outcome it logged as soon as the outcome was known.
func TestClaudeResetSpendLogsTheBalance(t *testing.T) {
	hook := new(logtest.Hook)
	previous := log.StandardLogger().ReplaceHooks(log.LevelHooks{})
	log.AddHook(hook)
	t.Cleanup(func() { log.StandardLogger().ReplaceHooks(previous) })
	f := newResetSpendFixture(t, resetFixtureGrants()...)
	code, answer := f.spend(t, spendBody(map[string]any{"auth_index": f.index, "kind": "fable"}))
	if code != http.StatusOK {
		t.Fatalf("status=%d answer=%v", code, answer)
	}
	for _, entry := range hook.AllEntries() {
		if entry.Message != "Claude banked reset spend balance" {
			continue
		}
		after, _ := entry.Data["resets_left_after"].(map[string]int)
		if entry.Data["request_id"] != answer["request_id"] || after["fable"] != 1 {
			t.Fatalf("balance log=%v", entry.Data)
		}
		return
	}
	t.Fatal("no balance was logged")
}

// ---------------------------------------------------------------- round 2

// round2Grants gives each kind one grant with two credits, so a second claim
// on one grant shows as a second credit spent.
func round2Grants() []fakeResetGrant {
	end := resetFixtureEpoch.Add(30 * 24 * time.Hour).Format(time.RFC3339)
	return []fakeResetGrant{
		{ID: "fable-two", Label: "Fixture Fable", ResetsLeft: 2, EndsAt: end, Type: "full", GivesBackWeekly: true, Clears: []string{}, UsableNow: true},
		{ID: "weekly-two", Label: "Fixture weekly", ResetsLeft: 2, EndsAt: end, Type: "weekly", Clears: []string{"seven_day"}, UsableNow: true},
	}
}

// round2SecondProcessDir names the ledger directory of the second hub
// process; round2SecondProcessHold keeps that process, and its lock, alive
// until its standard input closes.
const (
	round2SecondProcessDir  = "CLAUDE_RESET_SECOND_PROCESS_DIR"
	round2SecondProcessHold = "CLAUDE_RESET_SECOND_PROCESS_HOLD"
)

// TestClaudeResetSpendLedgerSecondProcessHelper is the second hub process of
// the R2-1 table, run by re-executing this test binary: it opens the journal
// of the directory it is given through one spend on its own fake provider,
// and prints what happened.
func TestClaudeResetSpendLedgerSecondProcessHelper(t *testing.T) {
	dir := os.Getenv(round2SecondProcessDir)
	if dir == "" {
		t.Skip("runs only as the second process of the ledger table")
	}
	f := newResetSpendFixture(t, round2Grants()...)
	f.h.logDir, f.logDir = dir, dir
	code, answer := r2SpendQuietly(f.h, "round2-second-process", spendBody(map[string]any{"auth_index": f.index, "kind": "weekly"}))
	_, _, claims := f.provider.counts()
	fmt.Printf("second-process: status=%d claims=%d refusal=%v\n", code, len(claims), answer["ledger_refusal"])
	if os.Getenv(round2SecondProcessHold) != "" {
		fmt.Println("second-process: holding")
		_, _ = io.Copy(io.Discard, os.Stdin)
	}
}

// round2SecondProcess runs the second hub process on dir and returns its
// report. With hold, the process keeps its lock until release is called.
func round2SecondProcess(t *testing.T, dir string, hold bool) (string, func(), error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestClaudeResetSpendLedgerSecondProcessHelper$", "-test.count=1")
	cmd.Env = append(os.Environ(), round2SecondProcessDir+"="+dir)
	if hold {
		cmd.Env = append(cmd.Env, round2SecondProcessHold+"=1")
	}
	stdin, errIn := cmd.StdinPipe()
	stdout, errOut := cmd.StdoutPipe()
	if errIn != nil || errOut != nil {
		cancel()
		return "", func() {}, errors.Join(errIn, errOut)
	}
	if errStart := cmd.Start(); errStart != nil {
		cancel()
		return "", func() {}, errStart
	}
	var once sync.Once
	release := func() {
		once.Do(func() {
			_ = stdin.Close()
			go func() { _, _ = io.Copy(io.Discard, stdout) }()
			_ = cmd.Wait()
			cancel()
		})
	}
	t.Cleanup(release)
	report := ""
	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "second-process: status=") {
			report = line
			if !hold {
				break
			}
		}
		if line == "second-process: holding" {
			break
		}
	}
	if !hold {
		release()
	}
	if report == "" {
		return "", release, errors.New("the second process printed no report")
	}
	return report, release, nil
}

// Round 2, R2-1 (gate r2 MAJORs 2, 3 and 6; read r2 MAJOR 1 and MINORs 3 and
// 8): the hub owns its ledger file and checks it before every use. Whatever
// happens to the file under a running hub, before a first claim, between a
// claim's sent and answered lines, or after its answer, no further claim
// leaves, every later spend and replay is refused with the ledger's path and
// the cure, and nothing is ever recreated beside the sentinel. A second hub
// process on the directory, by any spelling, refuses every spend.
func TestClaudeResetSpendR2ALedgerChangedUnderARunningHubSendsNothingMore(t *testing.T) {
	var failNextSync atomic.Bool
	claudeResetSyncFile = func(file *os.File) error {
		if strings.HasSuffix(file.Name(), claudeResetSpendLedgerName) && failNextSync.CompareAndSwap(true, false) {
			return errors.New("fixture sync failure after a written line")
		}
		return file.Sync()
	}
	t.Cleanup(func() { claudeResetSyncFile = func(f *os.File) error { return f.Sync() } })
	lastLineStart := func(raw []byte) int {
		return bytes.LastIndexByte(bytes.TrimSuffix(raw, []byte("\n")), '\n') + 1
	}
	events := []struct {
		name  string
		apply func(path string) error
	}{
		{"removed", os.Remove},
		{"replaced by an empty file", func(path string) error {
			return errors.Join(os.Remove(path), os.WriteFile(path, nil, 0o600))
		}},
		{"replaced by an older copy", func(path string) error {
			raw, errRead := os.ReadFile(path)
			if errRead != nil {
				return errRead
			}
			return errors.Join(os.Remove(path), os.WriteFile(path, raw[:bytes.IndexByte(raw, '\n')+1], 0o600))
		}},
		{"truncated to a record boundary", func(path string) error {
			raw, errRead := os.ReadFile(path)
			if errRead != nil {
				return errRead
			}
			return os.Truncate(path, int64(lastLineStart(raw)))
		}},
		{"truncated mid-record", func(path string) error {
			raw, errRead := os.ReadFile(path)
			if errRead != nil {
				return errRead
			}
			start := lastLineStart(raw)
			return os.Truncate(path, int64(start+(len(raw)-start)/2))
		}},
		{"the next line's sync fails after its write", nil},
	}
	for _, event := range events {
		for _, when := range []string{"before a first claim", "between sent and answered", "after answered"} {
			t.Run(event.name+"/"+when, func(t *testing.T) {
				f := newResetSpendFixture(t, round2Grants()...)
				path := filepath.Join(f.logDir, claudeResetSpendLedgerName)
				bodyA := spendBody(map[string]any{"auth_index": f.index, "kind": "fable", "grant_id": "fable-two"})
				if code, first := f.spendKeyed(t, "round2-key-a", bodyA); code != http.StatusOK || first["result"] != "reset" {
					t.Fatalf("setup: status=%d answer=%v", code, first)
				}
				strike := func() {
					if event.apply == nil {
						failNextSync.Store(true)
						return
					}
					if err := event.apply(path); err != nil {
						t.Error(err)
					}
				}
				var reads atomic.Int32
				switch when {
				case "before a first claim":
					strike()
				case "between sent and answered":
					f.provider.set(func(p *fakeResetProvider) { p.onClaim = func(fakeResetClaim) { strike() } })
				default:
					f.provider.set(func(p *fakeResetProvider) {
						p.onRead = func() {
							if reads.Add(1) == 2 {
								strike()
							}
						}
					})
				}
				bodyB := spendBody(map[string]any{"auth_index": f.seatIndex(t, "other"), "kind": "weekly", "grant_id": "weekly-two"})
				codeB, answerB := f.spendKeyed(t, "round2-key-b", bodyB)
				failNextSync.Store(false)
				f.provider.set(func(p *fakeResetProvider) { p.onClaim, p.onRead = nil, nil })
				_, _, claims := f.provider.counts()
				switch {
				case when == "before a first claim" && (codeB != http.StatusServiceUnavailable || len(claims) != 1):
					t.Fatalf("the spend after the damage: status=%d answer=%v claims=%d", codeB, answerB, len(claims))
				case when == "between sent and answered" && (answerB["ledger_error"] == nil || len(claims) != 2):
					t.Fatalf("the spend in flight at the damage: status=%d answer=%v claims=%d", codeB, answerB, len(claims))
				case when == "after answered" && (answerB["balance_error"] == nil || len(claims) != 2):
					t.Fatalf("the spend answered at the damage: status=%d answer=%v claims=%d", codeB, answerB, len(claims))
				}
				for _, again := range []struct{ key, body string }{
					{"round2-key-c", spendBody(map[string]any{"auth_index": f.seatIndex(t, "twin"), "kind": "fable"})},
					{"round2-key-a", bodyA},
					{"round2-key-b", bodyB},
				} {
					code, answer := f.spendKeyed(t, again.key, again.body)
					if code != http.StatusServiceUnavailable || answer["ledger_refusal"] == nil || answer["ledger"] != filepath.Join(f.h.claudeResetSpends.mustDir(t, f.logDir), claudeResetSpendLedgerName) || answer["cure"] == nil {
						t.Fatalf("key %s after the damage: status=%d answer=%v", again.key, code, answer)
					}
				}
				if _, _, after := f.provider.counts(); len(after) != len(claims) {
					t.Fatalf("a claim left after the damage: claims=%d, then %d", len(claims), len(after))
				}
				if _, err := f.h.claudeResetHistory().seat(f.index); err == nil {
					t.Fatal("the listing trusted a ledger the hub refuses")
				}
				if event.name == "removed" {
					if _, err := os.Lstat(path); !os.IsNotExist(err) {
						t.Fatalf("a ledger was recreated beside its sentinel: %v", err)
					}
				}
			})
		}
	}

	// A second hub process on the same directory: whichever process holds the
	// lock first owns the ledger, and the other refuses every spend.
	t.Run("a second process/before a first claim", func(t *testing.T) {
		f := newResetSpendFixture(t, round2Grants()...)
		report, release, err := round2SecondProcess(t, f.logDir, true)
		if err != nil || !strings.Contains(report, "status=200 claims=1") {
			t.Fatalf("the second process did not take the ledger: %q %v", report, err)
		}
		code, answer := f.spend(t, spendBody(map[string]any{"auth_index": f.index, "kind": "fable"}))
		release()
		if requests, _, _ := f.provider.counts(); code != http.StatusServiceUnavailable || answer["ledger_refusal"] != claudeResetRefusalHeld || requests != 0 {
			t.Fatalf("this process spent beside the owner: status=%d answer=%v requests=%d", code, answer, requests)
		}
	})
	for _, when := range []string{"between sent and answered", "after answered"} {
		t.Run("a second process/"+when, func(t *testing.T) {
			f := newResetSpendFixture(t, round2Grants()...)
			reports := make(chan string, 1)
			second := func() {
				report, _, err := round2SecondProcess(t, f.logDir, false)
				if err != nil {
					report = err.Error()
				}
				reports <- report
			}
			if when == "between sent and answered" {
				f.provider.set(func(p *fakeResetProvider) { p.onClaim = func(fakeResetClaim) { second() } })
			}
			code, answer := f.spend(t, spendBody(map[string]any{"auth_index": f.index, "kind": "fable"}))
			if code != http.StatusOK || answer["result"] != "reset" || answer["ledger_error"] != nil || answer["balance_error"] != nil {
				t.Fatalf("the owner's spend: status=%d answer=%v", code, answer)
			}
			if when == "after answered" {
				second()
			}
			if report := <-reports; report != "second-process: status=503 claims=0 refusal="+claudeResetRefusalHeld {
				t.Fatalf("the second process: %q", report)
			}
		})
	}

	// Two spellings of one directory are one lock: a symlink resolves to the
	// same journal, and a letter-case variant on a volume that folds case
	// meets the lock the first journal holds.
	t.Run("the same directory by a symlink or another letter case", func(t *testing.T) {
		f := newResetSpendFixture(t, round2Grants()...)
		// A lettered directory name, so a letter-case variant spells it differently.
		f.logDir = filepath.Join(t.TempDir(), "ledger")
		f.h.logDir = f.logDir
		f.provider.set(func(p *fakeResetProvider) { p.claimStatus = http.StatusBadGateway })
		if _, first := f.spend(t, spendBody(map[string]any{"auth_index": f.index, "kind": "fable"})); first["result"] != "unknown" {
			t.Fatalf("setup: %v", first)
		}
		f.provider.set(func(p *fakeResetProvider) { p.claimStatus = 0 })
		link := filepath.Join(t.TempDir(), "ledger-link")
		if err := os.Symlink(f.logDir, link); err != nil {
			t.Fatal(err)
		}
		spellings := []string{link}
		variant := filepath.Join(filepath.Dir(f.logDir), strings.ToUpper(filepath.Base(f.logDir)))
		if one, errOne := os.Stat(f.logDir); errOne == nil {
			if two, errTwo := os.Stat(variant); errTwo == nil && os.SameFile(one, two) && variant != f.logDir {
				spellings = append(spellings, variant)
			} else {
				t.Log("the volume does not fold letter case; only the symlink is checked")
			}
		}
		for _, spelling := range spellings {
			second := &Handler{cfg: f.h.cfg, authManager: f.h.authManager, logDir: spelling, claudeResetClient: f.h.claudeResetClient}
			second.claudeResetSpends.now = f.clock.now
			t.Cleanup(func() { forgetClaudeResetJournal(spelling) })
			for _, seat := range []string{"fixture", "twin"} {
				code, answer := r2SpendQuietly(second, "round2-spelling-"+seat+"-"+filepath.Base(spelling), spendBody(map[string]any{"auth_index": f.seatIndex(t, seat), "kind": "fable"}))
				if code == http.StatusOK {
					t.Fatalf("%s through %s spent past the hold: %v", seat, spelling, answer)
				}
			}
			report, _, err := round2SecondProcess(t, spelling, false)
			if err != nil || report != "second-process: status=503 claims=0 refusal="+claudeResetRefusalHeld {
				t.Fatalf("a second process through %s: %q %v", spelling, report, err)
			}
		}
		if _, _, claims := f.provider.counts(); len(claims) != 1 {
			t.Fatalf("claims=%d", len(claims))
		}
	})
}

// mustDir is the journal identity of dir, as a refusal names its ledger.
func (l *claudeResetSpendLedger) mustDir(t *testing.T, dir string) string {
	t.Helper()
	resolved, err := claudeResetJournalDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

// Round 2, R2-1 after a restart (gate r2 MAJOR 2; read r2 MAJOR 1): a ledger
// missing, emptied, or shorter than the length its sentinel records is
// refused on load, so a redelivered key never claims twice.
func TestClaudeResetSpendR2AShortenedLedgerIsRefusedAfterARestart(t *testing.T) {
	for _, tc := range []struct {
		name   string
		damage func(path string) error
	}{
		{"removed, the sentinel left", os.Remove},
		{"cut to zero bytes", func(path string) error { return os.Truncate(path, 0) }},
		{"cut back to its sent line", func(path string) error {
			raw, errRead := os.ReadFile(path)
			if errRead != nil {
				return errRead
			}
			return os.WriteFile(path, raw[:bytes.IndexByte(raw, '\n')+1], 0o600)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newResetSpendFixture(t, round2Grants()...)
			body := spendBody(map[string]any{"auth_index": f.index, "kind": "fable", "grant_id": "fable-two"})
			if code, first := f.spendKeyed(t, "round2-replayed", body); code != http.StatusOK || first["result"] != "reset" {
				t.Fatalf("setup: status=%d answer=%v", code, first)
			}
			forgetClaudeResetJournal(f.logDir)
			if err := tc.damage(filepath.Join(f.logDir, claudeResetSpendLedgerName)); err != nil {
				t.Fatal(err)
			}
			f.restart()
			code, again := f.spendKeyed(t, "round2-replayed", body)
			if _, _, claims := f.provider.counts(); len(claims) != 1 || code != http.StatusServiceUnavailable || again["ledger_refusal"] == nil {
				t.Fatalf("one key: claims=%d status=%d answer=%v", len(claims), code, again)
			}
		})
	}
}

// Round 2, R2-2 (gate r2 MAJOR 4; read r2 MINOR 4): a claim the provider
// answered is never retried after a restart because its answer could not be
// written: the ledger, which holds its sent line and no answer, refuses the
// retry.
func TestClaudeResetSpendR2KnownOutcomeIsNeverRetriedAfterARestart(t *testing.T) {
	f := newResetSpendFixture(t, round2Grants()...)
	path := filepath.Join(f.logDir, claudeResetSpendLedgerName)
	f.provider.set(func(p *fakeResetProvider) {
		p.onClaim = func(fakeResetClaim) {
			if err := errors.Join(os.Rename(path, path+".aside"), os.Mkdir(path, 0o700)); err != nil {
				t.Error(err)
			}
		}
	})
	code, answer := f.spend(t, spendBody(map[string]any{"auth_index": f.index, "kind": "fable", "grant_id": "fable-two"}))
	if code != http.StatusOK || answer["result"] != "reset" || answer["ledger_error"] == nil || answer["retry_until"] != nil {
		t.Fatalf("setup: status=%d answer=%v", code, answer)
	}
	requestID, _ := answer["request_id"].(string)
	if err := errors.Join(os.Remove(path), os.Rename(path+".aside", path)); err != nil {
		t.Fatal(err)
	}
	f.provider.set(func(p *fakeResetProvider) { p.onClaim = nil })
	f.restart()
	code, retried := f.spend(t, spendBody(map[string]any{"auth_index": f.index, "kind": "fable", "retry_request_id": requestID}))
	if _, _, claims := f.provider.counts(); len(claims) != 1 || code != http.StatusConflict {
		t.Fatalf("a claim the hub knew had succeeded was sent again: claims=%d status=%d answer=%v", len(claims), code, retried)
	}
}

// Round 2, R2-3 (gate r2 MAJOR 5; read r2 MINOR 5): a claim leaves only while
// its hold and retry window, which run from its sent line's stamp, are whole:
// the dispatch check applies the window to a first claim as to a retry, and a
// sent line that took longer than the dispatch limit to become durable sends
// nothing. A claim that leaves holds its seat and account for its window, in
// the running hub and after a restart.
func TestClaudeResetSpendR2HoldStartsWhenTheSentLineIsDurable(t *testing.T) {
	for _, tc := range []struct {
		stall  time.Duration
		reason string
	}{
		{11 * time.Minute, claudeResetAbortRetryWindow},
		{claudeResetDispatchLimit, claudeResetAbortLedgerStall},
	} {
		t.Run("a sent line durable after "+tc.stall.String(), func(t *testing.T) {
			f := newResetSpendFixture(t, round2Grants()...)
			f.provider.set(func(p *fakeResetProvider) { p.claimStatus = http.StatusBadGateway })
			f.jumpWhenSentIsDurable(tc.stall)
			code, first := f.spend(t, spendBody(map[string]any{"auth_index": f.index, "kind": "fable"}))
			if _, _, claims := f.provider.counts(); len(claims) != 0 || code != http.StatusConflict || first["reason"] != tc.reason {
				t.Fatalf("a claim left %v after its stamp: claims=%d status=%d answer=%v", tc.stall, len(claims), code, first)
			}
		})
	}
	t.Run("a claim that leaves holds for its window", func(t *testing.T) {
		f := newResetSpendFixture(t, round2Grants()...)
		f.provider.set(func(p *fakeResetProvider) { p.claimStatus = http.StatusBadGateway })
		f.jumpWhenSentIsDurable(claudeResetDispatchLimit - time.Second)
		_, first := f.spend(t, spendBody(map[string]any{"auth_index": f.index, "kind": "fable"}))
		if first["result"] != "unknown" {
			t.Fatalf("setup: %v", first)
		}
		f.provider.set(func(p *fakeResetProvider) { p.claimStatus = 0 })
		attempt := func(label string) {
			for _, seat := range []string{"fixture", "twin"} {
				if code, answer := f.spend(t, spendBody(map[string]any{"auth_index": f.seatIndex(t, seat), "kind": "fable"})); code != http.StatusConflict {
					t.Fatalf("%s: %s status=%d answer=%v", label, seat, code, answer)
				}
			}
		}
		f.clock.advance(claudeResetRetryWindow - claudeResetDispatchLimit - time.Second)
		attempt("in the running hub")
		f.restart()
		attempt("after a restart")
		if _, _, claims := f.provider.counts(); len(claims) != 1 {
			t.Fatalf("claims=%d", len(claims))
		}
	})
}

// Round 2, R2-4 (gate r2 MAJOR 7 and MINOR 9; read r2 MINORs 2 and 6, NOTE 9):
// recovery checks everything admission checks, and a last line with no
// newline is torn whatever it parses as, refused the same way on every start
// and never written to.
func TestClaudeResetSpendR2RecoveryChecksWhatAdmissionChecks(t *testing.T) {
	account := claudeResetAccount(resetFixtureOrganization)
	line := func(f *resetSpendFixture, fields map[string]any, drop ...string) string {
		record := map[string]any{
			"phase": "sent", "operation": "0123456789abcdef0123456789abcdef", "account": account,
			"at": f.clock.now().Add(-time.Minute).Format(time.RFC3339Nano), "auth_index": f.index, "kind": "fable",
			"grant_id": "fable-two", "named_grant": "", "request_id": "fixture-request-1", "retry": false, "result": "unknown",
			"resets_left_before": map[string]int{"fable": 2},
		}
		for key, value := range fields {
			record[key] = value
		}
		for _, key := range drop {
			delete(record, key)
		}
		encoded, _ := json.Marshal(record)
		return string(encoded) + "\n"
	}
	ago := func(f *resetSpendFixture, d time.Duration) string {
		return f.clock.now().Add(-d).Format(time.RFC3339Nano)
	}
	settled := func(f *resetSpendFixture) string {
		return line(f, map[string]any{"at": ago(f, 2*time.Minute)}) + line(f, map[string]any{"phase": "answered", "result": "reset", "at": ago(f, 2*time.Minute)})
	}
	for _, tc := range []struct {
		name   string
		ledger func(f *resetSpendFixture) string
		line   int
	}{
		{"a settled request id sent again as a first claim", func(f *resetSpendFixture) string {
			return settled(f) + line(f, map[string]any{"operation": "fedcba9876543210fedcba9876543210"})
		}, 3},
		{"a first claim while another seat of its account held one", func(f *resetSpendFixture) string {
			return line(f, nil) + line(f, map[string]any{"operation": "fedcba9876543210fedcba9876543210", "request_id": "fixture-request-2", "auth_index": f.seatIndex(t, "twin")})
		}, 2},
		{"an answer naming another grant than its claim", func(f *resetSpendFixture) string {
			return line(f, nil) + line(f, map[string]any{"phase": "answered", "result": "reset", "named_grant": "fable-two"})
		}, 2},
		{"two balances for one answer", func(f *resetSpendFixture) string {
			balance := line(f, map[string]any{"phase": "balance", "resets_left_after": map[string]int{"fable": 1}, "at": ago(f, 2*time.Minute)})
			return settled(f) + balance + balance
		}, 4},
		{"a retry of a claim whose answer is not recorded", func(f *resetSpendFixture) string {
			return line(f, nil) + line(f, map[string]any{"operation": "fedcba9876543210fedcba9876543210", "retry": true})
		}, 2},
		{"a record stamped before the record above it", func(f *resetSpendFixture) string {
			other, otherAccount := f.seatIndex(t, "other"), claudeResetAccount(resetFixtureOtherOrganization)
			return line(f, map[string]any{"auth_index": other, "account": otherAccount, "operation": "fedcba9876543210fedcba9876543210", "request_id": "fixture-request-0"}) +
				line(f, map[string]any{"at": ago(f, 20*time.Minute)})
		}, 2},
		{"a sound last line with no newline", func(f *resetSpendFixture) string { return strings.TrimSuffix(line(f, nil), "\n") }, 1},
		{"a last line with no timestamp and no newline", func(f *resetSpendFixture) string { return strings.TrimSuffix(line(f, nil, "at"), "\n") }, 1},
		{"a last line with no account and no newline", func(f *resetSpendFixture) string { return strings.TrimSuffix(line(f, nil, "account"), "\n") }, 1},
		{"an empty object with no newline", func(f *resetSpendFixture) string { return "{}" }, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newResetSpendFixture(t, round2Grants()...)
			path := filepath.Join(f.logDir, claudeResetSpendLedgerName)
			ledger := tc.ledger(f)
			if err := os.WriteFile(path, []byte(ledger), 0o600); err != nil {
				t.Fatal(err)
			}
			for start := 1; start <= 2; start++ {
				f.restart()
				for _, body := range []string{
					spendBody(map[string]any{"auth_index": f.index, "kind": "fable"}),
					spendBody(map[string]any{"auth_index": f.seatIndex(t, "twin"), "kind": "fable"}),
					spendBody(map[string]any{"auth_index": f.index, "kind": "fable", "retry_request_id": "fixture-request-1"}),
				} {
					code, answer := f.spend(t, body)
					if code != http.StatusServiceUnavailable || answer["ledger_refusal"] != claudeResetRefusalInvalidLine || answer["ledger_line"] != float64(tc.line) {
						t.Fatalf("start %d, %s: status=%d answer=%v", start, body, code, answer)
					}
				}
			}
			if requests, _, _ := f.provider.counts(); requests != 0 {
				t.Fatalf("a spend reached the provider: requests=%d", requests)
			}
			if raw, _ := os.ReadFile(path); string(raw) != ledger {
				t.Fatalf("the hub wrote to a ledger it refused: %q", raw)
			}
		})
	}
}

// Round 2, R2-5 (gate r2 MAJOR 8 and MINOR 10; read r2 MINOR 7, NOTEs 11 and
// 12): a redelivery of a key whose outcome is on disk reports that outcome,
// even while the spend still reads the balance, with the status and retry
// deadline its first answer carried; a refused ledger names its path, its
// first unacceptable line and the cure.
func TestClaudeResetSpendR2ReplayAndRefusalSayWhatTheLedgerHolds(t *testing.T) {
	t.Run("redelivered while the balance read hangs", func(t *testing.T) {
		f := newResetSpendFixture(t, round2Grants()...)
		var reads atomic.Int32
		arrived, release := make(chan struct{}), make(chan struct{})
		var releaseOnce sync.Once
		releaseRead := func() { releaseOnce.Do(func() { close(release) }) }
		t.Cleanup(releaseRead)
		f.provider.set(func(p *fakeResetProvider) {
			p.onRead = func() {
				if reads.Add(1) == 2 {
					close(arrived)
					<-release
				}
			}
		})
		body := spendBody(map[string]any{"auth_index": f.index, "kind": "fable", "grant_id": "fable-two"})
		done := make(chan int, 1)
		go func() {
			code, _ := r2SpendQuietly(f.h, "round2-hung", body)
			done <- code
		}()
		select {
		case <-arrived:
		case code := <-done:
			t.Fatalf("the spend answered %d before its balance read", code)
		}
		code, again := r2SpendQuietly(f.h, "round2-hung", body)
		codeFresh, fresh := r2SpendQuietly(f.h, "round2-fresh", body)
		releaseRead()
		if first := <-done; first != http.StatusOK {
			t.Fatalf("the first request answered %d", first)
		}
		if code != http.StatusOK || again["replayed"] != true || again["result"] != "reset" || strings.Contains(fmt.Sprint(again["error"]), "nothing spent") {
			t.Fatalf("a redelivery after a durable success: status=%d answer=%v", code, again)
		}
		if codeFresh != http.StatusConflict {
			t.Fatalf("a fresh key while the seat is in flight: status=%d answer=%v", codeFresh, fresh)
		}
		if _, _, claims := f.provider.counts(); len(claims) != 1 {
			t.Fatalf("claims=%d", len(claims))
		}
	})
	t.Run("a replay keeps the first answer's status and retry deadline", func(t *testing.T) {
		f := newResetSpendFixture(t, round2Grants()...)
		f.provider.set(func(p *fakeResetProvider) { p.claimStatus = http.StatusBadGateway })
		firstBody := spendBody(map[string]any{"auth_index": f.index, "kind": "fable"})
		_, first := f.spendKeyed(t, "round2-first", firstBody)
		requestID, _ := first["request_id"].(string)
		retryUntil := first["retry_until"]
		if first["result"] != "unknown" || retryUntil == nil {
			t.Fatalf("setup: %v", first)
		}
		f.clock.advance(time.Minute)
		retryBody := spendBody(map[string]any{"auth_index": f.index, "kind": "fable", "retry_request_id": requestID})
		if _, retried := f.spendKeyed(t, "round2-retry", retryBody); retried["result"] != "unknown" || retried["retry_until"] != retryUntil {
			t.Fatalf("an unresolved retry: %v", retried)
		}
		if code, replay := f.spendKeyed(t, "round2-retry", retryBody); code != http.StatusOK || replay["replayed"] != true || replay["retry_until"] != retryUntil {
			t.Fatalf("the unresolved retry replayed without its first send's deadline: status=%d answer=%v", code, replay)
		}
		// A dispatch refusal: the sent line becomes durable at the dispatch limit.
		other := f.seatIndex(t, "other")
		f.jumpWhenSentIsDurable(claudeResetDispatchLimit)
		notSentCode, notSent := f.spendKeyed(t, "round2-not-sent", spendBody(map[string]any{"auth_index": other, "kind": "fable"}))
		if notSentCode != http.StatusConflict || notSent["reason"] != claudeResetAbortLedgerStall {
			t.Fatalf("setup of a refusal: status=%d answer=%v", notSentCode, notSent)
		}
		for _, restart := range []bool{false, true} {
			if restart {
				f.restart()
			}
			if code, replay := f.spendKeyed(t, "round2-retry", retryBody); code != http.StatusOK || replay["replayed"] != true || replay["retry_until"] != retryUntil {
				t.Fatalf("restart=%v, the retry replayed: status=%d answer=%v", restart, code, replay)
			}
			if code, replay := f.spendKeyed(t, "round2-not-sent", spendBody(map[string]any{"auth_index": other, "kind": "fable"})); code != notSentCode || replay["result"] != claudeResetResultNotSent {
				t.Fatalf("restart=%v, the refusal replayed: status=%d answer=%v", restart, code, replay)
			}
		}
	})
	t.Run("a refused ledger names its path, its line and the cure", func(t *testing.T) {
		f := newResetSpendFixture(t, round2Grants()...)
		path := filepath.Join(f.logDir, claudeResetSpendLedgerName)
		if err := os.WriteFile(path, []byte("not a record\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		f.restart()
		code, answer := f.spend(t, spendBody(map[string]any{"auth_index": f.index, "kind": "fable"}))
		if code != http.StatusServiceUnavailable || answer["ledger"] != filepath.Join(f.h.claudeResetSpends.mustDir(t, f.logDir), claudeResetSpendLedgerName) ||
			answer["ledger_line"] != float64(1) || !strings.Contains(fmt.Sprint(answer["cure"]), "sentinel") || !strings.Contains(fmt.Sprint(answer["error"]), "this request sent nothing") {
			t.Fatalf("status=%d answer=%v", code, answer)
		}
	})
}

// Gate r2 MINOR 16: a repeated or comma-joined Idempotency-Key is refused
// before any provider call.
func TestClaudeResetSpendR2RefusesAnAmbiguousIdempotencyKey(t *testing.T) {
	f := newResetSpendFixture(t, round2Grants()...)
	body := spendBody(map[string]any{"auth_index": f.index, "kind": "fable"})
	for name, keys := range map[string][]string{"two header fields": {"round2-a", "round2-b"}, "comma-joined": {"round2-a, round2-b"}, "one comma": {"round2-a,b"}} {
		rec := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(rec)
		ctx.Request = httptest.NewRequest(http.MethodPost, "/v8/management/claude/limit-resets/spend", strings.NewReader(body))
		for _, key := range keys {
			ctx.Request.Header.Add("Idempotency-Key", key)
		}
		f.h.SpendClaudeLimitReset(ctx)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: status=%d body=%s", name, rec.Code, rec.Body)
		}
	}
	if requests, _, _ := f.provider.counts(); requests != 0 {
		t.Fatalf("requests=%d", requests)
	}
}

// Gate r2 MINOR 19: a listing opens the spend journal only for the first
// Claude entry it builds.
func TestClaudeResetSpendR2ListingReadsTheLedgerOnlyWhenAsked(t *testing.T) {
	f := newResetSpendFixture(t, round2Grants()...)
	history := f.h.claudeResetHistory()
	resolved := f.h.claudeResetSpends.mustDir(t, f.logDir)
	claudeResetJournals.mu.Lock()
	_, opened := claudeResetJournals.byPath[resolved]
	claudeResetJournals.mu.Unlock()
	if opened {
		t.Fatal("a listing opened the journal before any entry asked")
	}
	if _, err := os.Stat(filepath.Join(f.logDir, claudeResetLedgerLockName)); !os.IsNotExist(err) {
		t.Fatalf("a listing took the lock before any entry asked: %v", err)
	}
	if last, err := history.seat(f.index); err != nil || last != nil {
		t.Fatalf("last=%v err=%v", last, err)
	}
}

// Round 2, own review: the clock, the dispatch and the lock file.
func TestClaudeResetSpendR2OwnReviewFindings(t *testing.T) {
	// MAJOR 1: after the clock steps back, a sent line is never stamped
	// ahead of it, and a retry judged on a clock behind its claim's stamp
	// sends nothing, in the running hub and after a restart.
	t.Run("a clock stepped back stretches no window", func(t *testing.T) {
		f := newResetSpendFixture(t, round2Grants()...)
		f.clock.advance(2 * time.Hour)
		f.provider.set(func(p *fakeResetProvider) { p.claimStatus = http.StatusBadGateway })
		_, first := f.spend(t, spendBody(map[string]any{"auth_index": f.index, "kind": "fable"}))
		requestID, _ := first["request_id"].(string)
		if first["result"] != "unknown" {
			t.Fatalf("setup: %v", first)
		}
		f.provider.set(func(p *fakeResetProvider) { p.claimStatus = 0 })
		f.clock.advance(-2 * time.Hour)
		for _, restart := range []bool{false, true} {
			if restart {
				f.restart()
			}
			if code, answer := f.spend(t, spendBody(map[string]any{"auth_index": f.seatIndex(t, "other"), "kind": "weekly"})); code != http.StatusServiceUnavailable || !strings.Contains(fmt.Sprint(answer["error"]), "clock") {
				t.Fatalf("restart=%v, a first claim on a clock behind the ledger: status=%d answer=%v", restart, code, answer)
			}
			if code, answer := f.spend(t, spendBody(map[string]any{"auth_index": f.index, "kind": "fable", "retry_request_id": requestID})); code != http.StatusConflict || !strings.Contains(fmt.Sprint(answer["error"]), "clock") {
				t.Fatalf("restart=%v, a retry on a clock behind its claim: status=%d answer=%v", restart, code, answer)
			}
		}
		if _, _, claims := f.provider.counts(); len(claims) != 1 {
			t.Fatalf("claims=%d", len(claims))
		}
	})
	// MINOR 2: a ledger that stops being trusted between the sent line and
	// the dispatch sends nothing.
	t.Run("a ledger refused before the dispatch sends nothing", func(t *testing.T) {
		f := newResetSpendFixture(t, round2Grants()...)
		path := filepath.Join(f.logDir, claudeResetSpendLedgerName)
		// The ledger goes as the sent line's write completes: the last sync
		// of that append is the sentinel's.
		var removed atomic.Bool
		claudeResetSyncFile = func(file *os.File) error {
			errSync := file.Sync()
			if strings.HasSuffix(file.Name(), claudeResetLedgerSentinelName) && removed.CompareAndSwap(false, true) {
				if errRemove := os.Remove(path); errRemove != nil {
					t.Error(errRemove)
				}
			}
			return errSync
		}
		t.Cleanup(func() { claudeResetSyncFile = func(f *os.File) error { return f.Sync() } })
		code, answer := f.spend(t, spendBody(map[string]any{"auth_index": f.index, "kind": "fable"}))
		if _, _, claims := f.provider.counts(); len(claims) != 0 || code != http.StatusServiceUnavailable || answer["reason"] != claudeResetAbortLedgerRefused {
			t.Fatalf("claims=%d status=%d answer=%v", len(claims), code, answer)
		}
	})
	// MINOR 3: a lock file removed or replaced under a running hub refuses
	// every spend, since another process could lock a new one.
	t.Run("a lock file replaced under a running hub", func(t *testing.T) {
		f := newResetSpendFixture(t, round2Grants()...)
		if code, first := f.spend(t, spendBody(map[string]any{"auth_index": f.index, "kind": "fable"})); code != http.StatusOK {
			t.Fatalf("setup: status=%d answer=%v", code, first)
		}
		lockPath := filepath.Join(f.logDir, claudeResetLedgerLockName)
		if err := errors.Join(os.Remove(lockPath), os.WriteFile(lockPath, nil, 0o600)); err != nil {
			t.Fatal(err)
		}
		code, answer := f.spend(t, spendBody(map[string]any{"auth_index": f.seatIndex(t, "other"), "kind": "weekly"}))
		if _, _, claims := f.provider.counts(); len(claims) != 1 || code != http.StatusServiceUnavailable || answer["ledger_refusal"] != claudeResetRefusalChanged {
			t.Fatalf("claims=%d status=%d answer=%v", len(claims), code, answer)
		}
	})
}

// Round 2, the one Codex round (round 6) on the delta.
func TestClaudeResetSpendR2CodexRoundFindings(t *testing.T) {
	// MAJOR 2: the claim is judged on a fresh clock after any wait for the
	// ledger's lock, not before it.
	t.Run("the dispatch judges the claim after the wait for the ledger", func(t *testing.T) {
		f := newResetSpendFixture(t, round2Grants()...)
		journal, err := f.h.claudeResetSpends.journal(f.logDir)
		if err != nil {
			t.Fatal(err)
		}
		journal.mu.Lock()
		if refusal := journal.startLocked(); refusal != nil {
			journal.mu.Unlock()
			t.Fatal(refusal)
		}
		started := f.clock.now()
		done := make(chan string, 1)
		go func() {
			done <- journal.dispatch(func() string {
				if f.clock.now().Sub(started) >= claudeResetRetryWindow {
					return claudeResetAbortRetryWindow
				}
				return ""
			})
		}()
		f.clock.advance(claudeResetRetryWindow)
		journal.mu.Unlock()
		if got := <-done; got != claudeResetAbortRetryWindow {
			t.Fatalf("the claim was judged before the wait: %q", got)
		}
	})
	// MAJOR 3: every ancestor is synced; one the service may traverse but
	// not read is skipped, while the ledger's own directory must open.
	t.Run("an unreadable ancestor is skipped, the ledger's directory is not", func(t *testing.T) {
		if runtime.GOOS == "windows" || os.Geteuid() == 0 {
			t.Skip("needs a directory its owner can traverse but not read")
		}
		root := t.TempDir()
		ancestor := filepath.Join(root, "traverse-only")
		dir := filepath.Join(ancestor, "logs")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(ancestor, 0o311); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(ancestor, 0o700) })
		if err := syncClaudeResetDirectories([]string{dir, ancestor, root}); err != nil {
			t.Fatalf("an unreadable ancestor failed the sync: %v", err)
		}
		if err := syncClaudeResetDirectories([]string{ancestor}); err == nil {
			t.Fatal("an unreadable ledger directory was skipped")
		}
	})
	// MINOR 4: a replay of a retry that was not sent keeps its status and
	// still shows the retry that settled the claim afterwards.
	t.Run("a replay of an aborted retry shows the settling retry", func(t *testing.T) {
		f := newResetSpendFixture(t, round2Grants()...)
		f.provider.set(func(p *fakeResetProvider) { p.claimStatus = http.StatusBadGateway })
		_, first := f.spend(t, spendBody(map[string]any{"auth_index": f.index, "kind": "fable"}))
		requestID, _ := first["request_id"].(string)
		if first["result"] != "unknown" {
			t.Fatalf("setup: %v", first)
		}
		f.provider.set(func(p *fakeResetProvider) { p.claimStatus = 0 })
		retryBody := spendBody(map[string]any{"auth_index": f.index, "kind": "fable", "retry_request_id": requestID})
		f.jumpWhenSentIsDurable(claudeResetDispatchLimit)
		codeB, retryB := f.spendKeyed(t, "round2-retry-b", retryBody)
		if codeB != http.StatusConflict || retryB["reason"] != claudeResetAbortLedgerStall {
			t.Fatalf("setup of the aborted retry: status=%d answer=%v", codeB, retryB)
		}
		if _, retryC := f.spendKeyed(t, "round2-retry-c", retryBody); retryC["result"] != "reset" {
			t.Fatalf("setup of the settling retry: %v", retryC)
		}
		code, replay := f.spendKeyed(t, "round2-retry-b", retryBody)
		latest, _ := replay["latest"].(map[string]any)
		if code != codeB || replay["result"] != claudeResetResultNotSent || latest["result"] != "reset" || replay["retry_until"] != nil {
			t.Fatalf("status=%d answer=%v", code, replay)
		}
	})
}
