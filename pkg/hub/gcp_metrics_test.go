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
	"time"
)

func TestGCPTokenMetrics_AccessToken(t *testing.T) {
	m := NewGCPTokenMetrics()

	m.RecordAccessTokenRequest(true, 100*time.Millisecond)
	m.RecordAccessTokenRequest(true, 200*time.Millisecond)
	m.RecordAccessTokenRequest(false, 50*time.Millisecond)

	snap := m.GetSnapshot()
	if snap.AccessTokenRequests != 3 {
		t.Errorf("expected 3 requests, got %d", snap.AccessTokenRequests)
	}
	if snap.AccessTokenSuccesses != 2 {
		t.Errorf("expected 2 successes, got %d", snap.AccessTokenSuccesses)
	}
	if snap.AccessTokenFailures != 1 {
		t.Errorf("expected 1 failure, got %d", snap.AccessTokenFailures)
	}
	if snap.IAMLatencyP50Ms == 0 {
		t.Error("expected non-zero P50 latency")
	}
}

func TestGCPTokenMetrics_IDToken(t *testing.T) {
	m := NewGCPTokenMetrics()

	m.RecordIDTokenRequest(true, 150*time.Millisecond)
	m.RecordRateLimitRejection()

	snap := m.GetSnapshot()
	if snap.IDTokenRequests != 1 {
		t.Errorf("expected 1 ID token request, got %d", snap.IDTokenRequests)
	}
	if snap.RateLimitRejections != 1 {
		t.Errorf("expected 1 rate limit rejection, got %d", snap.RateLimitRejections)
	}
}
