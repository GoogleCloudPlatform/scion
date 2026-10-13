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
	"sync"
	"sync/atomic"
	"testing"
)

// externalBearerMetricCall is one recorded scion_hub_external_bearer_total
// increment.
type externalBearerMetricCall struct {
	kind      ExternalBearerKind
	principal ExternalBearerPrincipal
	outcome   ExternalBearerOutcome
}

// fakeExternalBearerMetrics records every RecordExternalBearer call for
// assertion. Safe for concurrent use (needed for the singleflight-collapsed
// path, and general defensiveness).
type fakeExternalBearerMetrics struct {
	mu    sync.Mutex
	calls []externalBearerMetricCall
}

// attachExternalBearerMetrics wires a fresh fakeExternalBearerMetrics into
// cfg via the *atomic.Pointer indirection AuthConfig.ExternalBearerMetrics
// requires (see its doc comment): production wires this after New()
// returns, but a test can just store directly, since doExternalBearerRequest
// builds the middleware from this cfg value after the field is set.
func attachExternalBearerMetrics(cfg *AuthConfig) *fakeExternalBearerMetrics {
	fake := &fakeExternalBearerMetrics{}
	var rec ExternalBearerMetricsRecorder = fake
	var p atomic.Pointer[ExternalBearerMetricsRecorder]
	p.Store(&rec)
	cfg.ExternalBearerMetrics = &p
	return fake
}

// wantOneCall asserts that exactly one RecordExternalBearer call happened,
// with the exact triple given.
func wantOneCall(t *testing.T, fake *fakeExternalBearerMetrics, want externalBearerMetricCall) {
	t.Helper()
	calls := fake.allCalls()
	if len(calls) != 1 {
		t.Fatalf("RecordExternalBearer called %d time(s), want exactly 1: calls=%+v", len(calls), calls)
	}
	if calls[0] != want {
		t.Errorf("recorded call = %+v, want %+v", calls[0], want)
	}
}

func (f *fakeExternalBearerMetrics) RecordExternalBearer(kind ExternalBearerKind, principal ExternalBearerPrincipal, outcome ExternalBearerOutcome) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, externalBearerMetricCall{kind, principal, outcome})
}

func (f *fakeExternalBearerMetrics) allCalls() []externalBearerMetricCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]externalBearerMetricCall, len(f.calls))
	copy(out, f.calls)
	return out
}
