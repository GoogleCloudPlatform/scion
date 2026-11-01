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
)

func pathsOf(errs []ValidationError) []string {
	out := make([]string, len(errs))
	for i, e := range errs {
		out[i] = e.Path
	}
	return out
}

// A cross-entry placement finding refuses the save only when the save
// introduces it; one already stored is a warning.
func TestPlacementPairSaveChecks(t *testing.T) {
	k8s := V1RuntimeConfig{Type: "kubernetes", Namespace: "agents", NodeSelector: map[string]string{PoolNodeSelectorKey: "agents"}}
	docker := V1RuntimeConfig{Type: "docker"}
	poolBatch := V1ProfileConfig{Runtime: "k8s", NodeSelector: map[string]string{PoolNodeSelectorKey: "batch"}}
	withClass := V1ProfileConfig{Runtime: "gone", RuntimeClassName: "gvisor"}

	cases := []struct {
		name                  string
		before, after         map[string]V1RuntimeConfig
		beforeP, afterP       map[string]V1ProfileConfig
		wantErrs, wantWarning []string
	}{
		{"new pool conflict refused", map[string]V1RuntimeConfig{"k8s": k8s}, map[string]V1RuntimeConfig{"k8s": k8s},
			nil, map[string]V1ProfileConfig{"batch": poolBatch}, []string{"profiles.batch.node_selector"}, nil},
		{"stored pool conflict warns", map[string]V1RuntimeConfig{"k8s": k8s}, map[string]V1RuntimeConfig{"k8s": k8s},
			map[string]V1ProfileConfig{"batch": poolBatch}, map[string]V1ProfileConfig{"batch": poolBatch}, nil, []string{"profiles.batch.node_selector"}},
		{"runtime change introduces conflict", map[string]V1RuntimeConfig{"k8s": {Type: "kubernetes"}}, map[string]V1RuntimeConfig{"k8s": k8s},
			map[string]V1ProfileConfig{"batch": poolBatch}, map[string]V1ProfileConfig{"batch": poolBatch}, []string{"profiles.batch.node_selector"}, nil},
		{"deleting the runtime of a placed profile refused", map[string]V1RuntimeConfig{"gone": k8s}, map[string]V1RuntimeConfig{},
			map[string]V1ProfileConfig{"p": withClass}, map[string]V1ProfileConfig{"p": withClass}, []string{"profiles.p.runtime"}, nil},
		{"placement on a non-Kubernetes entry refused", nil, map[string]V1RuntimeConfig{"d": {Type: "docker", RuntimeClassName: "gvisor"}},
			nil, nil, []string{"runtimes.d"}, nil},
		{"placed profile on a docker entry refused", map[string]V1RuntimeConfig{"d": docker}, map[string]V1RuntimeConfig{"d": docker},
			nil, map[string]V1ProfileConfig{"p": {Runtime: "d", RuntimeClassName: "gvisor"}}, []string{"profiles.p"}, nil},
		{"built-in runtime entry counts", nil, nil,
			nil, map[string]V1ProfileConfig{"p": {Runtime: "kubernetes", RuntimeClassName: "gvisor"}}, nil, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			errs, warnings := PlacementPairSaveChecks(tc.before, tc.after, tc.beforeP, tc.afterP)
			if got := pathsOf(errs); !equalStrings(got, tc.wantErrs) {
				t.Errorf("errors = %v, want %v", got, tc.wantErrs)
			}
			if got := pathsOf(warnings); !equalStrings(got, tc.wantWarning) {
				t.Errorf("warnings = %v, want %v", got, tc.wantWarning)
			}
		})
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
