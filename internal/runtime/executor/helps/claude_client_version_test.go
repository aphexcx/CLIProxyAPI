package helps

import (
	"net/http"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestDetectClaudeCodeRequestVerifiedNewerSoftware(t *testing.T) {
	for _, tc := range []struct {
		name, ua, header, value string
		off                     bool
		want                    string
	}{
		{name: "newer", want: "2.1.400"},
		{name: "sdk", ua: "claude-cli/2.1.400 (external, sdk-cli, agent-sdk/0.3.300)", want: "2.1.400"},
		{name: "new minor", ua: "claude-cli/2.2.0 (external, cli)", want: "2.2.0"},
		{name: "older", ua: "claude-cli/2.1.100 (external, cli)"},
		{name: "equal", ua: "claude-cli/2.1.321 (external, cli)"},
		{name: "curl", ua: "curl/8.0"},
		{name: "malformed", ua: "claude-cli/2.1.400junk (external, cli)"},
		{name: "missing app", header: "X-App"},
		{name: "python", header: "X-Stainless-Lang", value: "python"},
		{name: "runtime mismatch", header: "X-Stainless-Runtime", value: "python"},
		{name: "missing SDK", header: "X-Stainless-Package-Version"},
		{name: "old SDK", header: "X-Stainless-Package-Version", value: "0.1.0", want: "2.1.400"},
		{name: "negative SDK", header: "X-Stainless-Package-Version", value: "-1.0.0"},
		{name: "signed SDK", header: "X-Stainless-Package-Version", value: "+1.0.0"},
		{name: "suffixed SDK", header: "X-Stainless-Package-Version", value: "0.140.0-beta"},
		{name: "bad runtime", header: "X-Stainless-Runtime-Version", value: "26.12.1"},
		{name: "old runtime", header: "X-Stainless-Runtime-Version", value: "v24.11.0", want: "2.1.400"},
		{name: "negative runtime", header: "X-Stainless-Runtime-Version", value: "v-1.0.0"},
		{name: "off", off: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			headers := measuredClaudeCodeHelperHeaders("claude-code-20250219,new-client-beta")
			ua := tc.ua
			if ua == "" {
				ua = "claude-cli/2.1.400 (external, cli)"
			}
			headers.Set("User-Agent", ua)
			if tc.header != "" {
				headers.Set(tc.header, tc.value)
			}
			cfg := &config.Config{}
			if tc.off {
				enabled := false
				cfg.ClaudeHeaderDefaults.PassthroughNewerClient = &enabled
			}
			detection := DetectClaudeCodeRequest(headers, claudeCodeDetectionPayload(validClaudeCodeMetadataUserID), false, cfg)
			if detection.PassthroughVersion != tc.want {
				t.Fatalf("passthrough version = %q, want %q", detection.PassthroughVersion, tc.want)
			}
			if tc.want != "" && !detection.Confirmed {
				t.Fatal("verified newer native request was not confirmed")
			}
			if tc.header != "" && tc.want == "" && detection.Confirmed {
				t.Fatal("inconsistent newer client was confirmed")
			}
			if tc.off && !detection.Confirmed {
				t.Fatal("disabled policy changed legacy confirmation")
			}
		})
	}
}

func TestClaudeRequestVersionDoesNotPersistSoftwareOrChangePlatform(t *testing.T) {
	headers := measuredClaudeCodeHelperHeaders("claude-code-20250219")
	headers.Set("User-Agent", "claude-cli/2.1.400 (external, cli)")
	headers.Set("X-Stainless-Package-Version", "0.140.0")
	headers.Set("X-Stainless-Runtime-Version", "v27.0.0")
	cfg := &config.Config{ClaudeHeaderDefaults: config.ClaudeHeaderDefaults{OS: "Linux", Arch: "x64"}}
	request, _ := http.NewRequest(http.MethodPost, "https://example.test", nil)
	ApplyClaudeDefaultDeviceProfileHeaders(request, cfg)
	ApplyClaudeRequestVersionHeaders(request, headers, DetectClaudeCodeRequest(headers, nil, false, cfg), false)
	if got := request.Header.Get("User-Agent"); got != headers.Get("User-Agent") {
		t.Fatalf("User-Agent = %q", got)
	}
	if request.Header.Get("X-Stainless-OS") != "Linux" || request.Header.Get("X-Stainless-Arch") != "x64" {
		t.Fatal("request version changed the seat platform")
	}
	if DefaultClaudeVersion(cfg) != "2.1.321" || ClaudeRequestVersion(nil, cfg) != "2.1.321" {
		t.Fatal("newer request changed the configured baseline")
	}
	ApplyClaudeDefaultDeviceProfileHeaders(request, cfg)
	ApplyClaudeRequestVersionHeaders(request, nil, DetectClaudeCodeRequest(nil, nil, false, cfg), false)
	if got := request.Header.Get("User-Agent"); got != "claude-cli/2.1.321 (external, cli)" {
		t.Fatalf("next caller inherited newer software: %q", got)
	}
}
