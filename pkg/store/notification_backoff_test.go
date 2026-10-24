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

import (
	"math"
	"testing"
	"time"
)

// The retry backoff starts at 5 minutes, doubles per attempt and stops at
// one hour (ptone/scion#5763).
func TestNotificationRetryBackoff(t *testing.T) {
	cases := []struct {
		attempts int
		want     time.Duration
	}{
		{-1, 5 * time.Minute},
		{0, 5 * time.Minute},
		{1, 5 * time.Minute},
		{2, 10 * time.Minute},
		{3, 20 * time.Minute},
		{4, 40 * time.Minute},
		{5, time.Hour},
		{6, time.Hour},
		{10, time.Hour},
		{100, time.Hour},
		{math.MaxInt32, time.Hour},
	}
	for _, tc := range cases {
		if got := NotificationRetryBackoff(tc.attempts); got != tc.want {
			t.Errorf("NotificationRetryBackoff(%d) = %v, want %v", tc.attempts, got, tc.want)
		}
	}
}

// The schedule never decreases and never exceeds the cap.
func TestNotificationRetryBackoff_MonotonicAndCapped(t *testing.T) {
	prev := time.Duration(0)
	for n := 1; n <= 64; n++ {
		got := NotificationRetryBackoff(n)
		if got < prev {
			t.Fatalf("NotificationRetryBackoff(%d) = %v, below the previous %v", n, got, prev)
		}
		if got > NotificationRetryBackoffMax {
			t.Fatalf("NotificationRetryBackoff(%d) = %v, above the cap %v", n, got, NotificationRetryBackoffMax)
		}
		prev = got
	}
}

// Every reason constant is accepted, and nothing else is.
func TestValidNotificationReason(t *testing.T) {
	for _, r := range []string{
		NotificationReasonExpired,
		NotificationReasonSubscriptionGone,
		NotificationReasonSubscriberGone,
		NotificationReasonWatchedAgentGone,
	} {
		if !ValidNotificationReason(r) {
			t.Errorf("ValidNotificationReason(%q) = false, want true", r)
		}
	}
	for _, r := range []string{"", "suspended", "EXPIRED", NotificationOutcomeFailed} {
		if ValidNotificationReason(r) {
			t.Errorf("ValidNotificationReason(%q) = true, want false", r)
		}
	}
}
