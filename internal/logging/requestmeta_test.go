package logging

import (
	"context"
	"net/http"
	"testing"
	"testing/synctest"
	"time"
)

func TestWithFreshResponseHeadersHolderIsolatesAttempts(t *testing.T) {
	requestCtx := WithResponseHeadersHolder(context.Background())
	SetResponseHeaders(requestCtx, http.Header{"X-Upstream-Attempt": []string{"request"}})

	firstAttemptCtx := WithFreshResponseHeadersHolder(requestCtx)
	if headers := GetResponseHeaders(firstAttemptCtx); len(headers) != 0 {
		t.Fatalf("fresh attempt inherited request headers: %#v", headers)
	}
	SetResponseHeaders(firstAttemptCtx, http.Header{"X-Upstream-Attempt": []string{"first"}})

	secondAttemptCtx := WithFreshResponseHeadersHolder(firstAttemptCtx)
	if headers := GetResponseHeaders(secondAttemptCtx); len(headers) != 0 {
		t.Fatalf("second attempt inherited first attempt headers: %#v", headers)
	}
	SetResponseHeaders(secondAttemptCtx, http.Header{"X-Upstream-Attempt": []string{"second"}})

	if got := GetResponseHeaders(requestCtx).Get("X-Upstream-Attempt"); got != "request" {
		t.Fatalf("request holder = %q, want request", got)
	}
	if got := GetResponseHeaders(firstAttemptCtx).Get("X-Upstream-Attempt"); got != "first" {
		t.Fatalf("first attempt holder = %q, want first", got)
	}
	if got := GetResponseHeaders(secondAttemptCtx).Get("X-Upstream-Attempt"); got != "second" {
		t.Fatalf("second attempt holder = %q, want second", got)
	}
}

func TestResponseHeadersSnapshotKeepsReceiptTime(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := WithFreshResponseHeadersHolder(context.Background())
		if headers, at := GetResponseHeadersSnapshot(ctx); len(headers) != 0 || !at.IsZero() {
			t.Fatal("empty holder has a timestamp")
		}
		at := time.Now()
		SetResponseHeaders(ctx, http.Header{"X-Test": {"original"}})
		<-time.After(time.Second)
		MergeResponseHeaders(ctx, http.Header{"X-Other": {"merged"}})
		headers, observedAt := GetResponseHeadersSnapshot(ctx)
		if !observedAt.Equal(at) || headers.Get("X-Other") != "merged" {
			t.Fatal("snapshot was re-dated by later processing")
		}
		headers.Set("X-Test", "mutated")
		if GetResponseHeaders(ctx).Get("X-Test") != "original" {
			t.Fatal("snapshot aliases the holder")
		}
		fresh := WithFreshResponseHeadersHolder(ctx)
		if _, observedAt := GetResponseHeadersSnapshot(fresh); !observedAt.IsZero() {
			t.Fatal("retry inherited the previous attempt's time")
		}
	})
}
