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

import "sync"

// ExternalBearerSnapshotMetrics is a dependency-free, in-process recorder for
// the three design §4.7 counters, following the same shape as
// BrokerAuthMetrics (metrics.go) and GCPTokenMetrics (gcp_metrics.go): no
// GCP/OTel dependency, always constructed, and exposed as a JSON snapshot on
// GET /metrics. It exists so the ge_exchange.requests soak gate never
// depends on cfg.Hub.GCPProjectID being set — every Hub has this section,
// GCP export or not.
//
// Server.New wires one instance as the default recorder for all three
// design §4.7 counters, and passes the same instance into
// NewOTelExternalBearerMetrics when an OTel exporter is later configured
// (cmd/server_foreground.go), so both write to the same counts: OTel export
// is additive, never a replacement, for this snapshot.
type ExternalBearerSnapshotMetrics struct {
	mu             sync.Mutex
	externalBearer map[ExternalBearerOutcome]int64
	cache          map[GoogleValidatorCacheResult]int64
	exchange       map[GEExchangeOutcome]int64
}

// NewExternalBearerSnapshotMetrics creates an empty recorder.
func NewExternalBearerSnapshotMetrics() *ExternalBearerSnapshotMetrics {
	return &ExternalBearerSnapshotMetrics{
		externalBearer: make(map[ExternalBearerOutcome]int64),
		cache:          make(map[GoogleValidatorCacheResult]int64),
		exchange:       make(map[GEExchangeOutcome]int64),
	}
}

// RecordExternalBearer implements ExternalBearerMetricsRecorder. Only the
// outcome is counted in this snapshot: the full kind/principal/outcome
// cross-product is available from the OTel-exported series when GCP export
// is configured (design §4.7); the in-process section keeps one dimension so
// its shape never depends on which combinations have occurred.
func (m *ExternalBearerSnapshotMetrics) RecordExternalBearer(_ ExternalBearerKind, _ ExternalBearerPrincipal, outcome ExternalBearerOutcome) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.externalBearer[outcome]++
}

// RecordGoogleValidatorCache implements GoogleValidatorCacheMetricsRecorder.
func (m *ExternalBearerSnapshotMetrics) RecordGoogleValidatorCache(result GoogleValidatorCacheResult) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cache[result]++
}

// RecordGEExchangeRequest implements GEExchangeMetricsRecorder.
func (m *ExternalBearerSnapshotMetrics) RecordGEExchangeRequest(outcome GEExchangeOutcome) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.exchange[outcome]++
}

// ExternalBearerMetricsSnapshot is the GET /metrics JSON shape for the three
// design §4.7 counters, served as the "externalBearer" section of
// handleMetrics's combinedMetrics, next to the "broker" and "gcp" sections.
// Every key is the full closed label set (external_bearer_metrics.go),
// present at zero if never recorded, so the JSON shape is deterministic and
// never depends on which outcomes happened to occur. This snapshot is
// per-process and resets on restart — a soak check must sample every
// replica over the full window, not read a single sample once.
type ExternalBearerMetricsSnapshot struct {
	ExternalBearerTotal       map[string]int64 `json:"externalBearerTotal"`
	GoogleValidatorCacheTotal map[string]int64 `json:"googleValidatorCacheTotal"`
	GEExchangeRequestsTotal   map[string]int64 `json:"geExchangeRequestsTotal"`
}

// GetSnapshot returns a point-in-time snapshot with deterministic keys.
func (m *ExternalBearerSnapshotMetrics) GetSnapshot() *ExternalBearerMetricsSnapshot {
	m.mu.Lock()
	defer m.mu.Unlock()

	snap := &ExternalBearerMetricsSnapshot{
		ExternalBearerTotal:       make(map[string]int64, len(externalBearerOutcomes())),
		GoogleValidatorCacheTotal: make(map[string]int64, len(googleValidatorCacheResults())),
		GEExchangeRequestsTotal:   make(map[string]int64, len(geExchangeOutcomes())),
	}
	for _, o := range externalBearerOutcomes() {
		snap.ExternalBearerTotal[string(o)] = m.externalBearer[o]
	}
	for _, r := range googleValidatorCacheResults() {
		snap.GoogleValidatorCacheTotal[string(r)] = m.cache[r]
	}
	for _, o := range geExchangeOutcomes() {
		snap.GEExchangeRequestsTotal[string(o)] = m.exchange[o]
	}
	return snap
}
