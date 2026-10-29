package claude

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"
)

// OAuthUsageWindow is one account or named model window from the usage endpoint.
type OAuthUsageWindow struct {
	Utilization float64
	ResetsAt    time.Time
	ObservedAt  time.Time // Response header receipt; zero for a body parsed in isolation.
}

func parseOAuthUsage(body []byte) (map[string]OAuthUsageWindow, error) {
	type usageWindow struct {
		Utilization *float64 `json:"utilization"`
		Percent     *float64 `json:"percent"`
		ResetsAt    string   `json:"resets_at"`
		Scope       struct {
			Model *struct {
				DisplayName string `json:"display_name"`
			} `json:"model"`
		} `json:"scope"`
	}
	var payload struct {
		FiveHour *usageWindow  `json:"five_hour"`
		SevenDay *usageWindow  `json:"seven_day"`
		Limits   []usageWindow `json:"limits"`
	}
	if errDecode := json.Unmarshal(body, &payload); errDecode != nil || strings.TrimSpace(string(body)) == "null" {
		return nil, fmt.Errorf("parse Claude OAuth usage: invalid response")
	}
	windows := make(map[string]OAuthUsageWindow)
	add := func(name string, window *usageWindow, percent *float64) {
		if window == nil || percent == nil || math.IsNaN(*percent) || math.IsInf(*percent, 0) || *percent < 0 {
			return
		}
		reset, errParse := time.Parse(time.RFC3339Nano, window.ResetsAt)
		if errParse != nil {
			return
		}
		windows[name] = OAuthUsageWindow{Utilization: *percent, ResetsAt: reset}
	}
	if payload.FiveHour != nil {
		add("five_hour", payload.FiveHour, payload.FiveHour.Utilization)
	}
	if payload.SevenDay != nil {
		add("seven_day", payload.SevenDay, payload.SevenDay.Utilization)
	}
	for _, window := range payload.Limits {
		if window.Scope.Model == nil {
			continue
		}
		name := strings.TrimSpace(window.Scope.Model.DisplayName)
		if name == "" || name == "five_hour" || name == "seven_day" {
			continue
		}
		percent := window.Percent
		if percent == nil {
			percent = window.Utilization
		}
		add(name, &window, percent)
	}
	return windows, nil
}
