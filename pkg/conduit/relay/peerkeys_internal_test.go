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
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// testPeer is one enrolled node in these tests.
type testPeer struct {
	id   string
	kid  string
	role PeerRole
	pub  ed25519.PublicKey
	priv ed25519.PrivateKey
}

func newTestPeer(t *testing.T, id, kid string, role PeerRole) testPeer {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return testPeer{id: id, kid: kid, role: role, pub: pub, priv: priv}
}

func testDirectory(t *testing.T, peers ...testPeer) *PeerDirectory {
	t.Helper()
	var es []PeerEnrollment
	for _, p := range peers {
		es = append(es, PeerEnrollment{ID: p.id, Role: p.role, Keys: []PeerKey{{ID: p.kid, PublicKey: p.pub}}})
	}
	d, err := NewPeerDirectory(es)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// keyedAuth is the authenticator of node self: signing with key (nil: the
// legacy signature) and verifying against dir.
func keyedAuth(t *testing.T, self string, key ed25519.PrivateKey, dir *PeerDirectory, now func() time.Time) *KeyedPeerAuth {
	t.Helper()
	a, err := NewKeyedPeerAuth(KeyedPeerAuthConfig{Legacy: peerSecretAuth(t, testPeerSecret, self, now), Directory: dir, Key: key})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func keyedRequest(t *testing.T, signer PeerAuth) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/internal/v1/hub/admit", nil)
	SetPeerTarget(req, "hub", 0)
	if err := signer.Sign(req); err != nil {
		t.Fatal(err)
	}
	return req
}

// forgeV3 signs req as asserted with key under kid: a per-peer key
// signature a node holding key makes for another peer's id.
func forgeV3(t *testing.T, req *http.Request, asserted, kid string, key ed25519.PrivateKey) {
	t.Helper()
	ts := "0"
	if v := req.Header.Get(HeaderPeerTimestamp); v != "" {
		ts = v
	}
	nonce := "forged-nonce-0001"
	req.Header.Set(HeaderPeerID, asserted)
	req.Header.Set(HeaderPeerTimestamp, ts)
	req.Header.Set(HeaderPeerNonce, nonce)
	req.Header.Set(HeaderPeerKeyID, kid)
	req.Header.Set(HeaderPeerSignatureVersion, peerSignatureVersion3)
	sig := ed25519.Sign(key, peerV3Input(kid, canonical(req, asserted, ts, nonce)))
	req.Header.Set(HeaderPeerSignature, base64.RawURLEncoding.EncodeToString(sig))
}

// TestKeyedPeerAuth_BindsIDToKey: a per-peer key signature verifies only
// for the id the key is enrolled under. Peer A's key never authenticates a
// request asserting peer B.
func TestKeyedPeerAuth_BindsIDToKey(t *testing.T) {
	now := func() time.Time { return time.Unix(1_700_000_000, 0) }
	a := newTestPeer(t, "relay-a", "a1", PeerRoleRelay)
	b := newTestPeer(t, "relay-b", "b1", PeerRoleRelay)
	hub := newTestPeer(t, "hub-0", "h1", PeerRoleHub)
	dir := testDirectory(t, a, b, hub)
	verifier := keyedAuth(t, hub.id, hub.priv, dir, now)

	t.Run("own key round trip", func(t *testing.T) {
		req := keyedRequest(t, keyedAuth(t, a.id, a.priv, dir, now))
		if req.Header.Get(HeaderPeerSignatureVersion) != peerSignatureVersion3 || req.Header.Get(HeaderPeerKeyID) != a.kid {
			t.Fatalf("not a per-peer key signature: version %q kid %q", req.Header.Get(HeaderPeerSignatureVersion), req.Header.Get(HeaderPeerKeyID))
		}
		peer, err := verifier.Verify(req)
		if err != nil || peer != a.id {
			t.Fatalf("Verify = %q, %v; want %q", peer, err, a.id)
		}
	})
	refused := func(t *testing.T, req *http.Request) {
		t.Helper()
		if peer, err := verifier.Verify(req); !errors.Is(err, ErrPeerUnauthenticated) {
			t.Fatalf("Verify = %q, %v; want refused", peer, err)
		}
	}
	t.Run("A's key and kid asserting B", func(t *testing.T) {
		req := keyedRequest(t, keyedAuth(t, a.id, a.priv, dir, now))
		forgeV3(t, req, b.id, a.kid, a.priv)
		refused(t, req)
	})
	t.Run("A's key under B's kid", func(t *testing.T) {
		req := keyedRequest(t, keyedAuth(t, a.id, a.priv, dir, now))
		forgeV3(t, req, b.id, b.kid, a.priv)
		refused(t, req)
	})
	t.Run("A's key asserting the hub", func(t *testing.T) {
		req := keyedRequest(t, keyedAuth(t, a.id, a.priv, dir, now))
		forgeV3(t, req, hub.id, hub.kid, a.priv)
		refused(t, req)
	})
	t.Run("unknown kid", func(t *testing.T) {
		req := keyedRequest(t, keyedAuth(t, a.id, a.priv, dir, now))
		forgeV3(t, req, a.id, "zz", a.priv)
		refused(t, req)
	})
	t.Run("unenrolled id", func(t *testing.T) {
		c := newTestPeer(t, "relay-c", "c1", PeerRoleRelay)
		req := keyedRequest(t, keyedAuth(t, a.id, a.priv, dir, now))
		forgeV3(t, req, c.id, c.kid, c.priv)
		refused(t, req)
	})
	t.Run("tampered request", func(t *testing.T) {
		req := keyedRequest(t, keyedAuth(t, a.id, a.priv, dir, now))
		req.Header.Set(HeaderPeerTarget, "relay-b")
		refused(t, req)
	})
}

// TestKeyedPeerAuth_SkewAndReplay: per-peer key signatures share the skew
// window and the replay cache with the legacy format.
func TestKeyedPeerAuth_SkewAndReplay(t *testing.T) {
	at := time.Unix(1_700_000_000, 0)
	now := func() time.Time { return at }
	a := newTestPeer(t, "relay-a", "a1", PeerRoleRelay)
	hub := newTestPeer(t, "hub-0", "h1", PeerRoleHub)
	dir := testDirectory(t, a, hub)
	verifier := keyedAuth(t, hub.id, hub.priv, dir, now)
	signer := keyedAuth(t, a.id, a.priv, dir, now)

	req := keyedRequest(t, signer)
	if _, err := verifier.Verify(req); err != nil {
		t.Fatalf("first use: %v", err)
	}
	if _, err := verifier.Verify(req); !errors.Is(err, ErrPeerUnauthenticated) {
		t.Fatalf("replay accepted: %v", err)
	}
	stale := keyedAuth(t, a.id, a.priv, dir, func() time.Time { return at.Add(-2 * DefaultPeerMaxSkew) })
	if _, err := verifier.Verify(keyedRequest(t, stale)); !errors.Is(err, ErrPeerUnauthenticated) {
		t.Fatalf("a timestamp outside the skew window was accepted: %v", err)
	}
}

// TestKeyedPeerAuth_LegacyUnchanged: a node without a key signs as before,
// and legacy signatures still verify (the opt-in step; the migration
// window that restricts them comes later).
func TestKeyedPeerAuth_LegacyUnchanged(t *testing.T) {
	now := func() time.Time { return time.Unix(1_700_000_000, 0) }
	hub := newTestPeer(t, "hub-0", "h1", PeerRoleHub)
	dir := testDirectory(t, hub)
	legacySigner := keyedAuth(t, "relay-x", nil, dir, now)
	req := keyedRequest(t, legacySigner)
	if req.Header.Get(HeaderPeerSignatureVersion) == peerSignatureVersion3 {
		t.Fatal("a node without a key signed with a per-peer key")
	}
	peer, err := keyedAuth(t, hub.id, hub.priv, dir, now).Verify(req)
	if err != nil || peer != "relay-x" {
		t.Fatalf("legacy Verify = %q, %v", peer, err)
	}
}

// TestNewKeyedPeerAuth_OwnKeyMustBeEnrolled: a node whose key is not
// enrolled for its own instance id is refused at construction (startup).
func TestNewKeyedPeerAuth_OwnKeyMustBeEnrolled(t *testing.T) {
	now := func() time.Time { return time.Unix(1_700_000_000, 0) }
	a := newTestPeer(t, "relay-a", "a1", PeerRoleRelay)
	b := newTestPeer(t, "relay-b", "b1", PeerRoleRelay)
	dir := testDirectory(t, a, b)
	for name, tc := range map[string]struct {
		self string
		key  ed25519.PrivateKey
	}{
		"B's key on A":    {a.id, b.priv},
		"unenrolled self": {"relay-c", a.priv},
		"unenrolled key":  {a.id, newTestPeer(t, "x", "x1", PeerRoleRelay).priv},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := NewKeyedPeerAuth(KeyedPeerAuthConfig{Legacy: peerSecretAuth(t, testPeerSecret, tc.self, now), Directory: dir, Key: tc.key})
			if err == nil {
				t.Fatal("a key not enrolled for this node was accepted")
			}
		})
	}
}

