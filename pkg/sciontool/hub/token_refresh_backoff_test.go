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

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/clock"
)

// TestTokenRefreshLoop_BackoffSequence pins the refresh loop's retry
// delays: consecutive failures wait 30s, 1m, 2m, 4m, then 5m (the cap)
// with no jitter, and a success resets the next failure to 30s.
func TestTokenRefreshLoop_BackoffSequence(t *testing.T) {
	start := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	clk := clock.NewFake(start)
	h := newKickHub(t, clk)
	want := []time.Duration{
		30 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute,
		5 * time.Minute, 5 * time.Minute,
	}
	for n := 1; n <= len(want); n++ {
		h.fail[n] = true
	}
	ok := len(want) + 1
	h.fail[ok+1] = true
	l := startKickLoop(t, h)

	l.c.KickTokenRefresh()
	now := start
	for i, d := range want {
		h.waitStarted(i + 1)
		l.nextWait(now.Add(d), false, 0)
		clk.Advance(d)
		now = now.Add(d)
	}

	h.waitStarted(ok)
	l.waitRefreshed()
	if got := h.arrivedAt(ok); !got.Equal(now) {
		t.Fatalf("request %d arrived at %v, want %v", ok, got, now)
	}
	l.nextWait(start.Add(time.Duration(10+ok)*time.Hour-2*time.Hour), true, 0)

	l.c.KickTokenRefresh()
	h.waitStarted(ok + 1)
	l.nextWait(now.Add(tokenRefreshRetryBaseDelay), false, 0)
}
