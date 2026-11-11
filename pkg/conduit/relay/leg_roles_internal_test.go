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
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// legRelay is a relay "relay-x" (generation 0) whose relay-peer auth knows
// relay-a (relay), hub-0 (hub) and itself.
func legRelay(t *testing.T) (r *Relay, a, hub testPeer, dir *PeerDirectory, now func() time.Time) {
	t.Helper()
	now = func() time.Time { return time.Unix(1_700_000_000, 0) }
	a = newTestPeer(t, "relay-a", "a1", PeerRoleRelay)
	hub = newTestPeer(t, "hub-0", "h1", PeerRoleHub)
	self := newTestPeer(t, "relay-x", "x1", PeerRoleRelay)
	dir = testDirectory(t, a, hub, self)
	r = &Relay{cfg: Config{InstanceID: self.id, PeerAuth: keyedAuth(t, self.id, self.priv, dir, now)}}
	return r, a, hub, dir, now
}

// legRequest is a request to relay-x signed by signer, with body.
func legRequest(t *testing.T, signer PeerAuth, method, path, body string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		sum := sha256.Sum256([]byte(body))
		req.Header.Set(HeaderBodySHA256, hex.EncodeToString(sum[:]))
	}
	SetPeerTarget(req, "relay-x", 0)
	if err := signer.Sign(req); err != nil {
		t.Fatal(err)
	}
	if req.Header.Get(HeaderPeerSignatureVersion) != peerSignatureVersion3 {
		t.Fatal("fixture: not a per-peer key signature")
	}
	return req
}

// TestAuthenticateIngressPeer_EnrolledRoles: the ingress leg accepts a
// hub's per-peer key signature and refuses a peer enrolled as a relay.
func TestAuthenticateIngressPeer_EnrolledRoles(t *testing.T) {
	r, a, hub, dir, now := legRelay(t)

	rec := httptest.NewRecorder()
	peer, ok := r.AuthenticateIngressPeer(rec, legRequest(t, keyedAuth(t, hub.id, hub.priv, dir, now), http.MethodGet, "/api/v1/conduit", ""))
	if !ok || peer != hub.id {
		t.Fatalf("hub ingress leg: peer %q ok %v (%d %s), want accepted", peer, ok, rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	if _, ok := r.AuthenticateIngressPeer(rec, legRequest(t, keyedAuth(t, a.id, a.priv, dir, now), http.MethodGet, "/api/v1/conduit", "")); ok || rec.Code != http.StatusForbidden {
		t.Fatalf("relay ingress leg: ok %v, %d; want refused 403", ok, rec.Code)
	}
}

// TestDrain_EnrolledRoles: a drain signed with a hub's per-peer key is
// authenticated and served end to end; the same call from a peer enrolled
// as a relay is refused with 403.
func TestDrain_EnrolledRoles(t *testing.T) {
	r, a, hub, dir, now := legRelay(t)
	drain := func(p testPeer) *httptest.ResponseRecorder {
		t.Helper()
		req := legRequest(t, keyedAuth(t, p.id, p.priv, dir, now), http.MethodPost, InternalPathPrefix+"drain", `{"ingress_instance":"`+p.id+`"}`)
		rec := httptest.NewRecorder()
		peer, ok := r.AuthenticatePeer(rec, req)
		if !ok {
			t.Fatalf("%s: not authenticated: %d %s", p.id, rec.Code, rec.Body.String())
		}
		r.serveDrain(rec, req, peer)
		return rec
	}
	if rec := drain(hub); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "goaway_sent") {
		t.Fatalf("hub drain: %d %s, want 200 with a drain response", rec.Code, rec.Body.String())
	}
	if rec := drain(a); rec.Code != http.StatusForbidden {
		t.Fatalf("relay drain: %d %s, want 403", rec.Code, rec.Body.String())
	}
}