// TestRequirePeerRole: a peer enrolled with another role is refused; an
// unenrolled peer or an authenticator without enrolled peers is not
// checked.
func TestRequirePeerRole(t *testing.T) {
	now := func() time.Time { return time.Unix(1_700_000_000, 0) }
	a := newTestPeer(t, "relay-a", "a1", PeerRoleRelay)
	hub := newTestPeer(t, "hub-0", "h1", PeerRoleHub)
	auth := keyedAuth(t, hub.id, hub.priv, testDirectory(t, a, hub), now)
	if err := RequirePeerRole(auth, a.id, PeerRoleHub); !errors.Is(err, ErrPeerRole) {
		t.Fatalf("a relay passed a hub-only call: %v", err)
	}
	if err := RequirePeerRole(auth, hub.id, PeerRoleRelay); !errors.Is(err, ErrPeerRole) {
		t.Fatalf("a hub passed a relay-only call: %v", err)
	}
	if err := RequirePeerRole(auth, a.id, PeerRoleRelay); err != nil {
		t.Fatalf("a relay refused a relay call: %v", err)
	}
	if err := RequirePeerRole(auth, "relay-unenrolled", PeerRoleHub); err != nil {
		t.Fatalf("an unenrolled peer was checked: %v", err)
	}
	if err := RequirePeerRole(peerSecretAuth(t, testPeerSecret, "x", now), a.id, PeerRoleHub); err != nil {
		t.Fatalf("an authenticator without enrolled peers checked a role: %v", err)
	}
}

