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
	"context"
	"sync"
	"testing"
)

// renewalRecord is one OnStreamRenewal call.
type renewalRecord struct {
	streamID   uint32
	credential string
	welcome    bool
}

// TestOnStreamRenewalReceivesRenewals: on the target side each
// AuthRefresh{stream_id, credential} the relay sends reaches
// Config.OnStreamRenewal with the stream id, the credential bytes and the
// session context (which carries the Welcome). A stream's Renew
// (StreamRenewer) sends exactly that frame. The sessions and the stream
// stay up (design v2.21.17 §3.5, "Target-side deadlines").
func TestOnStreamRenewalReceivesRenewals(t *testing.T) {
	in := make(chan Stream, 1)
	var mu sync.Mutex
	var got []renewalRecord
	p := newPair(t, Config{
		StreamHandler: acceptAll(in),
		OnStreamRenewal: func(ctx context.Context, streamID uint32, credential []byte) {
			mu.Lock()
			defer mu.Unlock()
			got = append(got, renewalRecord{streamID, string(credential), WelcomeFromContext(ctx) != nil})
		},
	}, Config{})
	rs, err := p.relay.OpenStream(context.Background(), tcpOpen())
	if err != nil {
		t.Fatal(err)
	}
	_ = recvStream(t, in)

	r, ok := rs.(StreamRenewer)
	if !ok {
		t.Fatal("a relay-opened stream does not implement StreamRenewer")
	}
	if err := r.Renew([]byte("renewal-1")); err != nil {
		t.Fatal(err)
	}
	if err := p.relay.RefreshAuth(nil, rs.ID()); err != nil { // a notice without a grant
		t.Fatal(err)
	}
	eventually(t, "the target received both renewals", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(got) == 2
	})
	mu.Lock()
	defer mu.Unlock()
	want := []renewalRecord{{rs.ID(), "renewal-1", true}, {rs.ID(), "", true}}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("renewal %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	if p.dialer.isDone() || p.relay.isDone() {
		t.Fatalf("a session ended: dialer %v, relay %v", p.dialer.Err(), p.relay.Err())
	}
}
