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

package hub

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// ---------------------------------------------------------------------------
// O1 — the in-process design §4.7 counters exist and move regardless of GCP
// export configuration, and are served on GET /metrics next to the broker
// and GCP-token sections.
// ---------------------------------------------------------------------------

func TestExternalBearerSnapshotMetrics_RecordsMoveTheSnapshot(t *testing.T) {
	m := NewExternalBearerSnapshotMetrics()

	before := m.GetSnapshot()
	if got := before.ExternalBearerTotal[string(ExternalBearerOutcomeOK)]; got != 0 {
		t.Fatalf("initial ok count = %d, want 0", got)
	}

	m.RecordExternalBearer(ExternalBearerKindIDToken, ExternalBearerPrincipalUser, ExternalBearerOutcomeOK)
	m.RecordExternalBearer(ExternalBearerKindAccessToken, ExternalBearerPrincipalUser, ExternalBearerOutcomeOK)
	m.RecordExternalBearer(ExternalBearerKindIDToken, ExternalBearerPrincipalUnknown, ExternalBearerOutcomeRejected)
	m.RecordGoogleValidatorCache(GoogleValidatorCacheHit)
	m.RecordGEExchangeRequest(GEExchangeOutcomeOK)

	after := m.GetSnapshot()
	if got := after.ExternalBearerTotal[string(ExternalBearerOutcomeOK)]; got != 2 {
		t.Errorf("ok count = %d, want 2", got)
	}
	if got := after.ExternalBearerTotal[string(ExternalBearerOutcomeRejected)]; got != 1 {
		t.Errorf("rejected count = %d, want 1", got)
	}
	if got := after.GoogleValidatorCacheTotal[string(GoogleValidatorCacheHit)]; got != 1 {
		t.Errorf("cache hit count = %d, want 1", got)
	}
	if got := after.GEExchangeRequestsTotal[string(GEExchangeOutcomeOK)]; got != 1 {
		t.Errorf("exchange ok count = %d, want 1", got)
	}
}

// TestExternalBearerSnapshotMetrics_DeterministicKeys proves the snapshot's
// key set is always the full closed label set, present at zero, regardless
// of what has (or hasn't) been recorded — the shape must not depend on which
// outcomes happened to occur.
func TestExternalBearerSnapshotMetrics_DeterministicKeys(t *testing.T) {
	m := NewExternalBearerSnapshotMetrics()
	snap := m.GetSnapshot()

	if got, want := len(snap.ExternalBearerTotal), len(externalBearerOutcomes()); got != want {
		t.Errorf("ExternalBearerTotal has %d keys, want %d", got, want)
	}
	for _, o := range externalBearerOutcomes() {
		if _, ok := snap.ExternalBearerTotal[string(o)]; !ok {
			t.Errorf("ExternalBearerTotal missing key %q", o)
		}
	}
	if got, want := len(snap.GoogleValidatorCacheTotal), len(googleValidatorCacheResults()); got != want {
		t.Errorf("GoogleValidatorCacheTotal has %d keys, want %d", got, want)
	}
	for _, r := range googleValidatorCacheResults() {
		if _, ok := snap.GoogleValidatorCacheTotal[string(r)]; !ok {
			t.Errorf("GoogleValidatorCacheTotal missing key %q", r)
		}
	}
	if got, want := len(snap.GEExchangeRequestsTotal), len(geExchangeOutcomes()); got != want {
		t.Errorf("GEExchangeRequestsTotal has %d keys, want %d", got, want)
	}
	for _, o := range geExchangeOutcomes() {
		if _, ok := snap.GEExchangeRequestsTotal[string(o)]; !ok {
			t.Errorf("GEExchangeRequestsTotal missing key %q", o)
		}
	}
}

// TestHandleMetrics_ExternalBearerSection proves GET /metrics serves the
// design §4.7 counters as an "externalBearer" JSON section, next to
// "broker"/"gcp", and that it moves when a request is recorded — reachable
// on a Server that never had GCP export wired (externalBearerSnapshot is
// constructed by New() unconditionally; this test builds it directly since
// it only needs the field, not a full New()).
func TestHandleMetrics_ExternalBearerSection(t *testing.T) {
	srv := &Server{externalBearerSnapshot: NewExternalBearerSnapshotMetrics()}
	srv.externalBearerSnapshot.RecordGEExchangeRequest(GEExchangeOutcomeOK)

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	w := httptest.NewRecorder()
	srv.handleMetrics(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: body=%s", w.Code, w.Body.String())
	}

	var decoded struct {
		ExternalBearer *ExternalBearerMetricsSnapshot `json:"externalBearer"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("decode: %v: body=%s", err, w.Body.String())
	}
	if decoded.ExternalBearer == nil {
		t.Fatal("externalBearer section missing from /metrics response")
	}
	if got := decoded.ExternalBearer.GEExchangeRequestsTotal[string(GEExchangeOutcomeOK)]; got != 1 {
		t.Errorf("geExchangeRequestsTotal.ok = %d, want 1", got)
	}
	// The full closed key set must be present even though only one outcome
	// was ever recorded.
	if got, want := len(decoded.ExternalBearer.ExternalBearerTotal), len(externalBearerOutcomes()); got != want {
		t.Errorf("externalBearerTotal has %d keys, want %d", got, want)
	}
}

// TestHandleMetrics_NoMetricsWhenNothingWired proves the pre-existing
// "no_metrics" fallback still fires when broker, GCP and external-bearer
// snapshots are all absent (a Server built without New(), the same shape
// TestGEExchangeHandler_NotConfigured and friends use elsewhere).
func TestHandleMetrics_NoMetricsWhenNothingWired(t *testing.T) {
	srv := &Server{}

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	w := httptest.NewRecorder()
	srv.handleMetrics(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: body=%s", w.Code, w.Body.String())
	}
	var decoded map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("decode: %v: body=%s", err, w.Body.String())
	}
	if decoded["status"] != "no_metrics" {
		t.Errorf("status = %q, want %q", decoded["status"], "no_metrics")
	}
}
