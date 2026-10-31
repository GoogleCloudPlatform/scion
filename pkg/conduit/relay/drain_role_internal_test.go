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

// TestServeDrain_RefusesRelayRole: a drain asserting a peer enrolled as a
// relay is refused, even when ingress_instance names that peer; only a hub
// drains its ingress sessions.
func TestServeDrain_RefusesRelayRole(t *testing.T) {
	now := func() time.Time { return time.Unix(1_700_000_000, 0) }
	a := newTestPeer(t, "relay-a", "a1", PeerRoleRelay)
	hub := newTestPeer(t, "hub-0", "h1", PeerRoleHub)
	r := &Relay{cfg: Config{PeerAuth: keyedAuth(t, hub.id, hub.priv, testDirectory(t, a, hub), now)}}
	body := `{"ingress_instance":"relay-a"}`
	sum := sha256.Sum256([]byte(body))
	req := httptest.NewRequest(http.MethodPost, InternalPathPrefix+"drain", strings.NewReader(body))
	req.Header.Set(HeaderBodySHA256, hex.EncodeToString(sum[:]))
	rec := httptest.NewRecorder()
	r.serveDrain(rec, req, a.id)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("drain by a relay-enrolled peer: %d %s, want 403", rec.Code, rec.Body.String())
	}
}
