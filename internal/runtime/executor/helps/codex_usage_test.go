package helps

import (
	"encoding/json"
	"fmt"
	"testing"
)

func TestCodexUsageAndEventQuotaShapesAgree(t *testing.T) {
	usage := []byte(`{"rate_limit":{"primary_window":{"used_percent":42,"limit_window_seconds":18000,"reset_at":1893456000}},"additional_rate_limits":[{"limit_name":"Spark","rate_limit":{"secondary_window":{"used_percent":8,"limit_window_seconds":604800,"reset_at":1893456000}}}]}`)
	event := []byte(`{"type":"codex.rate_limits","rate_limits":{"primary":{"used_percent":42,"window_minutes":300,"reset_at":1893456000}},"additional_rate_limits":{"Spark":{"secondary":{"used_percent":8,"window_minutes":10080,"reset_at":1893456000}}}}`)
	left, right := ParseCodexUsageHeaders(usage), ParseCodexQuotaEventHeaders(event)
	if len(left) == 0 || fmt.Sprint(left) != fmt.Sprint(right) {
		t.Fatalf("shape mismatch: %v / %v", left, right)
	}
}

func TestCodexUsageBoundsAdditionalLimits(t *testing.T) {
	var limits []any
	for i := range 100 {
		limits = append(limits, map[string]any{"limit_name": fmt.Sprintf("limit-%02d", i), "rate_limit": map[string]any{"primary_window": map[string]any{"used_percent": 1, "limit_window_seconds": 18000, "reset_at": 1893456000}}})
	}
	body, errMarshal := json.Marshal(map[string]any{"additional_rate_limits": limits})
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	headers := ParseCodexUsageHeaders(body)
	if len(headers) != 8*4 {
		t.Fatalf("unbounded additional limits: %d headers", len(headers))
	}
	if ParseCodexUsageHeaders([]byte(`{"rate_limit":`)) != nil {
		t.Fatal("accepted malformed usage")
	}
}
