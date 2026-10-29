package auth

import (
	"bufio"
	"context"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	internallogging "github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
)

func TestClaudeQuotaConcurrentSuccessReceiptOrder(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		name := "older-completes-first"
		if reverse {
			name = "newer-completes-first"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				m, id, _ := newClaudeQuotaManager(t)
				reset := time.Now().Add(time.Hour)
				older := claudeQuotaHeaderContext("7d_oi", "0.5", reset) // T0 headers.
				<-time.After(time.Second)
				newer := claudeQuotaHeaderContext("7d_oi", "1.0", reset) // T1 headers.
				contexts := []context.Context{older, newer}
				if reverse {
					contexts[0], contexts[1] = contexts[1], contexts[0]
				}
				for _, ctx := range contexts {
					<-time.After(time.Second) // T2 and T3 completions.
					m.MarkResult(ctx, Result{AuthID: id, Provider: "claude", Model: quotaFable, Success: true})
				}
				auth, _ := m.GetByID(id)
				if window := auth.Quota.Windows["Fable"]; window.Utilization != 100 {
					t.Fatalf("newest observation lost: %+v", window)
				}
				if blocked, _, until := isAuthBlockedForModel(auth, quotaFable, time.Now()); !blocked || !until.Equal(reset) {
					t.Fatalf("newest header did not hold model: blocked=%v until=%v", blocked, until)
				}
			})
		})
	}
}

func TestClaudeQuotaDelayedPollReceiptOrder(t *testing.T) {
	for _, timestamped := range []bool{true, false} {
		name := "header-receipt"
		if !timestamped {
			name = "missing-receipt"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				m, id, _ := newClaudeQuotaManager(t)
				base, _ := m.GetByID(id)
				reset := time.Now().Add(time.Hour)
				receipt := time.Now() // T0: usage headers arrived, body still blocked.
				if !timestamped {
					receipt = time.Time{}
				}
				bodyReady, done := make(chan struct{}), make(chan struct{})
				poller := &claudeQuotaTestPoller{poll: func(context.Context, *Auth) (map[string]QuotaWindow, error) {
					<-bodyReady
					return map[string]QuotaWindow{"Fable": {Utilization: 0, ResetsAt: reset, ObservedAt: receipt}}, nil
				}}
				go func() {
					m.pollClaudeQuotaCredential(context.Background(), m.runtimeConfig.Load().(*config.Config), base, poller)
					close(done)
				}()
				synctest.Wait()
				<-time.After(time.Second)
				m.MarkResult(claudeQuotaHeaderContext("7d_oi", "1.0", reset), Result{AuthID: id, Provider: "claude", Model: quotaFable, Success: true}) // T1.
				<-time.After(time.Second)
				close(bodyReady) // T2: the older poll finally completes.
				<-done
				auth, _ := m.GetByID(id)
				if auth.Quota.Windows["Fable"].Utilization != 100 {
					t.Fatal("late poll overwrote newer header usage")
				}
				if blocked, _, _ := isAuthBlockedForModel(auth, quotaFable, time.Now()); !blocked {
					t.Fatal("late poll released newer header hold")
				}
			})
		})
	}
}

func TestClaudeQuotaReceiptTieNeverReleasesHold(t *testing.T) {
	for _, order := range [][]string{{"0.5", "1.0"}, {"1.0", "0.5"}} {
		t.Run(strings.Join(order, "-"), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				m, id, _ := newClaudeQuotaManager(t)
				reset := time.Now().Add(time.Hour)
				first := claudeQuotaHeaderContext("7d_oi", order[0], reset)
				second := claudeQuotaHeaderContext("7d_oi", order[1], reset)
				for _, ctx := range []context.Context{first, second} {
					<-time.After(time.Second)
					m.MarkResult(ctx, Result{AuthID: id, Provider: "claude", Model: quotaFable, Success: true})
				}
				auth, _ := m.GetByID(id)
				if auth.Quota.Windows["Fable"].Utilization != 100 {
					t.Fatal("at-cap observation lost the receipt tie")
				}
				if blocked, _, _ := isAuthBlockedForModel(auth, quotaFable, time.Now()); !blocked {
					t.Fatal("receipt tie left the model selectable")
				}
			})
		})
	}
}

func TestClaudeQuotaHTTP200CompleteFablePair(t *testing.T) {
	// Constructed parser-shape fixture: no captured live 200 with the complete
	// 7d_oi pair is on record. The live shape remains a rollout verification.
	const response = "HTTP/1.1 200 OK\r\n" +
		"Content-Type: application/json\r\n" +
		"Anthropic-Ratelimit-Unified-Status: allowed\r\n" +
		"Anthropic-Ratelimit-Unified-5h-Utilization: 0.25\r\n" +
		"Anthropic-Ratelimit-Unified-5h-Reset: 4102444800\r\n" +
		"Anthropic-Ratelimit-Unified-7d-Utilization: 0.50\r\n" +
		"Anthropic-Ratelimit-Unified-7d-Reset: 4102444800\r\n" +
		"Anthropic-Ratelimit-Unified-7d_oi-Utilization: 1.0\r\n" +
		"Anthropic-Ratelimit-Unified-7d_oi-Reset: 4102444800\r\n" +
		"Content-Length: 2\r\n\r\n{}"
	resp, errRead := http.ReadResponse(bufio.NewReader(strings.NewReader(response)), nil)
	if errRead != nil {
		t.Fatal(errRead)
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			t.Error(errClose)
		}
	}()
	m, id, _ := newClaudeQuotaManager(t)
	ctx := internallogging.WithFreshResponseHeadersHolder(context.Background())
	internallogging.SetResponseHeaders(ctx, resp.Header)
	m.MarkResult(ctx, Result{AuthID: id, Provider: "claude", Model: quotaFable, Success: resp.StatusCode == http.StatusOK})
	auth, _ := m.GetByID(id)
	if window := auth.Quota.Windows["Fable"]; window.Utilization != 100 || window.ResetsAt.Unix() != 4102444800 {
		t.Fatalf("complete pair not parsed: %+v", window)
	}
	if blocked, _, _ := isAuthBlockedForModel(auth, quotaFable, time.Now()); !blocked {
		t.Fatal("parsed 200 pair did not install the Fable hold")
	}
	if blocked, _, _ := isAuthBlockedForModel(auth, quotaSonnet, time.Now()); blocked {
		t.Fatal("Fable pair blocked Sonnet")
	}
}
