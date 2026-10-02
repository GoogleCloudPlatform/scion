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

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
)

func TestIsValidExitReason(t *testing.T) {
	tests := []struct {
		reason string
		want   bool
	}{
		// Valid: empty means "no reason given"
		{"", true},
		// Valid terminal activities
		{"crashed", true},
		{"limits_exceeded", true},
		// Valid: Kubernetes pod disruption reasons
		{"preempted", true},
		{"evicted", true},
		// Invalid: non-terminal activities
		{"working", false},
		{"thinking", false},
		{"executing", false},
		{"waiting_for_input", false},
		{"completed", false},
		{"blocked", false},
		{"stalled", false},
		{"offline", false},
		// Invalid: arbitrary strings
		{"random_string", false},
		{"error", false},
		{"stopped", false},
	}

	for _, tc := range tests {
		t.Run("reason="+tc.reason, func(t *testing.T) {
			got := isValidExitReason(tc.reason)
			if got != tc.want {
				t.Errorf("isValidExitReason(%q) = %v, want %v", tc.reason, got, tc.want)
			}
		})
	}
}

func TestExitStatusMessage(t *testing.T) {
	ec137 := 137
	ec0 := 0

	tests := []struct {
		name     string
		reason   state.ExitReason
		exitCode *int
		want     string
	}{
		{"preempted, non-zero exit", state.ExitReasonPreempted, &ec137, "Agent pod was preempted, exit code 137"},
		{"preempted, zero exit", state.ExitReasonPreempted, &ec0, "Agent pod was preempted"},
		{"preempted, nil exit", state.ExitReasonPreempted, nil, "Agent pod was preempted"},
		{"evicted, non-zero exit", state.ExitReasonEvicted, &ec137, "Agent pod was evicted, exit code 137"},
		{"evicted, zero exit", state.ExitReasonEvicted, &ec0, "Agent pod was evicted"},
		{"evicted, nil exit", state.ExitReasonEvicted, nil, "Agent pod was evicted"},
		{"crashed, non-zero exit", state.ExitReasonCrashed, &ec137, "Agent crashed with exit code 137"},
		{"crashed, zero exit (should not happen, but no crash wording)", state.ExitReasonCrashed, &ec0, ""},
		{"empty reason, non-zero exit", state.ExitReason(""), &ec137, "Agent crashed with exit code 137"},
		{"empty reason, nil exit", state.ExitReason(""), nil, ""},
		{"limits_exceeded, non-zero exit", state.ExitReasonLimitsExceeded, &ec137, "Agent crashed with exit code 137"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := exitStatusMessage(tc.reason, tc.exitCode)
			if got != tc.want {
				t.Errorf("exitStatusMessage(%q, %v) = %q, want %q", tc.reason, tc.exitCode, got, tc.want)
			}
		})
	}
}

func TestIsGenericStopMessage(t *testing.T) {
	tests := []struct {
		msg  string
		want bool
	}{
		{"", true},
		{"Agent stopped", true},
		{"Session ended", true},
		{"Agent pod was preempted", false},
		{"Agent crashed with exit code 1", false},
		{"custom user message", false},
	}

	for _, tc := range tests {
		t.Run(tc.msg, func(t *testing.T) {
			if got := isGenericStopMessage(tc.msg); got != tc.want {
				t.Errorf("isGenericStopMessage(%q) = %v, want %v", tc.msg, got, tc.want)
			}
		})
	}
}
