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

package api

import "testing"

func TestResolvedPlacementInEffect(t *testing.T) {
	cases := []struct {
		name string
		p    *ResolvedPlacement
		want bool
	}{
		{"nil", nil, false},
		{"zero", &ResolvedPlacement{}, false},
		{"empty collections", &ResolvedPlacement{NodeSelector: map[string]string{}, Tolerations: []K8sToleration{}}, false},
		{"sources only", &ResolvedPlacement{Sources: map[string][]string{"node_selector": {"runtimes.a"}}}, false},
		{"node selector", &ResolvedPlacement{NodeSelector: map[string]string{"pool": "agents"}}, true},
		{"tolerations", &ResolvedPlacement{Tolerations: []K8sToleration{{Key: "dedicated", Operator: "Exists"}}}, true},
		{"runtime class", &ResolvedPlacement{RuntimeClassName: "gvisor"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.p.InEffect(); got != tc.want {
				t.Fatalf("InEffect() = %v, want %v", got, tc.want)
			}
		})
	}
}
