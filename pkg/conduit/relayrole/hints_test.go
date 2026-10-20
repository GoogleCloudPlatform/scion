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

package relayrole

import (
	"testing"
	"time"
)

// TestHintSchedule (contracts §3, relay.HubTunnelOpen): usable hints give
// the first re-check at authz_until minus 0.2 of the interval; a missing
// pair is absent; a partial pair or any out-of-range value is invalid.
func TestHintSchedule(t *testing.T) {
	receipt := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	at := func(d time.Duration) int64 { return receipt.Add(d).Unix() }
	for _, tc := range []struct {
		name      string
		until     int64
		intervalS int64
		fallback  string
		first     time.Time
	}{
		{name: "usable", until: at(10 * time.Minute), intervalS: 600, first: receipt.Add(8 * time.Minute)},
		{name: "usable at the skew edge", until: at(10*time.Minute + 30*time.Second), intervalS: 600, first: receipt.Add(8*time.Minute + 30*time.Second)},
		{name: "usable at the bounds", until: at(time.Minute), intervalS: 60, first: receipt.Add(48 * time.Second)},
		{name: "absent", fallback: HintFallbackAbsent},
		{name: "authz_until only", until: at(10 * time.Minute), fallback: HintFallbackInvalid},
		{name: "interval only", intervalS: 600, fallback: HintFallbackInvalid},
		{name: "interval under 1m", until: at(30 * time.Second), intervalS: 59, fallback: HintFallbackInvalid},
		{name: "interval over 7d", until: at(time.Hour), intervalS: 7*24*3600 + 1, fallback: HintFallbackInvalid},
		{name: "negative interval", until: at(time.Hour), intervalS: -600, fallback: HintFallbackInvalid},
		{name: "authz_until at receipt", until: at(0), intervalS: 600, fallback: HintFallbackInvalid},
		{name: "authz_until before receipt", until: at(-time.Minute), intervalS: 600, fallback: HintFallbackInvalid},
		{name: "authz_until past receipt plus interval plus skew", until: at(10*time.Minute + 31*time.Second), intervalS: 600, fallback: HintFallbackInvalid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			first, interval, fallback := hintSchedule(receipt, tc.until, tc.intervalS)
			if fallback != tc.fallback {
				t.Fatalf("fallback %q, want %q", fallback, tc.fallback)
			}
			if tc.fallback != "" {
				if !first.IsZero() || interval != 0 {
					t.Fatalf("schedule %v every %v with a fallback, want none", first, interval)
				}
				return
			}
			if !first.Equal(tc.first) || interval != time.Duration(tc.intervalS)*time.Second {
				t.Fatalf("first re-check %v every %v, want %v every %ds", first, interval, tc.first, tc.intervalS)
			}
		})
	}
}

// TestHintFallbacksCounted: each fallback is counted under its reason.
func TestHintFallbacksCounted(t *testing.T) {
	s := &StreamScheduler{}
	s.countHintFallback(HintFallbackAbsent)
	s.countHintFallback(HintFallbackInvalid)
	s.countHintFallback(HintFallbackInvalid)
	if a, i := s.HintFallbacks(HintFallbackAbsent), s.HintFallbacks(HintFallbackInvalid); a != 1 || i != 2 {
		t.Fatalf("absent=%d invalid=%d, want 1 and 2", a, i)
	}
	if n := s.HintFallbacks("other"); n != 0 {
		t.Fatalf("unknown reason counted %d, want 0", n)
	}
}
