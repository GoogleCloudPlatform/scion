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
	"bytes"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"
)

// nonceStep is one Verify call in a replay-cache scenario. Offsets are
// from a fixed t0; the verifier's MaxSkew is 60s, so an entry expires
// 120s after its request timestamp.
type nonceStep struct {
	name    string
	now     time.Duration // verifier's current time
	at      time.Duration // request timestamp (a new request)
	replay  int           // 1-based step whose request is sent again; 0 signs a new one
	wantErr string        // "" means admitted
	cached  []int         // steps whose nonces are cached afterwards
}

func runNonceSteps(t *testing.T, maxNonces int, steps []nonceStep) {
	t.Helper()
	key := bytes.Repeat([]byte{7}, 32)
	t0 := time.Unix(1_800_000_000, 0)
	var now time.Time
	v, err := newHMACPeerAuthWithKey(HMACPeerAuthConfig{SelfID: "relay-b", MaxSkew: 60 * time.Second, MaxNonces: maxNonces, Now: func() time.Time { return now }}, key)
	if err != nil {
		t.Fatal(err)
	}
	reqs := make([]*http.Request, len(steps))
	for i, st := range steps {
		now = t0.Add(st.now)
		if st.replay > 0 {
			reqs[i] = reqs[st.replay-1]
		} else {
			at := t0.Add(st.at)
			signer, err := newHMACPeerAuthWithKey(HMACPeerAuthConfig{SelfID: "relay-a", Now: func() time.Time { return at }}, key)
			if err != nil {
				t.Fatal(err)
			}
			reqs[i] = httptest.NewRequest(http.MethodPost, "/internal/streams", nil)
			if err := signer.Sign(reqs[i]); err != nil {
				t.Fatal(err)
			}
		}
		_, err := v.Verify(reqs[i])
		switch {
		case st.wantErr == "" && err != nil:
			t.Fatalf("step %d (%s): Verify: %v", i+1, st.name, err)
		case st.wantErr != "" && (err == nil || !strings.Contains(err.Error(), st.wantErr)):
			t.Fatalf("step %d (%s): Verify err = %v, want %q", i+1, st.name, err, st.wantErr)
		}
		var got []int
		for j := 0; j <= i; j++ {
			if steps[j].replay > 0 {
				continue
			}
			if _, ok := v.nonces["relay-a\x00"+reqs[j].Header.Get(HeaderPeerNonce)]; ok {
				got = append(got, j+1)
			}
		}
		if !slices.Equal(got, st.cached) {
			t.Fatalf("step %d (%s): cached steps %v, want %v", i+1, st.name, got, st.cached)
		}
		if len(v.expiry) != len(v.nonces) {
			t.Fatalf("step %d (%s): %d queued entries for %d cached nonces", i+1, st.name, len(v.expiry), len(v.nonces))
		}
	}
}

// TestHMACPeerAuthNonceEvictsOnlyExpiredAtCap: entries are evicted only
// when the cache is at its cap, and then only those whose expiry has
// passed (an entry at exactly its expiry stays), in expiry order whatever
// order they were added in. Below the cap an expired entry stays.
func TestHMACPeerAuthNonceEvictsOnlyExpiredAtCap(t *testing.T) {
	runNonceSteps(t, 3, []nonceStep{
		{name: "expires at 170s", now: 0, at: 50 * time.Second, cached: []int{1}},
		{name: "expires at 70s", now: 0, at: -50 * time.Second, cached: []int{1, 2}},
		{name: "below the cap the expired 70s entry stays", now: 100 * time.Second, at: 100 * time.Second, cached: []int{1, 2, 3}},
		{name: "at the cap only the 70s entry is evicted", now: 110 * time.Second, at: 110 * time.Second, cached: []int{1, 3, 4}},
		{name: "at exactly 170s the 170s entry stays", now: 170 * time.Second, at: 170 * time.Second, wantErr: "replay cache full", cached: []int{1, 3, 4}},
		{name: "at 180s the 170s entry is evicted", now: 180 * time.Second, at: 180 * time.Second, cached: []int{3, 4, 6}},
	})
}

// TestHMACPeerAuthNonceCacheFullOfLiveEntries: at the cap with no expired
// entry, a new nonce is refused and every cached entry is kept, including
// the one closest to expiry.
func TestHMACPeerAuthNonceCacheFullOfLiveEntries(t *testing.T) {
	runNonceSteps(t, 2, []nonceStep{
		{name: "first, expires at 120s", now: 0, at: 0, cached: []int{1}},
		{name: "second fills the cache", now: 100 * time.Second, at: 100 * time.Second, cached: []int{1, 2}},
		{name: "full, nothing expired", now: 110 * time.Second, at: 110 * time.Second, wantErr: "replay cache full", cached: []int{1, 2}},
		{name: "still full at exactly 120s", now: 120 * time.Second, at: 120 * time.Second, wantErr: "replay cache full", cached: []int{1, 2}},
		{name: "replay of a cached nonce", now: 120 * time.Second, replay: 2, wantErr: "replayed nonce", cached: []int{1, 2}},
	})
}

// TestHMACPeerAuthNonceCacheFullWithExpiredEntry: at the cap, an expired
// entry is removed and its slot is used for the new nonce; the unexpired
// entries are kept and their replays are still refused.
func TestHMACPeerAuthNonceCacheFullWithExpiredEntry(t *testing.T) {
	runNonceSteps(t, 2, []nonceStep{
		{name: "first, expires at 120s", now: 0, at: 0, cached: []int{1}},
		{name: "second fills the cache", now: 100 * time.Second, at: 100 * time.Second, cached: []int{1, 2}},
		{name: "first has expired: its slot is used", now: 130 * time.Second, at: 130 * time.Second, cached: []int{2, 3}},
		{name: "replay of the kept entry", now: 130 * time.Second, replay: 2, wantErr: "replayed nonce", cached: []int{2, 3}},
		{name: "full again with live entries", now: 130 * time.Second, at: 130 * time.Second, wantErr: "replay cache full", cached: []int{2, 3}},
	})
}

// TestHMACPeerAuthReplayAfterEvictionRefusedBySkew: once a nonce has been
// evicted, resending the same request is refused by the timestamp check,
// so eviction never makes a seen request acceptable again.
func TestHMACPeerAuthReplayAfterEvictionRefusedBySkew(t *testing.T) {
	runNonceSteps(t, 2, []nonceStep{
		{name: "admitted, expires at 120s", now: 0, at: 0, cached: []int{1}},
		{name: "second fills the cache", now: 100 * time.Second, at: 100 * time.Second, cached: []int{1, 2}},
		{name: "first is evicted at the cap", now: 130 * time.Second, at: 130 * time.Second, cached: []int{2, 3}},
		{name: "first request again", now: 130 * time.Second, replay: 1, wantErr: "timestamp outside the allowed skew", cached: []int{2, 3}},
	})
}
