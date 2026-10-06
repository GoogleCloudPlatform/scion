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

package conduit

import (
	"math/rand/v2"
	"testing"
	"time"

	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
)

// TestGoAwayRedialSpread: when a relay drains, the redial delays of its
// dialers spread across the whole reconnect window instead of clustering.
// N=200 simulated dialers, each with its own seeded RNG (deterministic),
// receive the same planned GoAway. Over 10 equal buckets of the window
// (20 dialers each on average), every bucket is populated, no bucket holds
// more than 40 dialers (twice the uniform average), and no delay exceeds
// the window. Wider windows, including one above the backoff maximum,
// are honoured the same way.
func TestGoAwayRedialSpread(t *testing.T) {
	const (
		dialers   = 200
		buckets   = 10
		maxBucket = 2 * dialers / buckets
	)
	for _, window := range []time.Duration{5 * time.Second, 30 * time.Second, 90 * time.Second, MaxReconnectWindow} {
		t.Run(window.String(), func(t *testing.T) {
			ga := &conduitv1.GoAway{Code: CloseRelayRestart, ReconnectAfterMs: uint32(window / time.Millisecond)}
			var counts [buckets]int
			for i := range dialers {
				rng := rand.New(rand.NewPCG(uint64(i), 0x5eed))
				bo := &Backoff{Rand: rng.Int64N}
				d := redialDelay(ga, MinPlannedDrainLife, bo)
				if d < 0 || d > window {
					t.Fatalf("dialer %d: delay %v outside [0, %v]", i, d, window)
				}
				b := int(int64(d) * buckets / (int64(window) + 1))
				counts[b]++
			}
			for b, n := range counts {
				if n == 0 {
					t.Errorf("bucket %d of %v is empty: redials cluster (counts %v)", b, window, counts)
				}
				if n > maxBucket {
					t.Errorf("bucket %d of %v holds %d dialers, above %d (counts %v)", b, window, n, maxBucket, counts)
				}
			}
		})
	}
}

// TestGoAwayWindowAboveCapIsCapped: a reconnect window above
// MaxReconnectWindow is drawn from [0, MaxReconnectWindow].
func TestGoAwayWindowAboveCapIsCapped(t *testing.T) {
	ga := &conduitv1.GoAway{Code: CloseRelayRestart, ReconnectAfterMs: uint32((2 * MaxReconnectWindow) / time.Millisecond)}
	top := &Backoff{Rand: func(n int64) int64 { return n - 1 }}
	if d := redialDelay(ga, MinPlannedDrainLife, top); d != MaxReconnectWindow {
		t.Fatalf("delay %v, want the %v cap", d, MaxReconnectWindow)
	}
}