// TestNewPeerDirectory_Refusals: malformed enrollments are refused.
func TestNewPeerDirectory_Refusals(t *testing.T) {
	a := newTestPeer(t, "relay-a", "a1", PeerRoleRelay)
	k := []PeerKey{{ID: "a1", PublicKey: a.pub}}
	for name, es := range map[string][]PeerEnrollment{
		"no id":        {{Role: PeerRoleRelay, Keys: k}},
		"duplicate id": {{ID: "x", Role: PeerRoleRelay, Keys: k}, {ID: "x", Role: PeerRoleHub, Keys: k}},
		"bad role":     {{ID: "x", Role: "admin", Keys: k}},
		"no keys":      {{ID: "x", Role: PeerRoleRelay}},
		"bad kid":      {{ID: "x", Role: PeerRoleRelay, Keys: []PeerKey{{ID: "a b", PublicKey: a.pub}}}},
		"dup kid":      {{ID: "x", Role: PeerRoleRelay, Keys: []PeerKey{{ID: "a1", PublicKey: a.pub}, {ID: "a1", PublicKey: a.pub}}}},
		"short key":    {{ID: "x", Role: PeerRoleRelay, Keys: []PeerKey{{ID: "a1", PublicKey: a.pub[:8]}}}},
	} {
		if _, err := NewPeerDirectory(es); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// TestPeerKeyOptions: the settings are decoded, a key file needs a
// configured instance id, and bad input is refused without key material
// in the error.
func TestPeerKeyOptions(t *testing.T) {
	a := newTestPeer(t, "relay-a", "a1", PeerRoleRelay)
	der, err := x509.MarshalPKCS8PrivateKey(a.priv)
	if err != nil {
		t.Fatal(err)
	}
	keyFile := filepath.Join(t.TempDir(), "peer.key")
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	peers := []config.HubConduitPeer{{ID: a.id, Role: "relay", Keys: []config.HubConduitPeerKey{{KeyID: a.kid, PublicKey: base64.StdEncoding.EncodeToString(a.pub)}}}}

	es, key, err := PeerKeyOptions(config.HubConduitConfig{InstanceID: a.id, PeerKeyFile: keyFile, Peers: peers})
	if err != nil || len(es) != 1 || !key.Equal(a.priv) {
		t.Fatalf("PeerKeyOptions = %d peers, key set %v, %v", len(es), key != nil, err)
	}
	if _, _, err := PeerKeyOptions(config.HubConduitConfig{PeerKeyFile: keyFile, Peers: peers}); err == nil {
		t.Error("a key file without a configured instance id was accepted")
	}
	if _, _, err := PeerKeyOptions(config.HubConduitConfig{InstanceID: a.id, PeerKeyFile: keyFile + ".missing", Peers: peers}); err == nil {
		t.Error("a missing key file was accepted")
	}
	bad := []config.HubConduitPeer{{ID: a.id, Role: "relay", Keys: []config.HubConduitPeerKey{{KeyID: a.kid, PublicKey: "not-base64!"}}}}
	if _, _, err := PeerKeyOptions(config.HubConduitConfig{Peers: bad}); err == nil {
		t.Error("a malformed public key was accepted")
	}
	if es, key, err := PeerKeyOptions(config.HubConduitConfig{}); err != nil || es != nil || key != nil {
		t.Errorf("no settings: %v, %v, %v", es, key, err)
	}
}

// TestNewConduitPeerAuth_Keyed: enrolled peers make the authenticator a
// KeyedPeerAuth (in hmac mode, and inside the oidc wrapper, which forwards
// roles).
func TestNewConduitPeerAuth_Keyed(t *testing.T) {
	a := newTestPeer(t, "relay-a", "a1", PeerRoleRelay)
	hub := newTestPeer(t, "hub-0", "h1", PeerRoleHub)
	peers := []PeerEnrollment{
		{ID: a.id, Role: a.role, Keys: []PeerKey{{ID: a.kid, PublicKey: a.pub}}},
		{ID: hub.id, Role: hub.role, Keys: []PeerKey{{ID: hub.kid, PublicKey: hub.pub}}},
	}
	auth, _, err := NewConduitPeerAuth(ConduitPeerAuthOptions{Mode: config.ConduitPeerAuthHMAC, SelfID: a.id, PeerSecret: string(testPeerSecret), Peers: peers, PeerKey: a.priv})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := auth.(*KeyedPeerAuth); !ok {
		t.Fatalf("auth is %T, want *KeyedPeerAuth", auth)
	}
	if _, _, err := NewConduitPeerAuth(ConduitPeerAuthOptions{Mode: config.ConduitPeerAuthHMAC, SelfID: a.id, PeerSecret: string(testPeerSecret), Peers: peers, PeerKey: hub.priv}); err == nil {
		t.Fatal("a key enrolled for another id was accepted at startup")
	}
	wrapped := &signedOIDCPeerAuth{signer: auth}
	if role, ok := wrapped.PeerRole(hub.id); !ok || role != PeerRoleHub {
		t.Fatalf("oidc wrapper PeerRole = %q, %v", role, ok)
	}
}
