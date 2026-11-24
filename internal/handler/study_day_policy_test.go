package handler

import (
	"encoding/json"
	"net/http"
	"testing"
)

func TestStudyDayPolicyRoundTripAndConflict(t *testing.T) {
	_, mux, pool := testServer(t)
	defer pool.Close()
	firstToken := loginDemo(t, mux, "Kelma Review")
	secondToken := loginDemo(t, mux, "Kelma Immersion")

	rr := reqJSON(t, mux, "GET", "/v2/study-day-policy", nil, firstToken)
	if rr.Code != http.StatusOK {
		t.Fatalf("default policy: got %d body=%s", rr.Code, rr.Body.String())
	}
	var defaultPolicy struct {
		Version      int64  `json:"version"`
		TimezoneID   string `json:"timezone_id"`
		DayStartHour int    `json:"day_start_hour"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &defaultPolicy); err != nil {
		t.Fatalf("decode default policy: %v", err)
	}
	if defaultPolicy.Version != 0 || defaultPolicy.TimezoneID != "UTC" || defaultPolicy.DayStartHour != 4 {
		t.Fatalf("unexpected default policy: %+v", defaultPolicy)
	}

	candidate := map[string]any{
		"base_version":    0,
		"timezone_id":     "America/New_York",
		"day_start_hour":  4,
		"idempotency_key": "first-policy-write",
	}
	rr = reqJSON(t, mux, "PUT", "/v2/study-day-policy", candidate, firstToken)
	if rr.Code != http.StatusOK {
		t.Fatalf("put policy: got %d body=%s", rr.Code, rr.Body.String())
	}
	var saved map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &saved); err != nil {
		t.Fatalf("decode saved policy: %v", err)
	}
	if saved["version"] != float64(1) || saved["timezone_id"] != "America/New_York" {
		t.Fatalf("unexpected saved policy: %+v", saved)
	}

	rr = reqJSON(t, mux, "PUT", "/v2/study-day-policy", candidate, firstToken)
	if rr.Code != http.StatusOK {
		t.Fatalf("idempotent retry: got %d body=%s", rr.Code, rr.Body.String())
	}
	rr = reqJSON(t, mux, "GET", "/v2/study-day-policy", nil, secondToken)
	if rr.Code != http.StatusOK {
		t.Fatalf("second client fetch: got %d body=%s", rr.Code, rr.Body.String())
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &saved); err != nil {
		t.Fatalf("decode second client policy: %v", err)
	}
	if saved["version"] != float64(1) || saved["day_start_hour"] != float64(4) {
		t.Fatalf("second client did not receive policy: %+v", saved)
	}

	conflicting := map[string]any{
		"base_version":    0,
		"timezone_id":     "Europe/Paris",
		"day_start_hour":  3,
		"idempotency_key": "stale-policy-write",
	}
	rr = reqJSON(t, mux, "PUT", "/v2/study-day-policy", conflicting, secondToken)
	if rr.Code != http.StatusConflict {
		t.Fatalf("stale policy: got %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestStudyDayPolicyRejectsInvalidTimezone(t *testing.T) {
	_, mux, pool := testServer(t)
	defer pool.Close()
	token := loginDemo(t, mux, "Kelma Review")

	rr := reqJSON(t, mux, "PUT", "/v2/study-day-policy", map[string]any{
		"base_version":    0,
		"timezone_id":     "not/a-zone",
		"day_start_hour":  4,
		"idempotency_key": "invalid-policy",
	}, token)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("invalid timezone: got %d body=%s", rr.Code, rr.Body.String())
	}
}
