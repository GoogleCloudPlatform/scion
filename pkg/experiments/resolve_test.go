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

package experiments

import "testing"

func resolveTestRegistry(t *testing.T) *Registry {
	t.Helper()
	exp := func(name string, def bool, layers ...Layer) Experiment {
		return Experiment{Name: name, Title: "T", Description: "D", Default: def, Layers: layers, Stage: StageAlpha,
			Issue: "ptone/scion#1", Owner: "o", ReviewBy: "2027-01-01"}
	}
	r, err := NewRegistry([]Experiment{
		exp("hub.on_by_default", true, LayerServer),
		exp("web.on_by_default", true, LayerWeb),
	}, []string{"hub.retired"})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// TestResolve: unknown and retired names are off; a malformed overrides
// document turns off server-layer experiments only; an override wins over
// the default.
func TestResolve(t *testing.T) {
	r := resolveTestRegistry(t)
	for _, tc := range []struct {
		name      string
		overrides map[string]bool
		malformed bool
		want      bool
	}{
		{name: "hub.on_by_default", want: true},
		{name: "hub.on_by_default", overrides: map[string]bool{"hub.on_by_default": false}, want: false},
		{name: "hub.on_by_default", malformed: true, want: false},
		{name: "web.on_by_default", malformed: true, want: true},
		{name: "hub.unknown", overrides: map[string]bool{"hub.unknown": true}, want: false},
		{name: "hub.retired", overrides: map[string]bool{"hub.retired": true}, want: false},
	} {
		if got := r.Resolve(tc.name, tc.overrides, tc.malformed); got != tc.want {
			t.Errorf("Resolve(%s, %v, malformed=%v) = %v, want %v", tc.name, tc.overrides, tc.malformed, got, tc.want)
		}
	}
}
