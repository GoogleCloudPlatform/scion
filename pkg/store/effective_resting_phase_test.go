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

package store

import "testing"

// TestEffectiveRestingPhase pins the resume-memory overlay
// (ptone/scion#4669): only an error row with a memory and no active launch
// reads as its remembered phase.
func TestEffectiveRestingPhase(t *testing.T) {
	cases := []struct {
		name string
		a    *Agent
		want string
	}{
		{"nil", nil, ""},
		{"error with memory, launch ended", &Agent{Phase: "error", LaunchResumePhase: "suspended", LaunchState: LaunchStateEnded}, "suspended"},
		{"error with memory, never launched", &Agent{Phase: "error", LaunchResumePhase: "created"}, "created"},
		{"error with memory, launch active", &Agent{Phase: "error", LaunchResumePhase: "suspended", LaunchState: LaunchStateActive}, "error"},
		{"error without memory", &Agent{Phase: "error", LaunchState: LaunchStateEnded}, "error"},
		{"stopped with stale memory", &Agent{Phase: "stopped", LaunchResumePhase: "suspended", LaunchState: LaunchStateEnded}, "stopped"},
		{"starting with memory", &Agent{Phase: "starting", LaunchResumePhase: "suspended", LaunchState: LaunchStateActive}, "starting"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := EffectiveRestingPhase(tc.a); got != tc.want {
				t.Fatalf("EffectiveRestingPhase = %q, want %q", got, tc.want)
			}
		})
	}
}
