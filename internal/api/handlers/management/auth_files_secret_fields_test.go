package management

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// secretFieldNames are the keys no credential list may carry at any depth.
var secretFieldNames = map[string]bool{
	"access_token":  true,
	"refresh_token": true,
	"api_key":       true,
	"key":           true,
	"secret":        true,
	"cookie":        true,
	"password":      true,
}

// Invented values only: the fork is public.
const (
	secretFixtureClaudeAccess  = "sk-ant-oat-fixture-access-0001"
	secretFixtureClaudeRefresh = "sk-ant-ort-fixture-refresh-0001"
	secretFixtureCodexAccess   = "codex-fixture-access-0001"
	secretFixtureCodexRefresh  = "codex-fixture-refresh-0001"
)

// secretFixtureExtras are secret-shaped metadata a stored credential can carry
// besides its tokens; each seat stores them and none may reach the list.
var secretFixtureExtras = map[string]any{
	"api_key":  "fixture-api-key-0001",
	"secret":   "fixture-client-secret-0001",
	"cookie":   "fixture-session-cookie-0001",
	"password": "fixture-password-0001",
}

func secretFixtureCodexIDToken(t *testing.T) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	payload, errMarshal := json.Marshal(map[string]any{
		"email": "coder@example.test",
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id":                "acct-example-0001",
			"chatgpt_plan_type":                 "plus",
			"chatgpt_subscription_active_until": "2099-01-01T00:00:00Z",
		},
	})
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	return header + "." + base64.RawURLEncoding.EncodeToString(payload) + "."
}

// newSecretFieldsFixtureHandler lists one Claude and one Codex OAuth seat. Without
// codexPlanInMetadata the Codex seat's plan lives only in its ID token's claims.
func newSecretFieldsFixtureHandler(t *testing.T, idToken string, codexPlanInMetadata bool) *Handler {
	t.Helper()
	authDir := t.TempDir()
	cfg := &config.Config{AuthDir: authDir}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.SetConfig(cfg)
	expiry := time.Now().UTC().Add(24 * time.Hour).Format(time.RFC3339)

	seats := []*coreauth.Auth{
		{
			ID:       "claude-1a2b3c4d-seat@example.test.json",
			FileName: "claude-1a2b3c4d-seat@example.test.json",
			Provider: "claude",
			Status:   coreauth.StatusActive,
			Metadata: map[string]any{
				"type":              "claude",
				"email":             "seat@example.test",
				"organization_name": "Example Org",
				"organization_uuid": "00000000-0000-4000-8000-000000000001",
				"account_uuid":      "00000000-0000-4000-8000-000000000002",
				"plan_tier":         "Ultra",
				"access_token":      secretFixtureClaudeAccess,
				"refresh_token":     secretFixtureClaudeRefresh,
				"expired":           expiry,
			},
		},
		{
			ID:       "codex-9c0d1e2f-coder@example.test-plus.json",
			FileName: "codex-9c0d1e2f-coder@example.test-plus.json",
			Provider: "codex",
			Status:   coreauth.StatusActive,
			Metadata: map[string]any{
				"type":          "codex",
				"email":         "coder@example.test",
				"plan_type":     "plus",
				"account_id":    "acct-example-0001",
				"access_token":  secretFixtureCodexAccess,
				"refresh_token": secretFixtureCodexRefresh,
				"id_token":      idToken,
				"expired":       expiry,
			},
		},
	}
	if !codexPlanInMetadata {
		delete(seats[1].Metadata, "plan_type")
	}
	for _, seat := range seats {
		for key, value := range secretFixtureExtras {
			seat.Metadata[key] = value
		}
		path := filepath.Join(authDir, seat.FileName)
		raw, errMarshal := json.Marshal(seat.Metadata)
		if errMarshal != nil {
			t.Fatal(errMarshal)
		}
		if errWrite := os.WriteFile(path, raw, 0o600); errWrite != nil {
			t.Fatal(errWrite)
		}
		seat.Attributes = map[string]string{"path": path, "source": path}
		registerAuthForLookupTest(t, manager, seat)
	}
	return NewHandlerWithoutConfigFilePath(cfg, manager)
}

