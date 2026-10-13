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
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
)

// testExperiment returns a minimally valid experiment for registry
// construction in tests.
func testExperiment(name string, def bool, layers ...experiments.Layer) experiments.Experiment {
	return experiments.Experiment{
		Name:        name,
		Title:       "Test experiment",
		Description: "Used only in pkg/hub tests.",
		Default:     def,
		Layers:      layers,
		Stage:       experiments.StageBeta,
		Issue:       "ptone/scion#2217",
		Owner:       "test",
		ReviewBy:    "2026-12-31",
	}
}

// testRegistry returns a registry with hub.test_gate (server+web, default
// on), web.only_thing (web-only, default on), and hub.server_only
// (server-only, default on, so a test asserting web-layer filtering has a
// name that must NOT appear in a web-facing result), per ptone/scion#2217.
func testRegistry(t *testing.T) *experiments.Registry {
	t.Helper()
	reg, err := experiments.NewRegistry([]experiments.Experiment{
		testExperiment("hub.test_gate", true, experiments.LayerServer, experiments.LayerWeb),
		testExperiment("web.only_thing", true, experiments.LayerWeb),
		testExperiment("hub.server_only", true, experiments.LayerServer),
	}, nil)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	return reg
}
