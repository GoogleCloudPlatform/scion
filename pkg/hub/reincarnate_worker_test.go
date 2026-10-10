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

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// TestReincarnationInFlight_MatchesStore pins the hub wrapper
// reincarnationInFlight to store.ReincarnationInFlight over every
// store.ReincarnationState constant, the empty string and an unknown value
// (ptone/scion#4160): none/empty and failed are not in flight, everything
// else is.
func TestReincarnationInFlight_MatchesStore(t *testing.T) {
	tests := []struct {
		name  string
		state string
		want  bool
	}{
		{"none constant", store.ReincarnationStateNone, false},
		{"empty", "", false},
		{"pending", store.ReincarnationStatePending, true},
		{"stopping", store.ReincarnationStateStopping, true},
		{"provisioning", store.ReincarnationStateProvisioning, true},
		{"starting", store.ReincarnationStateStarting, true},
		{"failed", store.ReincarnationStateFailed, false},
		{"unknown", "not-a-reincarnation-state", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hubGot := reincarnationInFlight(&store.Agent{ReincarnationState: tt.state})
			storeGot := store.ReincarnationInFlight(tt.state)
			if hubGot != storeGot {
				t.Errorf("state %q: hub reincarnationInFlight = %v, store.ReincarnationInFlight = %v", tt.state, hubGot, storeGot)
			}
			if hubGot != tt.want {
				t.Errorf("state %q: reincarnationInFlight = %v, want %v", tt.state, hubGot, tt.want)
			}
		})
	}
}
