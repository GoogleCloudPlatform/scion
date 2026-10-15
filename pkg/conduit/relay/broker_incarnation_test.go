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
// broker's history and a return after the window are not flaps.
func TestBrokerIncarnationFlapMemory(t *testing.T) {
	t0 := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	var b brokerIncarnations
	for i, step := range []struct {
		broker, inc string
		at          time.Duration
		flap        bool
	}{
		{"b1", "inc-1", 0, false},
		{"b1", "inc-1", time.Minute, false},        // same incarnation again
		{"b1", "inc-2", 2 * time.Minute, false},    // a new incarnation
		{"b2", "inc-1", 3 * time.Minute, false},    // another broker's history is separate
		{"b1", "inc-1", 4 * time.Minute, true},     // back to inc-1 within the window
		{"b1", "inc-2", 5 * time.Minute, true},     // and back to inc-2
		{"b1", "inc-2", 6 * time.Minute, false},    // a reconnect of the current one
		{"b1", "inc-3", 6*time.Minute + 1, false},  // a new incarnation
		{"b1", "inc-1", 15*time.Minute + 1, false}, // inc-1 last seen at 4m: past the window
		{"b1", "inc-3", 16*time.Minute + 1, true},  // inc-3 seen 10m ago: still within it
		{"b1", "inc-2", 30 * time.Minute, false},   // every entry is past the window
		{"b2", "inc-2", 30*time.Minute + 1, false}, // b2's history was forgotten
	} {
		if got := b.admit(step.broker, step.inc, t0.Add(step.at)); got != step.flap {
			t.Fatalf("step %d (%s %s at %v): flap = %v, want %v", i, step.broker, step.inc, step.at, got, step.flap)
		}
	}
}
