package management

import (
	"bytes"
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
		{"claude", "plan_tier", "  Ultra  ", "Ultra"},
		{"codex", "plan_type", "  pro  ", "pro"},
	} {
		t.Run(tc.provider+"/"+tc.field, func(t *testing.T) {
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

var storedPlanCases = []struct{ provider, field, stored, want string }{
	{"claude", "plan_tier", "Max 20x", "Ultra"},
	{"claude", "plan_tier", "  Max 20x  ", "Ultra"},
	{"claude", "plan_tier", "Max 5x", "Max"},
	{"claude", "plan_tier", "Team premium", "Team premium"},
	{"claude", "plan_tier", "Max", "Max"},
	{"claude", "plan_tier", "Ultra", "Ultra"},
	{"claude", "plan_tier", "default_future", "default_future"},
	{"claude", "plan_tier", "max 20x", "max 20x"},
	{"claude", "plan_tier", "Max 20x plus", "Max 20x plus"},
	{"codex", "plan_type", "Max 20x", "Max 20x"},
}

func TestBuildAuthFileEntryStoredPlan(t *testing.T) {
	for _, tc := range storedPlanCases {
		auth := &coreauth.Auth{
			ID: "seat.json", FileName: "seat.json", Provider: tc.provider,
			Attributes: map[string]string{"runtime_only": "true"},
			Metadata:   map[string]any{tc.field: tc.stored},
		}
		entry := (&Handler{}).buildAuthFileEntry(auth, authFileQuotaContext{v8: true})
		if got := entry[tc.field]; got != tc.want {
			t.Errorf("%s %q: got %v, want %q", tc.field, tc.stored, got, tc.want)
		}
		if auth.Metadata[tc.field] != tc.stored {
			t.Errorf("%s %q: list modified metadata", tc.field, tc.stored)
		}
	}
}

func TestListAuthFilesFromDiskStoredPlan(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "seat.json")
	h := &Handler{cfg: &config.Config{AuthDir: dir}}
	for _, tc := range storedPlanCases {
		data, err := json.Marshal(map[string]string{"type": tc.provider, tc.field: tc.stored})
		if err != nil {
			t.Fatal(err)
		}
		if errWrite := os.WriteFile(path, data, 0o600); errWrite != nil {
			t.Fatal(errWrite)
		}
		recorder := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(recorder)
		ctx.Request = httptest.NewRequest("GET", "/", nil)
		ctx.Set(ConfigV8ContextKey, true)
		h.ListAuthFiles(ctx)
		var payload struct{ Files []map[string]any }
		if errDecode := json.Unmarshal(recorder.Body.Bytes(), &payload); errDecode != nil {
			t.Fatal(errDecode)
		}
		if recorder.Code != 200 || len(payload.Files) != 1 || payload.Files[0][tc.field] != tc.want {
			t.Errorf("%s %q: status %d, entries %v, want %q", tc.field, tc.stored, recorder.Code, payload.Files, tc.want)
		}
		after, errRead := os.ReadFile(path)
		if errRead != nil || !bytes.Equal(data, after) {
			t.Fatalf("credential changed after list: %v", errRead)
		}
	}
}

func TestListAuthFilesFromDiskV8AccountIdentity(t *testing.T) {
	for _, tc := range []struct{ provider, field, value string }{
		{"claude", "organization_name", "Example Organization"},
		{"claude", "plan_tier", "Ultra"},
		{"codex", "plan_type", "pro"},
	} {
		t.Run(tc.provider+"/"+tc.field, func(t *testing.T) {
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
