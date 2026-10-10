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

package config

import (
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

// TestMergeTelemetryConfig_SamplingRatesNotShared pins ptone/scion#4218
// (acceptance 7): merging sampling rates builds a fresh map instead of
// writing into the base config's map. Hub defaults and settings sit in base
// position and are shared between agents, so a written-through map would
// leak one agent's rates into the next.
func TestMergeTelemetryConfig_SamplingRatesNotShared(t *testing.T) {
	base := &api.TelemetryConfig{Filter: &api.TelemetryFilterConfig{Sampling: &api.TelemetrySamplingConfig{
		Rates: map[string]float64{"a": 0.1, "b": 0.2},
	}}}
	override := &api.TelemetryConfig{Filter: &api.TelemetryFilterConfig{Sampling: &api.TelemetrySamplingConfig{
		Rates: map[string]float64{"b": 0.9, "c": 0.5},
	}}}

	got := MergeScionConfig(&api.ScionConfig{Telemetry: base}, &api.ScionConfig{Telemetry: override})

	rates := got.Telemetry.Filter.Sampling.Rates
	want := map[string]float64{"a": 0.1, "b": 0.9, "c": 0.5}
	if len(rates) != len(want) {
		t.Fatalf("merged rates = %v, want %v", rates, want)
	}
	for k, v := range want {
		if rates[k] != v {
			t.Errorf("merged rates[%q] = %v, want %v", k, rates[k], v)
		}
	}
	baseRates := base.Filter.Sampling.Rates
	if len(baseRates) != 2 || baseRates["a"] != 0.1 || baseRates["b"] != 0.2 {
		t.Errorf("base rates mutated: %v", baseRates)
	}
	// Writing into the merged map must not reach the base either.
	rates["z"] = 1
	if _, ok := baseRates["z"]; ok {
		t.Error("merged rates map is shared with the base map")
	}
}
