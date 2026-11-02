package management

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestBuildAuthFileEntryV8AccountIdentity(t *testing.T) {
	for _, tc := range []struct {
		provider string
		field    string
		value    string
		want     string
	}{
		{"claude", "organization_name", "  Example Organization  ", "Example Organization"},
		{"codex", "plan_type", "  pro  ", "pro"},
	} {
		t.Run(tc.provider, func(t *testing.T) {
			auth := &coreauth.Auth{
				ID: "seat.json", FileName: "seat.json", Provider: tc.provider,
				Attributes: map[string]string{"runtime_only": "true"},
				Metadata: map[string]any{
					"email": "user@example.com", tc.field: tc.value,
					"access_token": "private-access", "refresh_token": "private-refresh",
					"organization_uuid": "private-uuid", "unrelated": "private-extra",
				},
			}
			h := &Handler{}
			entry := h.buildAuthFileEntry(auth, authFileQuotaContext{v8: true})
			if got := entry[tc.field]; got != tc.want {
				t.Fatalf("%s = %#v, want %q", tc.field, got, tc.want)
			}
			encoded, errMarshal := json.Marshal(entry)
			if errMarshal != nil {
				t.Fatal(errMarshal)
			}
			if strings.Contains(string(encoded), "private-") {
				t.Fatal("credential metadata leaked into the list entry")
			}
			if _, ok := h.buildAuthFileEntry(auth, authFileQuotaContext{})[tc.field]; ok {
				t.Fatal("v0 entry changed")
			}
			for _, value := range []any{nil, "", "  ", 42, map[string]any{"name": "Org"}} {
				auth.Metadata[tc.field] = value
				if _, ok := h.buildAuthFileEntry(auth, authFileQuotaContext{v8: true})[tc.field]; ok {
					t.Fatalf("%s should omit non-text or empty values", tc.field)
				}
			}
			auth.Metadata[tc.field] = tc.value
			auth.Provider = "gemini"
			if _, ok := h.buildAuthFileEntry(auth, authFileQuotaContext{v8: true})[tc.field]; ok {
				t.Fatal("identity metadata leaked to another provider")
			}
		})
	}
}

func TestListAuthFilesFromDiskV8AccountIdentity(t *testing.T) {
	for _, tc := range []struct{ provider, field, value string }{
		{"claude", "organization_name", "Example Organization"},
		{"codex", "plan_type", "pro"},
	} {
		t.Run(tc.provider, func(t *testing.T) {
			dir := t.TempDir()
			h := &Handler{cfg: &config.Config{AuthDir: dir}}
			for _, value := range []any{"  " + tc.value + "  ", "  ", nil, 42} {
				data, errMarshal := json.Marshal(map[string]any{
					"type": tc.provider, tc.field: value, "access_token": "private-access",
				})
				if errMarshal != nil {
					t.Fatal(errMarshal)
				}
				if errWrite := os.WriteFile(filepath.Join(dir, "seat.json"), data, 0o600); errWrite != nil {
					t.Fatal(errWrite)
				}
				for _, v8 := range []bool{false, true} {
					recorder := httptest.NewRecorder()
					ctx, _ := gin.CreateTestContext(recorder)
					ctx.Request = httptest.NewRequest("GET", "/", nil)
					ctx.Set(ConfigV8ContextKey, v8)
					h.ListAuthFiles(ctx)
					var payload struct {
						Files []map[string]any `json:"files"`
					}
					if errDecode := json.Unmarshal(recorder.Body.Bytes(), &payload); errDecode != nil {
						t.Fatal(errDecode)
					}
					if recorder.Code != 200 || len(payload.Files) != 1 {
						t.Fatalf("expected one entry, status=%d count=%d", recorder.Code, len(payload.Files))
					}
					want := any(nil)
					if v8 && value == "  "+tc.value+"  " {
						want = tc.value
					}
					if got := payload.Files[0][tc.field]; got != want {
						t.Fatalf("v8=%v %s=%#v, want %#v", v8, tc.field, got, want)
					}
					if strings.Contains(recorder.Body.String(), "private-access") {
						t.Fatal("credential leaked into disk list")
					}
				}
			}
		})
	}
}
