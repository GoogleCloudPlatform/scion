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
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

var (
	testSessionSecret = []byte("session-secret-0123456789abcdefghijklmn")
	testPeerSecret    = []byte("peer-secret-0123456789abcdefghijklmnopqr")
)

func peerSecretAuth(t *testing.T, secret []byte, self string, now func() time.Time) *HMACPeerAuth {
	t.Helper()
	a, err := NewHMACPeerAuthFromPeerSecret(HMACPeerAuthConfig{Secret: append([]byte(nil), secret...), SelfID: self, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func signedRequest(t *testing.T, signer *HMACPeerAuth) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/internal/v1/hub/admit", nil)
	SetPeerTarget(req, "hub", 0)
	if err := signer.Sign(req); err != nil {
		t.Fatal(err)
	}
	return req
}

// TestPeerSecretEmptyRefused: the constructor refuses an empty secret (the
// strength rules are config.ValidatePeerSecret's, applied by callers).
func TestPeerSecretEmptyRefused(t *testing.T) {
	if _, err := NewHMACPeerAuthFromPeerSecret(HMACPeerAuthConfig{SelfID: "r"}); err == nil {
		t.Fatal("an empty peer secret was accepted")
	}
	if _, err := NewHMACPeerAuthFromPeerSecret(HMACPeerAuthConfig{Secret: testPeerSecret, SelfID: "r"}); err != nil {
		t.Fatalf("a valid peer secret: %v", err)
	}
}

// TestPeerSecretSignerRoundTripAndReplay (criterion 30): a request signed
// with the peer-secret key verifies once; the same request again, or one
// whose timestamp is outside the window, is refused.
func TestPeerSecretSignerRoundTripAndReplay(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	clock := func() time.Time { return now }
	signer := peerSecretAuth(t, testPeerSecret, "relay-a", clock)
	verifier := peerSecretAuth(t, testPeerSecret, "hub", clock)

	req := signedRequest(t, signer)
	if peer, err := verifier.Verify(req); err != nil || peer != "relay-a" {
		t.Fatalf("first Verify = %q, %v", peer, err)
	}
	if _, err := verifier.Verify(req); !errors.Is(err, ErrPeerUnauthenticated) || !strings.Contains(err.Error(), "replayed nonce") {
		t.Fatalf("replayed Verify err = %v, want a replayed-nonce refusal", err)
	}

	old := time.Unix(1_800_000_000-61, 0)
	stale := signedRequest(t, peerSecretAuth(t, testPeerSecret, "relay-a", func() time.Time { return old }))
	if _, err := verifier.Verify(stale); !errors.Is(err, ErrPeerUnauthenticated) {
		t.Fatalf("Verify of a request 61s old: %v, want a refusal", err)
	}
}

// TestPeerSecretIndependentOfSessionSecret (design §3.9): the relay-peer
// key is derived only from the peer secret, under its own label. A
// session-secret holder cannot produce a signature the peer-secret
// verifier accepts (neither through the in-process derivation nor through
// the peer-secret derivation of its own secret), the reverse holds too,
// and the same input under the two derivations gives two different keys.
func TestPeerSecretIndependentOfSessionSecret(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	clock := func() time.Time { return now }
	peerVerifier := peerSecretAuth(t, testPeerSecret, "hub", clock)
	sessionVerifier, err := NewHMACPeerAuthFromSecret(HMACPeerAuthConfig{Secret: append([]byte(nil), testSessionSecret...), SelfID: "hub", Now: clock})
	if err != nil {
		t.Fatal(err)
	}

	fromSession, err := NewHMACPeerAuthFromSecret(HMACPeerAuthConfig{Secret: append([]byte(nil), testSessionSecret...), SelfID: "relay-a", Now: clock})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := peerVerifier.Verify(signedRequest(t, fromSession)); !errors.Is(err, ErrPeerUnauthenticated) {
		t.Fatalf("session-secret signature accepted by the peer-secret verifier: %v", err)
	}
	if _, err := peerVerifier.Verify(signedRequest(t, peerSecretAuth(t, testSessionSecret, "relay-a", clock))); !errors.Is(err, ErrPeerUnauthenticated) {
		t.Fatalf("a signature keyed from the session secret through the peer-secret derivation was accepted: %v", err)
	}
	if _, err := sessionVerifier.Verify(signedRequest(t, peerSecretAuth(t, testPeerSecret, "relay-a", clock))); !errors.Is(err, ErrPeerUnauthenticated) {
		t.Fatalf("peer-secret signature accepted by the session-secret verifier: %v", err)
	}

	sameInputSession, err := NewHMACPeerAuthFromSecret(HMACPeerAuthConfig{Secret: append([]byte(nil), testPeerSecret...), SelfID: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(sameInputSession.key, peerSecretAuth(t, testPeerSecret, "x", clock).key) {
		t.Fatal("the in-process and peer-secret derivations share a label: the same input gives the same key")
	}
}
