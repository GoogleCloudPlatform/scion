// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Tests in this file reinitialise the package-global messaging.DivergenceMetrics
// counter and assert on its values. Because the counter is shared mutable state
// (a package-level *DivergenceCounter backed by atomic.Int64 fields), concurrent
// tests that each reset and increment it would observe each other's writes and
// fail intermittently. These tests MUST run serially — do not add t.Parallel().
package hub

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/messaging"
)

func TestHandleAdminMessagingDivergence_GET(t *testing.T) {
	// Seed the global counter so we can verify the handler reads it.
	// DivergenceMetrics is a package-level var; tests run serially within
	// the package so this is safe.
	messaging.DivergenceMetrics = &messaging.DivergenceCounter{}
	messaging.DivergenceMetrics.Inc(true)  // 1 match
	messaging.DivergenceMetrics.Inc(true)  // 2 matches
	messaging.DivergenceMetrics.Inc(false) // 1 mismatch
	messaging.DivergenceMetrics.IncFallback()
	messaging.DivergenceMetrics.IncFallback()

	srv := &Server{
		startTime: time.Date(2026, 8, 30, 0, 0, 0, 0, time.UTC),
	}
	srv.SetHubID("test-hub-id")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/messaging/divergence", nil)
	rr := httptest.NewRecorder()
	srv.handleAdminMessagingDivergence(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}

	ct := rr.Header().Get("Content-Type")
	if ct != "application/json" {
		t.Errorf("expected Content-Type application/json, got %q", ct)
	}

	var resp divergenceBoardResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	// Verify counter values.
	if resp.Matches != 2 {
		t.Errorf("expected matches=2, got %d", resp.Matches)
	}
	if resp.Mismatches != 1 {
		t.Errorf("expected mismatches=1, got %d", resp.Mismatches)
	}
	if resp.Comparisons != 3 {
		t.Errorf("expected comparisons=3, got %d", resp.Comparisons)
	}
	if resp.Fallbacks != 2 {
		t.Errorf("expected fallbacks=2, got %d", resp.Fallbacks)
	}

	// Arithmetic consistency: comparisons must equal matches + mismatches.
	if resp.Comparisons != resp.Matches+resp.Mismatches {
		t.Errorf("comparisons (%d) != matches (%d) + mismatches (%d)",
			resp.Comparisons, resp.Matches, resp.Mismatches)
	}

	// Verify identity fields.
	if resp.HubID != "test-hub-id" {
		t.Errorf("expected hub_id=test-hub-id, got %q", resp.HubID)
	}
	if resp.ProcessStartTime == "" {
		t.Error("expected process_start_time to be non-empty")
	}
	if resp.ProcessUptime == "" {
		t.Error("expected process_uptime to be non-empty")
	}
}

// TestHandleAdminMessagingDivergence_CaveatKeysPresent asserts the presence
// of load-bearing caveat keys by name. These caveats are the only thing
// preventing a reader from misinterpreting the counter values. Removing them
// is a semantic regression — this test turns "please keep these" into a
// build-enforced invariant.
func TestHandleAdminMessagingDivergence_CaveatKeysPresent(t *testing.T) {
	messaging.DivergenceMetrics = &messaging.DivergenceCounter{}

	srv := &Server{
		startTime: time.Now(),
	}
	srv.SetHubID("test-hub")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/messaging/divergence", nil)
	rr := httptest.NewRecorder()
	srv.handleAdminMessagingDivergence(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}

	// Decode into a raw map so we assert key presence independently of the
	// Go struct — a struct change that drops a field will fail here.
	var raw map[string]json.RawMessage
	if err := json.NewDecoder(rr.Body).Decode(&raw); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	caveatsRaw, ok := raw["caveats"]
	if !ok {
		t.Fatal("response missing top-level 'caveats' key")
	}

	var caveats map[string]string
	if err := json.Unmarshal(caveatsRaw, &caveats); err != nil {
		t.Fatalf("failed to decode caveats: %v", err)
	}

	// These are the load-bearing keys. If you are here because this test
	// broke, do NOT remove the assertion — read the caveat text to understand
	// why it exists, and preserve or replace it with an equivalent disclosure.
	requiredKeys := []string{
		"scope",
		"scope_detail",
		"mismatch_composition",
		"consistency_check_fails_open",
		"unbackfilled_blind_spot",
		"not_go_no_go",
		"counter_snapshot",
	}
	for _, key := range requiredKeys {
		val, present := caveats[key]
		if !present {
			t.Errorf("caveats missing required key %q", key)
		} else if val == "" {
			t.Errorf("caveat %q is present but empty", key)
		}
	}
}

// TestHandleAdminMessagingDivergence_ReadOnly asserts that the endpoint
// rejects all non-GET methods. The handler must never mutate the counter.
func TestHandleAdminMessagingDivergence_ReadOnly(t *testing.T) {
	srv := &Server{
		startTime: time.Now(),
	}

	for _, method := range []string{
		http.MethodPost,
		http.MethodPut,
		http.MethodPatch,
		http.MethodDelete,
	} {
		t.Run(method, func(t *testing.T) {
			req := httptest.NewRequest(method, "/api/v1/admin/messaging/divergence", nil)
			rr := httptest.NewRecorder()
			srv.handleAdminMessagingDivergence(rr, req)

			if rr.Code != http.StatusMethodNotAllowed {
				t.Errorf("%s: expected 405, got %d", method, rr.Code)
			}
		})
	}
}
