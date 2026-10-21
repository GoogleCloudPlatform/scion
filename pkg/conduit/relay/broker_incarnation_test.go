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

package relay

import (
	"testing"
	"time"
)

// TestBrokerIncarnationFlapMemory: a flap is a return, after another
// incarnation, to one admitted for the same broker within the window.
// Repeated admissions of one incarnation, a new incarnation, another
// broker's history and a return after BrokerIncarnationFlapWindow are not
// flaps. The memory
// is updated in connection_epoch order: an admission below the recorded
// epoch is no flap and records nothing.
func TestBrokerIncarnationFlapMemory(t *testing.T) {
	t0 := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	var b brokerIncarnations
	for i, step := range []struct {
		broker, inc string
		epoch       int64
		at          time.Duration
		flap        bool
	}{
		{"b1", "inc-1", 1, 0, false},
		{"b1", "inc-1", 2, time.Minute, false},         // same incarnation again
		{"b1", "inc-2", 3, 2 * time.Minute, false},     // a new incarnation
		{"b2", "inc-1", 1, 3 * time.Minute, false},     // another broker's history is separate
		{"b1", "inc-1", 4, 4 * time.Minute, true},      // back to inc-1 within BrokerIncarnationFlapWindow
		{"b1", "inc-2", 5, 5 * time.Minute, true},      // and back to inc-2
		{"b1", "inc-2", 6, 6 * time.Minute, false},     // a reconnect of the current one
		{"b1", "inc-3", 8, 6*time.Minute + 1, false},   // a new incarnation
		{"b1", "inc-1", 7, 6*time.Minute + 2, false},   // below the recorded epoch: no flap, nothing recorded
		{"b1", "inc-3", 9, 6*time.Minute + 3, false},   // inc-3 is still the last recorded one
		{"b1", "inc-1", 10, 15*time.Minute + 1, false}, // inc-1 last recorded at 4m: past BrokerIncarnationFlapWindow
		{"b1", "inc-3", 11, 16*time.Minute + 3, true},  // inc-3 recorded 10m ago: still within it
		{"b1", "inc-2", 12, 30 * time.Minute, false},   // every entry is past BrokerIncarnationFlapWindow
		{"b2", "inc-2", 2, 30*time.Minute + 1, false},  // b2's history was forgotten
	} {
		if got := b.admit(step.broker, step.inc, step.epoch, t0.Add(step.at)); got != step.flap {
			t.Fatalf("step %d (%s %s epoch %d at %v): flap = %v, want %v", i, step.broker, step.inc, step.epoch, step.at, got, step.flap)
		}
	}
}
