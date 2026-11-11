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
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// TestNoPeerAuthRefusesBothWays: a node that serves no internal relay API
// neither accepts a relay-peer request nor signs one, and a peer call fails
// before any request is sent.
func TestNoPeerAuthRefusesBothWays(t *testing.T) {
	var a PeerAuth = NoPeerAuth{}
	req := httptest.NewRequest(http.MethodGet, "/internal/v1/conduit/self", nil)

	err := a.Sign(req)
	if err == nil {
		t.Fatal("Sign returned nil: an unsigned request could be sent")
	}
	if !errors.Is(err, ErrNoInternalRelayAPI) {
		t.Fatalf("Sign error = %v, want ErrNoInternalRelayAPI", err)
	}
	for _, h := range []string{HeaderPeerID, HeaderPeerTimestamp} {
		if req.Header.Get(h) != "" {
			t.Fatalf("Sign set %s", h)
		}
	}

	peer, err := a.Verify(req)
	if !errors.Is(err, ErrPeerUnauthenticated) || peer != "" {
		t.Fatalf("Verify = %q, %v; want no peer and ErrPeerUnauthenticated", peer, err)
	}

	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { hits.Add(1) }))
	defer srv.Close()
	c := &PeerClient{HTTP: srv.Client(), Auth: a}
	if _, err := c.Probe(context.Background(), srv.URL, "relay-b", 1); !errors.Is(err, ErrNoInternalRelayAPI) {
		t.Fatalf("Probe error = %v, want ErrNoInternalRelayAPI", err)
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("%d request(s) reached the peer; want none", n)
	}
}