func listAuthFilesForSecretCheck(t *testing.T, h *Handler, path string, v8 bool) (string, []map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodGet, path, nil)
	if v8 {
		ctx.Set(ConfigV8ContextKey, true)
	}
	h.ListAuthFiles(ctx)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Files []map[string]any `json:"files"`
	}
	if errDecode := json.Unmarshal(rec.Body.Bytes(), &resp); errDecode != nil {
		t.Fatal(errDecode)
	}
	if len(resp.Files) != 2 {
		t.Fatalf("len(files) = %d, want 2", len(resp.Files))
	}
	return rec.Body.String(), resp.Files
}

// findSecretFields returns the path of every secret-named key under value. On the
// v8 read any id_token key is one; on the frozen v0 shape only a string id_token is.
func findSecretFields(value any, path string, v8 bool) []string {
	var found []string
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			childPath := path + "." + key
			lower := strings.ToLower(key)
			_, isString := child.(string)
			if secretFieldNames[lower] || (lower == "id_token" && (v8 || isString)) {
				found = append(found, childPath)
			}
			found = append(found, findSecretFields(child, childPath, v8)...)
		}
	case []any:
		for index, child := range typed {
			found = append(found, findSecretFields(child, path+"["+strconv.Itoa(index)+"]", v8)...)
		}
	}
	return found
}

func TestListAuthFilesNeverSendsSecretFields(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	idToken := secretFixtureCodexIDToken(t)

	for _, route := range []struct {
		name string
		path string
		v8   bool
	}{
		{name: "v8", path: "/v8/management/credentials", v8: true},
		{name: "v0", path: "/v0/management/auth-files", v8: false},
	} {
		t.Run(route.name, func(t *testing.T) {
			h := newSecretFieldsFixtureHandler(t, idToken, true)
			body, files := listAuthFilesForSecretCheck(t, h, route.path, route.v8)

			secrets := []string{
				secretFixtureClaudeAccess,
				secretFixtureClaudeRefresh,
				secretFixtureCodexAccess,
				secretFixtureCodexRefresh,
				idToken,
			}
			for _, extra := range secretFixtureExtras {
				secrets = append(secrets, extra.(string))
			}
			for _, secret := range secrets {
				if strings.Contains(body, secret) {
					t.Errorf("response carries a fixture secret value %q", secret)
				}
			}

			for _, file := range files {
				name, _ := file["name"].(string)
				if found := findSecretFields(file, name, route.v8); len(found) > 0 {
					t.Errorf("secret-named fields reached the list: %v", found)
				}
				if !strings.HasPrefix(name, "codex-") {
					continue
				}
				// The decoded claims stay readable for the page's Codex quota calls.
				claimsKey := "codex_claims"
				if !route.v8 {
					claimsKey = "id_token"
				}
				claims, ok := file[claimsKey].(map[string]any)
				if !ok {
					t.Fatalf("%s: %s = %#v, want the decoded claims object", name, claimsKey, file[claimsKey])
				}
				if got := claims["chatgpt_account_id"]; got != "acct-example-0001" {
					t.Errorf("%s: %s.chatgpt_account_id = %#v", name, claimsKey, got)
				}
			}
		})
	}
}

// A panel released before codex_claims existed reads the account id, plan and
// subscription end at the top level of a v8 entry (it read them from id_token too).
// Until every hub serves a newer panel, the v8 read keeps them there, or that
// panel's Codex quota calls lose their account header.
func TestListAuthFilesV8KeepsCodexClaimsReadableByReleasedPanels(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	// Only the claims carry the plan, as for a credential stored without plan_type.
	h := newSecretFieldsFixtureHandler(t, secretFixtureCodexIDToken(t), false)
	_, files := listAuthFilesForSecretCheck(t, h, "/v8/management/credentials", true)

	for _, file := range files {
		if file["type"] != "codex" {
			continue
		}
		for key, want := range map[string]any{
			"chatgpt_account_id":                "acct-example-0001",
			"plan_type":                         "plus",
			"chatgpt_subscription_active_until": "2099-01-01T00:00:00Z",
		} {
			if got := file[key]; got != want {
				t.Errorf("%s = %#v, want %#v", key, got, want)
			}
		}
		return
	}
	t.Fatal("no Codex entry in the list")
}
