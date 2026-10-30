package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// PollQuota observes the original ChatGPT seat without refreshing credentials.
func (e *CodexAutoExecutor) PollQuota(ctx context.Context, auth *cliproxyauth.Auth) (map[string]cliproxyauth.QuotaWindow, error) {
	return e.httpExec.PollQuota(ctx, auth)
}

func (e *CodexAutoExecutor) PollQuotaUsage(ctx context.Context, auth *cliproxyauth.Auth) (map[string]cliproxyauth.QuotaWindow, json.RawMessage, error) {
	return e.httpExec.PollQuotaUsage(ctx, auth)
}

func (e *CodexExecutor) PollQuota(ctx context.Context, auth *cliproxyauth.Auth) (map[string]cliproxyauth.QuotaWindow, error) {
	windows, _, errPoll := e.PollQuotaUsage(ctx, auth)
	return windows, errPoll
}

func (e *CodexExecutor) PollQuotaUsage(ctx context.Context, auth *cliproxyauth.Auth) (map[string]cliproxyauth.QuotaWindow, json.RawMessage, error) {
	if auth == nil || auth.AuthKind() != cliproxyauth.AuthKindOAuth {
		return nil, nil, fmt.Errorf("codex quota: OAuth seat required")
	}
	token, _ := auth.Metadata["access_token"].(string)
	accountID, _ := auth.Metadata["account_id"].(string)
	if strings.TrimSpace(token) == "" || strings.TrimSpace(accountID) == "" {
		return nil, nil, fmt.Errorf("codex quota: missing OAuth credentials")
	}
	req, errRequest := http.NewRequestWithContext(ctx, http.MethodGet, "https://chatgpt.com/backend-api/wham/usage", nil)
	if errRequest != nil {
		return nil, nil, fmt.Errorf("codex quota: cannot create request")
	}
	applyCodexHeadersWithSIWC(req, auth, false, token, false, e.cfg, nil)
	client := helps.NewUtlsHTTPClient(ctx, e.cfg, auth, 0)
	// Do not follow a usage redirect with a seat's account headers.
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, errDo := client.Do(req)
	if errDo != nil {
		return nil, nil, fmt.Errorf("codex quota: usage request failed")
	}
	observedAt := time.Now()
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Debug("codex quota: response close failed")
		}
	}()
	if resp.StatusCode != http.StatusOK {
		return nil, nil, statusErr{code: resp.StatusCode, msg: "codex quota: usage unavailable"}
	}
	const maxUsageBytes = 1 << 20
	body, errRead := io.ReadAll(io.LimitReader(resp.Body, maxUsageBytes+1))
	if errRead != nil || len(body) > maxUsageBytes {
		return nil, nil, fmt.Errorf("codex quota: invalid usage body")
	}
	headers := helps.ParseCodexUsageHeaders(body)
	windows := cliproxyauth.CodexQuotaWindows(headers, observedAt)
	if len(windows) == 0 {
		return nil, nil, fmt.Errorf("codex quota: no valid usage windows")
	}
	// Retain only quota metadata, never arbitrary upstream response fields.
	usage := map[string]any{}
	if plan := headers.Get("X-Codex-Plan-Type"); plan != "" {
		usage["plan_type"] = plan
	}
	credits := map[string]any{}
	for _, field := range []struct{ header, key string }{{"Has-Credits", "has_credits"}, {"Unlimited", "unlimited"}} {
		if value, errParse := strconv.ParseBool(headers.Get("X-Codex-Credits-" + field.header)); errParse == nil {
			credits[field.key] = value
		}
	}
	if value := headers.Get("X-Codex-Credits-Balance"); value != "" {
		if _, errParse := strconv.ParseFloat(value, 64); errParse == nil {
			credits["balance"] = value
		}
	}
	if len(credits) > 0 {
		usage["credits"] = credits
	}
	encoded, errEncode := json.Marshal(usage)
	if errEncode != nil {
		return nil, nil, fmt.Errorf("codex quota: cannot encode usage")
	}
	return windows, encoded, nil
}
